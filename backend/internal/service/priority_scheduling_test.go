package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func priorityCandidate(id int64, rate float64, load int) openAIAccountCandidateScore {
	return openAIAccountCandidateScore{account: &Account{ID: id, Name: "test", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, RateMultiplier: &rate, Extra: map[string]any{AccountCostMultiplierExtraKey: rate}, Concurrency: 10}, loadKnown: true, loadInfo: &AccountLoadInfo{AccountID: id, LoadRate: load, CurrentConcurrency: load / 10}}
}
func TestPriorityScoringExperienceAndCost(t *testing.T) {
	c := DefaultPrioritySchedulingConfig()
	c.TargetTTFTMs = 3000
	c.Mode = "profit"
	good := withAccountPerformanceFixture(PrioritySchedulingSignal{Samples: 20, P90TTFTMs: 500, QualityPassed: 10, QualitySamples: 10})
	cheap := scorePriorityCandidate(c, priorityCandidate(1, 0.2, 10), good, time.Now())
	expensive := scorePriorityCandidate(c, priorityCandidate(2, 1, 10), good, time.Now())
	require.Greater(t, cheap.Score, expensive.Score)
	require.Equal(t, "eligible", cheap.Tier)
	slow := good
	slow.P90TTFTMs = 5000
	slow = withAccountPerformanceFixture(slow)
	require.Equal(t, "degraded", scorePriorityCandidate(c, priorityCandidate(1, 0, 10), slow, time.Now()).Tier)
	bad := good
	bad.QualityPassed = 5
	bad = withAccountPerformanceFixture(bad)
	require.Equal(t, "degraded", scorePriorityCandidate(c, priorityCandidate(1, 0, 10), bad, time.Now()).Tier)
	require.Equal(t, "degraded", scorePriorityCandidate(c, priorityCandidate(1, 0, 90), good, time.Now()).Tier)
	noLoad := priorityCandidate(1, 0, 0)
	noLoad.loadKnown = false
	require.Equal(t, "eligible", scorePriorityCandidate(c, noLoad, good, time.Now()).Tier)
	missing := scorePriorityCandidate(c, priorityCandidate(1, 0.2, 10), PrioritySchedulingSignal{}, time.Now())
	require.Equal(t, "insufficient", missing.Tier)
	require.Contains(t, missing.Reasons, "latency_insufficient")
}
func TestPriorityConfigValidationAndScope(t *testing.T) {
	c := DefaultPrioritySchedulingConfig()
	require.NoError(t, ValidatePrioritySchedulingConfig(c))
	require.False(t, c.applies(nil, "m"))
	c.Enabled = true
	c.GroupIDs = []int64{5}
	c.Models = []string{"m"}
	id := int64(5)
	require.True(t, c.applies(&id, "m"))
	require.False(t, c.applies(nil, "m"))
	require.False(t, c.applies(&id, "other"))
	for _, mutate := range []func(*PrioritySchedulingConfig){func(c *PrioritySchedulingConfig) { c.Mode = "oops" }, func(c *PrioritySchedulingConfig) { c.TargetTTFTMs = 0 }, func(c *PrioritySchedulingConfig) { c.WindowMinutes = 1441 }, func(c *PrioritySchedulingConfig) { c.CostWeight = math.NaN() }, func(c *PrioritySchedulingConfig) { c.GroupIDs = []int64{-1} }, func(c *PrioritySchedulingConfig) {
		c.QualityWeight = 0
		c.LatencyWeight = 0
		c.LoadWeight = 0
		c.CostWeight = 0
	}} {
		bad := c
		mutate(&bad)
		require.Error(t, ValidatePrioritySchedulingConfig(bad))
	}
}

type priorityReaderStub struct {
	UsageLogRepository
	mu     sync.Mutex
	calls  int
	signal map[int64]PrioritySchedulingSignal
	err    error
}

