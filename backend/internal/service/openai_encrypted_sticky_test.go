package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestEncryptedHTTPLegacySelectionWaitsForSuccessfulHistoryAcceptance(t *testing.T) {
	for _, mode := range []string{"priority-lru", "load-batch", "load-fallback"} {
		for _, initiallyBound := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/initially-bound=%t", mode, initiallyBound), func(t *testing.T) {
				groupID := int64(38100)
				const session = "legacy-encrypted-http"
				concurrency := schedulerTestConcurrencyCache{}
				if mode == "load-fallback" {
					concurrency.loadBatchErr = errors.New("fixture load cache unavailable")
				}
				svc := newLegacySchedulerDecisionTestService(newLegacySchedulerDecisionTestAccounts(groupID, true), mode != "priority-lru", concurrency)
				cache := &schedulerTestGatewayCache{}
				svc.cache = cache
				want := int64(0)
				if initiallyBound {
					want = 38101
					require.NoError(t, svc.setStickySessionAccountID(t.Context(), &groupID, session, want, time.Hour))
				}
				ctx := WithOpenAIEncryptedReasoningAffinity(t.Context(), []byte(`{"input":[{"type":"reasoning","encrypted_content":"fixture-history"}]}`))
				ctx = svc.WithOpenAIEncryptedSessionBinding(ctx, &groupID, session)
				selection, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, session, "gpt-5.1", map[int64]struct{}{38101: {}})
				require.NoError(t, err)
				require.NotNil(t, selection)
				t.Cleanup(func() { releaseLegacySchedulerDecisionSelection(selection) })
				require.Equal(t, int64(38102), selection.Account.ID)
				require.Equal(t, want, cache.sessionBindings["openai:"+session], "selection must not commit a replacement before its response succeeds")
				require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctx, &groupID, session, selection.Account.ID))
				require.Equal(t, want, cache.sessionBindings["openai:"+session])
				require.NoError(t, svc.BindOpenAIEncryptedSessionAfterSuccess(ctx, &groupID, session, selection.Account.ID))
				require.Equal(t, selection.Account.ID, cache.sessionBindings["openai:"+session])
			})
		}
	}
}

func TestEncryptedHTTPBindingWaitsForSuccessfulHistoryAcceptance(t *testing.T) {
	for _, initiallyBound := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-session", true: "existing-session"}[initiallyBound], func(t *testing.T) {
			f := newStickyEscapeRebindFixture()
			if !initiallyBound {
				delete(f.cache.sessionBindings, "openai:rebind-session")
			}
			ctx := WithOpenAIEncryptedReasoningAffinity(t.Context(), []byte(`{"input":[{"type":"reasoning","encrypted_content":"synthetic-history"}]}`))
			ctx = f.service.WithOpenAIEncryptedSessionBinding(ctx, f.request.GroupID, f.request.SessionHash)
			f.request.EncryptedReasoning = true
			selected, _, err := f.scheduler.Select(ctx, f.request)
			require.NoError(t, err)
			t.Cleanup(func() { releaseStickyEscapeRebindSelection(selected) })
			require.NotNil(t, selected)
			require.Equal(t, int64(947102), selected.Account.ID)
			want := int64(0)
			if initiallyBound {
				want = 947101
			}
			require.Equal(t, want, f.cache.sessionBindings["openai:rebind-session"], "selection does not prove the replacement accepts the encrypted history")
			require.NoError(t, f.service.BindStickySessionAfterProfitAdmission(ctx, f.request.GroupID, f.request.SessionHash, selected.Account.ID))
			require.Equal(t, want, f.cache.sessionBindings["openai:rebind-session"], "slot admission is not a successful response either")
			require.NoError(t, f.service.BindOpenAIEncryptedSessionAfterSuccess(ctx, f.request.GroupID, f.request.SessionHash, selected.Account.ID))
			require.Equal(t, selected.Account.ID, f.cache.sessionBindings["openai:rebind-session"], "only successful acceptance commits the replacement")
		})
	}
}

