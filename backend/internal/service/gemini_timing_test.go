//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Mirror the middleware's write observation so a parser that intentionally
// keeps collecting billable usage after a write failure cannot look healthy.
type geminiTimingWriter struct {
	gin.ResponseWriter
	collector *requesttiming.Collector
	fail      bool
	cancel    context.CancelFunc
}

func (w *geminiTimingWriter) Write(p []byte) (int, error) {
	start := time.Now()
	if w.fail {
		w.collector.Written(start, 0, io.ErrClosedPipe, false)
		return 0, io.ErrClosedPipe
	}
	n, err := w.ResponseWriter.Write(p)
	w.collector.Written(start, n, err, false)
	if w.cancel != nil && strings.Contains(string(p), "last") {
		w.cancel()
	}
	return n, err
}

func (w *geminiTimingWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func TestGeminiTimingLoopback(t *testing.T) {
	for _, path := range []string{"native", "messages", "chat", "buffered"} {
		for _, ending := range []string{"STOP", "MAX_TOKENS", "SAFETY", "error", "eof", "write_error", "malformed", "done_marker"} {
			t.Run(path+"/"+ending, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					send := func(data string) {
						_, _ = io.WriteString(w, "data: "+data+"\n\n")
						_ = http.NewResponseController(w).Flush()
					}
					send(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"private reasoning"}]}}]}}`)
					send(`{"response":{"candidates":[{"content":{"parts":[{"text":"first"}]}}]}}`)
					time.Sleep(40 * time.Millisecond)
					_, _ = io.WriteString(w, ": heartbeat\n\n")
					_ = http.NewResponseController(w).Flush()
					time.Sleep(30 * time.Millisecond)
					if ending == "malformed" {
						send(`{"response":`)
					}
					send(`{"response":{"candidates":[{"content":{"parts":[{"text":"last"}]}}]}}`)
					time.Sleep(20 * time.Millisecond)
					switch ending {
					case "eof":
					case "done_marker":
						send("[DONE]")
					case "error":
						send(`{"error":{"code":503,"status":"UNAVAILABLE","message":"synthetic failure"}}`)
					default:
						reason := ending
						if ending == "write_error" || ending == "malformed" {
							reason = "STOP"
						}
						send(`{"response":{"candidates":[{"finishReason":"` + reason + `"}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":3,"candidatesTokenCount":120,"thoughtsTokenCount":20}}}`)
					}
				}))
				defer server.Close()
				collector := requesttiming.New(time.Now(), 0)
				ctx := requesttiming.With(t.Context(), collector)
				req := httptest.NewRequest(http.MethodPost, "/v1beta/models/fixture:streamGenerateContent", nil).WithContext(ctx)
				upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1internal:streamGenerateContent?alt=sse", nil)
				require.NoError(t, err)
				svc := &GeminiMessagesCompatService{cfg: &config.Config{}, httpUpstream: messagesTimingHTTPUpstream{client: server.Client()}}
				account := &Account{ID: 702, Platform: PlatformGemini, Type: AccountTypeOAuth, Credentials: map[string]any{"project_id": "fixture"}}
				resp, err := svc.doGeminiUpstream(upstreamReq, "", account)
				require.NoError(t, err)
				defer func() { _ = resp.Body.Close() }()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = req
				c.Writer = &geminiTimingWriter{ResponseWriter: c.Writer, collector: collector, fail: ending == "write_error"}
				switch path {
				case "native":
					_, err = svc.handleNativeStreamingResponse(c, resp, time.Now(), true, account, "fixture")
				case "messages":
					_, err = svc.handleStreamingResponse(c, resp, time.Now(), "fixture")
				case "chat":
					_, err = svc.handleChatCompletionsStreamingResponseFromGemini(c, resp, time.Now(), "fixture", true, true)
				case "buffered":
					_, _, _, err = collectGeminiSSEObserved(resp.Body, true, nil, requesttiming.ResponseContext(ctx, resp))
				}
				require.NoError(t, err, "timing must preserve existing response/billing behavior")
				require.NoError(t, resp.Body.Close())
				collector.Finish(c.Writer.Status(), false)
				collector.WhenFinished(func(s requesttiming.Snapshot) {
					require.Len(t, s.Attempts, 2, "logical send and physical HTTP attempt must stay separate")
					require.Nil(t, s.Attempts[0].TextStream)
					require.NotEmpty(t, s.Attempts[1].RouteFingerprint)
					v := requesttiming.DeriveContentObservation(s)
					if ending == "write_error" && path == "chat" {
						require.NotEqual(t, "observed", v.TextStream.Status)
						require.Nil(t, v.VisibleTextFirstMS, "Chat can fail writing its initial event before any upstream parse")
						return
					}
					require.Equal(t, "typed_gemini_content", v.TextStream.Source)
					if ending == "malformed" {
						require.Equal(t, "incomplete_capture", v.TextStream.Status)
						require.Nil(t, v.VisibleTextFirstMS)
						require.Nil(t, v.TextStream.Chunks)
						return
					}
					require.NotNil(t, v.ResponseByteFirstMS)
					require.NotNil(t, v.ReasoningFirstMS)
					require.NotNil(t, v.VisibleTextFirstMS)
					require.NotNil(t, v.TextStream.Chunks)
					require.Equal(t, 2, *v.TextStream.Chunks)
					require.GreaterOrEqual(t, *v.TextStream.MaxGapMS, 50.0)
					if ending == "STOP" || ending == "write_error" && path == "buffered" {
						require.Equal(t, "observed", v.TextStream.Status)
						require.GreaterOrEqual(t, *v.TextStream.TailWaitMS, 10.0)
					} else {
						require.Equal(t, "partial", v.TextStream.Status)
					}
					require.Nil(t, v.OutputRate.TokensPerSecond, "do not coerce billing counters into canonical text TPS")
					require.Nil(t, v.Cache.ReadRatio)
					raw, marshalErr := json.Marshal(s)
					require.NoError(t, marshalErr)
					require.NotContains(t, string(raw), "private reasoning")
					require.NotContains(t, string(raw), "synthetic failure")
				})
			})
		}
	}
}

