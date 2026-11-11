//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPriorityObservationRouteReplacementFeedback(t *testing.T) {
	for _, change := range []string{"base_url", "api_key", "api_protocol", "model_mapping", "proxy", "transport"} {
		t.Run(change, func(t *testing.T) {
			account := newOpenAIRejectedFieldTestAccount()
			account.Type = AccountTypeAPIKey
			account.Credentials["api_key"] = "synthetic-route-a"
			other := *account
			other.Credentials = shallowCopyMap(account.Credentials)
			other.Extra = shallowCopyMap(account.Extra)
			switch change {
			case "proxy":
				id := int64(7)
				other.ProxyID = &id
			case "transport":
				other.Extra["openai_ws_force_http"] = true
			case "model_mapping":
				other.Credentials[change] = map[string]any{"fixture": "another-model"}
			default:
				other.Credentials[change] = "synthetic-route-b"
			}
			require.NotEqual(t, AccountRouteFingerprint(account), AccountRouteFingerprint(&other))
			svc := newOpenAIRejectedFieldTestService(&httpUpstreamRecorder{})
			for range 5 {
				ctx, scope := withPriorityAttemptScope(t.Context(), "fixture", "/v1/responses")
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://synthetic.invalid/v1/responses", nil)
				require.NoError(t, err)
				observe := svc.observePriorityAttempt(req, account)
				resp := priorityMockResponse(503, `{"error":{"type":"server_error"}}`, false)
				observe(resp, nil)
				_, err = io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				scope.finish(nil)
			}
			penalties := svc.defaultFeedbackPenalties(t.Context(), nil, "fixture", []*Account{account})
			require.Greater(t, penalties[account.ID], 0.0)
			penalties = svc.defaultFeedbackPenalties(t.Context(), nil, "fixture", []*Account{&other})
			require.Zero(t, penalties[account.ID], "replacement route must start without the old route's failures")
			req := OpenAIAccountScheduleRequest{RequestedModel: "fixture", ObservationEffortScoped: true}
			scheduler := &defaultOpenAIAccountScheduler{service: svc, stats: svc.openaiAccountStats}
			cfg := DefaultPrioritySchedulingConfig()
			require.Equal(t, int64(5), scheduler.priorityErrorEvidence(req, cfg, account, time.Now()).Failures)
			require.Zero(t, scheduler.priorityErrorEvidence(req, cfg, &other, time.Now()).Failures)
			escape := openAIStickyEscapeConfig{enabled: true, errorRate: .2, ttftMs: 1000}
			_, _, _, oldEscape := scheduler.shouldEscapeStickyForRequest(req, account, escape)
			_, _, _, newEscape := scheduler.shouldEscapeStickyForRequest(req, &other, escape)
			require.True(t, oldEscape)
			require.False(t, newEscape)
			plan := scheduler.buildOpenAIAccountLoadPlan(t.Context(), req, []*Account{&other}, nil)
			require.Len(t, plan.allCandidates, 1)
			require.Zero(t, plan.allCandidates[0].errorRate)
		})
	}
}

func TestPriorityObservationRouteCapturedBeforeCompletion(t *testing.T) {
	account := newOpenAIRejectedFieldTestAccount()
	account.Type = AccountTypeAPIKey
	account.Credentials["api_key"] = "synthetic-route-a"
	original := *account
	original.Credentials = shallowCopyMap(account.Credentials)
	svc := newOpenAIRejectedFieldTestService(&httpUpstreamRecorder{})
	for range 5 {
		ctx, scope := withPriorityAttemptScope(t.Context(), "fixture", "/v1/responses")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://synthetic.invalid/v1/responses", nil)
		require.NoError(t, err)
		account.Credentials["api_key"] = "synthetic-route-a"
		observe := svc.observePriorityAttempt(req, account)
		account.Credentials["api_key"] = "synthetic-route-b"
		resp := priorityMockResponse(503, `{"error":{"type":"server_error"}}`, false)
		observe(resp, nil)
		_, err = io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		scope.finish(nil)
	}
	require.Zero(t, svc.defaultFeedbackPenalties(t.Context(), nil, "fixture", []*Account{account})[account.ID], "late completion must not relabel the physical send")
	require.Greater(t, svc.defaultFeedbackPenalties(t.Context(), nil, "fixture", []*Account{&original})[original.ID], 0.0)
	req := OpenAIAccountScheduleRequest{RequestedModel: "fixture"}
	input := newPrioritySnapshotInput(req, DefaultPrioritySchedulingConfig(), []openAIAccountCandidateScore{{account: &original}}, time.Now())
	input.limited = true
	require.Empty(t, input.candidates[0].account.Credentials, "snapshots must never retain authentication material")
	original.Credentials["api_key"] = "synthetic-route-b"
	snapshot := svc.prioritySchedulingSnapshot(input)
	require.Equal(t, int64(5), snapshot.Candidates[0].Failures, "snapshot refresh must use its captured route, not the stripped account or a later mutation")
}

