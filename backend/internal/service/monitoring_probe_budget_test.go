package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type monitoringBudgetRepoStub struct {
	MonitoringProbeBudgetRepository
	mu         sync.Mutex
	requests   []MonitoringProbeBudgetRequest
	denyAt     int
	err        error
	acquire    func(MonitoringProbeBudgetRequest, int) (string, error)
	renew      func(context.Context) (bool, error)
	released   int
	usages     []MonitoringProbeTokenUsage
	releaseErr error
}

func (r *monitoringBudgetRepoStub) Acquire(_ context.Context, request MonitoringProbeBudgetRequest) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, request)
	if r.acquire != nil {
		return r.acquire(request, len(r.requests))
	}
	if r.denyAt > 0 && len(r.requests) >= r.denyAt {
		return "", r.err
	}
	return "fixture-lease", nil
}
func (r *monitoringBudgetRepoStub) Renew(ctx context.Context, _ string) (bool, error) {
	if r.renew != nil {
		return r.renew(ctx)
	}
	return true, nil
}
func (r *monitoringBudgetRepoStub) Release(_ context.Context, _ string, usage MonitoringProbeTokenUsage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released++
	r.usages = append(r.usages, usage)
	return r.releaseErr
}

func monitoringBudgetRequest(t *testing.T, repo *monitoringBudgetRepoStub) *http.Request {
	t.Helper()
	svc := NewMonitoringProbeBudgetService(repo)
	req, err := http.NewRequestWithContext(svc.Context(t.Context(), "https://example.com/tenant/v1", true), http.MethodGet, "https://example.com/tenant/api/status", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer private-budget-fixture")
	return req
}

func TestMonitoringProbeBudgetTransportLifecycle(t *testing.T) {
	for _, outcome := range []string{"body", "transport_error", "empty_response"} {
		t.Run(outcome, func(t *testing.T) {
			repo := &monitoringBudgetRepoStub{}
			original := monitoringBudgetRequest(t, repo)
			req, finish, err := BeginMonitoringProbeRequest(original)
			require.NoError(t, err)
			require.Len(t, repo.requests, 1)
			require.Equal(t, config.UpstreamCredentialFingerprints(original), repo.requests[0].KeyHashes)
			require.Equal(t, monitoringProbeHash("https://example.com/tenant"), repo.requests[0].SiteHash)
			require.True(t, repo.requests[0].Recovery)
			require.NotContains(t, repo.requests[0].SiteHash, "example.com")
			var response *http.Response
			var transportErr error
			if outcome == "body" {
				response = &http.Response{Body: io.NopCloser(strings.NewReader("fixture"))}
			}
			if outcome == "transport_error" {
				transportErr = errors.New("fixture transport error")
			}
			require.ErrorIs(t, FinishMonitoringProbeResponse(req.Context(), response, transportErr, finish), transportErr)
			if response != nil {
				require.Zero(t, repo.released)
				data, readErr := io.ReadAll(response.Body)
				require.NoError(t, readErr)
				require.Equal(t, "fixture", string(data))
				require.NoError(t, response.Body.Close())
				require.NoError(t, response.Body.Close())
			}
			finish()
			require.Equal(t, 1, repo.released)
		})
	}
}

func TestMonitoringProbeBudgetRejectsChangedSiteAndUnverifiedCredentials(t *testing.T) {
	for _, change := range []string{"host", "scheme", "tenant", "no_credentials", "unknown_credentials", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			repo := &monitoringBudgetRepoStub{}
			req := monitoringBudgetRequest(t, repo)
			switch change {
			case "host":
				req.URL.Host = "other.example"
			case "scheme":
				req.URL.Scheme = "http"
			case "tenant":
				req.URL.Path = "/tenant-other/api/status"
			case "no_credentials":
				req.Header.Del("Authorization")
			case "unknown_credentials":
				req.Header.Set("Authorization", "Custom opaque")
			case "cancelled":
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			_, finish, err := BeginMonitoringProbeRequest(req)
			require.Error(t, err)
			require.Nil(t, finish)
			require.Empty(t, repo.requests)
		})
	}
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	require.NoError(t, err)
	got, finish, err := BeginMonitoringProbeRequest(req)
	require.NoError(t, err)
	require.Same(t, req, got)
	require.Nil(t, finish)
}

