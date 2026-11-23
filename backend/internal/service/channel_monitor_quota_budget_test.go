//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
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

type monitoringBoundaryResponseBody struct {
	io.Reader
	beforeRead func()
	onClose    func()
	closeErr   error
}

func (b *monitoringBoundaryResponseBody) Read(p []byte) (int, error) {
	if b.beforeRead != nil {
		b.beforeRead()
	}
	return b.Reader.Read(p)
}

func (b *monitoringBoundaryResponseBody) Close() error {
	if b.onClose != nil {
		b.onClose()
	}
	return b.closeErr
}

func TestReadMonitoringProbeResponseCanceledBodyIsNotEvidence(t *testing.T) {
	for _, phase := range []string{"before_read", "during_read", "during_close"} {
		t.Run(phase, func(t *testing.T) {
			cause := errors.New("probe caller retired")
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			var closed int
			body := &monitoringBoundaryResponseBody{Reader: strings.NewReader(`{"balance":10}`), onClose: func() { closed++ }}
			switch phase {
			case "before_read":
				cancel(cause)
			case "during_read":
				body.beforeRead = func() { cancel(cause) }
			case "during_close":
				body.onClose = func() { closed++; cancel(cause) }
			}
			data, err := ReadMonitoringProbeResponse(ctx, &http.Response{Body: body}, 1024)
			require.Equal(t, 1, closed, "body closure and lease settlement must precede a cancellation return")
			require.ErrorIs(t, err, cause)
			require.Nil(t, data, "late complete bytes must not be published as current quota evidence")
		})
	}
}

func TestReadMonitoringProbeResponseCancellationPreservesSettlement(t *testing.T) {
	for _, generation := range []bool{false, true} {
		for _, phase := range []string{"success", "read", "close", "close_with_settlement_failure"} {
			t.Run(fmt.Sprintf("generation=%t/%s", generation, phase), func(t *testing.T) {
				caller, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				cause := errors.New("response publication retired")
				budget := &monitoringBudgetRepoStub{}
				if phase == "close_with_settlement_failure" {
					budget.releaseErr = errors.New("fixture settlement failure")
				}
				ctx := NewMonitoringProbeBudgetService(budget).Context(caller, "https://fixture.invalid", false)
				method, path := http.MethodGet, "/quota"
				if generation {
					method, path = http.MethodPost, "/v1/responses"
				}
				req, err := http.NewRequestWithContext(ctx, method, "https://fixture.invalid"+path, nil)
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer response-publication-fixture")
				req, finish, err := BeginMonitoringProbeRequest(req)
				require.NoError(t, err)
				body := &monitoringBoundaryResponseBody{Reader: strings.NewReader(`{"usage":{"input_tokens":4,"output_tokens":3}}`)}
				if phase == "read" {
					body.beforeRead = func() { cancel(cause) }
				} else if phase != "success" {
					body.onClose = func() { cancel(cause) }
				}
				response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body, Request: req}
				require.NoError(t, FinishMonitoringProbeResponse(req.Context(), response, nil, finish))
				data, err := ReadMonitoringProbeResponse(ctx, response, 1024)
				switch phase {
				case "success":
					require.NoError(t, err, "normal lease release cancels only its internal context, not the caller")
					require.NotEmpty(t, data)
				case "close_with_settlement_failure":
					require.True(t, IsMonitoringProbeBudgetFailure(err), "local settlement failure must keep its typed deferral")
					require.Nil(t, data)
				default:
					require.ErrorIs(t, err, cause)
					require.Nil(t, data)
				}
				require.Equal(t, 1, budget.released)
				require.Len(t, budget.requests, 1)
				usage := MonitoringProbeTokenUsage{}
				if generation {
					usage = MonitoringProbeTokenUsage{Reported: true, Tokens: 7}
				}
				require.Equal(t, []MonitoringProbeTokenUsage{usage}, budget.usages, "canceled publication must not refund the physical call or reported tokens")
			})
		}
	}
}

func TestReadMonitoringProbeResponseCancellationRetainsIOErrors(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	cause, readErr, closeErr := errors.New("caller retired"), errors.New("fixture read failure"), errors.New("fixture close failure")
	body := &monitoringBoundaryResponseBody{Reader: io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(readErr)),
		onClose: func() { cancel(cause) }, closeErr: closeErr}
	data, err := ReadMonitoringProbeResponse(ctx, &http.Response{Body: body}, 1024)
	require.Nil(t, data)
	for _, expected := range []error{cause, readErr, closeErr} {
		require.ErrorIs(t, err, expected)
	}
}

