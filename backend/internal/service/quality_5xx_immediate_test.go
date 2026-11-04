//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/qualityqueue/queuetest"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type immediateQualityAccountRepo struct {
	AccountRepository
	calls             int
	availabilityCalls int
}

func TestAvailabilityTransportFailureAtFinalSend(t *testing.T) {
	for _, path := range []string{"gateway", "gateway_tls", "gemini", "antigravity", "openai"} {
		for _, outcome := range []string{"refused", "tls", "local", "capacity", "policy", "admission", "canceled", "probe"} {
			t.Run(path+"/"+outcome, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				if outcome == "tls" {
					server.Close()
					server = httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				} else {
					server.Close()
				}
				t.Cleanup(server.Close)
				target, err := url.Parse(server.URL)
				require.NoError(t, err)
				var upstream HTTPUpstream = &accountTestHTTPUpstream{target: target}
				localErr := errors.New("local fixture failure")
				switch outcome {
				case "capacity":
					localErr = ErrUpstreamKeyCapacity
				case "policy":
					localErr = ErrUpstreamKeyPolicy
				case "admission":
					localErr = denyOpenAITurn("fixture")
				}
				if outcome == "local" || outcome == "capacity" || outcome == "policy" || outcome == "admission" {
					upstream = &httpUpstreamRecorder{err: localErr}
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if outcome == "canceled" {
					cancel()
				}
				if outcome == "probe" {
					ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", nil)
				require.NoError(t, err)
				repo := &immediateQualityAccountRepo{}
				cfg := rawChatCompletionsTestConfig()
				limiter := ProvideRateLimitService(repo, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				account := &Account{ID: 42, Type: AccountTypeAPIKey, Platform: PlatformOpenAI}
				var response *http.Response
				switch path {
				case "gateway", "gateway_tls":
					svc := &GatewayService{httpUpstream: upstream, rateLimitService: limiter, cfg: cfg}
					if path == "gateway_tls" {
						response, err = svc.doGatewayUpstream(req, "", account, nil)
					} else {
						response, err = svc.doGatewayUpstream(req, "", account)
					}
				case "gemini":
					response, err = (&GeminiMessagesCompatService{httpUpstream: upstream, rateLimitService: limiter, cfg: cfg}).doGeminiUpstream(req, "", account)
				case "antigravity":
					response, err = (&AntigravityGatewayService{rateLimitService: limiter}).doAntigravityUpstream(req, "", account, upstream)
				case "openai":
					response, err = (&OpenAIGatewayService{httpUpstream: upstream, rateLimitService: limiter, cfg: cfg}).doOpenAIProxyAttempt(req, account, runtimeProxyEgress{})
				}
				require.Nil(t, response)
				require.Error(t, err)
				if outcome == "refused" || outcome == "probe" {
					var networkErr *net.OpError
					require.ErrorAs(t, err, &networkErr, "the fixture must actually reach the network")
				}
				if outcome == "tls" {
					require.ErrorContains(t, err, "certificate")
				}
				if outcome == "local" || outcome == "capacity" || outcome == "policy" || outcome == "admission" {
					require.ErrorIs(t, err, localErr)
				}
				want := 0
				if outcome == "refused" || outcome == "tls" {
					want = 1
				}
				require.Equal(t, want, repo.availabilityCalls)
				require.Zero(t, repo.calls, "transport failure is not an observed upstream 5xx")
			})
		}
	}
}

func TestAvailabilityTransportEvidenceBoundary(t *testing.T) {
	for _, outcome := range []string{"unobserved", "pool_wait", "dns", "connect", "tls", "reused", "write", "headers", "local_capacity", "local_policy", "local_admission", "canceled_error", "canceled_context", "expired_context", "probe", "response_and_error", "success"} {
		t.Run(outcome, func(t *testing.T) {
			repo := &immediateQualityAccountRepo{}
			limiter := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if outcome == "probe" {
				ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
			}
			if outcome == "expired_context" {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
			}
			outerCalls := 0
			ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{ConnectStart: func(string, string) { outerCalls++ }})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/v1/responses", nil)
			require.NoError(t, err)
			req, finish := limiter.beginAvailabilityTransport(req, &Account{ID: 42, Type: AccountTypeAPIKey})
			trace := httptrace.ContextClientTrace(req.Context())
			failure := context.DeadlineExceeded
			want := 0
			switch outcome {
			case "unobserved":
			case "pool_wait":
				if trace.GetConn != nil {
					trace.GetConn("fixture.invalid:443")
				}
			case "dns":
				trace.DNSStart(httptrace.DNSStartInfo{})
				want = 1
			case "tls":
				trace.TLSHandshakeStart()
				want = 1
			case "reused":
				trace.GotConn(httptrace.GotConnInfo{Reused: true})
				want = 1
			case "write":
				trace.WroteRequest(httptrace.WroteRequestInfo{})
				want = 1
			case "headers":
				trace.GotFirstResponseByte()
				want = 1
			default:
				trace.ConnectStart("tcp", "fixture.invalid:443")
				require.Equal(t, 1, outerCalls, "existing trace callbacks must remain composed")
				if outcome == "connect" {
					want = 1
				}
			}
			switch outcome {
			case "local_capacity":
				failure = fmt.Errorf("fixture: %w", ErrUpstreamKeyCapacity)
			case "local_policy":
				failure = fmt.Errorf("fixture: %w", ErrUpstreamKeyPolicy)
			case "local_admission":
				failure = denyOpenAITurn("fixture")
			case "canceled_error":
				failure = context.Canceled
			case "canceled_context":
				cancel()
			case "success":
				failure = nil
			}
			var response *http.Response
			if outcome == "response_and_error" || outcome == "success" {
				response = &http.Response{StatusCode: http.StatusTemporaryRedirect}
			}
			finish(response, failure)
			finish(response, failure)
			require.Equal(t, want, repo.availabilityCalls, "each attempt may invalidate only once")
			require.Zero(t, repo.calls)
		})
	}
}

func TestAvailabilityWSHandshakeTransportFailure(t *testing.T) {
	for _, mode := range []string{"pool", "passthrough"} {
		outcomes := []string{"refused", "tls", "redirect_loop", "probe"}
		if mode == "pool" {
			outcomes = append(outcomes, "canceled")
		}
		for _, outcome := range outcomes {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				var redirectCalls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				server.Close()
				switch outcome {
				case "tls":
					server = httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				case "redirect_loop":
					server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if redirectCalls.Add(1) > 10 {
							http.Error(w, "fixture redirect budget exceeded", http.StatusLoopDetected)
							return
						}
						http.Redirect(w, r, r.URL.String(), http.StatusTemporaryRedirect)
					}))
				}
				t.Cleanup(server.Close)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if outcome == "probe" {
					ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
				}
				if outcome == "canceled" {
					cancel()
				}
				cfg := passthroughLifecycleConfig()
				repo := &immediateQualityAccountRepo{}
				limiter := ProvideRateLimitService(repo, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				svc := newPassthroughLifecycleService(cfg, newStagedPassthroughConn())
				svc.rateLimitService = limiter
				account := passthroughLifecycleAccount()
				wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
				var err error
				if mode == "pool" {
					pool := newOpenAIWSConnPool(cfg)
					t.Cleanup(pool.Close)
					_, err = pool.dialConn(ctx, openAIWSAcquireRequest{Account: account, WSURL: wsURL})
					var dialErr *openAIWSDialError
					require.ErrorAs(t, err, &dialErr)
					require.Equal(t, outcome != "canceled", dialErr.networkAttempted)
					svc.handleOpenAIWSDialTransientFailure(ctx, account, "gpt-5.1", err)
				} else {
					account.Credentials["base_url"] = server.URL
					svc.openaiWSPassthroughDialer = newDefaultOpenAIWSClientDialer()
					downstream, serverErr := startPassthroughLifecycleServer(t, ctx, svc, account)
					t.Cleanup(func() { cancel(); downstream.Close() })
					client := dialPassthroughLifecycleClientWithPayload(t, downstream, `{"type":"response.create","model":"gpt-5.1","stream":true}`)
					t.Cleanup(func() { _ = client.CloseNow() })
					select {
					case err = <-serverErr:
					case <-time.After(5 * time.Second):
						t.Fatal("passthrough did not settle after handshake failure")
					}
				}
				require.Error(t, err)
				if outcome == "redirect_loop" {
					require.ErrorContains(t, err, "stopped after 10 redirects")
					require.EqualValues(t, 10, redirectCalls.Load())
				}
				want := 0
				if outcome == "refused" || outcome == "tls" {
					want = 1
				}
				require.Equal(t, want, repo.availabilityCalls)
				require.Zero(t, repo.calls)
			})
		}
	}
}

