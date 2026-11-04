//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type availabilityAdmissionRepo struct {
	ScheduledTestPlanRepository
	reports []*AvailabilityReport
	readErr error
	reads   int
	onRead  func(context.Context, *Account)
}

func (r *availabilityAdmissionRepo) ReadAccountAvailability(ctx context.Context, account *Account) ([]*AvailabilityReport, error) {
	r.reads++
	if r.onRead != nil {
		r.onRead(ctx, account)
	}
	return r.reports, r.readErr
}

func availabilityAdmissionPass(accountID int64, model string) *AvailabilityReport {
	now := time.Now()
	finished := now.Add(-time.Second)
	until := now.Add(time.Minute)
	return &AvailabilityReport{AccountID: accountID, Configured: true, Enabled: true, EvaluatedAt: now,
		Models: []AvailabilityModelState{{ModelID: model, Status: "pass", AvailabilityState: AvailabilityState{
			Decision: &AvailabilityProbeResult{Version: 1, Status: "pass", StartedAt: finished.Add(-time.Second), FinishedAt: finished,
				Scope: &AvailabilityRequestScope{Version: 1, Protocol: "responses_http", Model: model, ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}}, ValidUntil: &until,
		}}}}
}

func TestAvailabilityAdmissionFinalHTTP(t *testing.T) {
	for _, siwc := range []bool{false, true} {
		for _, state := range []string{"pass", "fail", "stale", "unknown", "rule_paused", "rule_changed", "route_changed", "new_fault_event", "read_error", "prepared_route_changed", "wrong_account", "wrong_model", "nil_report", "missing_decision", "expires_at_send", "missing_rule_models", "missing_evaluation", "no_rules", "unconfigured", "probe"} {
			name := "http/" + state
			if siwc {
				name = "siwc/" + state
			}
			t.Run(name, func(t *testing.T) {
				account := ticketTestAccount(901)
				if siwc {
					account.Extra = map[string]any{OpenAISIWCEnabledKey: true}
				}
				report := availabilityAdmissionPass(account.ID, "gpt-6-astra")
				report.Models[0].Decision.Scope.Stream = true
				repo := &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}
				ctx := t.Context()
				allowed := false
				switch state {
				case "pass":
					allowed = true
				case "no_rules":
					repo.reports, allowed = nil, true
				case "unconfigured":
					report.Configured, report.Enabled, report.Models, allowed = false, false, nil, true
				case "probe":
					ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
					repo.readErr, allowed = errors.New("probe must not read admission state"), true
				case "fail":
					report.Models[0].Status, report.Models[0].Decision.Status = "fail", "fail"
				case "stale", "expires_at_send":
					past := report.EvaluatedAt.Add(-time.Nanosecond)
					report.Models[0].ValidUntil = &past
					if state == "stale" {
						report.Models[0].Status = "stale"
					}
				case "unknown", "rule_changed", "route_changed", "new_fault_event":
					report.Models[0].Status, report.Models[0].Reason = "unknown", state
					report.Models[0].AvailabilityState = AvailabilityState{}
				case "rule_paused":
					report.Enabled = false
				case "read_error":
					repo.readErr = errors.New("fixture database outage")
				case "prepared_route_changed":
					repo.readErr = ErrAvailabilityRouteChanged
				case "wrong_account":
					report.AccountID++
				case "wrong_model":
					report.Models[0].ModelID = "other-model"
				case "nil_report":
					repo.reports = []*AvailabilityReport{nil}
				case "missing_decision":
					report.Models[0].Decision = nil
				case "missing_rule_models":
					report.Models = nil
				case "missing_evaluation":
					report.EvaluatedAt = time.Time{}
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}}
				svc := &OpenAIGatewayService{availabilityReader: repo, httpUpstream: upstream}
				payload := `{"model":"gpt-6-astra","input":"fixture","stream":true}`
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/responses", strings.NewReader(payload))
				require.NoError(t, err)
				response, err := svc.doOpenAIUpstream(request, "", account)
				if allowed {
					require.NoError(t, err)
					require.NotNil(t, response)
					require.NoError(t, response.Body.Close())
					require.Len(t, upstream.requests, 1)
					require.Equal(t, payload, string(upstream.lastBody), "admission must preserve the exact payload")
				} else {
					require.Nil(t, response)
					var denied *OpenAITurnAdmissionError
					require.ErrorAs(t, err, &denied)
					require.Empty(t, upstream.requests, "local rejection must send zero upstream requests")
				}
				reads := 1
				if state == "probe" {
					reads = 0
				}
				require.Equal(t, reads, repo.reads)
			})
		}
	}
}