func TestPriorityObservationRouteOAuthAndManagedIsolation(t *testing.T) {
	req := OpenAIAccountScheduleRequest{RequestedModel: "fixture", ObservationEffortScoped: true}
	a := &Account{ID: 77, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "synthetic-old", "refresh_token": "synthetic-old-refresh"}}
	key := priorityRequestAttemptKey(req, a)
	a.Credentials["access_token"], a.Credentials["refresh_token"] = "synthetic-renewed", "synthetic-renewed-refresh"
	require.Equal(t, key, priorityRequestAttemptKey(req, a), "normal token renewal must retain route evidence")
	stats := newOpenAIAccountRuntimeStats()
	stats.attempts.report(key, false, time.Now())
	a.AuthConnectionID = "grant-a"
	grant := priorityRequestAttemptKey(req, a)
	require.NotEqual(t, key, grant)
	stats.attempts.report(grant, false, time.Now())
	a.AuthConnectionID = "grant-b"
	require.Equal(t, grant.route.fingerprint, priorityRequestAttemptKey(req, a).route.fingerprint, "fixture deliberately shares the parent routing settings")
	require.Zero(t, stats.attempts.snapshot(priorityRequestAttemptKey(req, a), time.Now(), 10, 5).Failures, "a sibling grant must not borrow another grant or parent evidence")
}

func TestPriorityObservationRouteHistoryInvalidation(t *testing.T) {
	cfg := DefaultPrioritySchedulingConfig()
	reader := &priorityReaderStub{signal: map[int64]PrioritySchedulingSignal{1: {Samples: 10, P90TTFTMs: 25000}, 2: {Samples: 10, P90TTFTMs: 500}}}
	svc := priorityGateway(cfg, reader)
	req := OpenAIAccountScheduleRequest{RequestedModel: "fixture"}
	pool := []openAIAccountCandidateScore{priorityCandidate(1, .1, 0), priorityCandidate(2, .1, 0)}
	require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, time.Second, time.Millisecond)
	old := pool[0]
	replacement := *old.account
	replacement.Credentials = map[string]any{"base_url": "https://replacement.invalid"}
	pool[0].account = &replacement
	reader.mu.Lock()
	calls := reader.calls
	reader.signal[1] = PrioritySchedulingSignal{Samples: 10, P90TTFTMs: 500}
	reader.mu.Unlock()
	require.NotContains(t, svc.cachedPriorityHistory(req, cfg, pool).signals, int64(1), "read-only sticky lookup must reject a different route without launching SQL")
	reader.mu.Lock()
	require.Equal(t, calls, reader.calls)
	reader.mu.Unlock()
	history := svc.priorityHistory(req, cfg, pool)
	require.NotContains(t, history.signals, int64(1), "a route change invalidates its cached latency immediately")
	require.Equal(t, 10, history.signals[2].Samples, "route changes must retain unchanged peers")
	require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, 2*time.Second, time.Millisecond)
	require.Equal(t, 500.0, svc.cachedPriorityHistory(req, cfg, pool).signals[1].P90TTFTMs, "the replacement recovers only after a read for its own route")
}

func TestPriorityObservationRouteHistoryRejectsLateRead(t *testing.T) {
	reader := newPriorityRecomputeBarrierReader([]PrioritySchedulingSignal{{Samples: 10, P90TTFTMs: 25000}}, nil)
	t.Cleanup(func() { reader.releaseRead(0) })
	svc := &OpenAIGatewayService{usageLogRepo: reader}
	cfg := DefaultPrioritySchedulingConfig()
	req := OpenAIAccountScheduleRequest{RequestedModel: "fixture"}
	pool := []openAIAccountCandidateScore{priorityCandidate(1, .1, 0), priorityCandidate(2, .1, 0)}
	svc.priorityHistory(req, cfg, pool)
	select {
	case <-reader.entered:
	case <-time.After(time.Second):
		t.Fatal("history read did not start")
	}
	pool[0].account.Credentials = map[string]any{"base_url": "https://replacement.invalid"}
	svc.priorityHistory(req, cfg, pool)
	reader.releaseRead(0)
	require.Eventually(t, func() bool {
		svc.priorityScheduling.mu.Lock()
		defer svc.priorityScheduling.mu.Unlock()
		return svc.priorityScheduling.active == 0
	}, time.Second, time.Millisecond)
	history := svc.cachedPriorityHistory(req, cfg, pool)
	require.NotContains(t, history.signals, int64(1), "a SQL read captured for the old route must not repopulate the replacement")
	require.Equal(t, 10, history.signals[2].Samples, "a late read may still populate an unchanged peer")
}