func TestAvailabilityProtocolMissingTerminalInvalidates(t *testing.T) {
	for _, path := range []string{"responses", "passthrough", "chat", "messages", "native_messages", "anthropic", "anthropic_passthrough", "anthropic_chat", "anthropic_responses", "ws_http_bridge", "anthropic_buffered_chat", "anthropic_buffered_responses", "chat_buffered", "messages_buffered"} {
		for _, async := range []bool{false, true} {
			for _, outcome := range []string{"empty", "partial", "complete", "canceled", "probe"} {
				t.Run(fmt.Sprintf("%s/async=%t/%s", path, async, outcome), func(t *testing.T) {
					repo := &immediateQualityAccountRepo{}
					limiter := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
					cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
					if async {
						cfg.Gateway.StreamKeepaliveInterval = 60
					}
					svc := &OpenAIGatewayService{cfg: cfg, rateLimitService: limiter}
					account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					if outcome == "probe" {
						ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
					}
					payload := ""
					if outcome != "empty" {
						payload = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\",\"output_index\":0}\n\n"
					}
					if outcome == "complete" {
						payload += "data: " + controlledGatewayTerminal("target", "high") + "\n\n"
					}
					if path == "native_messages" || strings.HasPrefix(path, "anthropic") {
						payload = ""
						if outcome != "empty" {
							payload = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"model\":\"target\",\"usage\":{\"input_tokens\":1}}}\n\n"
						}
						if outcome == "complete" {
							payload += "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
						}
					}
					if outcome == "canceled" {
						cancel()
					}
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
					response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}
					var err error
					switch path {
					case "responses":
						_, err = svc.handleStreamingResponse(ctx, response, c, account, time.Now(), "target", "target")
					case "passthrough":
						_, err = svc.handleStreamingResponsePassthrough(ctx, response, c, account, time.Now(), "target", "target")
					case "chat":
						_, err = svc.handleChatStreamingResponse(response, c, account, "target", "target", "target", time.Now(), 0)
					case "messages":
						_, err = svc.handleAnthropicStreamingResponse(response, c, account, "target", "target", "target", time.Now())
					case "native_messages":
						_, err = svc.handleNativeAnthropicStreamingResponse(ctx, response, c, account, "target", "target", "target", nil, time.Now())
					case "chat_buffered":
						_, err = svc.handleChatBufferedStreamingResponse(response, c, account, "target", "target", "target", time.Now())
					case "messages_buffered":
						_, err = svc.handleAnthropicBufferedStreamingResponse(response, c, account, "target", "target", "target", time.Now())
					case "ws_http_bridge":
						svc.httpUpstream = &httpUpstreamRecorder{resp: response}
						request := []byte(`{"type":"response.create","model":"target","input":"fixture"}`)
						_, err = svc.proxyOpenAIWSHTTPBridgeTurn(ctx, c, account, "fixture", request, len(request), "target", "", "", "", "", 1, func([]byte) error { return nil })
					case "anthropic":
						gateway := &GatewayService{cfg: cfg, rateLimitService: limiter}
						_, err = gateway.handleStreamingResponse(ctx, response, c, account, time.Now(), "target", "target", false)
					case "anthropic_passthrough":
						gateway := &GatewayService{cfg: cfg, rateLimitService: limiter}
						_, err = gateway.handleStreamingResponseAnthropicAPIKeyPassthrough(ctx, response, c, account, time.Now(), "target")
					case "anthropic_chat":
						gateway := &GatewayService{cfg: cfg, rateLimitService: limiter}
						_, err = gateway.handleCCStreamingFromAnthropic(response, c, account, "target", "target", nil, time.Now())
					case "anthropic_responses":
						gateway := &GatewayService{cfg: cfg, rateLimitService: limiter}
						_, err = gateway.handleResponsesStreamingResponse(response, c, account, "target", "target", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
					case "anthropic_buffered_chat":
						gateway := &GatewayService{cfg: cfg, rateLimitService: limiter}
						_, err = gateway.handleCCBufferedFromAnthropic(response, c, account, "target", "target", nil, time.Now())
					case "anthropic_buffered_responses":
						gateway := &GatewayService{cfg: cfg, rateLimitService: limiter}
						_, err = gateway.handleResponsesBufferedStreamingResponse(response, c, account, "target", "target", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
					}
					require.NoError(t, response.Body.Close())
					want := 0
					if outcome == "empty" || outcome == "partial" {
						// Legacy Anthropic converters treat an empty preamble as
						// unobserved, not an incomplete started message.
						if outcome == "empty" && (path == "anthropic_chat" || path == "anthropic_responses") {
							require.Zero(t, repo.availabilityCalls)
							return
						}
						want = 1
						require.Error(t, err)
					}
					if outcome == "complete" {
						require.NoError(t, err)
					}
					require.Equal(t, want, repo.availabilityCalls, "only protocol-confirmed incomplete responses should invalidate")
					require.Zero(t, repo.calls, "protocol incompleteness must not fabricate a quality 5xx")
				})
			}
		}
	}
}

func TestAvailabilityProtocolDisconnectExclusions(t *testing.T) {
	repo := &immediateQualityAccountRepo{}
	limiter := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	limiter.observeAvailabilityIncomplete(t.Context(), &Account{ID: 42, Type: AccountTypeAPIKey}, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	limiter.observeAvailabilityIncomplete(ctx, &Account{ID: 42, Type: AccountTypeAPIKey}, false)
	require.Zero(t, repo.availabilityCalls)
}

func TestAvailabilityAnthropicBufferedFailureNeverPublishesSuccess(t *testing.T) {
	start := "event:message_start\ndata:{\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"model\":\"target\",\"content\":[],\"usage\":{\"input_tokens\":3}}}\n\n"
	partial := start + "event:content_block_start\ndata:{\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"partial-secret\"}}\n\n"
	stop := "event:message_stop\ndata:{\"type\":\"message_stop\"}\n\n"
	for _, api := range []string{"chat", "responses"} {
		for _, tc := range []struct {
			name, payload  string
			readErr        error
			status, faults int
			wantSSE        bool
		}{
			{name: "empty", status: 502, faults: 1},
			{name: "partial EOF", payload: partial, status: 502, faults: 1},
			{name: "partial read failure", payload: partial, readErr: io.ErrUnexpectedEOF, status: 502, faults: 1},
			{name: "overload stops queued success", payload: partial + "event:error\ndata:{\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"overloaded\"}}\n\n" + stop, wantSSE: true},
			{name: "safety stops queued success", payload: partial + "event:error\ndata:{\"type\":\"error\",\"error\":{\"type\":\"safety_error\",\"message\":\"request rejected by safety policy\"}}\n\n" + stop, status: 403},
		} {
			t.Run(api+"/"+tc.name, func(t *testing.T) {
				repo := &immediateQualityAccountRepo{}
				limiter := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				gateway := &GatewayService{rateLimitService: limiter}
				account := &Account{ID: 42, Type: AccountTypeAPIKey}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(t.Context())
				var reader io.Reader = strings.NewReader(tc.payload)
				if tc.readErr != nil {
					reader = io.MultiReader(reader, iotest.ErrReader(tc.readErr))
				}
				response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(reader)}
				limiter.observeQualityResponse(t.Context(), account, response, nil)
				var result *ForwardResult
				var err error
				if api == "chat" {
					result, err = gateway.handleCCBufferedFromAnthropic(response, c, account, "target", "target", nil, time.Now())
				} else {
					result, err = gateway.handleResponsesBufferedStreamingResponse(response, c, account, "target", "target", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
				}
				require.NoError(t, response.Body.Close())
				require.Error(t, err)
				require.Nil(t, result, "a partial buffer must not become a billable success")
				if tc.readErr != nil {
					require.ErrorIs(t, err, tc.readErr)
				}
				if tc.wantSSE {
					var sseErr *sseStreamErrorEventError
					require.ErrorAs(t, err, &sseErr)
					require.False(t, c.Writer.Written(), "the existing forwarding layer must handle upstream error events")
				} else {
					require.Equal(t, tc.status, recorder.Code)
				}
				require.NotContains(t, recorder.Body.String(), "partial-secret")
				require.Equal(t, tc.faults, repo.availabilityCalls)
				require.Zero(t, repo.calls, "local buffer errors must not fabricate upstream 5xx")
			})
		}
	}
}

func TestAvailabilityAnthropicBufferedCancellationBoundsIncompleteRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body, writer := io.Pipe()
	defer func() { _ = body.Close(); _ = writer.Close() }()
	repo := &immediateQualityAccountRepo{}
	limiter := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}
	cfg := &config.Config{Gateway: config.GatewayConfig{StreamDataIntervalTimeout: 1}}
	done := make(chan error, 1)
	go func() {
		_, err := (&GatewayService{cfg: cfg, rateLimitService: limiter}).handleCCBufferedFromAnthropic(response, c, &Account{ID: 42, Type: AccountTypeAPIKey}, "target", "target", nil, time.Now())
		done <- err
	}()
	_, err := io.WriteString(writer, "event:message_start\ndata:{\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"content\":[]}}\n\n")
	require.NoError(t, err)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("buffered read did not stop after request cancellation")
	}
	require.False(t, c.Writer.Written())
	require.Zero(t, repo.availabilityCalls)
	require.Zero(t, repo.calls)
}