func TestGeminiTimingForwardOutcome(t *testing.T) {
	for _, path := range []string{"native", "messages", "chat"} {
		t.Run(path, func(t *testing.T) {
			collector := requesttiming.New(time.Now(), 0)
			ctx := requesttiming.With(context.Background(), collector)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/fixture", nil).WithContext(ctx)
			svc := &GeminiMessagesCompatService{cfg: &config.Config{}}
			var err error
			switch path {
			case "native":
				_, err = svc.ForwardNative(ctx, c, nil, "", "generateContent", false, nil)
			case "messages":
				_, err = svc.Forward(ctx, c, nil, []byte(`{`))
			case "chat":
				_, err = svc.ForwardAsChatCompletions(ctx, c, nil, []byte(`{`))
			}
			require.Error(t, err)
			collector.Finish(c.Writer.Status(), false)
			collector.WhenFinished(func(s requesttiming.Snapshot) { require.Equal(t, "failed", s.Outcome) })
		})
	}
}

func TestGeminiTimingPublicForward(t *testing.T) {
	for _, path := range []string{"native", "messages", "chat"} {
		for _, stream := range []bool{false, true} {
			for _, outcome := range []string{"success", "http_error", "canceled"} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", path, stream, outcome), func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						if outcome == "http_error" {
							w.WriteHeader(http.StatusServiceUnavailable)
							_, _ = io.WriteString(w, geminiRetryTest503Body)
							return
						}
						payload := `{"candidates":[{"content":{"parts":[{"thought":true,"text":"private reasoning"},{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":3,"candidatesTokenCount":120,"thoughtsTokenCount":20}}`
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: "+payload+"\n\n")
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, payload)
						}
					}))
					defer server.Close()
					collector := requesttiming.New(time.Now(), 0)
					ctx, cancel := context.WithCancel(requesttiming.With(t.Context(), collector))
					defer cancel()
					if outcome == "canceled" {
						cancel()
					}
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/fixture", nil).WithContext(ctx)
					cfg := &config.Config{}
					cfg.Security.URLAllowlist.AllowInsecureHTTP = true
					svc := &GeminiMessagesCompatService{cfg: cfg, httpUpstream: messagesTimingHTTPUpstream{client: server.Client()}}
					account := &Account{ID: 703, Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-secret", "base_url": server.URL}}
					body := []byte(fmt.Sprintf(`{"model":"gemini-fixture","stream":%v,"max_tokens":256,"messages":[{"role":"user","content":"fixture"}]}`, stream))
					var result *ForwardResult
					var err error
					switch path {
					case "native":
						action := "generateContent"
						if stream {
							action = "streamGenerateContent"
						}
						result, err = svc.ForwardNative(ctx, c, account, "gemini-fixture", action, stream, []byte(geminiTransportTestNativeBody))
					case "messages":
						result, err = svc.Forward(ctx, c, account, body)
					case "chat":
						result, err = svc.ForwardAsChatCompletions(ctx, c, account, body)
					}
					if outcome == "success" {
						require.NoError(t, err)
						require.NotNil(t, result)
						require.Equal(t, 7, result.Usage.InputTokens)
						require.Equal(t, 3, result.Usage.CacheReadInputTokens)
						require.Equal(t, 140, result.Usage.OutputTokens)
						if stream {
							require.NotNil(t, result.FirstTokenMs)
						}
					} else {
						require.Error(t, err)
					}
					collector.Finish(c.Writer.Status(), ctx.Err() != nil)
					collector.WhenFinished(func(s requesttiming.Snapshot) {
						require.Len(t, s.Attempts, 2)
						wantOutcome := "failed"
						if outcome == "success" {
							wantOutcome = "success"
						}
						require.Equal(t, wantOutcome, s.Outcome)
						v := requesttiming.DeriveContentObservation(s)
						if outcome == "success" {
							require.NotNil(t, v.ReasoningFirstMS)
							require.NotNil(t, v.VisibleTextFirstMS)
							require.Equal(t, "typed_gemini_content", v.TextStream.Source)
							if stream {
								require.Equal(t, "observed", v.TextStream.Status)
								require.Equal(t, 1, *v.TextStream.Chunks)
							} else {
								require.Nil(t, v.TextStream.Chunks)
							}
						} else {
							require.NotEqual(t, "observed", v.TextStream.Status)
							require.Nil(t, v.VisibleTextFirstMS)
						}
						require.Nil(t, v.OutputRate.TokensPerSecond)
						require.Nil(t, v.Cache.ReadRatio)
						raw, marshalErr := json.Marshal(s)
						require.NoError(t, marshalErr)
						require.NotContains(t, string(raw), "fixture-secret")
					})
				})
			}
		}
	}
}