func TestPriorityObservationRouteHistoryQueryOwnership(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprint(managed), func(t *testing.T) {
			reader := &observationQueryReader{}
			svc := &OpenAIGatewayService{usageLogRepo: reader}
			account := priorityCandidate(1, .1, 0).account
			account.Credentials = map[string]any{"api_key": "synthetic-original"}
			if managed {
				account.Extra["openai_connections_managed"] = true
			}
			want := priorityAccountObservationRoute(account).historyFingerprint()
			if managed {
				require.Empty(t, want, "a parent fingerprint does not prove which grant generated historical usage")
			} else {
				require.NotEmpty(t, want)
			}
			req := OpenAIAccountScheduleRequest{RequestedModel: "fixture"}
			input := newPrioritySnapshotInput(req, DefaultPrioritySchedulingConfig(), []openAIAccountCandidateScore{{account: account}}, time.Now())
			account.Credentials["api_key"] = "synthetic-replacement"
			require.Eventually(t, func() bool { return svc.priorityHistory(req, input.config, input.candidates).ready() }, time.Second, time.Millisecond)
			reader.mu.Lock()
			require.Len(t, reader.queries, 1)
			require.Equal(t, map[int64]string{1: want}, reader.queries[0].AccountRouteFingerprints, "SQL must receive the captured route, never a digest rebuilt from stripped credentials")
			reader.mu.Unlock()
		})
	}
}

func TestPriorityObservationRouteSnapshotCannotRewindCache(t *testing.T) {
	cfg := DefaultPrioritySchedulingConfig()
	reader := &priorityReaderStub{signal: map[int64]PrioritySchedulingSignal{1: {Samples: 10, P90TTFTMs: 25000}}}
	svc := priorityGateway(cfg, reader)
	req := OpenAIAccountScheduleRequest{RequestedModel: "fixture"}
	pool := []openAIAccountCandidateScore{priorityCandidate(1, .1, 0)}
	require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, time.Second, time.Millisecond)
	input := newPrioritySnapshotInput(req, cfg, pool, time.Now())
	pool[0].account.Credentials = map[string]any{"base_url": "https://replacement.invalid"}
	reader.mu.Lock()
	reader.signal[1] = PrioritySchedulingSignal{Samples: 10, P90TTFTMs: 500}
	reader.mu.Unlock()
	require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, 2*time.Second, time.Millisecond)
	snapshot := svc.prioritySchedulingSnapshot(input)
	require.False(t, snapshot.HistoryReady, "an old snapshot must not adopt the replacement route's history")
	require.Equal(t, "unavailable", snapshot.HistoryStatus)
	require.Equal(t, 500.0, svc.cachedPriorityHistory(req, cfg, pool).signals[1].P90TTFTMs, "reading an old snapshot must not evict current live-route evidence")
}

func TestPriorityObservationRouteSelectionWaitRecheck(t *testing.T) {
	svc, scheduler, req, pool, _ := latencyModeFixture(t, PriorityLatencyStop)
	setModeSignal(svc, req, 1, func(v *PrioritySchedulingSignal) { v.P90TTFTMs, v.LatestBadLatencyAt = 500, nil })
	selected, err := modeSelect(t, scheduler, req)
	require.NoError(t, err)
	require.NoError(t, svc.CheckPriorityLatencySelection(t.Context(), selected))
	for _, item := range selected.priorityRequest.priorityLatencyPool.accounts {
		require.Empty(t, item.account.Credentials)
		require.NotNil(t, item.observationRoute)
	}
	svc.usageLogRepo = &priorityReaderStub{err: context.DeadlineExceeded}
	original := selected.Account.Credentials
	selected.Account.Credentials = map[string]any{"base_url": "https://replacement.invalid"}
	require.ErrorIs(t, svc.CheckPriorityLatencySelection(t.Context(), selected), ErrPriorityLatencyUnavailable, "slot waits must recheck the current selected route instead of its original healthy evidence")
	selected.Account.Credentials = original
	svc.usageLogRepo = &priorityReaderStub{signal: map[int64]PrioritySchedulingSignal{1: {Samples: 20, P90TTFTMs: 500}, 2: {Samples: 20, P90TTFTMs: 500}}}
	require.Eventually(t, func() bool { return svc.priorityHistory(req, svc.prioritySchedulingRuntimeConfig(), pool).ready() }, 2*time.Second, time.Millisecond)
	require.NoError(t, svc.CheckPriorityLatencySelection(t.Context(), selected), "returning to a proved healthy route recovers normally")
}

