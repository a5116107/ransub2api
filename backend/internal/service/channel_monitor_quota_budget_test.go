//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func accountQuotaBudgetFixture(t *testing.T, platform string) (*Account, *commandCodeAlphaUpstream, int, int) {
	t.Helper()
	switch platform {
	case PlatformCommandCode:
		return commandCodeUsageAccount(), newCommandCodeAlphaUpstream(`{"user":{"userName":"alice"},"org":null}`), 3, 4
	case PlatformCline:
		return clineTestAccount(4420), newClineAccountUpstream(commandCodeAlphaResponse{status: http.StatusOK, body: clineUsageLimitsBody}, `{"balance":12340000,"userId":"u-1"}`), 2, 3
	case PlatformMistral:
		return mistralUsageTestAccount(4421, ""), newMistralBillingUpstream(mistralTestBudgetBody, mistralTestSubscriptionBody), 2, 2
	default:
		t.Fatalf("unsupported fixture platform %s", platform)
		return nil, nil, 0, 0
	}
}

func TestCNProviderBalanceCheckMonitoringBudgetDispatchAndRecovery(t *testing.T) {
	for _, platform := range []string{PlatformCommandCode, PlatformCline, PlatformMistral, PlatformDeepseek, PlatformKimi} {
		t.Run(platform, func(t *testing.T) {
			var account *Account
			var upstream *commandCodeAlphaUpstream
			complete := 1
			if platform == PlatformDeepseek || platform == PlatformKimi {
				account = newDeepSeekBalanceProbeAccount()
				account.Platform = platform
				path, body := "/user/balance", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10"}]}`
				if platform == PlatformKimi {
					account.Credentials["account_mode"] = AccountModeCoding
					delete(account.Credentials, "base_url")
					target, err := http.NewRequest(http.MethodGet, kimiQuotaURL(account.GetOpenAIBaseURL()), nil)
					require.NoError(t, err)
					path, body = target.URL.Path, `{"limits":[]}`
				}
				upstream = &commandCodeAlphaUpstream{responses: map[string]commandCodeAlphaResponse{path: {status: http.StatusOK, body: body}}}
			} else {
				account, upstream, _, complete = accountQuotaBudgetFixture(t, platform)
			}
			account.Schedulable = true
			accounts := &cnBalanceProbeRepo{account: account}
			httpUpstream := monitoringBudgetHTTP{base: upstream}
			cfg := &config.Config{}
			svc := NewCNProviderBalanceCheckService(&fakeCNCheckRepo{byPlatform: map[string][]Account{platform: {*account}}},
				NewCNProviderBalanceService(accounts, nil, httpUpstream, cfg), NewCNProviderQuotaService(accounts, nil, httpUpstream, cfg), cfg, time.Minute)
			budget := &monitoringBudgetRepoStub{denyAt: 1, err: &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}}
			svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))

			svc.runOnce()

			require.Len(t, budget.requests, 1, "each background dispatch must install the shared policy")
			require.Empty(t, upstream.requests)
			require.Empty(t, accounts.extraWrites)
			budget.denyAt = 0
			svc.runOnce()
			require.Len(t, upstream.requests, complete)
			require.Len(t, budget.requests, complete+1)
			require.Equal(t, complete, budget.released)
			require.Len(t, accounts.extraWrites, 1, "the next original cycle must recover without inheriting the first cycle's local error")
			for _, request := range budget.requests {
				require.False(t, request.Recovery)
				require.False(t, request.MeterTokens)
			}
		})
	}
}

func TestProvideCNProviderBalanceCheckInstallsMonitoringBudget(t *testing.T) {
	budget := &monitoringBudgetRepoStub{}
	svc := ProvideCNProviderBalanceCheckService(nil, nil, nil, &config.Config{}, budget)
	require.Same(t, budget, svc.monitoringBudget.repo)
}

