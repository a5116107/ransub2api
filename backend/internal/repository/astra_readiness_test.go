package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type astraDiagnosticBudgetRepo struct {
	service.MonitoringProbeBudgetRepository
	requests int
	denyAt   int
	err      error
	released int
}

func (r *astraDiagnosticBudgetRepo) Acquire(context.Context, service.MonitoringProbeBudgetRequest) (string, error) {
	r.requests++
	if r.requests == r.denyAt {
		return "", r.err
	}
	return "diagnostic-fixture", nil
}
func (*astraDiagnosticBudgetRepo) Renew(context.Context, string) (bool, error) { return true, nil }
func (r *astraDiagnosticBudgetRepo) Release(context.Context, string) error     { r.released++; return nil }

func TestAstraGatewayMonitoringBudgetPreservesRouteEvidence(t *testing.T) {
	for _, denyAt := range []int{1, 2} {
		t.Run(fmt.Sprint(denyAt), func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.CodexGatewayPin = pinConfig()
			denial := &service.MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}
			budget := &astraDiagnosticBudgetRepo{denyAt: denyAt, err: denial}
			calls, history := 0, 0
			wrapper := &astraRoutingUpstream{cfg: cfg}
			wrapper.delegate = gatewayPinDelegate{call: func(req *http.Request, _ string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				bound, finish, err := service.BeginMonitoringProbeRequest(req)
				if err != nil {
					return nil, err
				}
				calls++
				resp := pinResponse("")
				if id == 299 {
					resp = pinResponse("__oailb=route; Path=/; Secure; Max-Age=230")
				}
				return resp, service.FinishMonitoringProbeResponse(bound.Context(), resp, nil, finish)
			}}
			resp, err := wrapper.Do(pinRequest(t), "", 299, 1)
			consumeAffinityResponse(t, resp, err)
			target := pinRequest(t)
			target.Header.Set("Authorization", "Bearer diagnostic-fixture")
			require.NoError(t, wrapper.VerifyAstraGatewayTarget(t.Context(), target, "", 300, 1))
			pool := wrapper.current(t.Context())
			before := pool.targetChecks[300]
			pool.historyRecorder = func(service.AstraGatewayHistoryRecord, bool) { history++ }
			previousCalls := calls
			ctx := service.NewMonitoringProbeBudgetService(budget).Context(t.Context(), "https://chatgpt.com", false)
			err = wrapper.VerifyAstraGatewayTarget(ctx, target, "", 300, 1)
			var deferred *service.MonitoringProbeBudgetDenial
			require.ErrorAs(t, err, &deferred)
			require.Equal(t, denial, deferred)
			require.Equal(t, previousCalls+denyAt-1, calls)
			require.Equal(t, before, pool.targetChecks[300], "a local postponement neither renews nor revokes the previous observation")
			require.Zero(t, history, "no provider failure or success is persisted")
			require.Equal(t, denyAt-1, budget.released)
		})
	}
}

func TestAstraGatewayMonitoringBudgetSourcePreparationNeutral(t *testing.T) {
	for _, sources := range []int{1, 2, 3} {
		for _, rotate := range []bool{false, true} {
			t.Run(fmt.Sprintf("sources=%d/rotate=%t", sources, rotate), func(t *testing.T) {
				wrapper, _, leases := rotationFixture(t, 3, "good")
				wrapper.cfg.Gateway.CodexGatewayPin.RotateNodes = rotate
				wrapper.cfg.Gateway.CodexGatewayPin.SourceAccountIDs = []int64{299, 301, 302}[:sources]
				denial := &service.MonitoringProbeBudgetDenial{Reason: "concurrency_limit", Scope: "global", RetryAt: time.Now().Add(time.Minute)}
				wrapper.preparer = func(context.Context, int64) error { return denial }
				err := wrapper.PrepareAstraGateway(t.Context())
				require.ErrorIs(t, err, denial)
				pool := wrapper.current(t.Context())
				require.Empty(t, pool.statuses, "a locally deferred source is not a failed provider")
				require.True(t, wrapper.retryAfter.IsZero())
				require.True(t, pool.rotationRetryAfter.IsZero())
				require.Zero(t, *leases)
			})
		}
	}
}

