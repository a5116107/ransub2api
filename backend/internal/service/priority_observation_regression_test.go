//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	hcplugin "github.com/hashicorp/go-plugin"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
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
	key := priorityRequestAttemptKey(input.request, input.candidates[0].account)
	key.route = priorityCandidateObservationRoute(input.candidates[0])
	evidence := svc.openaiAccountStats.attempts.snapshot(key, time.Now(), 10, 5)
	require.Equal(t, int64(5), evidence.Failures, "diagnostics must use the captured route, not stripped credentials or a later mutation")
	require.Zero(t, snapshot.Candidates[0].Failures, "passive attempt diagnostics must not replace the shared performance source")
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
	require.Zero(t, snapshot.Candidates[0].Successes, "passive endpoint evidence must not substitute for account performance")
	require.Nil(t, snapshot.Candidates[0].Performance)
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
	return map[int64]PrioritySchedulingSignal{1: withAccountPerformanceFixture(PrioritySchedulingSignal{Samples: 6, P90TTFTMs: 1234})}, nil
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

func TestPriorityObservationTurnCapturesEffort(t *testing.T) {
	for _, source := range []string{"context", "request"} {
		t.Run(source, func(t *testing.T) {
			ctx := context.Background()
			c := newOpenAIRejectedFieldTestContext(nil)
			if source == "context" {
				ctx = WithObservationEffort(ctx, "medium")
			} else {
				c.Request = c.Request.WithContext(WithObservationEffort(c.Request.Context(), "medium"))
			}
			svc := &OpenAIGatewayService{}
			admitted, err := svc.beginPriorityLatencyTurn(ctx, c, &Account{ID: 1, Platform: PlatformOpenAI}, "fixture")
			require.NoError(t, err)
			turn, ok := admitted.Value(priorityLatencyTurnKey{}).(*priorityLatencyTurn)
			require.True(t, ok)
			require.True(t, turn.request.ObservationEffortScoped, "direct forwarding must retain the exact observed effort population")
			require.Equal(t, "medium", turn.request.ObservationEffort)
		})
	}
}

func TestPriorityObservationWSTurnEffortIsolation(t *testing.T) {
	svc, scheduler, req, pool, _ := latencyModeFixture(t, PriorityLatencyStop)
	cfg := svc.prioritySchedulingRuntimeConfig()
	req.ObservationEffort, req.ObservationEffortScoped = "xhigh", true
	require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, 2*time.Second, time.Millisecond)
	next := req
	next.ObservationEffort = "medium"
	require.Eventually(t, func() bool { return svc.priorityHistory(next, cfg, pool).ready() }, 2*time.Second, time.Millisecond)
	setModeSignal(svc, next, 1, func(v *PrioritySchedulingSignal) { v.P90TTFTMs, v.LatestBadLatencyAt = 500, nil })
	reviewActivateStop(svc, next, 2)
	selected, err := modeSelect(t, scheduler, req)
	require.NoError(t, err)
	require.Equal(t, int64(2), selected.Account.ID)
	ctx := WithPrioritySchedulingObservation(context.Background(), req.GroupID, req.HistoryModel, req.InboundEndpoint)
	ctx = WithObservationEffort(ctx, req.ObservationEffort)
	ctx = ContextWithSelectionPriorityRequest(ctx, selected)
	c := newOpenAIRejectedFieldTestContext([]byte(`{"model":"gpt-test","reasoning":{"effort":"medium"}}`))
	c.Request = c.Request.WithContext(WithObservationEffort(ctx, "medium"))
	c.Request.Header.Set("Upgrade", "websocket")
	_, err = svc.beginPriorityLatencyTurn(WithObservationEffort(ctx, "medium"), c, selected.Account, req.RequestedModel)
	require.ErrorIs(t, err, ErrPriorityLatencyUnavailable, "a policy-changed first frame must rebuild membership for its final effort")
	c.Request = c.Request.WithContext(ctx)
	_, err = svc.nextPriorityLatencyWSTurn(ctx, c, selected.Account, req.RequestedModel, []byte(`{"model":"gpt-test","reasoning":{"effort":"medium"}}`))
	require.ErrorIs(t, err, ErrPriorityLatencyUnavailable, "a new WS turn must not borrow the first turn's healthy effort population")
	admitted, err := svc.nextPriorityLatencyWSTurn(ctx, c, selected.Account, req.RequestedModel, []byte(`{"model":"gpt-test","reasoning":{"effort":"xhigh"}}`))
	require.NoError(t, err, "returning to the original healthy effort recovers")
	turn, ok := admitted.Value(priorityLatencyTurnKey{}).(*priorityLatencyTurn)
	require.True(t, ok)
	require.Equal(t, "xhigh", turn.request.ObservationEffort)
	require.True(t, turn.request.ObservationEffortScoped)
	require.Equal(t, "xhigh", selected.priorityRequest.ObservationEffort, "turn-local scope must not mutate the original selection")
	_, err = svc.beginPriorityLatencyTurn(WithObservationEffort(admitted, "medium"), c, selected.Account, req.RequestedModel)
	require.ErrorIs(t, err, ErrPriorityLatencyUnavailable, "a changed effort before any write must not reuse the prepared turn marker")
	// Once generation starts, new observations cannot interrupt or replay it.
	turn.started = true
	_, err = svc.beginPriorityLatencyTurn(WithObservationEffort(admitted, "medium"), c, selected.Account, req.RequestedModel)
	require.NoError(t, err)
	require.Equal(t, "xhigh", turn.request.ObservationEffort)
	admitted, err = svc.nextPriorityLatencyWSTurn(ctx, c, selected.Account, req.RequestedModel, []byte(`{"model":"gpt-test"}`))
	require.NoError(t, err)
	turn, ok = admitted.Value(priorityLatencyTurnKey{}).(*priorityLatencyTurn)
	require.True(t, ok)
	require.True(t, turn.request.ObservationEffortScoped)
	require.Empty(t, turn.request.ObservationEffort, "a known omitted effort must not inherit xhigh")
}