func (r *priorityReaderStub) ReadPrioritySchedulingSignals(_ context.Context, query PrioritySchedulingQuery) (map[int64]PrioritySchedulingSignal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	// The SQL reader returns one aggregate row per requested account, including
	// explicitly empty history. Keep this test adapter faithful to that contract.
	signals := make(map[int64]PrioritySchedulingSignal, len(query.AccountIDs))
	for _, id := range query.AccountIDs {
		signals[id] = r.signal[id]
	}
	return signals, r.err
}
func priorityGateway(c PrioritySchedulingConfig, r *priorityReaderStub) *OpenAIGatewayService {
	settings := &SettingService{settingRepo: &prioritySettingStub{}}
	settings.prioritySchedulingConfig.config = c
	settings.prioritySchedulingConfig.expires = time.Now().Add(time.Hour)
	return &OpenAIGatewayService{settingService: settings, usageLogRepo: r}
}
func TestPrioritySchedulingOrderingAndFallback(t *testing.T) {
	c := DefaultPrioritySchedulingConfig()
	c.TargetTTFTMs = 3000
	c.Enabled = true
	c.Mode = "profit"
	r := &priorityReaderStub{signal: map[int64]PrioritySchedulingSignal{1: withAccountPerformanceFixture(PrioritySchedulingSignal{Samples: 20, P90TTFTMs: 6000, QualityPassed: 10, QualitySamples: 10}), 2: withAccountPerformanceFixture(PrioritySchedulingSignal{Samples: 20, P90TTFTMs: 1000, QualityPassed: 10, QualitySamples: 10})}}
	gateway := priorityGateway(c, r)
	scheduler, ok := newDefaultOpenAIAccountScheduler(gateway, nil).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, RequestedModel: "m", UseUpstreamTokenCost: true}
	candidates := []openAIAccountCandidateScore{priorityCandidate(1, 0.01, 10), priorityCandidate(2, 2, 10), priorityCandidate(3, 0.1, 10)}
	_, ready := gateway.prioritySignals(req, c, candidates)
	require.False(t, ready, "cold history must not block request")
	require.Eventually(t, func() bool { _, ready := gateway.prioritySignals(req, c, candidates); return ready }, time.Second, time.Millisecond)
	plan := openAIAccountLoadPlan{candidates: candidates, topK: 1}
	scheduler.applyPriorityScheduling(req, &plan)
	require.True(t, plan.priorityScheduling)
	order := scheduler.buildOpenAISelectionOrder(req, plan)
	require.Len(t, order, 3, "overflow remains available if best account loses its slot")
	require.Equal(t, int64(2), order[0].account.ID, "expensive eligible account must beat cheap degraded account")
	require.Equal(t, int64(3), order[1].account.ID)
	require.Equal(t, int64(1), order[2].account.ID)
	snap := gateway.PrioritySchedulingSnapshot()
	require.True(t, snap.HistoryReady)
	require.Equal(t, PriorityLatencyDeprioritize, snap.LatencyPenaltyMode)
	require.False(t, snap.LatencyFallback)
	require.Equal(t, int64(2), snap.Candidates[0].AccountID)
	for _, other := range []OpenAIAccountScheduleRequest{{Platform: PlatformOpenAI, RequestedModel: "m", RequiredImageCapability: OpenAIImagesCapabilityNative}, {Platform: PlatformGemini, RequestedModel: "m", UseUpstreamTokenCost: true}, {Platform: PlatformOpenAI, RequestedModel: "m"}} {
		p := openAIAccountLoadPlan{candidates: candidates}
		scheduler.applyPriorityScheduling(other, &p)
		require.False(t, p.priorityScheduling)
	}
	gateway.priorityScheduling.mu.Lock()
	for _, e := range gateway.priorityScheduling.entries {
		e.signals = nil
		e.observed = time.Time{}
		e.expires = time.Now().Add(time.Hour)
	}
	gateway.priorityScheduling.mu.Unlock()
	plan.priorityScheduling = false
	scheduler.applyPriorityScheduling(req, &plan)
	require.True(t, plan.priorityScheduling, "missing history must retain live capacity balancing")
	require.False(t, gateway.PrioritySchedulingSnapshot().HistoryReady)
}
func TestPrioritySignalRefreshCoalescesAndErrorsFallBack(t *testing.T) {
	c := DefaultPrioritySchedulingConfig()
	c.Enabled = true
	r := &priorityReaderStub{err: errors.New("offline")}
	gateway := priorityGateway(c, r)
	req := OpenAIAccountScheduleRequest{RequestedModel: "m"}
	pool := []openAIAccountCandidateScore{priorityCandidate(1, 1, 0)}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, ready := gateway.prioritySignals(req, c, pool); require.False(t, ready) }()
	}
	wg.Wait()
	require.Eventually(t, func() bool {
		gateway.priorityScheduling.mu.Lock()
		defer gateway.priorityScheduling.mu.Unlock()
		return gateway.priorityScheduling.active == 0
	}, time.Second, time.Millisecond)
	r.mu.Lock()
	require.Equal(t, 1, r.calls)
	r.mu.Unlock()
}

type prioritySettingStub struct {
	SettingRepository
	mu    sync.Mutex
	value string
	err   error
}