func TestChannelMonitorQuotaBudgetAccountAdaptersSettleBeforePublishing(t *testing.T) {
	for _, platform := range []string{PlatformCommandCode, PlatformCline, PlatformMistral} {
		for _, failure := range []string{"deny_first", "deny_primary", "deny_optional", "deny_last", "settle_first", "settle_primary", "settle_optional", "settle_last"} {
			t.Run(platform+"/"+failure, func(t *testing.T) {
				account, upstream, optionalAt, complete := accountQuotaBudgetFixture(t, platform)
				accounts := &cnBalanceProbeRepo{account: account}
				budget := &monitoringBudgetRepoStub{}
				at := 1
				if strings.HasSuffix(failure, "optional") {
					at = optionalAt
				} else if strings.HasSuffix(failure, "last") {
					at = complete
				} else if strings.HasSuffix(failure, "primary") {
					at = 2
				}
				if strings.HasPrefix(failure, "deny") {
					budget.denyAt, budget.err = at, &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "key_minute", RetryAt: time.Now().Add(time.Minute)}
				} else {
					budget.acquire = func(_ MonitoringProbeBudgetRequest, n int) (string, error) {
						if n == at {
							budget.releaseErr = errors.New("fixture settlement failure")
						}
						return "fixture-lease", nil
					}
				}
				ctx := withAccountTestMonitoringSite(NewMonitoringProbeBudgetService(budget).qualityContext(t.Context()), "")
				svc := NewCNProviderQuotaService(accounts, nil, monitoringBudgetHTTP{base: upstream}, &config.Config{})

				result, err := svc.QueryUsage(ctx, account.ID)

				require.Nil(t, result)
				require.True(t, IsMonitoringProbeBudgetFailure(err), "local budget failures must remain typed: %v", err)
				require.Empty(t, accounts.extraWrites, "optional quota requests cannot publish partial state after a local budget failure")
				require.Len(t, budget.requests, at)
				physical := at
				if strings.HasPrefix(failure, "deny") {
					physical--
				}
				require.Len(t, upstream.requests, physical)
				require.Equal(t, physical, budget.released)
				for _, request := range budget.requests {
					require.False(t, request.MeterTokens, "quota metadata is not generation usage")
					require.False(t, request.Recovery)
				}
			})
		}
	}
}

func TestChannelMonitorQuotaBudgetRejectsDetachedCNFetch(t *testing.T) {
	for _, platform := range []string{PlatformKimi, PlatformZhipu, PlatformMiniMax, PlatformOpenCodeGo, PlatformDeepseek} {
		t.Run(platform, func(t *testing.T) {
			account := newDeepSeekBalanceProbeAccount()
			account.Platform = platform
			if platform != PlatformDeepseek {
				account.Credentials["account_mode"] = AccountModeCoding
				delete(account.Credentials, "base_url")
			}
			accounts := &cnBalanceProbeRepo{account: account}
			upstream := monitoringBudgetHTTP{base: &cnBalanceResponseUpstream{statusCode: http.StatusOK,
				body: `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10"}]}`}}
			fetcher := NewChannelMonitorQuotaFetcher(nil,
				NewCNProviderQuotaService(accounts, nil, upstream, nil),
				NewCNProviderBalanceService(accounts, nil, upstream, nil), accounts, nil)
			repo := &quotaModeRepoStub{monitor: &ChannelMonitor{ID: 1, Provider: platform,
				CheckMode: MonitorCheckModeQuota, AccountID: &account.ID, PrimaryModel: "quota"}}
			svc := newQuotaModeService(repo)
			svc.SetQuotaFetcher(fetcher)
			budget := &monitoringBudgetRepoStub{denyAt: 1, err: &MonitoringProbeBudgetDenial{
				Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}}
			svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))

			results, err := svc.RunCheck(t.Context(), 1)
			require.NoError(t, err)
			require.Len(t, results, 1)
			require.True(t, results[0].Skipped, "local quota budget rejection is not supplier failure: %+v", results[0])
			require.NotNil(t, results[0].BudgetDeferral)
			require.Nil(t, results[0].Quota)
			require.Len(t, budget.requests, 1, "nested detached flights must retain physical metering")
			require.False(t, budget.requests[0].MeterTokens)
			require.Empty(t, accounts.extraWrites)
			require.Empty(t, repo.history)
			require.Empty(t, repo.markedIDs)
			if platform == PlatformDeepseek {
				budget.denyAt = 0
				results, err = svc.RunCheck(t.Context(), 1)
				require.NoError(t, err)
				require.False(t, results[0].Skipped, "budget recovery must not wait for supplier negative-cache TTL")
				require.True(t, results[0].Quota.Success)
				require.Len(t, budget.requests, 2)
				require.Equal(t, 1, budget.released)
				require.Len(t, accounts.extraWrites, 1)
				require.Len(t, repo.history, 1)
			}
		})
	}
}