func TestGeminiTimingSSEFraming(t *testing.T) {
	for _, tc := range []struct {
		name, wire   string
		visible, gap bool
	}{
		{"multiline", "data: {\ndata: \"candidates\":[{\"content\":{\"parts\":[{\"text\":\"answer\"}]}}]}\n\n", true, false},
		{"comments", ": heartbeat\nevent: message\ndata:{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"answer\"}]}}]}\n\n", true, false},
		{"partial eof", "data:{\"candidates\":", false, true},
		{"done marker", "data:[DONE]\n\n", false, false},
		{"foreign", "data:{\"type\":\"response.output_text.delta\",\"delta\":\"foreign\"}\n\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collector := requesttiming.New(time.Now(), 0)
			req := httptest.NewRequest(http.MethodPost, "/v1internal:streamGenerateContent", nil).WithContext(requesttiming.With(t.Context(), collector))
			req, _ = requesttiming.StartAttempt(req, 1, 0)
			req, trace := requesttiming.StartTransport(req)
			_, _, _, err := collectGeminiSSEObserved(strings.NewReader(tc.wire), false, nil, req.Context())
			require.NoError(t, err)
			trace.Response(&http.Response{StatusCode: http.StatusOK}, nil)
			collector.Finish(http.StatusOK, false)
			collector.WhenFinished(func(s requesttiming.Snapshot) {
				v := requesttiming.DeriveContentObservation(s)
				require.Equal(t, tc.visible, v.VisibleTextFirstMS != nil)
				_, gap := s.Attempts[1].Events["typed_content_truncated"]
				require.Equal(t, tc.gap, gap)
				require.Equal(t, "typed_gemini_content", v.TextStream.Source)
			})
		})
	}
}

