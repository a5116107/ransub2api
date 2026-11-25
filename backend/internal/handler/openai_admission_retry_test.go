//go:build unit

package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type initialAdmissionRepo struct {
	excelBPSFailoverAccountRepo
	cache        service.GatewayCache
	changed      map[int64]bool
	disabled     map[int64]bool
	readErr      error
	onRead       func()
	initialReads []int64
	slots        *helperConcurrencyCacheStub
}

func (r *initialAdmissionRepo) GetOpenAITurnAdmission(ctx context.Context, id int64) (*service.Account, *service.Account, error) {
	r.initialReads = append(r.initialReads, id)
	if r.onRead != nil {
		r.onRead()
	}
	if r.readErr != nil {
		return nil, nil, r.readErr
	}
	a, err := r.GetByID(ctx, id)
	if a != nil && r.changed[id] {
		a.Extra = maps.Clone(a.Extra)
		a.Extra["openai_excel_bps"] = false
	}
	if a != nil && r.disabled[id] {
		a.Schedulable = false
	}
	return a, nil, err
}

func newInitialAdmissionHandler(t *testing.T, repo *initialAdmissionRepo, upstream service.HTTPUpstream) *OpenAIGatewayHandler {
	t.Helper()
	for id := int64(1); id <= 2; id++ {
		repo.accounts = append(repo.accounts, service.Account{
			ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{3131}, Priority: int(id), Concurrency: 1,
			Credentials: map[string]any{"access_token": fmt.Sprintf("test-token-%d", id), "chatgpt_account_id": fmt.Sprintf("test-account-%d", id)},
			Extra:       map[string]any{"openai_excel_bps": true},
		})
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	repo.slots = &helperConcurrencyCacheStub{accountSeq: []bool{true, true, true}}
	concurrency := service.NewConcurrencyService(repo.slots)
	gw := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, repo.cache, cfg,
		nil, concurrency, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	h := NewOpenAIGatewayHandler(gw, concurrency, billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	h.maxAccountSwitches = 2
	return h
}

type encryptedHTTPBindingCache struct {
	testutil.StubGatewayCache
	mu       sync.Mutex
	bindings map[string]int64
}

func (s *encryptedHTTPBindingCache) GetSessionAccountID(_ context.Context, groupID int64, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id := s.bindings[fmt.Sprintf("%d:%s", groupID, key)]; id > 0 {
		return id, nil
	}
	return 0, service.ErrStickySessionNotFound
}

func (s *encryptedHTTPBindingCache) SetSessionAccountID(_ context.Context, groupID int64, key string, id int64, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindings == nil {
		s.bindings = make(map[string]int64)
	}
	s.bindings[fmt.Sprintf("%d:%s", groupID, key)] = id
	return nil
}

func (s *encryptedHTTPBindingCache) DeleteSessionAccountID(_ context.Context, groupID int64, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bindings, fmt.Sprintf("%d:%s", groupID, key))
	return nil
}

func (s *encryptedHTTPBindingCache) snapshot() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.bindings)
}