func TestChannelMonitorQuotaBudgetSettlementCannotPublishBalance(t *testing.T) {
	account := newDeepSeekBalanceProbeAccount()
	accounts := &cnBalanceProbeRepo{account: account}
	upstream := monitoringBudgetHTTP{base: &cnBalanceResponseUpstream{statusCode: http.StatusOK,
		body: `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10"}]}`}}
	fetcher := NewChannelMonitorQuotaFetcher(nil, nil,
		NewCNProviderBalanceService(accounts, nil, upstream, nil), accounts, nil)
	repo := &quotaModeRepoStub{monitor: &ChannelMonitor{ID: 1, Provider: PlatformDeepseek,
		CheckMode: MonitorCheckModeQuota, AccountID: &account.ID, PrimaryModel: "quota"}}
	svc := newQuotaModeService(repo)
	svc.SetQuotaFetcher(fetcher)
	budget := &monitoringBudgetRepoStub{releaseErr: errors.New("fixture settlement failure")}
	svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
	results, err := svc.RunCheck(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, results[0].Skipped)
	require.Empty(t, accounts.extraWrites, "settle and close before writing successful quota")
	require.Empty(t, repo.history)
	require.Empty(t, repo.markedIDs)
	require.Equal(t, 1, budget.released)
}

func TestChannelMonitorQuotaBudgetIndependentProviderClients(t *testing.T) {
	for _, generation := range []bool{false, true} {
		t.Run(map[bool]string{false: "quota_get", true: "codex_generation_refresh"}[generation], func(t *testing.T) {
			var sends atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sends.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"usage":{"input_tokens":4,"output_tokens":3}}`)
			}))
			t.Cleanup(server.Close)
			method, path := http.MethodGet, "/quota"
			if generation {
				method, path = http.MethodPost, "/codex/responses"
			}
			key := &clientAdmissionFixture{}
			budget := &monitoringBudgetRepoStub{denyAt: 1, err: &MonitoringProbeBudgetDenial{
				Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}}
			ctx := NewMonitoringProbeBudgetService(budget).Context(t.Context(), server.URL, false)
			request := func() *http.Request {
				req, err := http.NewRequestWithContext(ctx, method, server.URL+path, nil)
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer final-quota-fixture")
				return req
			}
			_, err := doAdmittedOpenAIClientRequest(server.Client(), request(), true, key)
			require.True(t, IsMonitoringProbeBudgetFailure(err))
			require.EqualValues(t, 1, key.finish.Load(), "budget denial releases prior shared-Key admission")
			require.Zero(t, sends.Load())
			budget.denyAt = 0
			ctx = NewMonitoringProbeBudgetService(budget).Context(t.Context(), server.URL, false)
			resp, err := doAdmittedOpenAIClientRequest(server.Client(), request(), true, key)
			require.NoError(t, err)
			_, err = ReadMonitoringProbeResponse(ctx, resp, 1024)
			require.NoError(t, err)
			require.EqualValues(t, 1, sends.Load())
			require.Len(t, budget.requests, 2)
			require.Equal(t, generation, budget.requests[1].MeterTokens)
			require.Equal(t, []string{config.UpstreamKeyFingerprint("final-quota-fixture")}, budget.requests[1].KeyHashes)
			want := MonitoringProbeTokenUsage{}
			if generation {
				want = MonitoringProbeTokenUsage{Reported: true, Tokens: 7}
			}
			require.Equal(t, []MonitoringProbeTokenUsage{want}, budget.usages)
		})
	}
}

func TestChannelMonitorQuotaBudgetIndependentClientKeepsOrdinaryPolicy(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(target.Close)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)
	client := source.Client()
	budget := &monitoringBudgetRepoStub{}
	ctx := NewMonitoringProbeBudgetService(budget).Context(t.Context(), source.URL, false)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer redirect-quota-fixture")
	probeClient := MonitoringProbeHTTPClient(ctx, client)
	require.NotSame(t, client, probeClient)
	require.Same(t, client.Transport, probeClient.Transport.(monitoringProbeTransport).base)
	require.Nil(t, client.CheckRedirect)
	resp, err := probeClient.Do(req)
	require.NoError(t, err)
	_, err = ReadMonitoringProbeResponse(ctx, resp, 1024)
	require.NoError(t, err)
	require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	require.Zero(t, redirected.Load())
	require.Len(t, budget.requests, 1)
	require.Equal(t, 1, budget.released)
	ordinary := context.Background()
	require.Same(t, client, MonitoringProbeHTTPClient(ordinary, client))
	req = req.WithContext(ordinary)
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.EqualValues(t, 1, redirected.Load())
	require.Len(t, budget.requests, 1, "ordinary admin queries keep their existing policy and are not metered")
}

func TestChannelMonitorQuotaBudgetAntigravityCacheAndPhysicalRequests(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sends.Add(1)
		require.Equal(t, "Bearer ag-budget-fixture", req.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(req.URL.Path, ":fetchAvailableModels") {
			_, _ = io.WriteString(w, `{"models":{"model-a":{"quotaInfo":{"remainingFraction":0.8}}}}`)
		} else {
			_, _ = io.WriteString(w, `{"currentTier":{"id":"g1-pro-tier"}}`)
		}
	}))
	t.Cleanup(server.Close)
	oldURLs, oldAvailability := antigravity.BaseURLs, antigravity.DefaultURLAvailability
	antigravity.BaseURLs = []string{server.URL}
	antigravity.DefaultURLAvailability = antigravity.NewURLAvailability(time.Minute)
	t.Cleanup(func() { antigravity.BaseURLs, antigravity.DefaultURLAvailability = oldURLs, oldAvailability })
	account := &Account{ID: 71, Platform: PlatformAntigravity, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "ag-budget-fixture", "project_id": "project-fixture"}}
	accounts := &cnBalanceProbeRepo{account: account}
	usage := &AccountUsageService{antigravityQuotaFetcher: NewAntigravityQuotaFetcher(nil, nil), cache: NewUsageCache()}
	fetcher := NewChannelMonitorQuotaFetcher(usage, nil, nil, accounts, nil)
	budget := &monitoringBudgetRepoStub{denyAt: 1, err: ErrMonitoringProbeBudgetUnavailable}
	fetcher.monitoringBudget = NewMonitoringProbeBudgetService(budget)
	snapshot, err := fetcher.Fetch(t.Context(), account.ID)
	require.Nil(t, snapshot)
	require.True(t, IsMonitoringProbeBudgetFailure(err))
	require.Zero(t, sends.Load())
	_, cached := usage.cache.antigravityCache.Load(account.ID)
	require.False(t, cached, "local deferral must not enter provider degraded/negative cache")
	budget.denyAt = 0
	for range 2 {
		snapshot, err = fetcher.Fetch(t.Context(), account.ID)
		require.NoError(t, err)
		require.True(t, snapshot.Success)
	}
	require.EqualValues(t, 2, sends.Load(), "models and subscription are distinct; cached waiters send nothing")
	require.Len(t, budget.requests, 3)
	require.Equal(t, 2, budget.released)
	for _, request := range budget.requests {
		require.False(t, request.MeterTokens, "non-generation POST does not fabricate unknown Token usage")
	}
	fetcher.cache = make(map[int64]monitorQuotaCacheEntry)
	usage.cache.antigravityCache.Delete(account.ID)
	budget.releaseErr = errors.New("fixture settlement failure")
	snapshot, err = fetcher.Fetch(t.Context(), account.ID)
	require.Nil(t, snapshot)
	require.True(t, IsMonitoringProbeBudgetFailure(err))
	require.EqualValues(t, 3, sends.Load(), "settlement failure after models must stop the subscription follow-up")
	_, cached = usage.cache.antigravityCache.Load(account.ID)
	require.False(t, cached)
	require.Empty(t, fetcher.cache)
}

func TestChannelMonitorQuotaBudgetOpenAIPrivacyClient(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sends.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(req.URL.Path, "/wham/usage") {
			_, _ = io.WriteString(w, `{"email":"quota@example.test","rate_limit":{"allowed":true,"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600}}}`)
		} else {
			_, _ = io.WriteString(w, `{"available_count":0}`)
		}
	}))
	t.Cleanup(server.Close)
	account := &Account{ID: 72, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "quota-fixture", "chatgpt_account_id": "quota-account"}}
	accounts := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "quota-fixture"}}
	svc := NewOpenAIQuotaService(accounts, nil, NewOpenAITokenProvider(accounts, tokens, nil), newQuotaRedirectingFactory(server), nil)
	budget := &monitoringBudgetRepoStub{denyAt: 1, err: ErrMonitoringProbeBudgetUnavailable}
	contextForProbe := func() context.Context {
		return withAccountTestMonitoringSite(NewMonitoringProbeBudgetService(budget).qualityContext(t.Context()), "")
	}
	_, err := svc.QueryUsage(contextForProbe(), account.ID)
	require.True(t, IsMonitoringProbeBudgetFailure(err))
	require.Zero(t, sends.Load())
	budget.denyAt = 0
	usage, err := svc.QueryUsage(contextForProbe(), account.ID)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.EqualValues(t, 2, sends.Load())
	require.Len(t, budget.requests, 3)
	for _, request := range budget.requests {
		require.False(t, request.MeterTokens)
	}
	require.Equal(t, 2, budget.released)
	writesBefore := accounts.extraUpdateCalls
	budget.releaseErr = errors.New("fixture settlement failure")
	usage, err = svc.QueryUsage(contextForProbe(), account.ID)
	require.Nil(t, usage)
	require.True(t, IsMonitoringProbeBudgetFailure(err))
	require.EqualValues(t, 3, sends.Load(), "failed settlement must stop reset-credit follow-up")
	require.Equal(t, writesBefore, accounts.extraUpdateCalls, "failed settlement must not refresh the earlier observation")
	budget = &monitoringBudgetRepoStub{denyAt: 2, err: ErrMonitoringProbeBudgetUnavailable}
	usage, err = svc.QueryUsage(contextForProbe(), account.ID)
	require.Nil(t, usage)
	require.True(t, IsMonitoringProbeBudgetFailure(err))
	require.EqualValues(t, 4, sends.Load(), "the quota GET completes but credit details are not sent")
	require.Equal(t, writesBefore, accounts.extraUpdateCalls, "a deferred quota chain must not refresh supplier state")
}