func (r *prioritySettingStub) GetValue(context.Context, string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return "", r.err
	}
	if r.value == "" {
		return "", ErrSettingNotFound
	}
	return r.value, nil
}
func (r *prioritySettingStub) Set(_ context.Context, _, v string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.value = v
	return nil
}
func TestPriorityConfigPersistenceAndColdStart(t *testing.T) {
	repo := &prioritySettingStub{}
	settings := &SettingService{settingRepo: repo}
	require.False(t, settings.prioritySchedulingRuntimeConfig().Enabled)
	c := DefaultPrioritySchedulingConfig()
	c.Enabled = true
	require.NoError(t, settings.SavePrioritySchedulingConfig(context.Background(), &c))
	require.True(t, settings.prioritySchedulingRuntimeConfig().Enabled)
	got, err := settings.GetPrioritySchedulingConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, c, got)
	c.Enabled = false
	require.NoError(t, settings.SavePrioritySchedulingConfig(context.Background(), &c))
	require.False(t, settings.prioritySchedulingRuntimeConfig().Enabled)
	repo.mu.Lock()
	repo.value = "{"
	repo.mu.Unlock()
	_, err = settings.GetPrioritySchedulingConfig(context.Background())
	require.Error(t, err)
}

type priorityConfigCASStub struct {
	*prioritySettingStub
	compareAndSet func(context.Context, *string, string) (bool, error)
}

func (r *priorityConfigCASStub) CompareAndSetPrioritySchedulingConfig(ctx context.Context, previous *string, value string) (bool, error) {
	return r.compareAndSet(ctx, previous, value)
}

func TestPriorityConfigCompatibilitySaveRetriesLatest(t *testing.T) {
	for conflicts := 0; conflicts <= 3; conflicts++ {
		t.Run(fmt.Sprint(conflicts), func(t *testing.T) {
			base := &prioritySettingStub{}
			repo := &priorityConfigCASStub{prioritySettingStub: base}
			settings := &SettingService{settingRepo: repo}
			initial := DefaultPrioritySchedulingConfig()
			initial.PreserveSessionAffinity = true
			initial.TargetTPS = 55
			require.NoError(t, settings.SavePrioritySchedulingConfig(t.Context(), &initial))
			latest := initial
			calls := 0
			repo.compareAndSet = func(ctx context.Context, previous *string, value string) (bool, error) {
				calls++
				stored, err := base.GetValue(ctx, prioritySchedulingSettingKey)
				require.NoError(t, err)
				require.NotNil(t, previous)
				require.Equal(t, stored, *previous, "each retry must reread the committed value")
				if calls <= conflicts {
					latest.PreserveSessionAffinity = !latest.PreserveSessionAffinity
					latest.ErrorRateWindowMinutes++
					latest.TargetTPS++
					raw, err := json.Marshal(latest)
					require.NoError(t, err)
					require.NoError(t, base.Set(ctx, prioritySchedulingSettingKey, string(raw)))
					return false, nil
				}
				return true, base.Set(ctx, prioritySchedulingSettingKey, value)
			}
			requested := initial
			requested.Mode = "profit"
			requested.PreserveSessionAffinity = false
			requested.ErrorRateWindowMinutes = 1
			requested.TargetTPS = 30
			before := requested
			err := settings.SavePrioritySchedulingConfig(t.Context(), &requested, "preserve_session_affinity", "error_rate_window_minutes", "target_tps")
			stored, readErr := settings.GetPrioritySchedulingConfig(t.Context())
			require.NoError(t, readErr)
			if conflicts == 3 {
				require.ErrorIs(t, err, ErrPrioritySchedulingConfigConflict)
				require.Equal(t, 3, calls)
				require.Equal(t, latest, stored, "conflict exhaustion must not write the stale merge")
				require.Equal(t, before, requested, "failure must not publish a configuration")
				require.Equal(t, initial, settings.prioritySchedulingRuntimeConfig())
				require.Equal(t, uint64(1), settings.prioritySchedulingConfig.revision)
				return
			}
			require.NoError(t, err)
			require.Equal(t, conflicts+1, calls)
			latest.Mode = "profit"
			require.Equal(t, latest, stored)
			require.Equal(t, stored, requested, "response value must be the committed merge")
			require.Equal(t, stored, settings.prioritySchedulingRuntimeConfig())
			require.Equal(t, uint64(2), settings.prioritySchedulingConfig.revision)
		})
	}
}