// Use the actual Forward transport observer and dashboard reader, with a CC
// capability for a Responses request. No paid upstream or projected counters.
func TestPriorityObservationResponsesEndpoint(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"mock","stream":false}`)
	upstream := &httpUpstreamRecorder{resp: priorityMockResponse(200, `{"id":"resp_mock","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`, false)}
	svc := newOpenAIRejectedFieldTestService(upstream)
	account := newOpenAIRejectedFieldTestAccount()
	ctx := WithPrioritySchedulingObservation(context.Background(), nil, "gpt-5.5", "/v1/responses")
	_, err := svc.Forward(ctx, newOpenAIRejectedFieldTestContext(body), account, body)
	require.NoError(t, err)
	req := OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.5", HistoryModel: "gpt-5.5", InboundEndpoint: "/v1/responses", RequiredCapability: OpenAIEndpointCapabilityChatCompletions}
	scheduler := &defaultOpenAIAccountScheduler{service: svc, stats: svc.openaiAccountStats}
	evidence := scheduler.priorityErrorEvidence(req, DefaultPrioritySchedulingConfig(), account, time.Now())
	require.Equal(t, int64(1), evidence.Successes, "Responses capability fallback must read actual Responses ingress")
	require.Equal(t, "/v1/responses", evidence.Endpoint)
	svc.priorityScheduling.latestInput = newPrioritySnapshotInput(req, DefaultPrioritySchedulingConfig(), []openAIAccountCandidateScore{{account: account}}, time.Now())
	snapshot := svc.PrioritySchedulingSnapshot()
	require.Equal(t, int64(1), snapshot.Candidates[0].Successes)
}

type observationQueryReader struct {
	UsageLogRepository
	mu        sync.Mutex
	queries   []PrioritySchedulingQuery
	endpoints map[string]map[int64]PrioritySchedulingSignal
}

func (r *observationQueryReader) ReadPrioritySchedulingSignals(_ context.Context, q PrioritySchedulingQuery) (map[int64]PrioritySchedulingSignal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, q)
	if r.endpoints != nil {
		return r.endpoints[q.InboundEndpoint], nil
	}
	return map[int64]PrioritySchedulingSignal{1: {Samples: 6, P90TTFTMs: 1234}}, nil
}

func TestPriorityObservationPublicHistoryModel(t *testing.T) {
	c := DefaultPrioritySchedulingConfig()
	reader := &observationQueryReader{}
	svc := &OpenAIGatewayService{usageLogRepo: reader}
	group := int64(11)
	req := OpenAIAccountScheduleRequest{RequestedModel: "route-model", HistoryModel: "public-alias", GroupID: &group}
	pool := []openAIAccountCandidateScore{priorityCandidate(1, .1, 0)}
	require.Eventually(t, func() bool { return svc.priorityHistory(req, c, pool).ready() }, time.Second, time.Millisecond)
	reader.mu.Lock()
	require.Len(t, reader.queries, 1)
	require.Equal(t, "public-alias", reader.queries[0].Model)
	require.Equal(t, "route-model", reader.queries[0].QualityModel)
	require.Equal(t, group, *reader.queries[0].GroupID)
	require.Equal(t, "/v1/responses", reader.queries[0].InboundEndpoint)
	reader.mu.Unlock()
	other := req
	other.HistoryModel = "distinct-public-alias"
	require.Empty(t, svc.priorityHistory(other, c, pool).signals)
	other = req
	other.GroupID = nil
	require.Empty(t, svc.priorityHistory(other, c, pool).signals)
}