func TestEncryptedHTTPFailoverOnlyCommitsSuccessfulReplacement(t *testing.T) {
	for _, outcome := range []string{"rejected", "completed", "partial-failed", "truncated"} {
		for _, passthrough := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/passthrough=%t/stream=%t", outcome, passthrough, stream), func(t *testing.T) {
					cache := &encryptedHTTPBindingCache{}
					repo := &initialAdmissionRepo{cache: cache}
					upstream := &excelBPSFailoverUpstream{answer: func(call int) *http.Response {
						switch call {
						case 1:
							return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"server_error","message":"fixture upstream unavailable"}}`))}
						case 2:
							if outcome == "rejected" {
								return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_encrypted_content","message":"fixture encrypted history rejected"}}`))}
							}
							if outcome != "completed" {
								wire := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial answer\"}\n\n"
								if outcome == "partial-failed" {
									wire += "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"fixture failure after output\"}}}\n\n"
								}
								return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
							}
							return excelBPSCompleted()
						default:
							return excelBPSCompleted()
						}
					}}
					h := newInitialAdmissionHandler(t, repo, upstream)
					repo.slots.accountSeq = []bool{true, true, true, true}
					for i := range repo.accounts {
						a := &repo.accounts[i]
						a.Type = service.AccountTypeAPIKey
						a.Credentials = map[string]any{"api_key": fmt.Sprintf("fixture-key-%d", a.ID), "base_url": "https://fixture.example.test"}
						a.Extra = map[string]any{"openai_passthrough": passthrough}
					}
					request := func(encrypted bool) string {
						c, rec := newExcelBPSFailoverTestContext(t.Context(), stream)
						c.Request.Header.Set("session_id", "history-affinity-regression")
						input := `[{"role":"user","content":"hello"}]`
						if encrypted {
							// Two unlocated opaque items must not be discarded to
							// manufacture recovery. Only the binding timing changes.
							input = `[{"type":"reasoning","encrypted_content":"fixture-one"},{"type":"reasoning","encrypted_content":"fixture-two"},{"role":"user","content":"continue"}]`
						}
						body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":%s}`, stream, input)
						c.Request.Body = io.NopCloser(strings.NewReader(body))
						c.Request.ContentLength = int64(len(body))
						h.Responses(c)
						return rec.Body.String()
					}
					require.Contains(t, request(false), "resp_bps_failover")
					original := cache.snapshot()
					require.Len(t, original, 1)
					for _, id := range original {
						require.Equal(t, int64(1), id)
					}
					upstream.onDo = func(_ int) {
						require.Equal(t, original, cache.snapshot(), "dispatch does not prove that the replacement completed successfully")
					}
					replacement := request(true)
					upstream.onDo = nil
					require.Equal(t, []int64{1, 1, 2}, upstream.calls())
					for _, body := range upstream.bodies[1:] {
						require.Contains(t, string(body), "fixture-one")
						require.Contains(t, string(body), "fixture-two")
					}
					wantAccount := int64(1)
					if outcome == "completed" {
						require.Contains(t, replacement, "resp_bps_failover")
						wantAccount = 2
						for key := range original {
							original[key] = wantAccount
						}
					} else {
						require.NotContains(t, replacement, "resp_bps_failover", "the replacement must really fail")
					}
					require.Equal(t, original, cache.snapshot(), "only a successful replacement becomes the next turn's sticky target")
					require.Contains(t, request(true), "resp_bps_failover")
					require.Equal(t, []int64{1, 1, 2, wantAccount}, upstream.calls(), "the next turn returns to the last successfully bound route")
					require.Equal(t, original, cache.snapshot())
					repo.slots.mu.Lock()
					defer repo.slots.mu.Unlock()
					require.Equal(t, repo.slots.accountAcquireCalls, repo.slots.accountReleaseCalls)
				})
			}
		}
	}
}

func TestOpenAIInitialAdmissionReselectsBeforeSending(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, chat := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("chat=%t/stream=%t", chat, stream), func(t *testing.T) {
				repo := &initialAdmissionRepo{changed: map[int64]bool{1: true}}
				upstream := &excelBPSFailoverUpstream{answer: func(int) *http.Response {
					resp := excelBPSCompleted()
					wire, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					delta := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"done\",\"output_index\":0,\"content_index\":0}\n\n"
					resp.Body = io.NopCloser(strings.NewReader(delta + string(wire)))
					return resp
				}}
				h := newInitialAdmissionHandler(t, repo, upstream)
				c, rec := newExcelBPSFailoverTestContext(context.Background(), stream)
				if chat {
					// Exercise the same Chat Completions messages shape as model tests.
					body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream)
					c.Request.Body = io.NopCloser(strings.NewReader(body))
					c.Request.ContentLength = int64(len(body))
					c.Request.URL.Path = "/v1/chat/completions"
					h.ChatCompletions(c)
				} else {
					h.Responses(c)
				}
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "done")
				require.Equal(t, []int64{2}, upstream.calls(), "the stale route must never be sent")
				require.Equal(t, int64(1), repo.initialReads[0])
				repo.slots.mu.Lock()
				acquired, released := repo.slots.accountAcquireCalls, repo.slots.accountReleaseCalls
				repo.slots.mu.Unlock()
				require.Equal(t, 2, acquired)
				require.Equal(t, acquired, released, "both rejected and successful attempts release their slots")
				require.NotContains(t, rec.Body.String(), "admission_unavailable")
				_, recordedUpstreamError := c.Get(service.OpsUpstreamErrorsKey)
				require.False(t, recordedUpstreamError, "a local route change is not an upstream failure")
			})
		}
	}
}