func TestAvailabilityAdmissionModelMappingAndAllMatchingRules(t *testing.T) {
	for _, scenario := range []string{"mapped", "wildcard", "oauth_normalization", "unrelated_fail", "matching_fail", "matching_paused", "matching_unknown"} {
		t.Run(scenario, func(t *testing.T) {
			account := &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"model_mapping": map[string]any{"alias": "upstream-model", "upstream-model": "must-not-map-twice"}}}
			logical, actual := "alias", "upstream-model"
			if scenario == "wildcard" {
				account.Credentials["model_mapping"] = map[string]any{"alias-*": "upstream-model"}
				logical = "alias-1"
			}
			if scenario == "oauth_normalization" {
				account.Type = AccountTypeOAuth
				account.Credentials["model_mapping"] = map[string]any{"alias": "gpt-5.4-xhigh"}
				actual = normalizeOpenAIModelForUpstream(account, "gpt-5.4-xhigh")
			}
			repo := &availabilityAdmissionRepo{reports: []*AvailabilityReport{availabilityAdmissionPass(account.ID, logical)}}
			repo.reports[0].Models[0].Decision.Scope.Model = actual
			allowed := true
			if strings.HasPrefix(scenario, "matching_") || scenario == "unrelated_fail" {
				second := availabilityAdmissionPass(account.ID, logical)
				if scenario == "unrelated_fail" {
					second.Models[0].ModelID = "unrelated-model"
				} else {
					allowed = false
				}
				second.Models[0].Status, second.Models[0].Decision.Status = "fail", "fail"
				if scenario == "matching_paused" {
					second.Enabled = false
				}
				if scenario == "matching_unknown" {
					second.Models[0].AvailabilityState = AvailabilityState{}
				}
				repo.reports = append(repo.reports, second)
			}
			svc := &OpenAIGatewayService{availabilityReader: repo}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(`{"model":"`+actual+`"}`))
			require.NoError(t, err)
			err = svc.admitAvailabilityRequest(request.Context(), request, account)
			if allowed {
				require.NoError(t, err)
			} else {
				require.True(t, IsOpenAITurnAdmissionError(err))
			}
		})
	}
}

func TestAvailabilityAdmissionRejectsUnconfirmedPayload(t *testing.T) {
	for _, payload := range []string{`{`, `{}`, `{"model":123}`, `{"model":""}`, `{"model":null}`, `[]`} {
		t.Run(payload, func(t *testing.T) {
			svc := &OpenAIGatewayService{availabilityReader: &availabilityAdmissionRepo{reports: []*AvailabilityReport{availabilityAdmissionPass(901, "target")}}}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(payload))
			require.NoError(t, err)
			require.True(t, IsOpenAITurnAdmissionError(svc.admitAvailabilityRequest(request.Context(), request, &Account{ID: 901})))
		})
	}
	for _, scenario := range []string{"no_replay", "no_body", "read_error", "get_body_error", "nil_replay", "send_body_error"} {
		t.Run(scenario, func(t *testing.T) {
			svc := &OpenAIGatewayService{availabilityReader: &availabilityAdmissionRepo{reports: []*AvailabilityReport{availabilityAdmissionPass(901, "target")}}}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(`{"model":"target"}`))
			require.NoError(t, err)
			switch scenario {
			case "no_replay":
				request.GetBody = nil
			case "no_body":
				request.Body = nil
			case "read_error":
				request.GetBody = func() (io.ReadCloser, error) { return passthroughErrReadCloser{err: io.ErrUnexpectedEOF}, nil }
			case "get_body_error":
				request.GetBody = func() (io.ReadCloser, error) { return nil, io.ErrUnexpectedEOF }
			case "nil_replay":
				request.GetBody = func() (io.ReadCloser, error) { return nil, nil }
			case "send_body_error":
				request.Body = passthroughErrReadCloser{err: io.ErrUnexpectedEOF}
			}
			require.True(t, IsOpenAITurnAdmissionError(svc.admitAvailabilityRequest(request.Context(), request, &Account{ID: 901})))
		})
	}
}