type monitoringBoundaryQuotaUpstream struct {
	HTTPUpstream
	response func(*http.Request) *http.Response
}

func (u monitoringBoundaryQuotaUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.response(req), nil
}

func TestCNProviderBalanceDeadlinePreservesPreviousEvidence(t *testing.T) {
	for _, phase := range []string{"response", "body"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				account := newDeepSeekBalanceProbeAccount()
				account.Extra = map[string]any{"deepseek_balance": 2.0, "deepseek_balance_low": true,
					"deepseek_balance_updated_at": "2026-01-01T00:00:00Z", "unrelated": "preserved"}
				accounts := &cnBalanceProbeRepo{account: account}
				budget := &monitoringBudgetRepoStub{}
				closed := 0
				upstream := monitoringBoundaryQuotaUpstream{response: func(req *http.Request) *http.Response {
					body := &monitoringBoundaryResponseBody{Reader: strings.NewReader(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10"}]}`),
						onClose: func() { closed++ }}
					if phase == "response" {
						<-req.Context().Done()
					} else {
						body.beforeRead = func() { <-req.Context().Done() }
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}
				}}
				ctx := withAccountTestMonitoringSite(NewMonitoringProbeBudgetService(budget).qualityContext(t.Context()), "")
				svc := NewCNProviderBalanceService(accounts, nil, monitoringBudgetHTTP{base: upstream}, nil)
				result, err := svc.QueryBalance(ctx, account.ID)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Nil(t, result)
				require.Empty(t, accounts.extraWrites, "an expired request must not renew balance or clear a prior low-balance flag")
				require.Equal(t, 1, closed)
				require.Len(t, budget.requests, 1)
				require.Equal(t, 1, budget.released, "the real physical attempt is still charged and settled")
				require.Equal(t, []MonitoringProbeTokenUsage{{}}, budget.usages, "quota requests must not invent generation usage")
				require.NoError(t, t.Context().Err(), "a provider deadline is distinct from caller cancellation")
			})
		})
	}
}

func TestCNProviderQuotaDeadlinePreservesPreviousEvidence(t *testing.T) {
	for _, platform := range []string{PlatformCommandCode, PlatformCline, PlatformMistral} {
		for _, phase := range []string{"response", "body"} {
			t.Run(platform+"/"+phase, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					account, base, _, _ := accountQuotaBudgetFixture(t, platform)
					accounts := &cnBalanceProbeRepo{account: account}
					budget := &monitoringBudgetRepoStub{}
					upstream := monitoringBoundaryQuotaUpstream{response: func(req *http.Request) *http.Response {
						response, _ := base.Do(req, "", account.ID, account.Concurrency)
						if phase == "response" {
							<-req.Context().Done()
						} else {
							original := response.Body
							response.Body = &monitoringBoundaryResponseBody{Reader: original, beforeRead: func() { <-req.Context().Done() },
								onClose: func() { _ = original.Close() }}
						}
						return response
					}}
					ctx := withAccountTestMonitoringSite(NewMonitoringProbeBudgetService(budget).qualityContext(t.Context()), "")
					svc := NewCNProviderQuotaService(accounts, nil, monitoringBudgetHTTP{base: upstream}, nil)
					result, err := svc.QueryUsage(ctx, account.ID)
					require.Error(t, err, "expired primary quota responses must not become successful provider evidence")
					require.Nil(t, result)
					require.Empty(t, accounts.extraWrites)
					require.Len(t, base.requests, 1, "a timed-out primary must not trigger optional metadata requests")
					require.Equal(t, 1, budget.released)
					require.Equal(t, []MonitoringProbeTokenUsage{{}}, budget.usages)
					require.NoError(t, t.Context().Err())
				})
			})
		}
	}
}