func TestPriorityObservationHistoryEndpointIsolation(t *testing.T) {
	cfg := DefaultPrioritySchedulingConfig()
	reader := &observationQueryReader{}
	svc := &OpenAIGatewayService{usageLogRepo: reader}
	req := OpenAIAccountScheduleRequest{RequestedModel: "route-model", HistoryModel: "public-alias", InboundEndpoint: "/v1/responses"}
	pool := []openAIAccountCandidateScore{priorityCandidate(1, .1, 0)}
	require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, time.Second, time.Millisecond)
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses/compact", "/v1/messages", "/unknown"} {
		t.Run(endpoint, func(t *testing.T) {
			other := req
			other.InboundEndpoint = endpoint
			require.NotEqual(t, priorityHistoryKey(req, cfg), priorityHistoryKey(other, cfg), "different ingress cannot share a latency population")
			require.Empty(t, svc.cachedPriorityHistory(other, cfg, pool).signals, "sticky balancing must not borrow another endpoint's evidence")
			require.Empty(t, svc.priorityHistory(other, cfg, pool).signals, "an asynchronous miss is unknown, not the previous endpoint's history")
		})
	}
	implicit := req
	implicit.InboundEndpoint = ""
	require.Equal(t, priorityHistoryKey(req, cfg), priorityHistoryKey(implicit, cfg), "legacy Responses defaults remain exact")
	implicit.RequireCompact = true
	explicit := req
	explicit.InboundEndpoint = "/v1/responses/compact"
	require.Equal(t, priorityHistoryKey(explicit, cfg), priorityHistoryKey(implicit, cfg))
	implicit.RequireCompact = false
	implicit.RequiredCapability = OpenAIEndpointCapabilityChatCompletions
	explicit.InboundEndpoint = "/v1/chat/completions"
	require.Equal(t, priorityHistoryKey(explicit, cfg), priorityHistoryKey(implicit, cfg))
	explicit.InboundEndpoint = " /v1/chat/completions "
	require.Equal(t, priorityHistoryKey(explicit, cfg), priorityHistoryKey(implicit, cfg), "canonical ingress whitespace cannot split the cache")
}

func TestPriorityObservationSelectionRetainsEffort(t *testing.T) {
	group := int64(11)
	req := OpenAIAccountScheduleRequest{GroupID: &group, RequestedModel: "route-model", HistoryModel: "public-alias", InboundEndpoint: "/v1/responses", ObservationEffortScoped: true, ObservationEffort: "xhigh"}
	owned := ownedPriorityScheduleRequest(req)
	require.True(t, owned.ObservationEffortScoped, "waiting for a slot must not fall back to account-wide history")
	require.Equal(t, req.ObservationEffort, owned.ObservationEffort)
	require.Equal(t, priorityHistoryKey(req, DefaultPrioritySchedulingConfig()), priorityHistoryKey(owned, DefaultPrioritySchedulingConfig()))
	group++
	require.Equal(t, int64(11), *owned.GroupID, "owned request must retain its group independently")
}

func TestPriorityObservationEndpointSelectionAndRecovery(t *testing.T) {
	svc, scheduler, req, pool, _ := latencyModeFixture(t, PriorityLatencyStop)
	cfg := svc.prioritySchedulingRuntimeConfig()
	bad := time.Now().Add(-time.Minute)
	reader := &observationQueryReader{endpoints: map[string]map[int64]PrioritySchedulingSignal{
		"/v1/chat/completions": {
			1: {Samples: 20, P90TTFTMs: 500},
			2: {Samples: 20, P90TTFTMs: 16000, LatestBadLatencyAt: &bad},
		},
	}}
	svc.usageLogRepo = reader
	selected, err := modeSelect(t, scheduler, req)
	require.NoError(t, err)
	require.Equal(t, int64(2), selected.Account.ID)
	chat := req
	chat.InboundEndpoint = "/v1/chat/completions"
	chat.SessionHash, chat.StickyAccountID = "mode-sticky", 1
	require.Empty(t, svc.cachedPriorityHistory(chat, cfg, pool).signals)
	require.Eventually(t, func() bool { return svc.priorityHistory(chat, cfg, pool).ready() }, 2*time.Second, time.Millisecond)
	selected, err = modeSelect(t, scheduler, chat)
	require.NoError(t, err)
	require.Equal(t, int64(1), selected.Account.ID, "Responses slowdown must not evict a healthy Chat sticky account")
	require.NoError(t, svc.CheckPriorityLatencySelection(context.Background(), selected))
	snapshot := svc.PrioritySchedulingSnapshot()
	require.NotNil(t, snapshot)
	require.True(t, snapshot.HistoryReady)
	require.Equal(t, int64(1), snapshot.Candidates[0].AccountID)
	for _, candidate := range snapshot.Candidates {
		require.Equal(t, chat.InboundEndpoint, candidate.Endpoint, "dashboard must retain the same endpoint population")
	}
	reader.mu.Lock()
	require.Len(t, reader.queries, 1)
	require.Equal(t, chat.InboundEndpoint, reader.queries[0].InboundEndpoint)
	reader.mu.Unlock()
	var wg sync.WaitGroup
	for i := range 60 {
		wg.Go(func() {
			request := req
			want := 16000.0
			if i%2 == 0 {
				request, want = chat, 500
			}
			signal := svc.cachedPriorityHistory(request, cfg, pool).signals[1]
			if signal.P90TTFTMs != want {
				t.Errorf("endpoint %q latency: got %v, want %v", request.InboundEndpoint, signal.P90TTFTMs, want)
			}
		})
	}
	wg.Wait()
	selected, err = modeSelect(t, scheduler, req)
	require.NoError(t, err)
	require.Equal(t, int64(2), selected.Account.ID, "returning to Responses must recover its own evidence, not replace it with Chat history")
}