func TestAvailabilityAdmissionChecksEveryModelCandidate(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		allowed       bool
	}{
		{"single", `{"model":"target"}`, true},
		{"identical_duplicate", `{"model":"target","model":"target"}`, true},
		{"unobserved_duplicate", `{"model":"target","model":"unobserved"}`, false},
		{"case_variant", `{"model":"target","Model":"unobserved"}`, false},
		{"escaped_variant", `{"model":"target","\u006dodel":"unobserved"}`, false},
		{"null_duplicate", `{"model":"target","model":null}`, false},
		{"empty_duplicate", `{"model":"target","model":""}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &availabilityAdmissionRepo{reports: []*AvailabilityReport{availabilityAdmissionPass(901, "target")}}
			svc := &OpenAIGatewayService{availabilityReader: repo}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(tc.payload))
			require.NoError(t, err)
			err = svc.admitAvailabilityRequest(request.Context(), request, &Account{ID: 901})
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.True(t, IsOpenAITurnAdmissionError(err), "every possible request model needs valid evidence: %v", err)
			}
		})
	}
}

func TestAvailabilityAdmissionChecksActualSendBody(t *testing.T) {
	for _, tc := range []struct {
		name, sent, replay string
		allowed            bool
	}{
		{"matching", `{"model":"target"}`, `{"model":"target"}`, true},
		{"unobserved_sent", `{"model":"unobserved"}`, `{"model":"target"}`, false},
		{"unobserved_replay", `{"model":"target"}`, `{"model":"unobserved"}`, false},
		{"different_payload", `{"model":"target","input":"sent"}`, `{"model":"target","input":"replay"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := ticketTestAccount(901)
			repo := &availabilityAdmissionRepo{reports: []*AvailabilityReport{availabilityAdmissionPass(account.ID, "target")}}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}}
			svc := &OpenAIGatewayService{availabilityReader: repo, httpUpstream: upstream}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(tc.sent))
			require.NoError(t, err)
			original := &passthroughCloseTrackingReadCloser{Reader: strings.NewReader(tc.sent)}
			request.Body = original
			t.Cleanup(func() { _ = original.Close() })
			request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(tc.replay)), nil }
			response, err := svc.doOpenAIUpstream(request, "", account)
			if tc.allowed {
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Equal(t, tc.sent, string(upstream.lastBody))
				require.True(t, original.closed, "the transport must still close the original send body")
			} else {
				require.Nil(t, response)
				require.True(t, IsOpenAITurnAdmissionError(err))
				require.Empty(t, upstream.requests)
			}
		})
	}
}

type availabilityTrackedBody struct {
	io.Reader
	closeErr error
	closed   bool
}

func (b *availabilityTrackedBody) Close() error {
	b.closed = true
	return b.closeErr
}