func TestPriorityObservationHTTPBridgeTurnEffort(t *testing.T) {
	svc, req, account, _, _, ctx := reviewNativeFixture(t, OpenAIWSIngressModeHTTPBridge)
	req.ObservationEffort, req.ObservationEffortScoped = "medium", true
	cfg := svc.prioritySchedulingRuntimeConfig()
	pool := []openAIAccountCandidateScore{{account: account}}
	require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, 2*time.Second, time.Millisecond)
	reviewActivateStop(svc, req, account.ID)
	ctx = WithObservationEffort(ctx, "xhigh")
	body := []byte(`{"model":"gpt-test","reasoning":{"effort":"medium"},"input":"synthetic","stream":true}`)
	c := newOpenAIRejectedFieldTestContext(body)
	c.Request = c.Request.WithContext(ctx)
	c.Request.Header.Set("Upgrade", "websocket")
	upstream := &httpUpstreamRecorder{resp: priorityMockResponse(200, `{"status":"completed","output":[]}`, false)}
	svc.httpUpstream = upstream
	_, err := svc.proxyOpenAIWSHTTPBridgeTurn(ctx, c, account, "sk-synthetic", body, len(body), req.RequestedModel, "", "", "", "", 2, func([]byte) error { return nil })
	require.ErrorIs(t, err, ErrPriorityLatencyUnavailable)
	require.Empty(t, upstream.requests, "HTTP bridge turns must check their own effort before any generation send")
}

func TestPriorityObservationHTTPBridgeLateAdmission(t *testing.T) {
	for _, turn := range []int{1, 2} {
		t.Run(fmt.Sprintf("turn=%d", turn), func(t *testing.T) {
			svc, req, account, _, _, ctx := reviewNativeFixture(t, OpenAIWSIngressModeHTTPBridge)
			req.ObservationEffort, req.ObservationEffortScoped = "medium", true
			cfg := svc.prioritySchedulingRuntimeConfig()
			pool := []openAIAccountCandidateScore{{account: account}}
			require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, 2*time.Second, time.Millisecond)
			setModeSignal(svc, req, account.ID, func(v *PrioritySchedulingSignal) { v.P90TTFTMs, v.LatestBadLatencyAt = 500, nil })
			var stop sync.Once
			ctx = requesttiming.WithClock(ctx, func() time.Time {
				// The bridge starts timing after initial admission, before its
				// physical send. Keep the real deadlines and transport intact.
				stop.Do(func() { reviewActivateStop(svc, req, account.ID) })
				return time.Now()
			})
			body := []byte(`{"model":"gpt-test","reasoning":{"effort":"medium"},"input":"synthetic","stream":true}`)
			c := newOpenAIRejectedFieldTestContext(body)
			c.Request = c.Request.WithContext(ctx)
			c.Request.Header.Set("Upgrade", "websocket")
			upstream := &httpUpstreamRecorder{resp: priorityMockResponse(200, `{"status":"completed","output":[]}`, false)}
			svc.httpUpstream = upstream
			var writes int
			_, err := svc.proxyOpenAIWSHTTPBridgeTurn(ctx, c, account, "sk-synthetic", body, len(body), req.RequestedModel, "", "", "", "", turn, func([]byte) error { writes++; return nil })
			require.ErrorIs(t, err, ErrPriorityLatencyUnavailable, "late local admission must retain its cause on every turn")
			require.Empty(t, upstream.requests, "late rejection must precede any provider request")
			require.Zero(t, writes, "local admission must not emit a provider 502")
		})
	}
}