func TestEncryptedHTTPBindingKeepsOriginalObservationAcrossRetries(t *testing.T) {
	for _, state := range []string{"original", "newer-success", "expired", "canceled", "profit-gate"} {
		t.Run(state, func(t *testing.T) {
			f := newStickyEscapeRebindFixture()
			ctx := WithOpenAIEncryptedReasoningAffinity(t.Context(), []byte(`{"input":[{"type":"reasoning","encrypted_content":"synthetic-history"}]}`))
			if state == "profit-gate" {
				ctx = context.WithValue(ctx, openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: *f.request.GroupID, platform: PlatformOpenAI, threshold: .5})
			}
			ctx = f.service.WithOpenAIEncryptedSessionBinding(ctx, f.request.GroupID, f.request.SessionHash)
			require.NoError(t, f.service.deleteStickySessionAccountID(ctx, f.request.GroupID, f.request.SessionHash))
			require.Equal(t, int64(947101), f.cache.sessionBindings["openai:rebind-session"], "temporary ineligibility must not erase history affinity")
			if state == "newer-success" {
				f.cache.sessionBindings["openai:rebind-session"] = 947103
			}
			if state == "expired" {
				delete(f.cache.sessionBindings, "openai:rebind-session")
			}
			// Reselection and a new cache observation must not replace the
			// original compare-and-swap baseline after a concurrent success.
			retry := withOpenAIStickyBindingSnapshot(ctx, f.request.GroupID, f.request.SessionHash, 947103, true)
			observeOpenAIStickyBinding(retry, f.request.GroupID, f.request.SessionHash, 947103, nil)
			require.Same(t, openAIStickyBindingSnapshotFromContext(ctx, f.request.GroupID, f.request.SessionHash), openAIStickyBindingSnapshotFromContext(retry, f.request.GroupID, f.request.SessionHash))
			if state == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			require.NoError(t, f.service.BindOpenAIEncryptedSessionAfterSuccess(ctx, f.request.GroupID, f.request.SessionHash, 947102))
			want := int64(947102)
			if state == "newer-success" {
				want = 947103
			}
			if state == "canceled" || state == "profit-gate" {
				want = 947101
			}
			require.Equal(t, want, f.cache.sessionBindings["openai:rebind-session"])
		})
	}
}