func TestAvailabilityOpenAIBufferedCompletionBoundary(t *testing.T) {
	for _, outcome := range []string{"done_without_terminal", "idle_timeout", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := &config.Config{Gateway: config.GatewayConfig{StreamDataIntervalTimeout: 1}}
			repo := &immediateQualityAccountRepo{}
			limiter := ProvideRateLimitService(repo, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			svc := &OpenAIGatewayService{cfg: cfg, rateLimitService: limiter}
			account := &Account{ID: 42, Type: AccountTypeAPIKey}
			body, writer := io.Pipe()
			defer func() { _ = body.Close(); _ = writer.Close() }()
			var responseBody io.ReadCloser = body
			if outcome == "done_without_terminal" {
				responseBody = io.NopCloser(strings.NewReader("data: [DONE]\n\n"))
			}
			if outcome == "canceled" {
				cancel()
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
			response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: responseBody}
			limiter.observeQualityResponse(ctx, account, response, nil)
			result, _, _, _, err := svc.readOpenAICompatBufferedTerminal(response, c, account, "fixture", "fixture")
			require.Nil(t, result)
			want := 1
			switch outcome {
			case "done_without_terminal":
				require.NoError(t, err, "the existing caller maps a missing terminal into its protocol error")
			case "idle_timeout":
				require.ErrorContains(t, err, "stream data interval timeout")
			case "canceled":
				want = 0
				require.ErrorContains(t, err, "stream data interval timeout", "client cancellation must not abort detached upstream billing reads")
			}
			require.Equal(t, want, repo.availabilityCalls)
			require.Zero(t, repo.calls)
			require.NoError(t, response.Body.Close())
		})
	}
}