func TestPriorityObservationScopedSelectionWaitRecheck(t *testing.T) {
	svc, scheduler, req, pool, _ := latencyModeFixture(t, PriorityLatencyStop)
	req.ObservationEffort, req.ObservationEffortScoped = "xhigh", true
	cfg := svc.prioritySchedulingRuntimeConfig()
	require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, 2*time.Second, time.Millisecond)
	selected, err := modeSelect(t, scheduler, req)
	require.NoError(t, err)
	require.Equal(t, int64(2), selected.Account.ID)
	require.NoError(t, svc.CheckPriorityLatencySelection(context.Background(), selected), "the same evidence must remain valid after slot acquisition")
	ctx := ContextWithSelectionPriorityRequest(context.Background(), selected)
	ctx, err = svc.beginPriorityLatencyTurn(ctx, nil, selected.Account, req.RequestedModel)
	require.NoError(t, err)
	setModeSignal(svc, req, 1, func(v *PrioritySchedulingSignal) { v.P90TTFTMs, v.LatestBadLatencyAt = 500, nil })
	setModeSignal(svc, req, 2, func(v *PrioritySchedulingSignal) { v.P90TTFTMs = 16000; now := time.Now(); v.LatestBadLatencyAt = &now })
	require.ErrorIs(t, svc.CheckPriorityLatencySelection(ctx, selected), ErrPriorityLatencyUnavailable, "a wait must still recheck the selected effort, not the older unscoped population")
	require.ErrorIs(t, svc.priorityLatencyBeforeSend(ctx, selected.Account), ErrPriorityLatencyUnavailable)
	setModeSignal(svc, req, 2, func(v *PrioritySchedulingSignal) { v.P90TTFTMs, v.LatestBadLatencyAt = 500, nil })
	require.NoError(t, svc.CheckPriorityLatencySelection(ctx, selected), "restored same-scope evidence recovers the selected account")
}

func TestPriorityObservationAttemptGroupAndAliasIsolation(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	svc := &OpenAIGatewayService{openaiAccountStats: stats}
	group := int64(11)
	ctx := WithPrioritySchedulingObservation(context.Background(), &group, " public-alias ", "/v1/chat/completions")
	ctx, scope := withPriorityAttemptScope(ctx, " padded-route ", "/v1/chat/completions")
	require.Equal(t, "public-alias", scope.model)
	require.Equal(t, int64(11), scope.groupID)
	_ = ctx
	req := OpenAIAccountScheduleRequest{RequestedModel: "padded-route", HistoryModel: "public-alias", InboundEndpoint: "/v1/chat/completions", GroupID: &group}
	account := priorityCandidate(1, .1, 0).account
	stats.attempts.report(priorityRequestAttemptKey(req, account), true, time.Now())
	scheduler := &defaultOpenAIAccountScheduler{service: svc, stats: stats}
	cfg := DefaultPrioritySchedulingConfig()
	require.Equal(t, int64(1), scheduler.priorityErrorEvidence(req, cfg, account, time.Now()).Successes)
	other := req
	other.HistoryModel = "other-alias"
	require.Zero(t, scheduler.priorityErrorEvidence(other, cfg, account, time.Now()).Successes)
	other = req
	other.GroupID = nil
	require.Zero(t, scheduler.priorityErrorEvidence(other, cfg, account, time.Now()).Successes)
	group = 12
	require.Equal(t, int64(11), scope.groupID, "observer owns group value")
	require.Zero(t, scheduler.priorityErrorEvidence(req, cfg, account, time.Now()).Successes)
}