func TestCodexUsageProbeMonitoringRequestDeadlinePreservesHeaders(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	account := codexProbeFixtureAccount()
	codexProbeLoopback(t, account, server)
	client, err := httppool.GetClient(httppool.Options{ProxyURL: account.Proxy.URL(), Timeout: 15 * time.Second, ResponseHeaderTimeout: 10 * time.Second})
	require.NoError(t, err)
	original := client.Transport
	t.Cleanup(func() { client.Transport = original })
	synctest.Test(t, func(t *testing.T) {
		var requests, closed int
		client.Transport = codexProbeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			require.Equal(t, "chatgpt.com", req.URL.Host)
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"},
				"X-Codex-Primary-Used-Percent": []string{"0"}, "X-Codex-Primary-Window-Minutes": []string{"300"}},
				Body: &monitoringBoundaryResponseBody{Reader: strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":3}}}\n\n"),
					beforeRead: func() { <-req.Context().Done() }, onClose: func() { closed++ }}, Request: req}, nil
		})
		repo := &codexProbeRecoveryRepo{accountUsageCodexProbeRepo: accountUsageCodexProbeRepo{updateExtraCh: make(chan map[string]any, 1)}, cleared: make(chan struct{}, 1)}
		svc := &AccountUsageService{accountRepo: repo}
		budget := &monitoringBudgetRepoStub{}
		ctx := NewMonitoringProbeBudgetService(budget).Context(t.Context(), "https://chatgpt.com", false)
		updates, err := svc.probeOpenAICodexSnapshot(ctx, account)
		synctest.Wait()
		require.ErrorIs(t, err, context.DeadlineExceeded, "the nested 15-second request deadline, not the still-live parent, owns publication")
		require.Empty(t, updates)
		require.Empty(t, repo.updateExtraCh)
		require.Empty(t, repo.cleared, "late zero-usage headers must not recover the prior cooldown")
		require.Equal(t, 77.0, account.Extra["codex_5h_used_percent"])
		require.Equal(t, 1, requests)
		require.Equal(t, 1, closed)
		require.Equal(t, 1, budget.released)
		require.Equal(t, []MonitoringProbeTokenUsage{{Reported: true, Tokens: 7}}, budget.usages)
		require.NoError(t, ctx.Err())
	})
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

			svc.runOnce(t.Context())

			require.Len(t, budget.requests, 1, "each background dispatch must install the shared policy")
			require.Empty(t, upstream.requests)
			require.Empty(t, accounts.extraWrites)
			budget.denyAt = 0
			svc.runOnce(t.Context())
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

func TestCNProviderBalanceCheckFairBudgetCycles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := cnQueueAccounts(t, 100)
		var mu sync.Mutex
		physical := map[int64]int{}
		budgetUsed := 0
		budget := &monitoringBudgetRepoStub{acquire: func(_ MonitoringProbeBudgetRequest, _ int) (string, error) {
			if budgetUsed >= 4 {
				return "", &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_minute", RetryAt: time.Now().Add(time.Minute)}
			}
			budgetUsed++
			return "queue-fixture-lease", nil
		}}
		prober := cnQueueQuotaProber{query: func(ctx context.Context, id int64) (*CNProviderQuotaProbeResult, error) {
			calls := 1
			if id == 1 {
				calls = 6 // Cannot complete inside this cycle; must not starve peers.
			}
			for range calls {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://fixture.invalid/usage", nil)
				if err != nil {
					return nil, err
				}
				req.Header.Set("Authorization", fmt.Sprintf("Bearer fixture-%d", id))
				req, release, err := BeginMonitoringProbeRequest(req)
				if err != nil {
					return nil, err
				}
				mu.Lock()
				physical[id]++
				mu.Unlock()
				time.Sleep(time.Millisecond)
				if err := FinishMonitoringProbeResponse(req.Context(), nil, nil, release); err != nil {
					return nil, err
				}
			}
			if id%17 == 0 {
				return nil, errors.New("fixture supplier failure")
			}
			return &CNProviderQuotaProbeResult{Success: true}, nil
		}}
		svc := &CNProviderBalanceCheckService{accountRepo: repo, quotaService: prober, cfg: &config.Config{}}
		svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
		for cycle := 0; cycle < 40; cycle++ {
			// Continuing inventory growth cannot take every older deferred turn.
			repo.byPlatform[PlatformKimi] = append(repo.byPlatform[PlatformKimi], Account{ID: int64(1000 + cycle), Platform: PlatformKimi,
				Type: AccountTypeAPIKey, Status: StatusActive, Credentials: map[string]any{"account_mode": AccountModeCoding}})
			before := len(budget.requests)
			budgetUsed = 0
			svc.runOnce(t.Context())
			if got := len(budget.requests) - before; got > 4+cnQuotaProbeConcurrency {
				t.Errorf("cycle %d: global exhaustion caused %d admission requests; want at most budget + in-flight workers", cycle, got)
			}
		}
		for id := int64(1); id <= 100; id++ {
			require.Positive(t, physical[id], "original account %d was starved across multiple cycles", id)
		}
		for _, request := range budget.requests {
			require.False(t, request.Recovery)
			require.False(t, request.MeterTokens)
		}
		require.Equal(t, budget.released, sumCNPhysicalCalls(physical), "every physical acquisition must be released")
	})
}

