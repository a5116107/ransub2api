//go:build unit

package service

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type messagesTimingHTTPUpstream struct{ client *http.Client }

func (u messagesTimingHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	req, tr := requesttiming.StartTransport(req)
	resp, err := u.client.Do(req)
	tr.Response(resp, err)
	return resp, err
}

func TestMessagesTimingForwardOutcome(t *testing.T) {
	for _, ending := range []string{"completed", "eof", "disconnected", "invalid_request"} {
		t.Run(ending, func(t *testing.T) {
			collector := requesttiming.New(time.Now(), 0)
			body := []byte(`{"model":"claude-fixture","stream":true,"messages":[{"role":"user","content":"fixture"}]}`)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body))).WithContext(requesttiming.With(t.Context(), collector))
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = req
			if ending == "disconnected" {
				c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer}
			}
			wire := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"fixture\"}}\n\n" +
				"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":120}}\n\n"
			if ending != "eof" {
				wire += "data: {\"type\":\"message_stop\"}\n\n"
			}
			svc := newMinimalGatewayService()
			svc.httpUpstream = &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}}
			account := newAnthropicAPIKeyAccountForTest()
			parsed := &ParsedRequest{Body: NewRequestBodyRef(body), Model: "claude-fixture", Stream: true}
			if ending == "invalid_request" {
				parsed = nil
			}
			result, err := svc.Forward(req.Context(), c, account, parsed)
			if ending == "eof" || ending == "invalid_request" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 120, result.Usage.OutputTokens)
			}
			collector.Finish(c.Writer.Status(), false)
			collector.WhenFinished(func(s requesttiming.Snapshot) {
				want := "success"
				switch ending {
				case "eof", "invalid_request":
					want = "failed"
				case "disconnected":
					want = "client_disconnected"
				}
				require.Equal(t, want, s.Outcome, "the production Forward boundary owns outcome; the test must not inject it")
				require.Equal(t, ending == "disconnected", s.ClientDisconnect)
			})
		})
	}
}