func TestPriorityObservationHTTPAfterRPMWait(t *testing.T) {
	svc, req, account, _, _, ctx := reviewNativeFixture(t, OpenAIWSIngressModeHTTPBridge)
	account.Type = AccountTypeOAuth
	account.Extra["base_rpm"] = 2
	req.ObservationEffort, req.ObservationEffortScoped = "medium", true
	cfg := svc.prioritySchedulingRuntimeConfig()
	pool := []openAIAccountCandidateScore{{account: account}}
	require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, 2*time.Second, time.Millisecond)
	setModeSignal(svc, req, account.ID, func(v *PrioritySchedulingSignal) { v.P90TTFTMs, v.LatestBadLatencyAt = 500, nil })
	ctx = WithObservationEffort(ctx, "medium")
	ctx, err := svc.beginPriorityLatencyTurn(ctx, nil, account, req.RequestedModel)
	require.NoError(t, err)
	cache := &availabilityAdmissionRPMCache{openAIRPMTestCache: &openAIRPMTestCache{counts: map[int64]int{}}, onAcquire: func() { reviewActivateStop(svc, req, account.ID) }}
	svc.rpmCache = cache
	upstream := &httpUpstreamRecorder{resp: priorityMockResponse(200, `{"status":"completed","output":[]}`, false)}
	svc.httpUpstream = upstream
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(`{"model":"gpt-test","reasoning":{"effort":"medium"}}`))
	require.NoError(t, err)
	response, err := svc.doOpenAIUpstream(request, "", account)
	if response != nil {
		require.NoError(t, response.Body.Close())
	}
	require.ErrorIs(t, err, ErrPriorityLatencyUnavailable, "an RPM wait is not the start of a physical generation")
	require.Empty(t, upstream.requests)
	require.Equal(t, 1, cache.counts[account.ID])
}

type priorityWaitingHTTPUpstream struct {
	*httpUpstreamRecorder
	before func(*http.Request) error
}

func (u *priorityWaitingHTTPUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	if err := u.before(req); err != nil {
		return nil, err
	}
	return u.httpUpstreamRecorder.Do(req, proxy, accountID, concurrency)
}

func TestPriorityObservationHTTPAfterTransportWait(t *testing.T) {
	for _, path := range []string{"native", "bps", "siwc"} {
		t.Run(path, func(t *testing.T) {
			svc, req, account, _, _, ctx := reviewNativeFixture(t, OpenAIWSIngressModeHTTPBridge)
			if path == "siwc" {
				account.Type = AccountTypeOAuth
				account.Extra[OpenAISIWCEnabledKey] = true
				require.Eventually(t, func() bool {
					return svc.priorityHistory(req, svc.prioritySchedulingRuntimeConfig(), []openAIAccountCandidateScore{{account: account}}).ready()
				}, 2*time.Second, time.Millisecond)
				setModeSignal(svc, req, account.ID, func(v *PrioritySchedulingSignal) { v.P90TTFTMs, v.LatestBadLatencyAt = 500, nil })
			}
			ctx = WithObservationEffort(ctx, "")
			ctx, err := svc.beginPriorityLatencyTurn(ctx, nil, account, req.RequestedModel)
			require.NoError(t, err)
			recorder := &httpUpstreamRecorder{resp: priorityMockResponse(200, `{"status":"completed","output":[]}`, false)}
			var waited bool
			svc.httpUpstream = &priorityWaitingHTTPUpstream{httpUpstreamRecorder: recorder, before: func(r *http.Request) error {
				waited = true
				reviewActivateStop(svc, req, account.ID)
				return CheckHTTPUpstreamSendGuard(r.Context())
			}}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/responses", strings.NewReader(`{"model":"gpt-test"}`))
			require.NoError(t, err)
			var response *http.Response
			if path != "bps" {
				response, err = svc.doOpenAIUpstream(request, "", account)
			} else {
				response, err = svc.doPriorityObservedHTTP(request, "", account)
			}
			if response != nil {
				require.NoError(t, response.Body.Close())
			}
			require.True(t, waited)
			require.ErrorIs(t, err, ErrPriorityLatencyUnavailable, "transport preparation is not a physical generation send")
			require.Empty(t, recorder.requests, "a stop published during transport admission must precede every provider send")
			turn, ok := ctx.Value(priorityLatencyTurnKey{}).(*priorityLatencyTurn)
			require.True(t, ok)
			require.False(t, turn.started, "a local final refusal must not freeze the turn as sent")
		})
	}
}

type priorityGuardPluginClient struct {
	pluginv1.TransportPluginClient
	stream   *priorityGuardPluginStream
	prepared func()
}

func (c *priorityGuardPluginClient) Forward(ctx context.Context, _ ...grpc.CallOption) (pluginv1.TransportPlugin_ForwardClient, error) {
	c.stream.ctx = ctx
	c.prepared()
	return c.stream, nil
}

type priorityGuardPluginStream struct {
	pluginv1.TransportPlugin_ForwardClient
	ctx   context.Context
	sends int
}

func (s *priorityGuardPluginStream) Send(*pluginv1.ForwardRequest) error {
	s.sends++
	return errors.New("fixture stops at first plugin frame")
}