func TestMonitoringProbeBudgetQualitySiteBoundaries(t *testing.T) {
	for _, mode := range []string{"configured", "overlapping_tenants", "default_origin", "changed_host", "changed_tenant", "changed_scheme", "unbound"} {
		t.Run(mode, func(t *testing.T) {
			repo := &monitoringBudgetRepoStub{}
			ctx := NewMonitoringProbeBudgetService(repo).qualityContext(t.Context())
			probe := ctx.Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext)
			probe.sites = []string{"https://example.com/tenant/v1"}
			target, expectedSite := "https://example.com/tenant/v1/responses", "https://example.com/tenant"
			switch mode {
			case "overlapping_tenants":
				probe.sites = []string{"https://example.com", "https://example.com/tenant/v1"}
			case "default_origin":
				probe.sites = []string{""}
				target, expectedSite = "https://chatgpt.com/backend-api/codex/responses", "https://chatgpt.com"
			case "changed_host":
				target = "https://other.example/tenant/v1/responses"
			case "changed_tenant":
				target = "https://example.com/tenant-other/v1/responses"
			case "changed_scheme":
				target = "http://example.com/tenant/v1/responses"
			case "unbound":
				probe.sites = nil
			}
			ctx = context.WithValue(ctx, monitoringProbeBudgetKey{}, probe)
			require.True(t, HTTPUpstreamRedirectsDisabled(ctx))
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer final-budget-fixture")
			_, finish, err := BeginMonitoringProbeRequest(req)
			switch mode {
			case "changed_host", "changed_tenant", "changed_scheme", "unbound":
				require.ErrorIs(t, err, ErrMonitoringProbeBudgetUnavailable)
				require.Empty(t, repo.requests)
				require.Nil(t, finish)
			default:
				require.NoError(t, err)
				require.Len(t, repo.requests, 1)
				require.Equal(t, monitoringProbeHash(expectedSite), repo.requests[0].SiteHash)
				finish()
				require.Equal(t, 1, repo.released)
			}
		})
	}
}

func TestMonitoringProbeBudgetWSBoundaries(t *testing.T) {
	for _, mode := range []string{"wss", "ws", "changed_host", "changed_tenant", "changed_scheme", "invalid_url", "http_url", "missing_keys", "unverified_keys", "unfenced_socket", "unmarked"} {
		t.Run(mode, func(t *testing.T) {
			repo := &monitoringBudgetRepoStub{}
			ctx := NewMonitoringProbeBudgetService(repo).Context(t.Context(), "https://example.com/tenant", false)
			target := "wss://example.com/tenant/v1/responses"
			keys := []string{"key-b", "key-a", "key-a"}
			switch mode {
			case "ws":
				ctx = NewMonitoringProbeBudgetService(repo).Context(t.Context(), "http://example.com/tenant", false)
				target = "ws://example.com/tenant/v1/responses"
			case "changed_host":
				target = "wss://other.example/tenant/v1/responses"
			case "changed_tenant":
				target = "wss://example.com/other/v1/responses"
			case "changed_scheme":
				target = "ws://example.com/tenant/v1/responses"
			case "invalid_url":
				target = "wss://example.com/%zz"
			case "http_url":
				target = "https://example.com/tenant/v1/responses"
			case "missing_keys":
				keys = nil
			case "unverified_keys":
				keys = []string{config.UnverifiedUpstreamCredential}
			case "unmarked":
				ctx = t.Context()
			}
			original := append([]string(nil), keys...)
			var turnCtx context.Context
			var finish func()
			var err error
			if mode == "unfenced_socket" {
				lease := &openAIWSConnLease{conn: &openAIWSConn{upstreamKeyFingerprints: keys, billingHandshake: &openAIWSBillingHandshake{target: target}}}
				turnCtx, finish, err = lease.beginMonitoringProbeRequest(ctx)
			} else {
				turnCtx, finish, err = beginMonitoringProbeWSRequest(ctx, target, keys, true)
			}
			switch mode {
			case "wss", "ws":
				require.NoError(t, err)
				require.Len(t, repo.requests, 1)
				require.Equal(t, []string{"key-a", "key-b"}, repo.requests[0].KeyHashes)
				require.Equal(t, original, keys, "admission must not sort shared pooled-connection fingerprints in place")
				require.NoError(t, finishMonitoringProbeWSRequest(turnCtx, finish))
				require.NoError(t, finishMonitoringProbeWSRequest(turnCtx, finish))
				require.Equal(t, 1, repo.released)
				require.NoError(t, ctx.Err())
			case "unmarked":
				require.NoError(t, err)
				require.Nil(t, finish)
				require.Empty(t, repo.requests)
			default:
				require.ErrorIs(t, err, ErrMonitoringProbeBudgetUnavailable)
				require.ErrorIs(t, monitoringProbeLocalError(ctx), ErrMonitoringProbeBudgetUnavailable)
				require.Empty(t, repo.requests)
				require.Nil(t, finish)
			}
		})
	}
}

func TestMonitoringProbeBudgetLeaseLossCancelsTransportAndReleases(t *testing.T) {
	for _, mode := range []string{"error", "lost", "blocked", "late_ack"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repo := &monitoringBudgetRepoStub{renew: func(ctx context.Context) (bool, error) {
					switch mode {
					case "error":
						return false, errors.New("fixture database unavailable")
					case "lost":
						return false, nil
					case "blocked":
						<-ctx.Done()
						return false, ctx.Err()
					default:
						time.Sleep(50 * time.Second)
						return true, nil
					}
				}}
				req, finish, err := BeginMonitoringProbeRequest(monitoringBudgetRequest(t, repo))
				require.NoError(t, err)
				response := &http.Response{Body: io.NopCloser(strings.NewReader("fixture"))}
				require.NoError(t, FinishMonitoringProbeResponse(req.Context(), response, nil, finish))
				time.Sleep(81 * time.Second)
				synctest.Wait()
				require.ErrorIs(t, context.Cause(req.Context()), ErrMonitoringProbeBudgetLeaseLost)
				_, err = io.ReadAll(response.Body)
				require.ErrorIs(t, err, ErrMonitoringProbeBudgetLeaseLost)
				require.NoError(t, response.Body.Close())
				require.Equal(t, 1, repo.released)
			})
		})
	}
}