func TestPriorityObservationCompletedCallFreshness(t *testing.T) {
	cfg := DefaultPrioritySchedulingConfig()
	reader := &priorityReaderStub{signal: map[int64]PrioritySchedulingSignal{1: {}}}
	svc := priorityGateway(cfg, reader)
	req := OpenAIAccountScheduleRequest{RequestedModel: "gpt-test", HistoryModel: "public-alias"}
	pool := []openAIAccountCandidateScore{priorityCandidate(1, .1, 0)}
	require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, time.Second, time.Millisecond)
	svc.priorityScheduling.mu.Lock()
	entry := svc.priorityScheduling.entries[priorityHistoryKey(req, cfg)]
	require.LessOrEqual(t, entry.expires.Sub(entry.observed), 5*time.Second, "completed persistence after an empty read must become visible within the cold refresh budget")
	entry.expires = time.Now().Add(-time.Second)
	svc.priorityScheduling.launches = [2]time.Time{}
	svc.priorityScheduling.mu.Unlock()
	// The reader now represents completed, persisted usage; no live/in-flight
	// timing is injected into the cache and nil TTFT stays unknown.
	reader.mu.Lock()
	reader.signal = map[int64]PrioritySchedulingSignal{1: {Samples: 1, P90TTFTMs: 1200, LatencyUnknownSamples: 1, ProfitSamples: 2}}
	reader.mu.Unlock()
	require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).signals[1].Samples == 1 }, time.Second, time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); svc.priorityHistory(req, cfg, pool) }()
	}
	wg.Wait()
	reader.mu.Lock()
	require.Equal(t, 2, reader.calls, "polls share two real completed reads")
	reader.mu.Unlock()
	got := svc.priorityHistory(req, cfg, pool).signals[1]
	require.Equal(t, 1, got.LatencyUnknownSamples)
}

func TestPriorityObservationStickyPenaltyAndExpiry(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	cfg := DefaultPrioritySchedulingConfig()
	cfg.Enabled = true
	cfg.Models = []string{"public-alias"}
	group := int64(11)
	cfg.GroupIDs = []int64{group}
	badAt := time.Now().Add(-time.Minute)
	reader := &priorityReaderStub{signal: map[int64]PrioritySchedulingSignal{
		1: {Samples: 20, P90TTFTMs: 20000, LatestBadLatencyAt: &badAt, QualitySamples: 10, QualityPassed: 10},
		2: {Samples: 20, P90TTFTMs: 500, QualitySamples: 10, QualityPassed: 10},
	}}
	svc := priorityGateway(cfg, reader)
	pool := []openAIAccountCandidateScore{priorityCandidate(1, .1, 0), priorityCandidate(2, 1, 0)}
	accounts := make([]Account, 0, 2)
	for _, item := range pool {
		item.account.Status = StatusActive
		item.account.Schedulable = true
		item.account.GroupIDs = []int64{group}
		accounts = append(accounts, *item.account)
	}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:sticky-penalty": 1}}
	svc.cache = cache
	svc.accountRepo = schedulerTestOpenAIAccountRepo{accounts: accounts}
	svc.cfg = newSchedulerTestOpenAIWSV2Config()
	svc.cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = false
	svc.concurrencyService = NewConcurrencyService(schedulerTestConcurrencyCache{})
	req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, GroupID: &group, RequestedModel: "gpt-test", HistoryModel: "public-alias", InboundEndpoint: "/v1/responses", UseUpstreamTokenCost: true}
	require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, time.Second, time.Millisecond)
	ctx := WithPrioritySchedulingObservation(context.Background(), &group, "public-alias", "/v1/responses")
	selectTurn := func(previous string, excluded map[int64]struct{}) (*AccountSelectionResult, OpenAIAccountScheduleDecision) {
		t.Helper()
		selection, decision, err := svc.SelectAccountWithSchedulerForCapability(ctx, &group, previous, "sticky-penalty", "gpt-test", excluded, OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true, PlatformOpenAI)
		require.NoError(t, err)
		require.NotNil(t, selection)
		if selection.ReleaseFunc != nil {
			t.Cleanup(selection.ReleaseFunc)
		}
		return selection, decision
	}
	selected, decision := selectTurn("", nil)
	require.Equal(t, int64(2), selected.Account.ID, "ordinary sticky cannot bypass measured penalty even with EWMA escape disabled")
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.Equal(t, int64(1), cache.sessionBindings["openai:sticky-penalty"], "one turn does not erase durable affinity")
	// A separate weighted shortcut must not undo the scored selection policy.
	weightedReq := req
	weightedReq.StickyWeighted = true
	weightedReq.StickyAccountID = 1
	weightedReq.SessionHash = "sticky-penalty"
	scheduler := &defaultOpenAIAccountScheduler{service: svc, stats: svc.openaiAccountStats}
	fallback, fallbackErr := scheduler.tryFallbackToWeightedSticky(ctx, weightedReq)
	require.NoError(t, fallbackErr)
	require.Nil(t, fallback, "weighted fallback cannot reintroduce the active measured penalty")
	for _, ownerReq := range []OpenAIAccountScheduleRequest{
		func() OpenAIAccountScheduleRequest { r := req; r.DisableStickyEscape = true; return r }(),
		func() OpenAIAccountScheduleRequest { r := req; r.GuardianParentAccountID = 1; return r }(),
		func() OpenAIAccountScheduleRequest { r := req; r.EncryptedReasoning = true; return r }(),
		func() OpenAIAccountScheduleRequest { r := req; r.PreserveStickyBinding = true; return r }(),
	} {
		require.False(t, scheduler.shouldYieldPriorityLatencySticky(ownerReq, pool[0].account))
	}
	// No global ban: the same slow account remains fallback capacity.
	selected, _ = selectTurn("", map[int64]struct{}{2: {}})
	require.Equal(t, int64(1), selected.Account.ID)
	// Provider response ownership wins, even during the measured penalty.
	require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(ctx, group, "resp_penalty_owner", 1, time.Hour))
	selected, decision = selectTurn("resp_penalty_owner", nil)
	require.Equal(t, int64(1), selected.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerPreviousResponse, decision.Layer)
	// Re-reading old data does not renew its ten-minute duration.
	svc.priorityScheduling.mu.Lock()
	entry := svc.priorityScheduling.entries[priorityHistoryKey(req, cfg)]
	expired := time.Now().Add(-11 * time.Minute)
	signal := entry.signals[1]
	signal.LatestBadLatencyAt = &expired
	entry.signals[1] = signal
	svc.priorityScheduling.mu.Unlock()
	selected, decision = selectTurn("", nil)
	require.Equal(t, int64(1), selected.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	fallback, fallbackErr = scheduler.tryFallbackToWeightedSticky(ctx, weightedReq)
	require.NoError(t, fallbackErr)
	require.NotNil(t, fallback, "expiry also restores the weighted sticky fallback")
	if fallback.ReleaseFunc != nil {
		fallback.ReleaseFunc()
	}
}