func TestPriorityObservationHTTPAfterPluginPreparation(t *testing.T) {
	svc, req, account, _, _, ctx := reviewNativeFixture(t, OpenAIWSIngressModeHTTPBridge)
	account.Type = AccountTypeOAuth
	require.Eventually(t, func() bool {
		return svc.priorityHistory(req, svc.prioritySchedulingRuntimeConfig(), []openAIAccountCandidateScore{{account: account}}).ready()
	}, 2*time.Second, time.Millisecond)
	setModeSignal(svc, req, account.ID, func(v *PrioritySchedulingSignal) { v.P90TTFTMs, v.LatestBadLatencyAt = 500, nil })
	ctx = WithObservationEffort(ctx, "")
	ctx, err := svc.beginPriorityLatencyTurn(ctx, nil, account, req.RequestedModel)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/v1/responses", nil)
	require.NoError(t, err)
	request, err = svc.preparePriorityLatencyHTTPSend(request, account)
	require.NoError(t, err)
	stream := &priorityGuardPluginStream{}
	client := &priorityGuardPluginClient{stream: stream, prepared: func() {
		reviewActivateStop(svc, req, account.ID)
	}}
	runtime := &pluginRuntime{api: client, client: &hcplugin.Client{}}
	manager := &PluginManager{}
	manager.route.Store(&pluginRoute{pluginID: 1, rolloutPercent: 100, runtime: runtime})
	response, handled, err := manager.RoundTripOpenAIOAuth(request.Context(), request, "", account)
	require.True(t, handled)
	require.Nil(t, response)
	require.ErrorIs(t, err, ErrPriorityLatencyUnavailable)
	require.Zero(t, stream.sends, "stream preparation must not freeze admission before the first plugin frame")
	require.ErrorIs(t, stream.ctx.Err(), context.Canceled, "refusal must close the prepared RPC stream")
	require.Zero(t, runtime.inFlight.Load(), "refusal must release the runtime's exact request")
	require.NoError(t, ctx.Err(), "RPC cleanup must preserve the parent turn")
	turn, ok := ctx.Value(priorityLatencyTurnKey{}).(*priorityLatencyTurn)
	require.True(t, ok)
	require.False(t, turn.started)
	setModeSignal(svc, req, account.ID, func(v *PrioritySchedulingSignal) { v.P90TTFTMs, v.LatestBadLatencyAt = 500, nil })
	client.prepared = func() {}
	response, handled, err = manager.RoundTripOpenAIOAuth(request.Context(), request, "", account)
	require.True(t, handled)
	require.Nil(t, response)
	var transportErr *PluginTransportError
	require.ErrorAs(t, err, &transportErr)
	require.True(t, transportErr.RequestSent, "an attempted metadata send remains non-replayable")
	require.Equal(t, 1, stream.sends)
	require.True(t, turn.started)
	require.Zero(t, runtime.inFlight.Load())
}

func TestPriorityObservationFinalHTTPLocalRefusal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		failures int64
	}{
		{"local", priorityLatencyAdmissionError(true), 0},
		{"http_wrapped_local", &url.Error{Op: "Post", URL: "http://fixture.invalid", Err: priorityLatencyAdmissionError(true)}, 0},
		{"provider_transport", &url.Error{Op: "Post", URL: "http://fixture.invalid", Err: io.ErrUnexpectedEOF}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newOpenAIRejectedFieldTestService(&httpUpstreamRecorder{})
			account := newOpenAIRejectedFieldTestAccount()
			ctx, scope := withPriorityAttemptScope(t.Context(), "fixture", "/v1/responses")
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/v1/responses", nil)
			require.NoError(t, err)
			observe := svc.observePriorityAttempt(request, account)
			observe(nil, tc.err)
			scope.finish(nil)
			key := priorityRequestAttemptKey(OpenAIAccountScheduleRequest{RequestedModel: "fixture", InboundEndpoint: "/v1/responses"}, account)
			got := svc.openaiAccountStats.attempts.snapshot(key, time.Now(), 10, 5)
			require.Equal(t, tc.failures, got.Failures)
			require.Zero(t, got.Successes, "a local refusal must not manufacture recovery evidence")
		})
	}
}

