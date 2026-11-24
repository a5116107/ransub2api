//go:build integration

package repository_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type queueSaveV2Repository struct {
	service.GroupMonitoringTurnRepository
	service.ChannelMonitorV2Repository
	service.ChannelMonitorV2CandyRepository
	gate func(context.Context, *service.MonitoringProbeBudgetDenial)
}

func (r *queueSaveV2Repository) FinishCandyProbe(ctx context.Context, result service.ChannelMonitorV2CandyResult) error {
	r.gate(ctx, result.BudgetDeferral)
	return r.ChannelMonitorV2CandyRepository.FinishCandyProbe(ctx, result)
}

type queueSaveV3Repository struct {
	service.GroupMonitoringTurnRepository
	service.ChannelMonitorV3Repository
	gate func(context.Context, *service.MonitoringProbeBudgetDenial)
}

func (r *queueSaveV3Repository) FinishProbe(ctx context.Context, result service.ChannelMonitorV3ProbeResult) error {
	r.gate(ctx, result.BudgetDeferral)
	return r.ChannelMonitorV3Repository.FinishProbe(ctx, result)
}

func TestMonitoringProbeSaveDrainHTTPPostgresStopAndRecovery(t *testing.T) {
	for _, mode := range []string{"v2", "v3"} {
		t.Run(mode, func(t *testing.T) { testMonitoringProbeSaveDrain(t, mode) })
	}
}