type observationMissingReader struct{ priorityReaderStub }

func (r *observationMissingReader) ReadPrioritySchedulingSignals(context.Context, PrioritySchedulingQuery) (map[int64]PrioritySchedulingSignal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return map[int64]PrioritySchedulingSignal{}, nil
}

func TestPriorityObservationQueryBudgetAndMissingData(t *testing.T) {
	cfg := DefaultPrioritySchedulingConfig()
	reader := &observationMissingReader{}
	svc := priorityGateway(cfg, &priorityReaderStub{})
	svc.usageLogRepo = reader
	pool := []openAIAccountCandidateScore{priorityCandidate(1, .1, 0)}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := OpenAIAccountScheduleRequest{RequestedModel: fmt.Sprintf("model-%d", i)}
			svc.priorityHistory(req, cfg, pool)
		}(i)
	}
	wg.Wait()
	require.Eventually(t, func() bool {
		svc.priorityScheduling.mu.Lock()
		defer svc.priorityScheduling.mu.Unlock()
		return svc.priorityScheduling.active == 0
	}, time.Second, time.Millisecond)
	reader.mu.Lock()
	require.Equal(t, 2, reader.calls, "global rolling budget permits two initial workers and bounds cross-scope bursts")
	reader.mu.Unlock()
	svc.priorityScheduling.mu.Lock()
	defer svc.priorityScheduling.mu.Unlock()
	require.LessOrEqual(t, len(svc.priorityScheduling.entries), priorityHistoryMaxScopes)
	for _, entry := range svc.priorityScheduling.entries {
		require.Empty(t, entry.signals, "an absent reader row must not be fabricated as zero observations")
	}
}

func TestPriorityObservationScopeCompatibility(t *testing.T) {
	cfg := DefaultPrioritySchedulingConfig()
	cfg.Enabled = true
	group := int64(11)
	cfg.GroupIDs = []int64{group}
	req := OpenAIAccountScheduleRequest{GroupID: &group, RequestedModel: "route-model", HistoryModel: "public-alias"}
	for _, model := range []string{"public-alias", "route-model"} {
		cfg.Models = []string{model}
		require.True(t, priorityApplies(cfg, req))
	}
	cfg.Models = []string{"distinct-model"}
	require.False(t, priorityApplies(cfg, req))
	cfg.Models = nil
	other := int64(12)
	req.GroupID = &other
	require.False(t, priorityApplies(cfg, req))
}