func TestAvailabilityAdmissionClosesReplayBodyOnFailures(t *testing.T) {
	for _, scenario := range []string{"valid", "read_error", "close_error", "get_body_error_with_body", "send_read_error"} {
		t.Run(scenario, func(t *testing.T) {
			const payload = `{"model":"target"}`
			body := &availabilityTrackedBody{Reader: strings.NewReader(payload)}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(payload))
			require.NoError(t, err)
			if scenario == "read_error" {
				body.Reader = passthroughErrReadCloser{err: io.ErrUnexpectedEOF}
			}
			if scenario == "close_error" {
				body.closeErr = io.ErrUnexpectedEOF
			}
			if scenario == "send_read_error" {
				request.Body = passthroughErrReadCloser{err: io.ErrUnexpectedEOF}
			}
			request.GetBody = func() (io.ReadCloser, error) {
				if scenario == "get_body_error_with_body" {
					return body, io.ErrUnexpectedEOF
				}
				return body, nil
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}}
			svc := &OpenAIGatewayService{availabilityReader: &availabilityAdmissionRepo{reports: []*AvailabilityReport{availabilityAdmissionPass(901, "target")}}, httpUpstream: upstream}
			response, err := svc.doOpenAIUpstream(request, "", ticketTestAccount(901))
			if scenario == "valid" {
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Len(t, upstream.requests, 1)
			} else {
				require.Nil(t, response)
				require.True(t, IsOpenAITurnAdmissionError(err))
				require.Empty(t, upstream.requests)
			}
			require.True(t, body.closed, "the replay body must be closed even when its construction, read, or close fails")
		})
	}
}

type availabilityAdmissionRPMCache struct {
	*openAIRPMTestCache
	onAcquire func()
}

func (c *availabilityAdmissionRPMCache) TryAcquireRPM(ctx context.Context, id int64, limit int) (bool, int, time.Time, error) {
	c.onAcquire()
	return c.openAIRPMTestCache.TryAcquireRPM(ctx, id, limit)
}

func TestAvailabilityAdmissionAfterRPMWait(t *testing.T) {
	account := ticketTestAccount(901)
	account.Extra = map[string]any{"base_rpm": 2}
	report := availabilityAdmissionPass(account.ID, "gpt-6-astra")
	repo := &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}
	cache := &availabilityAdmissionRPMCache{openAIRPMTestCache: &openAIRPMTestCache{counts: map[int64]int{}}, onAcquire: func() {
		report.Models[0].Status, report.Models[0].Decision.Status = "fail", "fail"
	}}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}}
	svc := &OpenAIGatewayService{availabilityReader: repo, rpmCache: cache, httpUpstream: upstream}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(`{"model":"gpt-6-astra"}`))
	require.NoError(t, err)
	response, err := svc.doOpenAIUpstream(request, "", account)
	require.Nil(t, response)
	require.True(t, IsOpenAITurnAdmissionError(err))
	require.Empty(t, upstream.requests, "a new failure during admission must prevent the physical send")
}

func TestAvailabilityAdmissionRereadsOnProxyFallback(t *testing.T) {
	account := ticketTestAccount(901)
	account.Proxy = proxyForTest(100, "127.0.0.1", 1)
	account.Proxy.FallbackMode = FallbackModeDirect
	report := availabilityAdmissionPass(account.ID, "gpt-6-astra")
	repo := &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}
	repo.onRead = func(context.Context, *Account) {
		if repo.reads == 2 {
			report.Models[0].Status, report.Models[0].Decision.Status = "fail", "fail"
		}
	}
	attempts := 0
	upstream := &runtimeFallbackUpstream{do: func(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		attempts++
		trace := httptrace.ContextClientTrace(request.Context())
		trace.GetConn("127.0.0.1:1")
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}}
	svc := &OpenAIGatewayService{availabilityReader: repo, httpUpstream: upstream}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(`{"model":"gpt-6-astra"}`))
	require.NoError(t, err)
	response, err := svc.doOpenAIUpstream(request, account.Proxy.URL(), account)
	require.Nil(t, response)
	require.True(t, IsOpenAITurnAdmissionError(err))
	require.Equal(t, 2, repo.reads)
	require.Equal(t, 1, attempts, "a failed recheck must prevent the fallback transport")
}