func TestOpenAIBareServerFailureSwitchesOnlyBeforeOutput(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, outcome := range []string{"server_error", "partial_output", "policy"} {
				t.Run(fmt.Sprintf("passthrough=%t/stream=%t/%s", passthrough, stream, outcome), func(t *testing.T) {
					repo := &initialAdmissionRepo{}
					upstream := &excelBPSFailoverUpstream{answer: func(call int) *http.Response {
						if call > 0 {
							return excelBPSCompleted()
						}
						body := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_failed\"}}\n\n"
						if outcome == "partial_output" && stream {
							body += "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial answer\"}\n\n"
						}
						code := "internal_server_error"
						if outcome == "policy" {
							code = "content_policy_violation"
						}
						body += fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"code\":%q,\"message\":\"siwc executor: stream ended without terminal event\"}}\n\n", code)
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
					}}
					h := newInitialAdmissionHandler(t, repo, upstream)
					for i := range repo.accounts {
						a := &repo.accounts[i]
						a.Type = service.AccountTypeAPIKey
						a.Credentials = map[string]any{"api_key": fmt.Sprintf("fixture-key-%d", a.ID), "base_url": "https://api.example.test"}
						a.Extra = map[string]any{"openai_passthrough": passthrough}
					}
					c, rec := newExcelBPSFailoverTestContext(context.Background(), stream)
					body := fmt.Sprintf(`{"model":"gpt-6-astra","input":"hello","stream":%t}`, stream)
					c.Request.Body = io.NopCloser(strings.NewReader(body))
					c.Request.ContentLength = int64(len(body))
					h.Responses(c)
					if outcome == "policy" || (outcome == "partial_output" && stream) {
						require.Equal(t, []int64{1}, upstream.calls())
						require.NotContains(t, rec.Body.String(), "resp_bps_failover")
						if stream && outcome == "partial_output" {
							require.Contains(t, rec.Body.String(), "partial answer")
						}
					} else {
						require.Equal(t, []int64{1, 2}, upstream.calls(), rec.Body.String())
						require.Equal(t, http.StatusOK, rec.Code)
						require.Contains(t, rec.Body.String(), "resp_bps_failover")
						require.NotContains(t, rec.Body.String(), "resp_failed")
						require.NotContains(t, rec.Body.String(), "internal_server_error")
					}
					repo.slots.mu.Lock()
					defer repo.slots.mu.Unlock()
					require.Equal(t, repo.slots.accountAcquireCalls, repo.slots.accountReleaseCalls)
				})
			}
		}
	}
}

func TestOpenAIInitialAdmissionReselectsDisabledAccountBeforeSending(t *testing.T) {
	for _, chat := range []bool{false, true} {
		t.Run(fmt.Sprintf("chat=%t", chat), func(t *testing.T) {
			repo := &initialAdmissionRepo{disabled: map[int64]bool{1: true}}
			upstream := &excelBPSFailoverUpstream{answer: func(int) *http.Response { return excelBPSCompleted() }}
			h := newInitialAdmissionHandler(t, repo, upstream)
			c, rec := newExcelBPSFailoverTestContext(context.Background(), false)
			if chat {
				c.Request.URL.Path = "/v1/chat/completions"
				h.ChatCompletions(c)
			} else {
				h.Responses(c)
			}
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, []int64{2}, upstream.calls(), "the disabled account must not send; another eligible account serves")
			repo.slots.mu.Lock()
			defer repo.slots.mu.Unlock()
			require.Equal(t, 2, repo.slots.accountAcquireCalls)
			require.Equal(t, repo.slots.accountAcquireCalls, repo.slots.accountReleaseCalls)
		})
	}
}