func TestAvailabilityAnthropicBufferedCanceledCompleteRetainsUsage(t *testing.T) {
	for _, api := range []string{"chat", "responses"} {
		t.Run(api, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			repo := &immediateQualityAccountRepo{}
			limiter := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			gateway := &GatewayService{rateLimitService: limiter}
			account := &Account{ID: 42, Type: AccountTypeAPIKey}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
			response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(anthropicSSEWithNegativeBlockIndex()))}
			var result *ForwardResult
			var err error
			if api == "chat" {
				result, err = gateway.handleCCBufferedFromAnthropic(response, c, account, "target", "target", nil, time.Now())
			} else {
				result, err = gateway.handleResponsesBufferedStreamingResponse(response, c, account, "target", "target", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
			}
			require.NoError(t, response.Body.Close())
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, 10, result.Usage.InputTokens)
			require.Equal(t, 5, result.Usage.OutputTokens)
			require.Zero(t, repo.availabilityCalls)
			require.Zero(t, repo.calls)
		})
	}
}

func TestAvailabilityNativeWSHTTPReadInvalidates(t *testing.T) {
	for _, outcome := range []string{"empty", "partial", "complete", "probe", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			upstream := &openAIWSCaptureConn{}
			if outcome != "empty" {
				upstream.events = append(upstream.events, []byte(`{"type":"response.output_text.delta","delta":"partial","response_id":"fixture"}`))
			}
			if outcome == "complete" {
				upstream.events = append(upstream.events, []byte(`{"type":"response.completed","response":{"id":"fixture","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`))
			}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: upstream})
			t.Cleanup(pool.Close)
			repo := &immediateQualityAccountRepo{}
			limiter := ProvideRateLimitService(repo, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			svc := &OpenAIGatewayService{cfg: cfg, openaiWSPool: pool, toolCorrector: NewCodexToolCorrector(), rateLimitService: limiter}
			account := passthroughLifecycleAccount()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if outcome == "probe" {
				ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
			}
			if outcome == "canceled" {
				cancel()
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			_, err := svc.forwardOpenAIWSV2(ctx, c, account, map[string]any{"model": "gpt-5.1", "input": "fixture", "stream": true}, "", "", "fixture", OpenAIWSProtocolDecision{}, false, true, "gpt-5.1", "gpt-5.1", time.Now(), 0, "", nil)
			want := 0
			if outcome == "empty" || outcome == "partial" {
				want = 1
				require.Error(t, err)
			}
			if outcome == "complete" {
				require.NoError(t, err)
			}
			require.Equal(t, want, repo.availabilityCalls)
			require.Zero(t, repo.calls)
		})
	}
}