func TestEncryptedHTTPBindingDoesNotChangeOtherSessionsOrPlainRequests(t *testing.T) {
	f := newStickyEscapeRebindFixture()
	ctx := WithOpenAIEncryptedReasoningAffinity(t.Context(), []byte(`{"input":"plaintext"}`))
	ctx = f.service.WithOpenAIEncryptedSessionBinding(ctx, f.request.GroupID, f.request.SessionHash)
	require.NoError(t, f.service.BindStickySessionAfterProfitAdmission(ctx, f.request.GroupID, f.request.SessionHash, 947102))
	require.Equal(t, int64(947102), f.cache.sessionBindings["openai:rebind-session"], "ordinary requests retain the original eager binding")
	ctx = WithOpenAIEncryptedReasoningAffinity(t.Context(), []byte(`{"input":[{"type":"reasoning","encrypted_content":"synthetic"}]}`))
	ctx = f.service.WithOpenAIEncryptedSessionBinding(ctx, f.request.GroupID, f.request.SessionHash)
	require.NoError(t, f.service.BindStickySessionAfterProfitAdmission(ctx, f.request.GroupID, "different-session", 947103))
	require.Equal(t, int64(947103), f.cache.sessionBindings["openai:different-session"])
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_EncryptedStickyKeepsTTFT(t *testing.T) {
	ctx := WithOpenAIEncryptedReasoningAffinity(context.Background(), []byte(`{"input":[{"type":"reasoning","encrypted_content":"synthetic"}]}`))
	groupID := int64(10101)
	accounts := []Account{
		{
			ID:          21101,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			GroupIDs:    []int64{groupID},
		},
		{
			ID:          21102,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    1,
			GroupIDs:    []int64{groupID},
		},
	}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session_hash_sticky_ttft": 21101}}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	concurrencyCache := schedulerTestConcurrencyCache{acquireResults: map[int64]bool{21102: true}}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
		openaiAccountStats: newOpenAIAccountRuntimeStats(),
	}
	fastTTFT := 14999
	svc.openaiAccountStats.report(21101, true, &fastTTFT)
	stableTTFT := 14999
	svc.openaiAccountStats.report(21101, true, &stableTTFT)

	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_hash_sticky_ttft", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21101), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}

	slowTTFT := 20000
	for i := 0; i < 3; i++ {
		svc.openaiAccountStats.report(21101, true, &slowTTFT)
	}

	selection, decision, err = svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_hash_sticky_ttft", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21101), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
	require.Equal(t, int64(21101), cache.sessionBindings["openai:session_hash_sticky_ttft"])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_EncryptedStickyKeepsErrorRate(t *testing.T) {
	ctx := WithOpenAIEncryptedReasoningAffinity(context.Background(), []byte(`{"input":[{"type":"reasoning","encrypted_content":"synthetic"}]}`))
	groupID := int64(10102)
	accounts := []Account{
		{ID: 21201, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}},
		{ID: 21202, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1, GroupIDs: []int64{groupID}},
	}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session_hash_sticky_error_rate": 21201}}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{21202: true}}),
		openaiAccountStats: newOpenAIAccountRuntimeStats(),
	}
	for i := 0; i < 3; i++ {
		svc.openaiAccountStats.report(21201, false, nil)
	}
	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_hash_sticky_error_rate", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21201), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	for i := 0; i < 2; i++ {
		svc.openaiAccountStats.report(21201, false, nil)
	}

	selection, decision, err = svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_hash_sticky_error_rate", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21201), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
	require.Equal(t, int64(21201), cache.sessionBindings["openai:session_hash_sticky_error_rate"])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_EncryptedStickyBusyStillEscapes(t *testing.T) {
	ctx := WithOpenAIEncryptedReasoningAffinity(context.Background(), []byte(`{"input":[{"type":"reasoning","encrypted_content":"synthetic"}]}`))
	groupID := int64(10103)
	accounts := []Account{
		{ID: 21301, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}},
		{ID: 21302, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1, GroupIDs: []int64{groupID}},
	}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session_hash_sticky_busy_escape": 21301}}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 2
	cfg.Gateway.Scheduling.StickySessionWaitTimeout = 45 * time.Second
	concurrencyCache := schedulerTestConcurrencyCache{
		acquireResults: map[int64]bool{21301: false, 21302: true},
		waitCounts:     map[int64]int{21301: 999},
		loadMap: map[int64]*AccountLoadInfo{
			21301: {AccountID: 21301, LoadRate: 95, WaitingCount: 9},
			21302: {AccountID: 21302, LoadRate: 1, WaitingCount: 0},
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_hash_sticky_busy_escape", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21302), selection.Account.ID)
	require.Nil(t, selection.WaitPlan)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.False(t, decision.StickySessionHit)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}
func TestEncryptedPriorityStickyCapacityStillRebalances(t *testing.T) {
	for _, test := range []struct {
		name                        string
		preserve, disable, previous bool
		otherGroup                  int64
		expectEscape                bool
	}{
		{name: "movable", expectEscape: true},
		{name: "task_owner", preserve: true},
		{name: "escape_disabled", disable: true},
		{name: "response_owner", previous: true},
		{name: "outside_group", otherGroup: 99},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultPrioritySchedulingConfig()
			cfg.Enabled = true
			groupID := int64(11)
			accounts := []Account{
				{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 30, GroupIDs: []int64{3, 5, 11, 13, 15}},
				{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 20, GroupIDs: []int64{11, 15}},
			}
			if test.otherGroup != 0 {
				accounts[1].GroupIDs = []int64{test.otherGroup}
			}
			gateway := priorityGateway(cfg, &priorityReaderStub{})
			gateway.accountRepo = schedulerTestOpenAIAccountRepo{accounts: accounts}
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:existing-session": 1}}
			gateway.cache = cache
			gateway.concurrencyService = NewConcurrencyService(schedulerTestConcurrencyCache{loadMap: map[int64]*AccountLoadInfo{1: {AccountID: 1, CurrentConcurrency: 18}, 2: {AccountID: 2}}})
			scheduler := &defaultOpenAIAccountScheduler{service: gateway, stats: newOpenAIAccountRuntimeStats()}
			req := OpenAIAccountScheduleRequest{EncryptedReasoning: true, Platform: PlatformOpenAI, GroupID: &groupID, SessionHash: "existing-session", StickyAccountID: 1, UseUpstreamTokenCost: true, PreserveStickyBinding: test.preserve, DisableStickyEscape: test.disable}
			if test.previous {
				req.PreviousResponseID = "resp-owner"
			}
			require.Equal(t, test.expectEscape, scheduler.shouldRebalancePrioritySticky(context.Background(), req, &accounts[0]))
			selected, escaped, err := scheduler.selectBySessionHash(context.Background(), req)
			require.NoError(t, err)
			require.Equal(t, test.expectEscape, escaped)
			if test.expectEscape {
				require.Nil(t, selected)
			} else {
				require.NotNil(t, selected)
				if selected.ReleaseFunc != nil {
					selected.ReleaseFunc()
				}
			}
			require.Equal(t, int64(1), cache.sessionBindings["openai:existing-session"])
		})
	}
}