func TestCNProviderBalanceCheckOlderTierDrainsBeforeRecentWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var oldCalls, recentCalls atomic.Int32
		var recovering atomic.Bool
		prober := cnQueueQuotaProber{query: func(ctx context.Context, id int64) (*CNProviderQuotaProbeResult, error) {
			if id == 1 {
				oldCalls.Add(1)
				if !recovering.Load() {
					close(entered)
					<-release
					denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_minute", RetryAt: time.Now().Add(time.Minute)}
					recordMonitoringProbeLocalError(ctx, denial)
					return nil, denial
				}
			} else {
				recentCalls.Add(1)
			}
			return &CNProviderQuotaProbeResult{Success: true}, nil
		}}
		svc := &CNProviderBalanceCheckService{accountRepo: cnQueueAccounts(t, 2), quotaService: prober, cfg: &config.Config{},
			turnSequence: 100, turns: map[int64]cnBalanceProbeTurn{1: {served: 1}, 2: {served: 100}}}
		svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(&monitoringBudgetRepoStub{}))
		go func() { svc.runOnce(t.Context()); close(done) }()
		<-entered
		synctest.Wait()
		before := recentCalls.Load()
		close(release)
		<-done
		require.Zero(t, before, "recent work must not race older in-flight work for its budget")
		require.Zero(t, recentCalls.Load(), "global exhaustion must stop the next age tier")
		require.EqualValues(t, 1, oldCalls.Load())
		require.EqualValues(t, 1, svc.turns[1].served, "unadmitted work retains its original age")
		recovering.Store(true)
		svc.runOnce(t.Context())
		require.EqualValues(t, 2, oldCalls.Load())
		require.EqualValues(t, 1, recentCalls.Load(), "recovered old work must release the younger tier")
		require.Equal(t, svc.turns[1].served, svc.turns[2].served, "one completed round must share an age tier")
	})
}

func TestCNProviderBalanceCheckOlderTierLocalDenialReleasesPeers(t *testing.T) {
	for _, scope := range []string{"key", "key_minute", "site", "site_minute"} {
		t.Run(scope, func(t *testing.T) {
			var recentCalls atomic.Int32
			svc := &CNProviderBalanceCheckService{accountRepo: cnQueueAccounts(t, 2), cfg: &config.Config{},
				turnSequence: 100, turns: map[int64]cnBalanceProbeTurn{1: {served: 1}, 2: {served: 100}},
				quotaService: cnQueueQuotaProber{query: func(ctx context.Context, id int64) (*CNProviderQuotaProbeResult, error) {
					if id == 1 {
						denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: scope, RetryAt: time.Now().Add(time.Minute)}
						recordMonitoringProbeLocalError(ctx, denial)
						return nil, denial
					}
					recentCalls.Add(1)
					return &CNProviderQuotaProbeResult{Success: true}, nil
				}}}
			svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(&monitoringBudgetRepoStub{}))
			svc.runOnce(t.Context())
			require.EqualValues(t, 1, recentCalls.Load(), "a local denial must release the next independent age tier")
			require.EqualValues(t, 1, svc.turns[1].served)
			require.Greater(t, svc.turns[2].served, uint64(100))
		})
	}
}

func sumCNPhysicalCalls(calls map[int64]int) int {
	total := 0
	for _, count := range calls {
		total += count
	}
	return total
}

func TestCNProviderBalanceCheckStopCancelsQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Gateway.CNProviders.BalanceCheckEnabled = true
		var starts atomic.Int32
		svc := NewCNProviderBalanceCheckService(cnQueueAccounts(t, 100), &CNProviderBalanceService{}, nil, cfg, time.Minute)
		svc.quotaService = cnQueueQuotaProber{query: func(ctx context.Context, _ int64) (*CNProviderQuotaProbeResult, error) {
			starts.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		baseline := runtime.NumGoroutine()
		svc.Start()
		time.Sleep(time.Minute)
		synctest.Wait()
		started, goroutines := starts.Load(), runtime.NumGoroutine()-baseline
		stopping := time.Now()
		svc.Stop()
		elapsed := time.Since(stopping)
		svc.Stop()
		require.EqualValues(t, cnQuotaProbeConcurrency, started)
		require.LessOrEqual(t, goroutines, 12, "queued accounts must not own a goroutine each")
		require.Less(t, elapsed, time.Second, "shutdown must cancel the cycle, not wait for its 300s deadline")
		require.Equal(t, started, starts.Load(), "shutdown must not start queued accounts with an expired context")
	})
}