func TestChannelMonitorQuotaBudgetGrokFlightDoesNotWriteSupplierFailure(t *testing.T) {
	account := healthyGrokQuotaOAuthAccount(73)
	accounts := &grokQuotaAccountRepo{mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
		accountsByID: map[int64]*Account{account.ID: account}}}
	base := &grokQuotaSequenceUpstream{}
	svc := NewGrokQuotaService(accounts, nil, NewGrokTokenProvider(accounts, nil), monitoringBudgetHTTP{base: base}, nil)
	budget := &monitoringBudgetRepoStub{denyAt: 1, err: ErrMonitoringProbeBudgetUnavailable}
	ctx := withAccountTestMonitoringSite(NewMonitoringProbeBudgetService(budget).qualityContext(t.Context()), "")
	result, err := svc.ProbeBilling(ctx, account.ID)
	require.Nil(t, result)
	require.True(t, IsMonitoringProbeBudgetFailure(err))
	require.Empty(t, base.snapshotRequests())
	require.Empty(t, accounts.updates)
	require.Zero(t, accounts.updateCalls)
}

func TestChannelMonitorQuotaBudgetMixedProbeRetainsIndependentObservation(t *testing.T) {
	swapMonitorHTTPClient(t)
	oldPing := monitorPingHTTPClient
	monitorPingHTTPClient = monitorHTTPClient
	t.Cleanup(func() { monitorPingHTTPClient = oldPing })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodHead {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(server.Close)
	account := newDeepSeekBalanceProbeAccount()
	accounts := &cnBalanceProbeRepo{account: account}
	repo := &quotaModeRepoStub{monitor: &ChannelMonitor{ID: 1, Provider: MonitorProviderOpenAI,
		CheckMode: MonitorCheckModeQuotaProbe, AccountID: &account.ID, PrimaryModel: "model-a",
		Endpoint: server.URL, APIKey: "OLD:model-fixture", BodyOverrideMode: MonitorBodyOverrideModeReplace,
		BodyOverride: map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}}}
	svc := newQuotaModeService(repo)
	svc.SetQuotaFetcher(NewChannelMonitorQuotaFetcher(nil, nil, NewCNProviderBalanceService(accounts, nil,
		monitoringBudgetHTTP{base: &cnBalanceResponseUpstream{statusCode: http.StatusOK, body: `{}`}}, nil), accounts, nil))
	budget := &monitoringBudgetRepoStub{denyAt: 3, err: &MonitoringProbeBudgetDenial{
		Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}}
	svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
	results, err := svc.RunCheck(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, MonitorStatusOperational, results[0].Status)
	require.False(t, results[0].Skipped)
	require.Nil(t, results[0].BudgetDeferral)
	require.NotNil(t, results[0].QuotaBudgetDeferral)
	require.Nil(t, results[0].Quota)
	require.Len(t, repo.history, 1)
	require.Nil(t, repo.history[0].Quota)
	require.Len(t, repo.markedIDs, 1)
	require.Empty(t, accounts.extraWrites)
}