func TestPriorityObservationHTTPTransportGuardLifecycle(t *testing.T) {
	svc, req, account, _, _, ctx := reviewNativeFixture(t, OpenAIWSIngressModeHTTPBridge)
	ctx = WithObservationEffort(ctx, "")
	ctx, err := svc.beginPriorityLatencyTurn(ctx, nil, account, req.RequestedModel)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/v1/responses", nil)
	require.NoError(t, err)
	request, err = svc.preparePriorityLatencyHTTPSend(request, account)
	require.NoError(t, err)
	turn, ok := ctx.Value(priorityLatencyTurnKey{}).(*priorityLatencyTurn)
	require.True(t, ok)
	require.False(t, turn.started)
	reviewActivateStop(svc, req, account.ID)
	require.ErrorIs(t, CheckHTTPUpstreamSendGuard(request.Context()), ErrPriorityLatencyUnavailable)
	require.False(t, turn.started)
	setModeSignal(svc, req, account.ID, func(v *PrioritySchedulingSignal) { v.P90TTFTMs, v.LatestBadLatencyAt = 500, nil })
	require.NoError(t, CheckHTTPUpstreamSendGuard(request.Context()), "an unsent refusal may recover without replaying anything")
	require.True(t, turn.started)
	reviewActivateStop(svc, req, account.ID)
	require.NoError(t, CheckHTTPUpstreamSendGuard(request.Context()), "an attempted logical turn retains its original retry admission")
	_, err = svc.nextPriorityLatencyWSTurn(request.Context(), nil, account, req.RequestedModel, []byte(`{"model":"gpt-test"}`))
	require.ErrorIs(t, err, ErrPriorityLatencyUnavailable, "the next logical turn must not inherit the previous send marker")
	cancelled, cancel := context.WithCancel(request.Context())
	cancel()
	require.ErrorIs(t, CheckHTTPUpstreamSendGuard(cancelled), context.Canceled)
	require.NoError(t, CheckHTTPUpstreamSendGuard(context.Background()))
}