type monitoringBudgetHTTP struct {
	base HTTPUpstream
}

func (h monitoringBudgetHTTP) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	req, finish, err := BeginMonitoringProbeRequest(req)
	if err != nil {
		if req != nil && req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	response, err := h.base.Do(req, proxy, id, concurrency)
	err = FinishMonitoringProbeResponse(req.Context(), response, err, finish)
	return response, err
}

func (h monitoringBudgetHTTP) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	req, finish, err := BeginMonitoringProbeRequest(req)
	if err != nil {
		if req != nil && req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	response, err := h.base.DoWithTLS(req, proxy, id, concurrency, profile)
	err = FinishMonitoringProbeResponse(req.Context(), response, err, finish)
	return response, err
}

func TestUpstreamBillingProbeBudgetPreservesEvidenceAndDefers(t *testing.T) {
	for _, at := range []int{1, 2} {
		t.Run(string(rune('0'+at)), func(t *testing.T) {
			now := time.Now().UTC()
			old := &UpstreamBillingProbeSnapshot{Status: "failed", FailureCount: 4, LastAttemptAt: now.Add(-time.Hour), NextProbeAt: now.Add(-time.Minute)}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
				Credentials: map[string]any{"api_key": "fixture-key", "base_url": "https://example.com"},
				Extra:       map[string]any{UpstreamBillingProbeEnabledExtraKey: true, UpstreamBillingProbeExtraKey: old}}
			accounts := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{1: account}}
			denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_minute", RetryAt: now.Add(time.Minute)}
			budget := &monitoringBudgetRepoStub{denyAt: at, err: denial}
			svc := newUpstreamBillingProbeTestService(accounts, monitoringBudgetHTTP{base: &upstreamBillingProbeHTTPStub{}}, nil)
			svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
			_, err := svc.ProbeAccount(t.Context(), 1)
			var got *MonitoringProbeBudgetDenial
			require.ErrorAs(t, err, &got)
			require.Equal(t, denial, got)
			require.Same(t, old, account.Extra[UpstreamBillingProbeExtraKey])
			require.Equal(t, 4, old.FailureCount)
			require.Equal(t, denial.RetryAt, upstreamBillingProbeDueAt(account))
			for _, request := range budget.requests {
				require.True(t, request.Recovery)
			}
			before := len(budget.requests)
			_, err = svc.probeScheduledAccount(t.Context(), 1, 5)
			require.NoError(t, err)
			require.Len(t, budget.requests, before)
		})
	}
}

func TestNewAPIMonitoringBudgetPricingDenialStopsInventoryWithoutFailure(t *testing.T) {
	svc, auth, upstream, accounts := newAPIConfigFixture()
	_, err := svc.SaveNewAPIConfig(t.Context(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
	require.NoError(t, err)
	old := accounts.accounts[1].Extra[UpstreamBillingProbeExtraKey]
	denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "key_minute", RetryAt: time.Now().Add(time.Minute)}
	budget := &monitoringBudgetRepoStub{denyAt: 3, err: denial}
	svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
	svc.accountTestService.httpUpstream = monitoringBudgetHTTP{base: upstream}
	writes := 0
	auth.writeCheck = func(context.Context) { writes++ }
	_, err = svc.ProbeAccount(t.Context(), 1)
	var got *MonitoringProbeBudgetDenial
	require.ErrorAs(t, err, &got)
	require.Equal(t, denial, got)
	require.Zero(t, writes)
	require.Equal(t, old, accounts.accounts[1].Extra[UpstreamBillingProbeExtraKey])
	require.Len(t, budget.requests, 3)
	for _, req := range budget.requests {
		require.False(t, req.Recovery)
	}
}

func TestNewAPIMonitoringBudgetPreviewStopsOnKeyProofDenial(t *testing.T) {
	svc, auth, upstream, _ := newAPIConfigFixture()
	denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_minute", RetryAt: time.Now().Add(time.Minute)}
	budget := &monitoringBudgetRepoStub{denyAt: 5, err: denial}
	svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
	svc.accountTestService.httpUpstream = monitoringBudgetHTTP{base: upstream}
	_, err := svc.SaveNewAPIConfig(t.Context(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1, 2}})
	require.ErrorIs(t, err, denial)
	require.Zero(t, auth.saves, "a deferred proof must not save a partial account binding")
	require.Len(t, budget.requests, 5, "do not continue to another selected account after a deferred proof")
}