func (u messagesTimingHTTPUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func TestMessagesTimingLoopback(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, ending := range []string{"completed", "error", "eof", "write_error", "terminal_only", "event_fallback", "capture_gap", "done_marker"} {
			name := "native/"
			if passthrough {
				name = "passthrough/"
			}
			t.Run(name+ending, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					send := func(event, data string) {
						if event != "" {
							_, _ = io.WriteString(w, "event: "+event+"\n")
						}
						_, _ = io.WriteString(w, "data: "+data+"\n\n")
						_ = http.NewResponseController(w).Flush()
					}
					send("message_start", `{"type":"message_start","message":{"model":"fixture","usage":{"input_tokens":10,"cache_read_input_tokens":30,"cache_creation_input_tokens":20}}}`)
					if ending != "terminal_only" {
						send("content_block_delta", `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"private reasoning"}}`)
						send("content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"first"}}`)
						time.Sleep(40 * time.Millisecond)
						send("ping", `{"type":"ping"}`)
						time.Sleep(30 * time.Millisecond)
						if ending == "capture_gap" {
							send("content_block_delta", `{"delta":`)
						}
						switch ending {
						case "event_fallback":
							send("content_block_delta", `{"delta":{"type":"text_delta","text":"last"}}`)
						default:
							send("content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"last"}}`)
						}
						time.Sleep(20 * time.Millisecond)
					}
					send("message_delta", `{"type":"message_delta","usage":{"output_tokens":120}}`)
					switch ending {
					case "error":
						send("error", `{"type":"error","error":{"type":"overloaded_error","message":"synthetic"}}`)
					case "eof":
					case "done_marker":
						_, _ = io.WriteString(w, "data: [DONE]\n\n")
					default:
						send("message_stop", `{"type":"message_stop"}`)
					}
				}))
				defer server.Close()
				collector := requesttiming.New(time.Now(), 0)
				ctx := requesttiming.With(t.Context(), collector)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{"model":"fixture","stream":true}`))
				require.NoError(t, err)
				svc := newMinimalGatewayService()
				svc.httpUpstream = messagesTimingHTTPUpstream{client: server.Client()}
				account := &Account{ID: 701, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
				resp, err := svc.doGatewayUpstream(req, "", account)
				require.NoError(t, err)
				defer func() { _ = resp.Body.Close() }()
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				// The caller has no attempt handle; the parser must recover the
				// actual physical attempt from the upstream response.
				c.Request = req
				if ending == "write_error" {
					c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer}
				}
				var result *streamingResult
				if passthrough {
					result, err = svc.handleStreamingResponseAnthropicAPIKeyPassthrough(ctx, resp, c, account, time.Now(), "fixture")
				} else {
					result, err = svc.handleStreamingResponse(ctx, resp, c, account, time.Now(), "fixture", "fixture", false)
				}
				if ending == "error" || ending == "eof" || (ending == "write_error" && !passthrough) {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				outcome := "success"
				if err != nil {
					outcome = "error"
				}
				requesttiming.Outcome(ctx, outcome, result != nil && result.clientDisconnect)
				require.NoError(t, resp.Body.Close())
				collector.Finish(c.Writer.Status(), false)
				collector.WhenFinished(func(s requesttiming.Snapshot) {
					require.Len(t, s.Attempts, 2, "one logical send and one physical HTTP attempt")
					require.Equal(t, "http", s.Attempts[1].Kind)
					require.EqualValues(t, 701, s.Attempts[1].AccountID)
					require.NotEmpty(t, s.Attempts[1].RouteFingerprint)
					require.Nil(t, s.Attempts[0].TextStream, "the parent never duplicates physical text counters")
					v := requesttiming.DeriveContentObservation(s)
					switch ending {
					case "write_error":
						require.NotEqual(t, "observed", v.TextStream.Status)
					case "terminal_only":
						require.Equal(t, "not_collected", v.TextStream.Status)
						require.Nil(t, v.VisibleTextFirstMS)
					case "capture_gap":
						require.Equal(t, "incomplete_capture", v.TextStream.Status)
						require.Nil(t, v.VisibleTextFirstMS)
						require.Nil(t, v.TextStream.MaxGapMS)
					default:
						require.NotNil(t, v.ResponseByteFirstMS)
						require.NotNil(t, v.ReasoningFirstMS)
						require.NotNil(t, v.VisibleTextFirstMS)
						require.Equal(t, "typed_messages_sse", v.TextStream.Source)
						require.Equal(t, 2, *v.TextStream.Chunks)
						require.GreaterOrEqual(t, *v.TextStream.MaxGapMS, 50.0)
						if ending == "error" || ending == "eof" || ending == "done_marker" {
							require.Equal(t, "partial", v.TextStream.Status)
						} else {
							require.Equal(t, "observed", v.TextStream.Status)
							require.GreaterOrEqual(t, *v.TextStream.TailWaitMS, 10.0)
						}
					}
					require.Nil(t, v.OutputRate.TokensPerSecond)
					require.Nil(t, v.Cache.ReadRatio)
					raw, marshalErr := json.Marshal(s)
					require.NoError(t, marshalErr)
					require.NotContains(t, string(raw), "private reasoning")
				})
				if ending == "completed" || ending == "terminal_only" {
					require.Equal(t, 10, result.usage.InputTokens)
					require.Equal(t, 120, result.usage.OutputTokens)
					require.Equal(t, 30, result.usage.CacheReadInputTokens)
					require.Equal(t, 20, result.usage.CacheCreationInputTokens)
					require.NotNil(t, result.firstTokenMs)
					require.Contains(t, recorder.Body.String(), `"type":"message_stop"`)
				}
			})
		}
	}
}

func TestMessagesTimingSSEFraming(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		visible    bool
		gap        bool
	}{
		{"multiline", "event: content_block_delta\ndata: {\ndata: \"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n", true, false},
		{"comments", "event:content_block_delta\n: ping\nid: fixture\ndata:{\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n", true, false},
		{"payload precedence", "event:content_block_delta\ndata:{\"type\":\"ping\",\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n", false, false},
		{"reset event", "event:content_block_delta\n\ndata:{\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n", false, false},
		{"crlf", "event:content_block_delta\r\ndata:{\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\r\n\r\n", true, false},
		{"malformed", "event:content_block_delta\ndata:{\"delta\":\n\n", false, true},
		{"discarded before next event", "event:content_block_delta\ndata:{\"delta\":\nevent: ping\ndata:{\"type\":\"ping\"}\n\n", false, true},
		{"partial eof", "event:content_block_delta\ndata:{\"delta\":", false, true},
		{"done marker", "data:[DONE]\n\n", false, false},
		{"foreign Responses event", "data:{\"type\":\"response.output_text.delta\",\"delta\":\"foreign\"}\n\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collector := requesttiming.New(time.Now(), 0)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(requesttiming.With(t.Context(), collector))
			req, _ = requesttiming.StartAttempt(req, 1, 0)
			req, trace := requesttiming.StartTransport(req)
			observer := sseTimingObserver{ctx: req.Context(), protocol: "messages"}
			scanner := bufio.NewScanner(strings.NewReader(tc.wire))
			for scanner.Scan() {
				observer.observeTimingLine(scanner.Text())
			}
			require.NoError(t, scanner.Err())
			observer.resetTimingEvent()
			trace.Response(&http.Response{StatusCode: http.StatusOK}, nil)
			collector.Finish(http.StatusOK, false)
			collector.WhenFinished(func(s requesttiming.Snapshot) {
				v := requesttiming.DeriveContentObservation(s)
				require.Equal(t, tc.visible, v.VisibleTextFirstMS != nil)
				_, gap := s.Attempts[1].Events["typed_content_truncated"]
				require.Equal(t, tc.gap, gap)
				if tc.gap {
					require.Equal(t, "typed_messages_sse", v.TextStream.Source, "a malformed Messages stream must not be labelled as Responses")
					require.Equal(t, "final_physical_http_messages_text_deltas_at_parser", v.TextStream.Scope)
				}
			})
		})
	}
}

func TestMessagesTimingDiagnosticLimit(t *testing.T) {
	collector := requesttiming.New(time.Now(), 0)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(requesttiming.With(t.Context(), collector))
	req, _ = requesttiming.StartAttempt(req, 1, 0)
	req, trace := requesttiming.StartTransport(req)
	observer := sseTimingObserver{ctx: req.Context(), protocol: "messages"}
	observer.observeTimingLine("data: " + strings.Repeat("x", maxSSETimingBytes+1))
	observer.observeTimingLine(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"discarded"}}`)
	observer.observeTimingLine("event: message_stop")
	observer.observeTimingLine(`data: {"type":"message_stop"}`)
	require.Empty(t, observer.timingData)
	trace.Response(&http.Response{StatusCode: http.StatusOK}, nil)
	collector.Finish(http.StatusOK, false)
	collector.WhenFinished(func(s requesttiming.Snapshot) {
		v := requesttiming.DeriveContentObservation(s)
		require.Equal(t, "incomplete_capture", v.TextStream.Status)
		require.Equal(t, "typed_messages_sse", v.TextStream.Source)
		require.Nil(t, v.VisibleTextFirstMS)
		require.Nil(t, v.TextStream.Chunks)
	})
}