func TestPriorityConfigCompatibilitySaveFailsClosed(t *testing.T) {
	for _, name := range []string{"read", "corrupt", "invalid_stored", "missing_atomic", "write", "canceled", "unknown_field", "invalid_requested", "nil_config"} {
		t.Run(name, func(t *testing.T) {
			base := &prioritySettingStub{}
			repo := &priorityConfigCASStub{prioritySettingStub: base}
			settings := &SettingService{settingRepo: repo}
			initial := DefaultPrioritySchedulingConfig()
			require.NoError(t, settings.SavePrioritySchedulingConfig(t.Context(), &initial))
			beforeExpires := settings.prioritySchedulingConfig.expires
			requested := initial
			requested.Enabled = true
			input := &requested
			preserve := []string{"preserve_session_affinity"}
			ctx := t.Context()
			switch name {
			case "read":
				base.err = errors.New("read failed")
			case "corrupt":
				base.value = "{"
			case "invalid_stored":
				bad := initial
				bad.Mode = "invalid"
				raw, err := json.Marshal(bad)
				require.NoError(t, err)
				base.value = string(raw)
			case "missing_atomic":
				settings.settingRepo = base
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "unknown_field":
				preserve = []string{"misspelled_field"}
			case "invalid_requested":
				requested.Mode = "invalid"
			case "nil_config":
				input = nil
			}
			beforeValue, beforeInput := base.value, requested
			calls := 0
			repo.compareAndSet = func(ctx context.Context, _ *string, _ string) (bool, error) {
				calls++
				if err := ctx.Err(); err != nil {
					return false, err
				}
				return false, errors.New("conditional write failed")
			}
			err := settings.SavePrioritySchedulingConfig(ctx, input, preserve...)
			require.Error(t, err)
			if name == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
			}
			wantCalls := 0
			if name == "write" || name == "canceled" {
				wantCalls = 1
			}
			require.Equal(t, wantCalls, calls)
			require.Equal(t, beforeValue, base.value, "never fall back to an unconditional write")
			require.Equal(t, beforeInput, requested)
			require.Equal(t, initial, settings.prioritySchedulingRuntimeConfig())
			require.Equal(t, uint64(1), settings.prioritySchedulingConfig.revision)
			require.Equal(t, beforeExpires, settings.prioritySchedulingConfig.expires)
			// An explicit replacement retains the existing repair behavior, including
			// when the old stored value is corrupt or no conditional adapter exists.
			base.err = nil
			repaired := initial
			repaired.Enabled = true
			require.NoError(t, settings.SavePrioritySchedulingConfig(t.Context(), &repaired))
			stored, err := settings.GetPrioritySchedulingConfig(t.Context())
			require.NoError(t, err)
			require.Equal(t, repaired, stored)
			require.Equal(t, repaired, settings.prioritySchedulingRuntimeConfig())
		})
	}
}

func (r *prioritySettingStub) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func TestPrioritySchedulingGatewayEntryAndDisable(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	c := DefaultPrioritySchedulingConfig()
	// This fixture compares 8s against 0.5s under the legacy 3s target.
	// The production default is now 15s; using it makes both accounts eligible
	// and allows the cheap account to win instead of testing the degraded tier.
	c.TargetTTFTMs = 3000
	c.Enabled = true
	c.Mode = "profit"
	reader := &priorityReaderStub{signal: map[int64]PrioritySchedulingSignal{1: withAccountPerformanceFixture(PrioritySchedulingSignal{Samples: 20, P90TTFTMs: 8000, QualityPassed: 10, QualitySamples: 10}), 2: withAccountPerformanceFixture(PrioritySchedulingSignal{Samples: 20, P90TTFTMs: 500, QualityPassed: 10, QualitySamples: 10})}}
	svc := priorityGateway(c, reader)
	candidates := []openAIAccountCandidateScore{priorityCandidate(1, 0.01, 0), priorityCandidate(2, 1, 0)}
	accounts := make([]Account, 0, 2)
	for _, candidate := range candidates {
		candidate.account.Status = StatusActive
		candidate.account.Schedulable = true
		accounts = append(accounts, *candidate.account)
	}
	svc.accountRepo = schedulerTestOpenAIAccountRepo{accounts: accounts}
	svc.cache = &schedulerTestGatewayCache{}
	svc.cfg = newSchedulerTestOpenAIWSV2Config()
	svc.concurrencyService = NewConcurrencyService(schedulerTestConcurrencyCache{})
	req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, RequestedModel: "gpt-test", UseUpstreamTokenCost: true}
	require.Eventually(t, func() bool { _, ready := svc.prioritySignals(req, c, candidates); return ready }, time.Second, time.Millisecond)
	selected, _, err := svc.SelectAccountWithScheduler(context.Background(), nil, "", "", "gpt-test", nil, OpenAIUpstreamTransportHTTPSSE, false)
	require.NoError(t, err)
	require.NotNil(t, selected)
	require.Equal(t, int64(2), selected.Account.ID)
	if selected.ReleaseFunc != nil {
		selected.ReleaseFunc()
	}
	require.NotNil(t, svc.PrioritySchedulingSnapshot())
	c.Enabled = false
	require.NoError(t, svc.settingService.SavePrioritySchedulingConfig(context.Background(), &c))
	require.Nil(t, svc.getOpenAIAccountScheduler(context.Background(), svc.prioritySchedulingRuntimeConfig().Enabled))
}