func TestEncryptedStickyHardGates(t *testing.T) {
	for _, gate := range []string{"excluded", "disabled", "wrong group", "wrong platform", "rate limit", "model", "transport"} {
		t.Run(gate, func(t *testing.T) {
			group := int64(9)
			acquired := []int64{}
			account := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{group}}
			req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, GroupID: &group, SessionHash: "hard-gate", StickyAccountID: 1, RequestedModel: "gpt-5.1", EncryptedReasoning: true}
			switch gate {
			case "excluded":
				req.ExcludedIDs = map[int64]struct{}{1: {}}
			case "disabled":
				account.Schedulable = false
			case "wrong group":
				account.GroupIDs = []int64{10}
			case "wrong platform":
				account.Platform = PlatformGrok
			case "rate limit":
				until := time.Now().Add(time.Hour)
				account.RateLimitResetAt = &until
			case "model":
				account.Credentials = map[string]any{"model_mapping": map[string]any{"other-model": "other-model"}}
			case "transport":
				req.RequiredTransport = OpenAIUpstreamTransportResponsesWebsocketV2
				account.Credentials = map[string]any{"openai_ws_force_http": true}
			}
			svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{account}}, cfg: &config.Config{}, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquiredIDs: &acquired})}
			scheduler := &defaultOpenAIAccountScheduler{service: svc, stats: newOpenAIAccountRuntimeStats()}
			selection, escaped, err := scheduler.selectBySessionHash(context.Background(), req)
			require.NoError(t, err)
			require.Nil(t, selection)
			require.False(t, escaped)
			require.Empty(t, acquired, "quality affinity cannot bypass hard gate")
		})
	}
}

func TestEncryptedStickyDetection(t *testing.T) {
	for _, tc := range []struct {
		body      string
		encrypted bool
	}{
		{`{"input":[{"type":"reasoning","encrypted_content":"synthetic"}]}`, true},
		{`{"input":{"type":"reasoning","encrypted_content":"synthetic"}}`, true},
		{`{"input":[{"type":"reasoning","encrypted_content":null}]}`, false},
		{`{"input":[{"type":"reasoning","encrypted_content":""}]}`, false},
		{`{"input":[{"type":"compaction","encrypted_content":"history"}]}`, false},
		{`{"input":[{"type":"agent_message","encrypted_content":"task"}]}`, false},
		{`{"input":[{"role":"user","content":"encrypted_content"}]}`, false},
	} {
		ctx := WithOpenAIEncryptedReasoningAffinity(context.Background(), []byte(tc.body))
		require.Equal(t, tc.encrypted, ctx.Value(openAIEncryptedReasoningAffinityKey{}))
	}
}