func TestAstraReadinessRequiresCurrentTargetPass(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	wrapper := &astraRoutingUpstream{cfg: cfg}
	pool := wrapper.current(t.Context())
	now := time.Now()
	route := codexGatewayRouteFromResponse(pinResponse("__oailb=first; Path=/; Secure; Max-Age=3600"), "/backend-api/codex/responses", now)
	pool.recordSource(299, now, route, true)
	snapshot := wrapper.AstraGatewaySnapshot(t.Context())
	require.Zero(t, snapshot.ReadyRoutes)
	require.Equal(t, "candidate", snapshot.Sources[0].State)
	require.Nil(t, snapshot.Sources[0].ExpiresAt)
	require.Zero(t, snapshot.Sources[0].RemainingSeconds)
	pool.targetChecks = map[int64]astraTargetValidation{300: {cookieFingerprint: sha256.Sum256([]byte("first")), sourceID: 299, passed: false, checked: now, expires: route.expires, reason: "target_probe_degraded"}}
	snapshot = wrapper.AstraGatewaySnapshot(t.Context())
	require.Zero(t, snapshot.ReadyRoutes)
	require.Equal(t, "source_passed_target_failed", snapshot.Sources[0].Reason)
	require.Nil(t, snapshot.Sources[0].ExpiresAt)
	require.Equal(t, "target_probe_degraded", snapshot.Targets[0].Reason)
	require.Nil(t, snapshot.Targets[0].ExpiresAt)
	require.Zero(t, snapshot.Targets[0].RemainingSeconds)
	check := pool.targetChecks[300]
	check.passed = true
	pool.targetChecks[300] = check
	snapshot = wrapper.AstraGatewaySnapshot(t.Context())
	require.Equal(t, 1, snapshot.ReadyRoutes)
	require.NotNil(t, snapshot.Sources[0].ExpiresAt)
	require.NotNil(t, snapshot.Targets[0].ExpiresAt)
	// A newer cookie on the same gateway/source must not inherit an older pass.
	next := *route
	next.cookie.Value = "second"
	pool.recordSource(299, now.Add(time.Second), &next, true)
	snapshot = wrapper.AstraGatewaySnapshot(t.Context())
	require.Zero(t, snapshot.ReadyRoutes)
	require.Nil(t, snapshot.Sources[0].ExpiresAt)
	require.Nil(t, snapshot.Targets[0].ExpiresAt)
}
func TestAstraReadinessSnapshotDoesNotWaitForNetwork(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	wrapper := &astraRoutingUpstream{cfg: cfg}
	pool := wrapper.current(t.Context())
	require.NoError(t, pool.targetProbeMu.Lock(t.Context()))
	defer pool.targetProbeMu.Unlock()
	done := make(chan struct{})
	go func() { wrapper.AstraGatewaySnapshot(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("status blocked on target network probe")
	}
}

func TestAstraSchedulingToggleRetainsPool(t *testing.T) {
	settings := config.AstraRoutingSettings{Revision: "one", CookiePool: pinConfig()}
	cfg := &config.Config{}
	cfg.SetAstraRoutingLoader(func(context.Context) config.AstraRoutingSettings { return settings })
	wrapper := &astraRoutingUpstream{cfg: cfg}
	pool := wrapper.current(t.Context())
	settings.AccountScheduling = true
	require.Same(t, pool, wrapper.current(t.Context()))
	settings.SchedulingMode = "groups"
	settings.SchedulingGroupIDs = []int64{7}
	require.Same(t, pool, wrapper.current(t.Context()))
	settings.Revision = "two"
	require.NotSame(t, pool, wrapper.current(t.Context()))
}

func TestAstraProbeTransportFailureRevokesExistingPass(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	fail := false
	wrapper := &astraRoutingUpstream{cfg: cfg}
	wrapper.delegate = gatewayPinDelegate{call: func(_ *http.Request, _ string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		if id == 299 {
			return pinResponse("__oailb=route; Path=/; Secure; Max-Age=230"), nil
		}
		if fail {
			return nil, errors.New("offline")
		}
		return pinResponse(""), nil
	}}
	resp, err := wrapper.Do(pinRequest(t), "", 299, 1)
	consumeAffinityResponse(t, resp, err)
	resp, err = wrapper.Do(forceAstraProbe(pinRequest(t)), "", 300, 1)
	consumeAffinityResponse(t, resp, err)
	require.Equal(t, 1, wrapper.AstraGatewaySnapshot(t.Context()).ReadyRoutes)
	fail = true
	req := pinRequest(t)
	err = wrapper.VerifyAstraGatewayTarget(t.Context(), req, "", 300, 1)
	require.Error(t, err)
	require.Zero(t, wrapper.AstraGatewaySnapshot(t.Context()).ReadyRoutes)
}

func TestAstraStateProbeBypassesBorrowingRecursion(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	calls := 0
	wrapper := &astraRoutingUpstream{cfg: cfg, delegate: gatewayPinDelegate{call: func(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		calls++
		require.True(t, service.IsOpenAICodexStateProbeRequest(r.Context()))
		return pinResponse(""), nil
	}}}
	req := pinRequest(t)
	req.Header.Set("Cookie", "__oailb=explicit-route")
	result := service.ProbeOpenAICodexStateRoute(t.Context(), wrapper, req, "exit", 300, 1, nil)
	require.Equal(t, service.OpenAICodexStateHealthy, result.Verdict)
	require.Equal(t, 2, calls)
	require.Nil(t, wrapper.pool, "standalone probes must not acquire or publish borrowing routes")
}