func testMonitoringProbeSaveDrain(t *testing.T, mode string) {
	t.Helper()
	const targets = 24
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	ready, replies, saveEntered, saveRelease := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseReplies, releaseSave sync.Once
	var sends, invalid atomic.Int32
	var held atomic.Bool
	var stop func()
	defer func() {
		releaseReplies.Do(func() { close(replies) })
		releaseSave.Do(func() { close(saveRelease) })
		if stop != nil {
			stop()
		}
	}()
	challenge := regexp.MustCompile(`(\d+) ([+-]) (\d+) = \?`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tenant/v1/responses" || r.Header.Get("Authorization") != "Bearer queue-save-fixture" {
			invalid.Add(1)
			http.Error(w, "invalid queue save fixture", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			invalid.Add(1)
			return
		}
		n := sends.Add(1)
		if n == 4 {
			close(ready) // The four initial requests were all physically admitted.
		}
		if n <= 4 {
			select {
			case <-ready:
			case <-r.Context().Done():
				return
			}
		}
		if n < 4 {
			select {
			case <-replies:
			case <-r.Context().Done():
				return
			}
		}
		answer := "21"
		if questions := challenge.FindAllSubmatch(body, -1); len(questions) > 0 {
			match := questions[len(questions)-1] // Skip the fixed few-shot examples.
			a, _ := strconv.Atoi(string(match[1]))
			b, _ := strconv.Atoi(string(match[3]))
			if string(match[2]) == "-" {
				b = -b
			}
			answer = strconv.Itoa(a + b)
		}
		text, _ := json.Marshal(answer)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%s}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":3,\"total_tokens\":7}}}\n\n", text)
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{Timezone: "UTC", Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	cfg.Default.RateMultiplier = 1
	settingsRepo := repository.NewSettingRepository(client)
	require.NoError(t, settingsRepo.SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: mode}))
	settings := service.NewSettingService(settingsRepo, cfg)
	accounts := repository.NewAccountRepository(client, db, nil)
	account := &service.Account{Name: "queue-save-fixture", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 20,
		Credentials: map[string]any{"api_key": "queue-save-fixture", "base_url": server.URL + "/tenant/v1"}}
	require.NoError(t, accounts.Create(ctx, account))
	groupRepo := repository.NewGroupRepository(client, db)
	group := &service.Group{Name: "queue-save-group", Platform: service.PlatformOpenAI, Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 1}
	require.NoError(t, groupRepo.Create(ctx, group))
	_, err := db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account.ID, group.ID)
	require.NoError(t, err)
	var before string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, account.ID).Scan(&before))
	policyRepo := repository.NewMonitoringProbeBudgetRepository(db)
	policy := service.DefaultMonitoringProbeBudgetSettings()
	policy.Enabled, policy.RecoveryReservePercent, policy.TokensPerDay = true, 0, 5
	policy.RequestsPerDay, policy.RequestsPerMinute = 100000, 1000
	policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency = 20, 20, 20
	policy, err = policyRepo.SaveSettings(ctx, policy)
	require.NoError(t, err)
	budget := service.NewMonitoringProbeBudgetService(policyRepo)
	upstream := repository.NewHTTPUpstream(cfg)
	slots := repository.NewConcurrencyCache(repository.Issue594TestRedis(), 1, 60)
	concurrency := service.NewConcurrencyService(slots)
	gateway := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, repository.NewGatewayCache(repository.Issue594TestRedis()), cfg, nil, concurrency,
		service.NewBillingService(cfg, nil), nil, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
	t.Cleanup(gateway.CloseOpenAIWSPool)
	tester := service.NewAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil)
	tester.SetMonitoringBudget(budget)
	groups := service.NewPelicanGroupTestService(repository.NewPelicanGroupTestRepository(db), groupRepo, nil, gateway, concurrency, tester, nil, nil)
	gate := func(saveCtx context.Context, denial *service.MonitoringProbeBudgetDenial) {
		if denial != nil && held.CompareAndSwap(false, true) {
			close(saveEntered)
			select {
			case <-saveRelease:
			case <-saveCtx.Done():
			}
		}
	}
	v2 := repository.NewChannelMonitorV2Repository(db)
	v3 := repository.NewChannelMonitorV3Repository(db)
	if mode == "v2" {
		current, loadErr := v2.GetConfig(ctx)
		require.NoError(t, loadErr)
		current.Enabled = true
		current.Platforms = []service.ChannelMonitorV2PlatformConfig{{Platform: service.PlatformOpenAI, Enabled: true}}
		for i := range targets {
			current.CandyProbes = append(current.CandyProbes, service.ChannelMonitorV2CandyProbe{GroupID: group.ID, Enabled: true, Model: fmt.Sprintf("probe-%03d", i), ReasoningEffort: "high", IntervalMinutes: 1})
		}
		_, err = v2.UpdateConfig(ctx, *current, current.Version)
		require.NoError(t, err)
	} else {
		current, loadErr := v3.GetConfig(ctx)
		require.NoError(t, loadErr)
		current.IntervalMinutes, current.MinRequests = 5, 1
		_, err = v3.UpdateConfig(ctx, *current, current.Version, 0)
		require.NoError(t, err)
		require.NoError(t, v3.UpdateIdleProbe(ctx, service.ChannelMonitorV3IdleProbe{Enabled: true, Groups: []service.ChannelMonitorV3IdleProbeGroup{{GroupID: group.ID}}}))
		for i := range targets {
			_, err = v3.CreateComponent(ctx, service.ChannelMonitorV3ComponentInput{Name: fmt.Sprintf("probe-%03d", i), GroupID: group.ID, Model: fmt.Sprintf("probe-%03d", i), Enabled: true, Visibility: "public"})
			require.NoError(t, err)
		}
	}
	tick := time.Now().UTC().Truncate(5 * time.Minute).Add(-30 * time.Second)
	start := func(at time.Time) {
		if mode == "v2" {
			store, ok := v2.(service.ChannelMonitorV2CandyRepository)
			require.True(t, ok)
			monitor := service.ProvideChannelMonitorV2Service(&queueSaveV2Repository{v2.(service.GroupMonitoringTurnRepository), v2, store, gate}, settings, groups)
			budget.SetGroupProbeQueues(monitor, nil)
			plans, results := repository.NewScheduledTestPlanRepository(db), repository.NewScheduledTestResultRepository(db)
			runner := service.ProvideScheduledTestRunnerService(plans, service.NewScheduledTestService(plans, results), tester, nil, cfg, &service.QualityJudgeService{}, nil, groups, monitor)
			stop = runner.Stop
		} else {
			prober := service.NewChannelMonitorV3Prober(&queueSaveV3Repository{v3.(service.GroupMonitoringTurnRepository), v3, gate}, groups, settings)
			budget.SetGroupProbeQueues(nil, prober)
			done := make(chan struct{})
			go func() { defer close(done); prober.RunDue(ctx, at) }()
			stop = func() { prober.Stop(); <-done }
		}
	}
	view := func() *service.MonitoringProbeQueueRuntime {
		summary, loadErr := budget.Summary(ctx)
		require.NoError(t, loadErr)
		if mode == "v2" {
			return summary.Coverage.GroupProbeQueues.V2
		}
		return summary.Coverage.GroupProbeQueues.V3
	}
	table := "channel_monitor_v2_candy_results"
	if mode == "v3" {
		table = "channel_monitor_v3_probe_results"
	}
	start(tick)
	select {
	case <-saveEntered:
	case <-time.After(75 * time.Second):
		t.Fatal("the native executor did not reach its first exhausted result save")
	}
	releaseReplies.Do(func() { close(replies) })
	require.Eventually(t, func() bool { v := view(); return v.Cycle != nil && v.Cycle.Active == 1 && v.Cycle.Finished == 4 }, 3*time.Second, 10*time.Millisecond)
	paused := view()
	require.EqualValues(t, 19, paused.Cycle.Queued)
	require.Zero(t, paused.Cycle.Deferred, "the held SQL result has not finished")
	require.Equal(t, "global_day", paused.Cycle.BudgetDeferral.Scope)
	var claims int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&claims))
	require.Equal(t, 5, claims, "only the original four admitted samples and one denied sample may claim before storage drains")
	require.EqualValues(t, 4, sends.Load())
	releaseSave.Do(func() { close(saveRelease) })
	require.Eventually(t, func() bool { return view().Cycle.FinishedAt != nil }, 3*time.Second, 10*time.Millisecond)
	stop()
	require.Equal(t, "budget_paused", view().State)
	require.EqualValues(t, 1, view().Cycle.Deferred)
	policy.TokensPerDay = 10000
	_, err = policyRepo.SaveSettings(ctx, policy)
	require.NoError(t, err)
	start(tick.Add(5 * time.Minute))
	require.Eventually(t, func() bool { v := view(); return v.State == "idle" && v.Cycle != nil && v.Cycle.Finished == targets }, 75*time.Second, 20*time.Millisecond)
	stop()
	require.EqualValues(t, targets+4, sends.Load())
	require.Nil(t, view().Cycle.BudgetDeferral)
	var passed, rejected int
	if mode == "v2" {
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE verdict='correct'),count(*) FILTER(WHERE verdict IN ('incorrect','error')) FROM channel_monitor_v2_candy_results`).Scan(&passed, &rejected))
	} else {
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE outcome='success'),count(*) FILTER(WHERE outcome='failure') FROM channel_monitor_v3_probe_results`).Scan(&passed, &rejected))
	}
	require.Equal(t, targets+4, passed, "drained and recovered requests must actually pass the original detector")
	require.Zero(t, rejected)
	usage, err := policyRepo.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, sends.Load(), usage.DayUsed)
	require.EqualValues(t, sends.Load()*7, usage.DayTokens)
	require.EqualValues(t, sends.Load(), usage.UsageReported)
	require.Zero(t, usage.UsageUnknown)
	require.Zero(t, usage.UsagePending)
	require.Zero(t, usage.ActiveRequests)
	active, err := slots.GetAccountConcurrency(ctx, account.ID)
	require.NoError(t, err)
	require.Zero(t, active)
	var after string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, account.ID).Scan(&after))
	require.Equal(t, before, after)
	var downstream int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&downstream))
	require.Zero(t, downstream)
	require.Zero(t, invalid.Load())
	t.Logf("%s native SQL/HTTP: 5 claims while saving, 4 admitted calls drained, 24 targets recovered after rebuild; 28 physical calls/196 actual tokens; no SQL-budget or Redis slot leaks", mode)
}