func TestCNProviderBalanceCheckQueueBudgetScope(t *testing.T) {
	for _, scope := range []string{"key", "key_minute", "site", "site_minute", "global", "global_minute", "global_day", "system"} {
		t.Run(scope, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				budget := &monitoringBudgetRepoStub{acquire: func(_ MonitoringProbeBudgetRequest, n int) (string, error) {
					if n == 1 {
						return "", &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: scope, RetryAt: time.Now().Add(time.Minute)}
					}
					return "scope-fixture-lease", nil
				}}
				var completed atomic.Int32
				prober := cnQueueQuotaProber{query: func(ctx context.Context, id int64) (*CNProviderQuotaProbeResult, error) {
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://fixture-%d.invalid/usage", id), nil)
					if err != nil {
						return nil, err
					}
					req.Header.Set("Authorization", fmt.Sprintf("Bearer fixture-%d", id))
					req, release, err := BeginMonitoringProbeRequest(req)
					if err != nil {
						return nil, err
					}
					time.Sleep(time.Millisecond)
					if err := FinishMonitoringProbeResponse(req.Context(), nil, nil, release); err != nil {
						return nil, err
					}
					completed.Add(1)
					return &CNProviderQuotaProbeResult{Success: true}, nil
				}}
				svc := &CNProviderBalanceCheckService{accountRepo: cnQueueAccounts(t, 100), quotaService: prober, cfg: &config.Config{}}
				svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
				svc.runOnce(svc.monitoringBudget.qualityContext(t.Context()))
				switch scope {
				case "key", "key_minute", "site", "site_minute":
					require.Len(t, budget.requests, 100, "local denial must not block independent origins/credentials")
					require.EqualValues(t, 99, completed.Load())
				default:
					require.LessOrEqual(t, len(budget.requests), cnQuotaProbeConcurrency, "global/system denial must stop further dispatch")
				}
				require.EqualValues(t, completed.Load(), budget.released, "all already-admitted work must settle")
			})
		})
	}
}

func TestCNProviderBalanceCheckQueueInventoryLifecycle(t *testing.T) {
	base := cnQueueAccounts(t, 12)
	failed := false
	repo := cnQueueInventoryRepo{fakeCNCheckRepo: base, list: func(ctx context.Context, platform string) ([]Account, error) {
		if failed && platform == PlatformCline {
			return nil, errors.New("fixture inventory unavailable")
		}
		return base.ListByPlatform(ctx, platform)
	}}
	svc := &CNProviderBalanceCheckService{accountRepo: repo, quotaService: &fakeCNQuotaProber{}, cfg: &config.Config{}}
	svc.runOnce(t.Context())
	require.Len(t, svc.turns, 12)
	clineID := base.byPlatform[PlatformCline][0].ID
	prior := svc.turns[clineID]
	failed = true
	svc.runOnce(t.Context())
	require.Equal(t, prior, svc.turns[clineID], "partial inventory must preserve absent accounts' deferred age")
	failed = false
	delete(base.byPlatform, PlatformCline)
	svc.runOnce(t.Context())
	require.Len(t, svc.turns, 8, "complete inventory must reclaim removed targets")
	clear(base.byPlatform)
	svc.runOnce(t.Context())
	require.Empty(t, svc.turns)
}

func TestCNProviderBalanceCheckQueueCancelsCollection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Gateway.CNProviders.BalanceCheckEnabled = true
		var lists, probes atomic.Int32
		repo := cnQueueInventoryRepo{list: func(ctx context.Context, _ string) ([]Account, error) {
			lists.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		svc := NewCNProviderBalanceCheckService(repo, &CNProviderBalanceService{}, nil, cfg, time.Minute)
		svc.quotaService = cnQueueQuotaProber{query: func(context.Context, int64) (*CNProviderQuotaProbeResult, error) {
			probes.Add(1)
			return &CNProviderQuotaProbeResult{Success: true}, nil
		}}
		svc.Start()
		svc.Start()
		time.Sleep(time.Minute)
		synctest.Wait()
		start := time.Now()
		svc.Stop()
		svc.Start()
		time.Sleep(time.Minute)
		require.Less(t, time.Since(start)-time.Minute, time.Second)
		require.EqualValues(t, 1, lists.Load(), "duplicate Start and Start after Stop must not create another ticker")
		require.Zero(t, probes.Load(), "canceled inventory must not dispatch")
	})
}