func TestPriorityObservationWSTurnEffortFields(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"nested", `{"model":"fixture","reasoning":{"effort":"medium"}}`, "medium"},
		{"scalar", `{"model":"fixture","reasoning_effort":"low"}`, "low"},
		{"messages", `{"model":"fixture","output_config":{"effort":"high"}}`, "high"},
		{"distinct max", `{"model":"fixture","reasoning":{"effort":"max"}}`, "max"},
		{"omitted", `{"model":"fixture"}`, ""},
		{"null", `{"model":"fixture","reasoning":null}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := OpenAIAccountScheduleRequest{RequestedModel: "fixture", ObservationEffort: "xhigh", ObservationEffortScoped: true}
			ctx := context.WithValue(WithObservationEffort(context.Background(), "xhigh"), prioritySelectionRequestKey{}, req)
			svc := &OpenAIGatewayService{}
			admitted, err := svc.nextPriorityLatencyWSTurn(ctx, nil, &Account{ID: 1, Platform: PlatformOpenAI}, "fixture", []byte(tc.body))
			require.NoError(t, err)
			turn, ok := admitted.Value(priorityLatencyTurnKey{}).(*priorityLatencyTurn)
			require.True(t, ok)
			require.True(t, turn.request.ObservationEffortScoped)
			require.Equal(t, tc.want, turn.request.ObservationEffort)
			original, ok := ctx.Value(prioritySelectionRequestKey{}).(OpenAIAccountScheduleRequest)
			require.True(t, ok)
			require.Equal(t, "xhigh", original.ObservationEffort)
		})
	}
}

func TestPriorityObservationDirectForwardEffort(t *testing.T) {
	for _, entry := range []string{"responses", "chat/completions", "messages"} {
		t.Run(entry, func(t *testing.T) {
			svc, req, account, _, _, ctx := reviewNativeFixture(t, OpenAIWSIngressModeHTTPBridge)
			account.Extra["openai_ws_force_http"] = true
			req.InboundEndpoint = "/v1/" + entry
			req.ObservationEffort, req.ObservationEffortScoped = "medium", true
			cfg := svc.prioritySchedulingRuntimeConfig()
			pool := []openAIAccountCandidateScore{{account: account}}
			require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, 2*time.Second, time.Millisecond)
			reviewActivateStop(svc, req, account.ID)
			ctx = WithPrioritySchedulingObservation(ctx, req.GroupID, req.HistoryModel, req.InboundEndpoint)
			ctx = WithObservationEffort(ctx, "xhigh")
			body := []byte(`{"model":"gpt-test","reasoning":{"effort":"medium"},"input":"synthetic"}`)
			switch entry {
			case "chat/completions":
				body = []byte(`{"model":"gpt-test","reasoning_effort":"medium","messages":[{"role":"user","content":"synthetic"}]}`)
			case "messages":
				body = []byte(`{"model":"gpt-test","output_config":{"effort":"medium"},"messages":[{"role":"user","content":"synthetic"}]}`)
			}
			c := newOpenAIRejectedFieldTestContext(body)
			c.Request = c.Request.WithContext(ctx)
			upstream := &httpUpstreamRecorder{resp: priorityMockResponse(200, `{"status":"completed","output":[]}`, false)}
			svc.httpUpstream = upstream
			var err error
			switch entry {
			case "chat/completions":
				_, err = svc.ForwardAsChatCompletions(ctx, c, account, body, "", "")
			case "messages":
				_, err = svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
			default:
				_, err = svc.Forward(ctx, c, account, body)
			}
			require.ErrorIs(t, err, ErrPriorityLatencyUnavailable)
			require.Empty(t, upstream.requests, "the current body's effort must be captured before direct-forward admission")
		})
	}
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
	reader.signal = map[int64]PrioritySchedulingSignal{1: withAccountPerformanceFixture(PrioritySchedulingSignal{Samples: 1, P90TTFTMs: 1200, LatencyUnknownSamples: 1, ProfitSamples: 2})}
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

func TestPrioritySessionAffinityPreservesEligibleBinding(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		protect, weighted, full, exclude bool
		stop, wrongScope, disabled       bool
		capacity, newSession, profit     bool
		quality                          bool
		want                             int64
	}{
		{name: "legacy_latency_escape", want: 2},
		{name: "protected_latency", protect: true, want: 1},
		{name: "protected_weighted", protect: true, weighted: true, want: 1},
		{name: "protected_full_waits", protect: true, full: true, want: 1},
		{name: "legacy_capacity_escape", capacity: true, want: 2},
		{name: "protected_capacity", protect: true, capacity: true, want: 1},
		{name: "new_session_still_scored", protect: true, newSession: true, weighted: true, want: 2},
		{name: "profit_gate_still_applies", protect: true, profit: true, want: 2},
		{name: "quality_isolation_still_applies", protect: true, quality: true, want: 2},
		{name: "exclusion_still_applies", protect: true, exclude: true, want: 2},
		{name: "disabled_still_applies", protect: true, disabled: true, want: 2},
		{name: "stop_still_applies", protect: true, stop: true, want: 2},
		{name: "different_group_unchanged", protect: true, wrongScope: true, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			cfg := DefaultPrioritySchedulingConfig()
			cfg.Enabled, cfg.PreserveSessionAffinity = true, tc.protect
			group := int64(11)
			cfg.GroupIDs = []int64{group}
			if tc.wrongScope {
				cfg.GroupIDs = []int64{12}
			}
			if tc.stop {
				cfg.LatencyPenaltyMode = PriorityLatencyStop
			}
			badAt := time.Now().Add(-time.Minute)
			reader := &priorityReaderStub{signal: map[int64]PrioritySchedulingSignal{
				1: withAccountPerformanceFixture(PrioritySchedulingSignal{Samples: 20, P90TTFTMs: 20000, LatestBadLatencyAt: &badAt, QualitySamples: 10, QualityPassed: 10}),
				2: withAccountPerformanceFixture(PrioritySchedulingSignal{Samples: 20, P90TTFTMs: 500, QualitySamples: 10, QualityPassed: 10}),
			}}
			if tc.capacity {
				signal := reader.signal[1]
				signal.P90TTFTMs, signal.LatestBadLatencyAt = 500, nil
				signal.Performance.AvgTTFTMs = new(500.0)
				reader.signal[1] = signal
			}
			svc := priorityGateway(cfg, reader)
			pool := []openAIAccountCandidateScore{priorityCandidate(1, .1, 0), priorityCandidate(2, 1, 0)}
			accounts := make([]Account, 0, len(pool))
			for _, item := range pool {
				item.account.Status, item.account.Schedulable = StatusActive, true
				item.account.GroupIDs = []int64{group}
				accounts = append(accounts, *item.account)
			}
			if tc.disabled {
				accounts[0].Schedulable = false
			}
			if tc.profit {
				rate := 2.0
				accounts[0].RateMultiplier = &rate
				accounts[0].Extra[AccountCostMultiplierExtraKey] = float64(2)
			}
			if tc.quality {
				accounts[0].Extra["model_rate_limits"] = map[string]any{"gpt-test": map[string]any{
					"reason": "quality_rule:7", "rate_limit_reset_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
				}}
			}
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:protected-session": 1}}
			svc.cache, svc.accountRepo, svc.cfg = cache, schedulerTestOpenAIAccountRepo{accounts: accounts}, newSchedulerTestOpenAIWSV2Config()
			svc.cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
			loadMap := map[int64]*AccountLoadInfo{}
			if tc.capacity {
				loadMap[1] = &AccountLoadInfo{AccountID: 1, CurrentConcurrency: 8, LoadRate: 80}
			}
			svc.concurrencyService = NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{1: !tc.full, 2: true}, loadMap: loadMap})
			svc.openaiAccountStats = newOpenAIAccountRuntimeStats()
			for range 8 {
				ttft := 30000
				if tc.capacity {
					ttft = 500
				}
				svc.openaiAccountStats.report(1, true, &ttft)
			}
			req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, GroupID: &group, SessionHash: "protected-session", StickyAccountID: 1,
				StickyWeighted: tc.weighted, RequestedModel: "gpt-test", InboundEndpoint: "/v1/responses", UseUpstreamTokenCost: true}
			if tc.newSession {
				req.SessionHash, req.StickyAccountID = "new-session", 0
			}
			if tc.exclude {
				req.ExcludedIDs = map[int64]struct{}{1: {}}
			}
			require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, time.Second, time.Millisecond)
			scheduler := &defaultOpenAIAccountScheduler{service: svc, stats: svc.openaiAccountStats}
			ctx := t.Context()
			if tc.profit {
				ctx = context.WithValue(ctx, openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: group, threshold: 1, pricingAt: time.Now()})
			}
			selected, decision, err := scheduler.Select(ctx, req)
			require.NoError(t, err)
			require.NotNil(t, selected)
			require.Equal(t, tc.want, selected.Account.ID)
			if selected.ReleaseFunc != nil {
				selected.ReleaseFunc()
			}
			if tc.protect && !tc.exclude && !tc.disabled && !tc.stop && !tc.wrongScope && !tc.newSession && !tc.profit && !tc.quality {
				require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
				require.Equal(t, int64(1), cache.sessionBindings["openai:protected-session"])
				if tc.full {
					require.False(t, selected.Acquired)
					require.NotNil(t, selected.WaitPlan)
					require.Equal(t, int64(1), selected.WaitPlan.AccountID)
				}
			}
		})
	}
}

func TestPrioritySessionAffinityAvailabilityFailureAndRecovery(t *testing.T) {
	svc, scheduler, req, _, _ := latencyModeFixture(t, PriorityLatencyDeprioritize)
	cfg := svc.prioritySchedulingRuntimeConfig()
	cfg.PreserveSessionAffinity = true
	require.NoError(t, svc.settingService.SavePrioritySchedulingConfig(t.Context(), &cfg))
	req.SessionHash, req.StickyAccountID, req.StickyWeighted = "mode-sticky", 1, true
	selected, err := modeSelect(t, scheduler, req)
	require.NoError(t, err)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.Account.ID)
	repo := &availabilityAdmissionRepo{}
	upstream := &httpUpstreamRecorder{}
	svc.availabilityReader, svc.httpUpstream = repo, upstream
	for _, state := range []string{"pass", "fail", "stale", "unknown", "read_error", "recovered"} {
		t.Run(state, func(t *testing.T) {
			report := availabilityAdmissionPass(selected.Account.ID, req.RequestedModel)
			repo.reports, repo.readErr = []*AvailabilityReport{report}, nil
			switch state {
			case "fail":
				report.Models[0].Status, report.Models[0].Decision.Status = "fail", "fail"
			case "stale":
				past := time.Now().Add(-time.Minute)
				report.Models[0].Status, report.Models[0].ValidUntil = "stale", &past
			case "unknown":
				report.Models[0].Status, report.Models[0].Decision = "unknown", nil
			case "read_error":
				repo.readErr = errors.New("fixture availability read failed")
			}
			before := len(upstream.requests)
			upstream.resp = &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}
			ctx := ContextWithSelectionPriorityRequest(t.Context(), selected)
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"fixture"}`))
			require.NoError(t, err)
			response, err := svc.doOpenAIUpstream(request, "", selected.Account)
			if state == "pass" || state == "recovered" {
				require.NoError(t, err)
				require.NotNil(t, response)
				require.NoError(t, response.Body.Close())
				require.Len(t, upstream.requests, before+1)
			} else {
				var denied *OpenAITurnAdmissionError
				require.ErrorAs(t, err, &denied)
				require.Nil(t, response)
				require.Len(t, upstream.requests, before, "session protection must not send through failed authoritative admission")
			}
		})
	}
	require.Equal(t, 6, repo.reads, "each send must consult the current state, not its selection-time pass")
	boundID, err := svc.getStickySessionAccountID(t.Context(), req.GroupID, req.SessionHash)
	require.NoError(t, err)
	require.Equal(t, int64(1), boundID)
}