func TestAvailabilityNativeWSIngressReadInvalidates(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		for _, outcome := range []string{"partial", "complete", "probe", "client_disconnect", "control_cancel"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if outcome == "probe" {
					ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
				}
				cfg := passthroughLifecycleConfig()
				cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
				cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 5
				cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
				cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
				cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 5
				upstream := newStagedPassthroughConn()
				svc := newPassthroughLifecycleService(cfg, upstream)
				pool := newOpenAIWSConnPool(cfg)
				pool.setClientDialerForTest(&stagedPassthroughDialer{conn: upstream})
				t.Cleanup(pool.Close)
				svc.openaiWSPool = pool
				repo := &immediateQualityAccountRepo{}
				svc.rateLimitService = ProvideRateLimitService(repo, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				account := passthroughLifecycleAccount()
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
				server, serverErr := startPassthroughLifecycleServer(t, ctx, svc, account)
				t.Cleanup(func() { cancel(); server.Close() })
				client := dialPassthroughLifecycleClientWithPayload(t, server, `{"type":"response.create","model":"gpt-5.1","stream":true}`)
				t.Cleanup(func() { _ = client.CloseNow() })
				upstream.Send(`{"type":"response.output_text.delta","delta":"partial","response_id":"fixture"}`)
				_, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
				require.NoError(t, err, "the active turn must reach downstream before injecting a failure")
				switch outcome {
				case "complete":
					upstream.Send(`{"type":"response.completed","response":{"id":"fixture","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
					_, err = readPassthroughLifecycleFrame(t, client, 3*time.Second)
					require.NoError(t, err)
					_ = client.CloseNow()
				case "client_disconnect":
					_ = client.CloseNow()
				case "control_cancel":
					cancel()
					_, _ = readPassthroughLifecycleFrame(t, client, 3*time.Second)
				default:
					upstream.Fail(io.EOF)
					_, err = readPassthroughLifecycleFrame(t, client, 3*time.Second)
					require.Error(t, err)
				}
				select {
				case err := <-serverErr:
					switch outcome {
					case "complete":
						require.NoError(t, err)
					case "client_disconnect":
						if err != nil {
							require.True(t, isOpenAIWSClientDisconnectError(err) || errors.Is(err, errOpenAIWSUsageDrainExpired), "unexpected client disconnect result: %v", err)
						}
					default:
						require.Error(t, err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("native WS ingress did not settle after failure or cancellation")
				}
				want := 0
				if outcome == "partial" {
					want = 1
				}
				require.Equal(t, want, repo.availabilityCalls)
				require.Zero(t, repo.calls, "transport closure is not an observed upstream 5xx")
			})
		}
	}
}

func (r *immediateQualityAccountRepo) ApplyAvailabilityFault(ctx context.Context, id int64) error {
	r.availabilityCalls++
	return nil
}

func TestAvailabilityNon5xxFaultProvider(t *testing.T) {
	for _, status := range []int{401, 408, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := queuetest.NewClient(mr.Addr())
			t.Cleanup(func() { _ = rdb.Close() })
			repo := &immediateQualityAccountRepo{}
			svc := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, rdb)
			t.Cleanup(func() { _ = svc.qualityTrigger.queue.Close() })
			account := &Account{ID: 42, Type: AccountTypeAPIKey}
			svc.observeQualityResponse(t.Context(), account, &http.Response{StatusCode: status}, nil)
			require.Equal(t, 1, repo.availabilityCalls, "the observed failure must invalidate availability before returning")
			require.Zero(t, repo.calls, "a non-5xx fault must not invoke quality 5xx protection")
			require.False(t, mr.Exists(quality5xxPendingKey), "non-5xx faults use the durable due time, not a mislabeled quality event")
			svc.observeQualityResponse(t.Context(), account, &http.Response{StatusCode: status}, context.Canceled)
			svc.observeQualityStatus(context.WithValue(t.Context(), qualityProbeContextKey{}, true), account, status)
			for _, excluded := range []int{200, 400, 403, 404, 409, 422, 600} {
				svc.observeQualityStatus(t.Context(), account, excluded)
			}
			require.Equal(t, 1, repo.availabilityCalls)
		})
	}
}

func TestAvailabilityNon5xxFaultWithoutRedis(t *testing.T) {
	repo := &immediateQualityAccountRepo{}
	svc := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		for _, status := range []int{401, 408, 429} {
			svc.observeQualityStatus(t.Context(), &Account{ID: 42, Type: accountType}, status)
		}
	}
	require.Equal(t, 6, repo.availabilityCalls)
	require.Zero(t, repo.calls)
	for _, account := range []*Account{nil, {ID: 0, Type: AccountTypeAPIKey}, {ID: 42, Type: AccountTypeSetupToken}, {ID: 42, Type: AccountTypeUpstream}} {
		svc.observeQualityStatus(t.Context(), account, 429)
	}
	require.Equal(t, 6, repo.availabilityCalls)
}

func TestAvailabilityResponseBodyFaultClassification(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		err                  error
		status               int
		cancel, close, probe bool
		want                 int
	}{
		{name: "normal EOF", err: io.EOF, status: 200},
		{name: "truncated body", err: io.ErrUnexpectedEOF, status: 200, want: 1},
		{name: "read failure", err: errors.New("fixture socket read failure"), status: 200, want: 1},
		{name: "upstream read timeout", err: context.DeadlineExceeded, status: 200, want: 1},
		{name: "read canceled", err: context.Canceled, status: 200},
		{name: "local capacity", err: ErrUpstreamKeyCapacity, status: 200},
		{name: "local key policy", err: ErrUpstreamKeyPolicy, status: 200},
		{name: "local turn admission", err: denyOpenAITurn("fixture"), status: 200},
		{name: "caller canceled", err: io.ErrUnexpectedEOF, status: 200, cancel: true},
		{name: "explicit cleanup", err: io.ErrUnexpectedEOF, status: 200, close: true},
		{name: "probe cannot recurse", err: io.ErrUnexpectedEOF, status: 200, probe: true},
		{name: "401 not counted twice", err: io.ErrUnexpectedEOF, status: 401, want: 1},
		{name: "policy response", err: io.ErrUnexpectedEOF, status: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &immediateQualityAccountRepo{}
			svc := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.probe {
				ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
			}
			response := &http.Response{StatusCode: tc.status, Body: io.NopCloser(iotest.ErrReader(tc.err))}
			svc.observeQualityResponse(ctx, &Account{ID: 42, Type: AccountTypeAPIKey}, response, nil)
			if tc.cancel {
				cancel()
			}
			if tc.close {
				require.NoError(t, response.Body.Close())
			}
			for range 2 {
				_, err := response.Body.Read(make([]byte, 1))
				require.ErrorIs(t, err, tc.err, "the observer must preserve the read error")
			}
			require.NoError(t, response.Body.Close())
			require.Equal(t, tc.want, repo.availabilityCalls, "one response must invalidate at most once")
			require.Zero(t, repo.calls)
		})
	}
}

func TestAvailabilityResponseBodyRealHTTPTruncation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "12")
		if _, err := w.Write([]byte("partial")); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	repo := &immediateQualityAccountRepo{}
	svc := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	svc.observeQualityResponse(t.Context(), &Account{ID: 42, Type: AccountTypeAPIKey}, response, nil)
	require.Zero(t, repo.availabilityCalls, "successful headers alone must not report a fault")
	content, err := io.ReadAll(response.Body)
	require.Equal(t, "partial", string(content))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, 1, repo.availabilityCalls, "an actual incomplete HTTP body must invalidate before the read returns")
	require.NoError(t, response.Body.Close())
	require.Equal(t, 1, repo.availabilityCalls)
}

func TestAvailabilitySIWCFaultObservation(t *testing.T) {
	for _, status := range []int{401, 408, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			repo := &immediateQualityAccountRepo{}
			limiter := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			account := ticketTestAccount(901)
			account.Extra = map[string]any{OpenAISIWCEnabledKey: true}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: make(http.Header), Body: http.NoBody}}
			svc := &OpenAIGatewayService{rateLimitService: limiter, httpUpstream: upstream}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.openai.com/v1/responses", strings.NewReader(`{"model":"target","input":"fixture"}`))
			require.NoError(t, err)
			response, err := svc.doOpenAIUpstream(request, "", account)
			require.NoError(t, err)
			require.Equal(t, status, response.StatusCode)
			require.NoError(t, response.Body.Close())
			require.Len(t, upstream.requests, 1)
			if status == 503 {
				require.Equal(t, 1, repo.calls)
				require.Zero(t, repo.availabilityCalls)
			} else {
				require.Equal(t, 1, repo.availabilityCalls)
				require.Zero(t, repo.calls)
			}
		})
	}
}

func (r *immediateQualityAccountRepo) ApplyQuality5xx(ctx context.Context, id int64) error {
	r.calls++
	return nil
}
func TestQuality5xxProductionProviderAppliesBeforeReturning(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := queuetest.NewClient(mr.Addr())
	defer func() { _ = rdb.Close() }()
	repo := &immediateQualityAccountRepo{}
	svc := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, rdb)
	require.NotNil(t, svc.qualityTrigger)
	defer func() { _ = svc.qualityTrigger.queue.Close() }()
	svc.observeQualityStatus(context.Background(), &Account{ID: 12, Type: AccountTypeOAuth}, 503)
	require.Equal(t, 1, repo.calls, "protection must finish before observer returns")
	members, err := mr.ZMembers(quality5xxPendingKey)
	require.NoError(t, err)
	require.NotEmpty(t, members)
	svc.observeQualityStatus(context.WithValue(context.Background(), qualityProbeContextKey{}, true), &Account{ID: 12, Type: AccountTypeOAuth}, 503)
	require.Equal(t, 1, repo.calls, "probes must not recursively start episodes")
	svc.observeQualityStatus(context.Background(), &Account{ID: 12, Type: AccountTypeOAuth}, 429)
	require.Equal(t, 1, repo.calls)
	svc.observeQualityStatus(context.Background(), &Account{ID: 43, Type: AccountTypeAPIKey}, 503)
	require.Equal(t, 2, repo.calls, "API Key episodes must be fenced before the observer returns")
	members, err = mr.ZMembers(quality5xxPendingKey)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"12", "43"}, members)
}
func TestQuality5xxPendingRemoveModelsNotCoveredByOlderFinish(t *testing.T) {
	p := pelicanPlan()
	now := time.Now()
	finished := now.Add(time.Second)
	p.LastRunAt = &finished
	p.PelicanConfig.Quality = &QualityPolicy{TriggerOnUpstream5xx: true, Action: QualityActionRemoveModel, RemoveModels: []string{"target"}}
	require.False(t, qualitySignalCovered(p, now), "old probe finishing after new signal must not acknowledge it")
}

func TestQuality5xxImmediateProtectionWithoutQueue(t *testing.T) {
	repo := &immediateQualityAccountRepo{}
	svc := ProvideRateLimitService(repo, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	require.NotNil(t, svc.qualityTrigger)
	trigger := svc.qualityTrigger
	account := &Account{ID: 42, Type: AccountTypeAPIKey}
	trigger.Observe(t.Context(), account, 503)
	require.Equal(t, 1, repo.calls, "missing Redis must not disable the durable fault fence")
	trigger.Observe(context.WithValue(t.Context(), qualityProbeContextKey{}, true), account, 503)
	trigger.Observe(t.Context(), account, 429)
	require.Equal(t, 1, repo.calls)
}

func TestQuality5xxWorkerProviderHasQueue(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := queuetest.NewClient(mr.Addr())
	defer func() { _ = rdb.Close() }()
	// Exercise provider wiring without starting singleton cron jobs with nil
	// repositories. Such jobs can outlive this test and panic on a later tick.
	cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}}
	svc := ProvideScheduledTestRunnerService(nil, nil, &AccountTestService{}, &RateLimitService{}, cfg, &QualityJudgeService{}, rdb, nil, &ChannelMonitorV2Service{})
	t.Cleanup(svc.Stop)
	require.NotNil(t, svc.qualityTrigger, "worker provider must connect the cross-instance queue")
	require.Nil(t, svc.cron)
	require.Nil(t, svc.triggerCancel)
}