func TestGeminiTimingCanceledAfterTerminalKeepsBilling(t *testing.T) {
	for _, path := range []string{"native", "messages", "chat"} {
		t.Run(path, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+`{"candidates":[{"content":{"parts":[{"text":"last"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":120}}`+"\n\n")
				_ = http.NewResponseController(w).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			collector := requesttiming.New(time.Now(), 0)
			ctx, cancel := context.WithTimeout(requesttiming.With(t.Context(), collector), 3*time.Second)
			defer cancel()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/fixture", nil).WithContext(ctx)
			c.Writer = &geminiTimingWriter{ResponseWriter: c.Writer, collector: collector, cancel: cancel}
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			svc := &GeminiMessagesCompatService{cfg: cfg, httpUpstream: messagesTimingHTTPUpstream{client: server.Client()}}
			account := &Account{ID: 703, Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture", "base_url": server.URL}}
			body := []byte(`{"model":"gemini-fixture","stream":true,"max_tokens":256,"messages":[{"role":"user","content":"fixture"}]}`)
			var result *ForwardResult
			var err error
			switch path {
			case "native":
				result, err = svc.ForwardNative(ctx, c, account, "gemini-fixture", "streamGenerateContent", true, []byte(geminiTransportTestNativeBody))
			case "messages":
				result, err = svc.Forward(ctx, c, account, body)
			case "chat":
				result, err = svc.ForwardAsChatCompletions(ctx, c, account, body)
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.ClientDisconnect)
			require.Equal(t, 120, result.Usage.OutputTokens)
			require.ErrorIs(t, ctx.Err(), context.Canceled, "the write must cancel the request before the fallback deadline")
			collector.Finish(c.Writer.Status(), true)
			collector.WhenFinished(func(s requesttiming.Snapshot) {
				require.Equal(t, "client_disconnected", s.Outcome)
				require.True(t, s.ClientDisconnect)
				require.Len(t, s.Attempts, 2)
				require.Equal(t, "completed", s.Attempts[1].Terminal, "a real STOP does not erase the later downstream outcome")
				require.Equal(t, "partial", requesttiming.DeriveContentObservation(s).TextStream.Status)
			})
		})
	}
}

func TestGeminiTimingDiagnosticLimit(t *testing.T) {
	collector := requesttiming.New(time.Now(), 0)
	req := httptest.NewRequest(http.MethodPost, "/v1internal:streamGenerateContent", nil).WithContext(requesttiming.With(t.Context(), collector))
	req, _ = requesttiming.StartAttempt(req, 1, 0)
	req, trace := requesttiming.StartTransport(req)
	observer := sseTimingObserver{ctx: req.Context(), protocol: "gemini"}
	observer.observeTimingLine("data:" + strings.Repeat("x", maxSSETimingBytes+1))
	observer.observeTimingLine("")
	observer.observeTimingLine(`data:{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}]}`)
	observer.resetTimingEvent()
	require.Empty(t, observer.timingData)
	trace.Response(&http.Response{StatusCode: http.StatusOK}, nil)
	collector.Finish(http.StatusOK, false)
	collector.WhenFinished(func(s requesttiming.Snapshot) {
		v := requesttiming.DeriveContentObservation(s)
		require.Equal(t, "typed_gemini_content", v.TextStream.Source)
		require.Equal(t, "incomplete_capture", v.TextStream.Status)
		require.Nil(t, v.VisibleTextFirstMS)
		require.Nil(t, v.TextStream.Chunks)
	})
}