func TestPrioritySessionAffinityRetainsResponseOwnership(t *testing.T) {
	svc, scheduler, req, pool, _ := latencyModeFixture(t, PriorityLatencyStop)
	cfg := svc.prioritySchedulingRuntimeConfig()
	cfg.PreserveSessionAffinity = true
	require.NoError(t, svc.settingService.SavePrioritySchedulingConfig(t.Context(), &cfg))
	// Saving changes the history scope. Establish the known slow-owner evidence
	// for that scope before asserting stop admission, not the cold fallback.
	require.Eventually(t, func() bool { return svc.priorityHistory(req, cfg, pool).ready() }, time.Second, time.Millisecond)
	const responseID = "resp_protected_owner"
	require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(t.Context(), 11, responseID, 1, time.Hour))
	req.PreviousResponseID, req.PreviousResponseCanMove, req.StickyWeighted = responseID, true, true
	selected, err := modeSelect(t, scheduler, req)
	require.Nil(t, selected)
	require.ErrorIs(t, err, ErrPriorityLatencyUnavailable, "a stopped response owner must not be replaced with a healthy non-owner")
	setModeSignal(svc, req, 1, func(signal *PrioritySchedulingSignal) {
		signal.P90TTFTMs, signal.LatestBadLatencyAt = 500, nil
		signal.Performance.AvgTTFTMs = new(500.0)
	})
	req.ExcludedIDs = map[int64]struct{}{1: {}}
	selected, err = modeSelect(t, scheduler, req)
	require.Nil(t, selected)
	require.ErrorIs(t, err, ErrPriorityLatencyUnavailable, "excluding a healthy owner must not make its response portable")
	req.ExcludedIDs = nil
	selected, err = modeSelect(t, scheduler, req)
	require.NoError(t, err)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.Account.ID, "recovery retains the original response owner")
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
		1: withAccountPerformanceFixture(PrioritySchedulingSignal{Samples: 20, P90TTFTMs: 20000, LatestBadLatencyAt: &badAt, QualitySamples: 10, QualityPassed: 10}),
		2: withAccountPerformanceFixture(PrioritySchedulingSignal{Samples: 20, P90TTFTMs: 500, QualitySamples: 10, QualityPassed: 10}),
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
	selectTurn := func(session, previous string, excluded map[int64]struct{}) (*AccountSelectionResult, OpenAIAccountScheduleDecision) {
		t.Helper()
		selection, decision, err := svc.SelectAccountWithSchedulerForCapability(ctx, &group, previous, session, "gpt-test", excluded, OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true, PlatformOpenAI)
		require.NoError(t, err)
		require.NotNil(t, selection)
		if selection.ReleaseFunc != nil {
			t.Cleanup(selection.ReleaseFunc)
		}
		return selection, decision
	}
	selected, decision := selectTurn("sticky-penalty", "", nil)
	require.Equal(t, int64(2), selected.Account.ID, "ordinary sticky cannot bypass measured penalty even with EWMA escape disabled")
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.Equal(t, int64(2), cache.sessionBindings["openai:sticky-penalty"], "an admitted replacement becomes the session's durable affinity")
	// A separate weighted shortcut must not undo the scored selection policy.
	weightedReq := req
	weightedReq.StickyWeighted = true
	weightedReq.StickyAccountID = 1
	weightedReq.SessionHash = "weighted-penalty"
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
	selected, _ = selectTurn("penalty-fallback", "", map[int64]struct{}{2: {}})
	require.Equal(t, int64(1), selected.Account.ID)
	// Provider response ownership wins, even during the measured penalty.
	require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(ctx, group, "resp_penalty_owner", 1, time.Hour))
	selected, decision = selectTurn("penalty-owner", "resp_penalty_owner", nil)
	require.Equal(t, int64(1), selected.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerPreviousResponse, decision.Layer)
	// An old timestamp alone does not recover the shared rolling mean.
	svc.priorityScheduling.mu.Lock()
	entry := svc.priorityScheduling.entries[priorityHistoryKey(req, cfg)]
	expired := time.Now().Add(-11 * time.Minute)
	signal := entry.signals[1]
	signal.LatestBadLatencyAt = &expired
	entry.signals[1] = signal
	svc.priorityScheduling.mu.Unlock()
	selected, decision = selectTurn("sticky-penalty", "", nil)
	require.Equal(t, int64(2), selected.Account.ID, "expiry must not move the escaped session back to its previous account")
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.Equal(t, int64(2), cache.sessionBindings["openai:sticky-penalty"])
	// A newly observed healthy mean restores the fallback without moving the
	// escaped session back to its former owner.
	svc.priorityScheduling.mu.Lock()
	signal = entry.signals[1]
	signal.Performance = cloneAccountPerformance(signal.Performance)
	recoveredTTFT := 500.0
	signal.Performance.AvgTTFTMs = &recoveredTTFT
	entry.signals[1] = signal
	svc.priorityScheduling.mu.Unlock()
	selected, _ = selectTurn("penalty-recovered", "", map[int64]struct{}{2: {}})
	require.Equal(t, int64(1), selected.Account.ID, "the recovered account remains available to a new session")
	fallback, fallbackErr = scheduler.tryFallbackToWeightedSticky(ctx, weightedReq)
	require.NoError(t, fallbackErr)
	require.NotNil(t, fallback, "a recovered shared mean restores the weighted sticky fallback")
	require.Equal(t, int64(1), fallback.Account.ID)
	require.Equal(t, int64(1), cache.sessionBindings["openai:weighted-penalty"])
	require.Equal(t, int64(2), cache.sessionBindings["openai:sticky-penalty"], "recovery in another session must not undo the replacement binding")
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

// Compatibility helper for passive transport collector regressions.
func (s *defaultOpenAIAccountScheduler) priorityErrorEvidence(req OpenAIAccountScheduleRequest, c PrioritySchedulingConfig, account *Account, now time.Time) PriorityErrorEvidence {
	key := priorityRequestAttemptKey(req, account)
	if s.stats == nil {
		return PriorityErrorEvidence{WindowMinutes: c.ErrorRateWindowMinutes, PriorSamples: max(1, c.ErrorRatePriorSamples), ObservedAt: now, Endpoint: key.endpoint}
	}
	return s.stats.attempts.snapshot(key, now, c.ErrorRateWindowMinutes, c.ErrorRatePriorSamples)
}