func TestPriorityLoadFactorDoesNotHideActualConcurrency(t *testing.T) {
	c := DefaultPrioritySchedulingConfig()
	good := withAccountPerformanceFixture(PrioritySchedulingSignal{Samples: 20, P90TTFTMs: 500, QualityPassed: 10, QualitySamples: 10})
	a := priorityCandidate(1, 1, 90)
	a.account.Concurrency = 100
	a.loadInfo.CurrentConcurrency = 90
	factor := 10000
	a.account.LoadFactor = &factor
	a.loadInfo.LoadRate = 0
	scored := scorePriorityCandidate(c, a, good, time.Now())
	require.Equal(t, "degraded", scored.Tier)
	require.Equal(t, 90, *scored.LoadPercent)
	a.loadInfo.CurrentConcurrency = 20
	boosted := scorePriorityCandidate(c, a, good, time.Now())
	a.account.LoadFactor = nil
	normal := scorePriorityCandidate(c, a, good, time.Now())
	require.Equal(t, boosted.Score, normal.Score, "load-factor overrides must not inflate actual capacity")
}
func TestPriorityExplicitPriorityWithinExperienceTier(t *testing.T) {
	a, b, poor := priorityCandidate(1, 1, 0), priorityCandidate(2, 1, 0), priorityCandidate(3, 0, 0)
	a.account.Priority = 1
	b.account.Priority = 2
	poor.account.Priority = 0
	a.score = 410
	b.score = 490
	poor.score = 99
	s, ok := newDefaultOpenAIAccountScheduler(&OpenAIGatewayService{}, nil).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	got := s.buildOpenAISelectionOrder(OpenAIAccountScheduleRequest{}, openAIAccountLoadPlan{priorityScheduling: true, topK: 1, candidates: []openAIAccountCandidateScore{b, poor, a}})
	require.Equal(t, int64(1), got[0].account.ID)
	require.Equal(t, int64(2), got[1].account.ID)
	require.Equal(t, int64(3), got[2].account.ID)
}

func TestPriorityOAuthProfitUsesUserChargeAndTheoreticalCost(t *testing.T) {
	c := DefaultPrioritySchedulingConfig()
	c.Mode = "profit"
	a := priorityCandidate(1, 0.001, 0)
	a.account.Type = AccountTypeOAuth
	a.account.Extra[AccountCostMultiplierExtraKey] = 0.1
	signal := withAccountPerformanceFixture(PrioritySchedulingSignal{Samples: 20, P90TTFTMs: 500, QualityPassed: 10, QualitySamples: 10, ProfitSamples: 10, Revenue: 10, BaseCost: 60})
	score := scorePriorityCandidate(c, a, signal, time.Now())
	require.Equal(t, "usage", score.EconomicsSource)
	require.Equal(t, 4.0, *score.Profit)
	require.InDelta(t, 0.4, *score.Margin, 0.0001)
	differentRate := 10.0
	a.account.RateMultiplier = &differentRate
	require.Equal(t, score.Score, scorePriorityCandidate(c, a, signal, time.Now()).Score, "OAuth profit must not be inferred from current account multiplier")
	signal.BaseCost = 120
	loss := scorePriorityCandidate(c, a, signal, time.Now())
	require.Equal(t, -2.0, *loss.Profit)
	require.Equal(t, "degraded", loss.Tier)
	signal.Revenue = 0
	zero := scorePriorityCandidate(c, a, signal, time.Now())
	require.Nil(t, zero.Margin)
	require.Equal(t, -12.0, *zero.Profit)
	require.False(t, math.IsNaN(zero.Score))
	signal.ProfitSamples = 0
	unknown := scorePriorityCandidate(c, a, signal, time.Now())
	require.Equal(t, "unknown", unknown.EconomicsSource)
	require.Nil(t, unknown.Profit)
	require.Equal(t, "insufficient", unknown.Tier)
}