type quotaBudgetClientUpstream struct {
	HTTPUpstream
	client *http.Client
}

func (u quotaBudgetClientUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}

func TestChannelMonitorQuotaBudgetFlightIsolationFromManualQueries(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var released atomic.Bool
	finish := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	t.Cleanup(finish)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		_, _ = io.WriteString(w, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10"}]}`)
	}))
	t.Cleanup(server.Close)
	account := newDeepSeekBalanceProbeAccount()
	account.Credentials["base_url"] = server.URL
	accounts := &cnBalanceProbeRepo{account: account}
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
		AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	quota := NewCNProviderBalanceService(accounts, nil, monitoringBudgetHTTP{base: quotaBudgetClientUpstream{
		client: server.Client()}}, cfg)
	manualDone := make(chan error, 1)
	go func() {
		_, err := quota.QueryBalanceForAccount(context.Background(), account)
		manualDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("manual query did not reach fixture")
	}
	fetcher := NewChannelMonitorQuotaFetcher(nil, nil, quota, accounts, cfg)
	budget := &monitoringBudgetRepoStub{denyAt: 1, err: ErrMonitoringProbeBudgetUnavailable}
	fetcher.monitoringBudget = NewMonitoringProbeBudgetService(budget)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	snapshot, err := fetcher.Fetch(ctx, account.ID)
	require.Nil(t, snapshot, "monitored query must not silently join the unmetered manual flight")
	require.True(t, IsMonitoringProbeBudgetFailure(err))
	require.Empty(t, accounts.extraWrites)
	finish()
	require.NoError(t, <-manualDone)
}