func TestEncryptedStickyRPMHardGate(t *testing.T) {
	ctx := WithOpenAIEncryptedReasoningAffinity(context.Background(), []byte(`{"input":[{"type":"reasoning","encrypted_content":"synthetic"}]}`))
	group := int64(9)
	acquired := []int64{}
	account := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{group}, Extra: map[string]any{"base_rpm": 1, "rpm_strategy": "sticky_exempt"}}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{account}}, cfg: &config.Config{}, rpmCache: &openAIRPMTestCache{counts: map[int64]int{1: 1}}, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquiredIDs: &acquired})}
	scheduler := &defaultOpenAIAccountScheduler{service: svc, stats: newOpenAIAccountRuntimeStats()}
	selection, escaped, err := scheduler.selectBySessionHash(ctx, OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, GroupID: &group, SessionHash: "rpm-gate", StickyAccountID: 1, RequestedModel: "gpt-5.1", EncryptedReasoning: true})
	require.NoError(t, err)
	require.Nil(t, selection)
	require.False(t, escaped)
	require.Empty(t, acquired)
}

func TestEncryptedStickyProfitVetoKeepsBinding(t *testing.T) {
	ctx := WithOpenAIEncryptedReasoningAffinity(profitControlTestCtx(profitControlTestGroup(23, 0.5, 0)), []byte(`{"input":[{"type":"reasoning","encrypted_content":"synthetic"}]}`))
	groupID := int64(23)
	now := time.Now()
	expensive := profitControlWSAccount(31, 0.8, now)

	cache := &stubGatewayCache{}
	store := NewOpenAIWSStateStore(cache)
	svc := &OpenAIGatewayService{
		accountRepo:        stubOpenAIAccountRepo{accounts: []Account{expensive}},
		cache:              cache,
		cfg:                newOpenAIWSV2TestConfig(),
		concurrencyService: NewConcurrencyService(stubConcurrencyCache{}),
		openaiWSStateStore: store,
	}
	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_profit", expensive.ID, time.Hour))

	selection, err := svc.SelectAccountByPreviousResponseID(ctx, &groupID, "resp_profit", "gpt-5.1", nil, false)
	require.NoError(t, err)
	require.Nil(t, selection, "上游倍率 0.8 超过阈值 0.5 的账号不应继续命中 previous_response_id 粘连")

	// 利润不合格与 quota auto-pause 同为暂时状态：绑定必须保留。
	boundAccountID, getErr := store.GetResponseAccount(ctx, groupID, "resp_profit")
	require.NoError(t, getErr)
	require.Equal(t, expensive.ID, boundAccountID)

	// 上游倍率回落（探测刷新）后同一绑定重新可用。
	recovered := profitControlWSAccount(31, 0.3, time.Now())
	svc.accountRepo = stubOpenAIAccountRepo{accounts: []Account{recovered}}
	selection, err = svc.SelectAccountByPreviousResponseID(ctx, &groupID, "resp_profit", "gpt-5.1", nil, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, recovered.ID, selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func newLegacySchedulerDecisionTestAccounts(groupID int64, secondWSEnabled bool) []Account {
	return []Account{
		{
			ID: 38101, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0,
			GroupIDs: []int64{groupID},
			Extra:    map[string]any{"openai_apikey_responses_websockets_v2_enabled": true},
		},
		{
			ID: 38102, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5,
			GroupIDs: []int64{groupID},
			Extra:    map[string]any{"openai_apikey_responses_websockets_v2_enabled": secondWSEnabled},
		},
	}
}

func newLegacySchedulerDecisionTestService(accounts []Account, loadBatch bool, concurrency schedulerTestConcurrencyCache) *OpenAIGatewayService {
	cfg := newSchedulerTestOpenAIWSV2Config()
	cfg.Gateway.Scheduling.LoadBatchEnabled = loadBatch
	return &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(concurrency),
	}
}

func releaseLegacySchedulerDecisionSelection(selection *AccountSelectionResult) {
	if selection != nil && selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}