func TestCNProviderBalanceCheckQueueOverlapAndPanicRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var started atomic.Int32
		proceed := make(chan struct{})
		svc := &CNProviderBalanceCheckService{accountRepo: cnQueueAccounts(t, 12), cfg: &config.Config{},
			quotaService: cnQueueQuotaProber{query: func(ctx context.Context, id int64) (*CNProviderQuotaProbeResult, error) {
				started.Add(1)
				<-proceed
				if id == 1 {
					panic("fixture provider panic")
				}
				return &CNProviderQuotaProbeResult{Success: true}, nil
			}}}
		done := make(chan struct{})
		go func() {
			defer close(done)
			svc.runOnce(t.Context())
		}()
		synctest.Wait()
		require.EqualValues(t, cnQuotaProbeConcurrency, started.Load())
		svc.runOnce(t.Context())
		require.EqualValues(t, cnQuotaProbeConcurrency, started.Load(), "an overlapping cycle must not duplicate the queue")
		close(proceed)
		<-done
		require.EqualValues(t, 12, started.Load(), "a failed worker must release its queue slot")
	})
}

type cnQueueBalanceRepo struct {
	AccountRepository
	load func(context.Context, int64) (*Account, error)
}

func (r cnQueueBalanceRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	return r.load(ctx, id)
}

func TestCNProviderBalanceCheckQueuePaygSerialWithoutBlockingQuota(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := cnQueueAccounts(t, 12)
		for platform := range repo.byPlatform {
			for i := range repo.byPlatform[platform] {
				repo.byPlatform[platform][i].ID += 1000
			}
		}
		for id := int64(101); id <= 105; id++ {
			repo.byPlatform[PlatformDeepseek] = append(repo.byPlatform[PlatformDeepseek], Account{ID: id, Platform: PlatformDeepseek,
				Status: StatusActive, Schedulable: true, Type: AccountTypeAPIKey})
		}
		var mu sync.Mutex
		active, maxActive, paygActive, maxPayg, calls, paygCalls := 0, 0, 0, 0, 0, 0
		quotaWhilePayg := false
		check := func(payg bool) {
			mu.Lock()
			active++
			calls++
			if payg {
				paygActive++
				paygCalls++
			} else if paygActive > 0 {
				quotaWhilePayg = true
			}
			maxActive, maxPayg = max(maxActive, active), max(maxPayg, paygActive)
			mu.Unlock()
			if payg {
				time.Sleep(3 * time.Millisecond)
			} else {
				time.Sleep(time.Millisecond)
			}
			mu.Lock()
			active--
			if payg {
				paygActive--
			}
			mu.Unlock()
		}
		balanceRepo := cnQueueBalanceRepo{load: func(context.Context, int64) (*Account, error) {
			check(true)
			return nil, errors.New("fixture balance unavailable")
		}}
		svc := &CNProviderBalanceCheckService{accountRepo: repo, cfg: &config.Config{},
			balanceService: NewCNProviderBalanceService(balanceRepo, nil, nil, nil),
			quotaService: cnQueueQuotaProber{query: func(context.Context, int64) (*CNProviderQuotaProbeResult, error) {
				check(false)
				return &CNProviderQuotaProbeResult{Success: true}, nil
			}}}
		// Older payg IDs run first without relying on platform collection order.
		svc.runOnce(t.Context())
		require.Equal(t, 17, calls)
		require.Equal(t, 5, paygCalls)
		require.Equal(t, 1, maxPayg)
		require.Equal(t, cnQuotaProbeConcurrency, maxActive)
		require.True(t, quotaWhilePayg, "a slow balance check must leave the other slots available")
		maxActive, maxPayg, calls, paygCalls, quotaWhilePayg = 0, 0, 0, 0, false
		svc.runOnce(t.Context())
		require.Equal(t, 17, calls)
		require.Equal(t, 5, paygCalls)
		require.Equal(t, 1, maxPayg)
		require.Equal(t, cnQuotaProbeConcurrency, maxActive, "completed peers must remain concurrent in the next round")
		require.True(t, quotaWhilePayg)
	})
}