func TestAvailabilityAdmissionConsumesReadAndParsingLatency(t *testing.T) {
	for _, stage := range []string{"read", "payload"} {
		for _, skew := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
			t.Run(stage+"/"+skew.String(), func(t *testing.T) {
				report := availabilityAdmissionPass(901, "target")
				report.EvaluatedAt = time.Now().Add(skew)
				until := report.EvaluatedAt.Add(10 * time.Millisecond)
				report.Models[0].ValidUntil = &until
				repo := &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(`{"model":"target"}`))
				require.NoError(t, err)
				if stage == "read" {
					repo.onRead = func(context.Context, *Account) { time.Sleep(20 * time.Millisecond) }
				} else {
					getBody := request.GetBody
					request.GetBody = func() (io.ReadCloser, error) {
						time.Sleep(20 * time.Millisecond)
						return getBody()
					}
				}
				svc := &OpenAIGatewayService{availabilityReader: repo}
				require.True(t, IsOpenAITurnAdmissionError(svc.admitAvailabilityRequest(request.Context(), request, &Account{ID: 901})), "processing time must consume validity despite local clock skew")
			})
		}
	}
}

func TestAvailabilityAdmissionFactoryWiring(t *testing.T) {
	repo := &availabilityAdmissionRepo{}
	svc := ProvideOpenAIGatewayService(OpenAIGatewayDependencies{ScheduledTests: repo})
	t.Cleanup(svc.StopOpenAICodexTicketHarvester)
	require.Same(t, repo, svc.availabilityReader)
}

func TestAvailabilityAdmissionRejectsUnscopedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, path, payload string
	}{
		{"responses", "/v1/responses", `{"model":"target","input":"hi"}`},
		{"chat", "/v1/chat/completions", `{"model":"target","messages":[{"role":"user","content":"hi"}]}`},
		{"image_tool", "/v1/responses", `{"model":"target","input":"draw","tools":[{"type":"image_generation"}]}`},
		{"fast", "/v1/responses", `{"model":"target","input":"hi","service_tier":"priority"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			repo := &availabilityAdmissionRepo{reports: []*AvailabilityReport{availabilityAdmissionPass(account.ID, "target")}}
			repo.reports[0].Models[0].Decision.Scope = nil
			svc := &OpenAIGatewayService{availabilityReader: repo}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid"+tc.path, strings.NewReader(tc.payload))
			require.NoError(t, err)
			err = svc.admitAvailabilityRequest(request.Context(), request, account)
			require.True(t, IsOpenAITurnAdmissionError(err), "a legacy pass without wire scope must not authorize this request")
		})
	}
}

func TestAvailabilityAdmissionHTTPPassDoesNotAuthorizeWSOrPrewarm(t *testing.T) {
	for _, prewarm := range []bool{false, true} {
		t.Run(strconv.FormatBool(prewarm), func(t *testing.T) {
			account := wsSSETestAccount()
			report := availabilityAdmissionPass(account.ID, "gpt-5.1")
			report.Models[0].Decision.Scope.Stream = true
			repo := &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}
			conn := &openAIWSCaptureConn{events: [][]byte{
				[]byte(`{"type":"response.completed","response":{"id":"prewarm","usage":{"input_tokens":1,"output_tokens":1}}}`),
				[]byte(`{"type":"response.completed","response":{"id":"business","usage":{"input_tokens":1,"output_tokens":1}}}`),
			}}
			upstream := wsSSEGuardHTTPUpstream()
			svc := wsSSETestService(t, &wsSSETestDialer{conn: conn}, upstream)
			svc.availabilityReader = repo
			svc.cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = prewarm
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			_, err := svc.Forward(t.Context(), c, account, []byte(wsSSETestRequest))
			require.True(t, IsOpenAITurnAdmissionError(err), "HTTP evidence must not authorize native WS: %v", err)
			conn.mu.Lock()
			defer conn.mu.Unlock()
			require.Empty(t, conn.writes, "local denial precedes every WS write, including prewarm")
			require.Empty(t, upstream.requests, "local admission is not a transport failure eligible for replay")
			require.Positive(t, repo.reads)
		})
	}
}