func TestPrismSharedKeyAdmissionReselectsBeforeSending(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var adapterCalls atomic.Int32
	adapter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		adapterCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(adapter.Close)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			repo := &initialAdmissionRepo{}
			upstream := &excelBPSFailoverUpstream{answer: func(int) *http.Response {
				resp := excelBPSCompleted()
				wire, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				resp.Body = io.NopCloser(strings.NewReader(strings.ReplaceAll(string(wire), "gpt-6-astra", "gpt-6.1-sol")))
				return resp
			}}
			h := newInitialAdmissionHandler(t, repo, upstream)
			repo.accounts[0].Extra = map[string]any{"openai_prism_browser": true}
			repo.accounts[1].Extra = map[string]any{"openai_passthrough": true}
			h.cfg.Gateway.PrismBrowser = config.GatewayPrismBrowserConfig{Enabled: true, BaseURL: adapter.URL + "/v1", APIKey: "fixture"}
			h.cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{config.UpstreamKeyFingerprint("fixture-supplier"): 1}}
			c, rec := newExcelBPSFailoverTestContext(context.Background(), stream)
			body := fmt.Sprintf(`{"model":"gpt-6.1-sol","stream":%t,"input":"hello"}`, stream)
			c.Request.Body = io.NopCloser(strings.NewReader(body))
			c.Request.ContentLength = int64(len(body))
			h.Responses(c)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "done")
			require.Equal(t, []int64{2}, upstream.calls(), "only the second account may send")
			require.Zero(t, adapterCalls.Load())
			require.NotContains(t, rec.Body.String(), "admission_unavailable")
			_, recorded := c.Get(service.OpsUpstreamErrorsKey)
			require.False(t, recorded, "local admission is not an upstream failure")
		})
	}
}

func TestOpenAIResponses_HTTPContinuationScopeRejectsAndRecovers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &initialAdmissionRepo{cache: testutil.NewRedisGatewayCache(t)}
	upstream := &excelBPSFailoverUpstream{answer: func(int) *http.Response { return excelBPSCompleted() }}
	h := newInitialAdmissionHandler(t, repo, upstream)
	repo.accounts = repo.accounts[:1]
	account := &repo.accounts[0]
	account.Type = service.AccountTypeAPIKey
	account.Credentials = map[string]any{"api_key": "creation-fixture", "base_url": "https://supplier.invalid/v1"}
	account.Extra = map[string]any{}
	origin := service.OpenAIResponseOwner{UserID: 100, APIKeyID: 99, Scope: service.OpenAIWSContinuationScope{
		AccountID: account.ID, RouteFingerprint: service.AccountRouteFingerprint(account), Model: "gpt-6-astra"}}
	require.NoError(t, h.gatewayService.BindOpenAIResponseOwner(t.Context(), 3131, "resp_http_scope", origin))
	require.NoError(t, service.NewOpenAIWSStateStore(repo.cache).BindResponseAccount(t.Context(), 3131, "resp_http_scope", account.ID, time.Minute))
	for _, changed := range []bool{true, false} {
		account.Credentials["api_key"] = "creation-fixture"
		if changed {
			account.Credentials["api_key"] = "replacement-fixture"
		}
		c, rec := newExcelBPSFailoverTestContext(t.Context(), false)
		body := `{"model":"gpt-6-astra","stream":false,"previous_response_id":"resp_http_scope","input":"continue"}`
		c.Request.Body = io.NopCloser(strings.NewReader(body))
		c.Request.ContentLength = int64(len(body))
		h.Responses(c)
		if changed {
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "admission_unavailable")
			require.Empty(t, upstream.calls(), "scope mismatch must not send or switch upstream")
			_, recordedUpstreamError := c.Get(service.OpsUpstreamErrorsKey)
			require.False(t, recordedUpstreamError, "scope mismatch is not a provider failure")
		} else {
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "done")
			require.Equal(t, []int64{account.ID}, upstream.calls())
		}
	}
	repo.slots.mu.Lock()
	acquired, released := repo.slots.accountAcquireCalls, repo.slots.accountReleaseCalls
	repo.slots.mu.Unlock()
	require.Equal(t, 2, acquired)
	require.Equal(t, acquired, released, "denied and recovered requests must both release their slots")
}

func TestOpenAIInitialAdmissionRetryBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, chat := range []bool{false, true} {
		for _, kind := range []string{"budget", "all_changed", "database_error", "cancel", "downstream_written", "conversation"} {
			t.Run(fmt.Sprintf("chat=%t/%s", chat, kind), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				repo := &initialAdmissionRepo{changed: map[int64]bool{1: true}}
				upstream := &excelBPSFailoverUpstream{answer: func(int) *http.Response { return excelBPSCompleted() }}
				h := newInitialAdmissionHandler(t, repo, upstream)
				c, rec := newExcelBPSFailoverTestContext(ctx, false)
				switch kind {
				case "budget":
					h.maxAccountSwitches = 0
				case "all_changed":
					repo.changed[2] = true
				case "database_error":
					repo.readErr = errors.New("test database unavailable")
				case "cancel":
					repo.onRead = cancel
				case "downstream_written":
					repo.onRead = func() { c.Writer.WriteHeaderNow() }
				case "conversation":
					c.Request.Body = http.NoBody
					body := `{"model":"gpt-6-astra","input":"hello","conversation":"conv_test"}`
					c.Request.Body = io.NopCloser(strings.NewReader(body))
					c.Request.ContentLength = int64(len(body))
				}
				if chat {
					c.Request.URL.Path = "/v1/chat/completions"
					h.ChatCompletions(c)
				} else {
					h.Responses(c)
				}
				require.Empty(t, upstream.calls(), "unsafe/exhausted admission must not send")
				if kind == "all_changed" {
					require.Equal(t, []int64{1, 2}, repo.initialReads)
				} else {
					require.Equal(t, []int64{1}, repo.initialReads)
				}
				if kind != "cancel" && kind != "downstream_written" {
					require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
					require.Contains(t, rec.Body.String(), "admission_unavailable")
				}
			})
		}
	}
}

func TestOpenAIInitialAdmissionCannotReplayContinuationOrOutput(t *testing.T) {
	for _, kind := range []string{"previous_response_id", "conversation", "partial_result", "written", "canceled", "budget", "already_excluded", "later_send"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			repo := &initialAdmissionRepo{changed: map[int64]bool{1: true}}
			upstream := &excelBPSFailoverUpstream{answer: func(int) *http.Response { t.Fatal("unexpected upstream call"); return nil }}
			h := newInitialAdmissionHandler(t, repo, upstream)
			c, _ := newExcelBPSFailoverTestContext(ctx, false)
			account, err := repo.GetByID(ctx, 1)
			require.NoError(t, err)
			_, admissionErr := h.gatewayService.Forward(ctx, c, account, []byte(`{"model":"gpt-6-astra"}`))
			require.True(t, service.IsOpenAIInitialAdmissionRejection(admissionErr))
			body := []byte(`{"model":"gpt-6-astra","input":"hello"}`)
			excluded := map[int64]struct{}{}
			switches, budget := 0, 2
			var result *service.OpenAIForwardResult
			switch kind {
			case "previous_response_id":
				body = []byte(`{"previous_response_id":"resp_test"}`)
			case "conversation":
				body = []byte(`{"conversation":{"id":"conv_test"}}`)
			case "partial_result":
				result = &service.OpenAIForwardResult{}
			case "written":
				_, err = c.Writer.Write([]byte(": heartbeat\n\n"))
				require.NoError(t, err)
			case "canceled":
				cancel()
			case "budget":
				budget = 0
			case "already_excluded":
				excluded[1] = struct{}{}
			case "later_send":
				admissionErr = &service.OpenAITurnAdmissionError{Reason: "account_binding_changed"}
			}
			require.False(t, retryOpenAIInitialAdmission(c, admissionErr, result, body, 1, excluded, &switches, budget))
			require.Zero(t, switches)
			require.Empty(t, upstream.calls())
		})
	}
}
