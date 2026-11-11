//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Preserve the real HTTP client and timing transport, while keeping every
// native OAuth endpoint and retry inside the local fixture server.
type antigravityTimingHTTPUpstream struct {
	client *http.Client
	target *url.URL
}

func antigravityTimingWire(path string, complete bool) string {
	if path == "upstream" {
		wire := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8}}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"last\"}}\n\n" +
			"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n"
		if complete {
			wire += "data: {\"type\":\"message_stop\"}\n\n"
		}
		return wire
	}
	wire := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"last\"}]}}]}}\n\n"
	if complete {
		wire += "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":3}}}\n\n"
	}
	return wire
}

func antigravityTimingConsume(s *AntigravityGatewayService, c *gin.Context, resp *http.Response, path string) (*antigravityStreamResult, error) {
	switch path {
	case "gemini":
		return s.handleGeminiStreamingResponse(c, resp, time.Now())
	case "gemini_json":
		return s.handleGeminiStreamToNonStreaming(c, resp, time.Now())
	case "messages":
		return s.handleClaudeStreamingResponse(c, resp, time.Now(), "fixture")
	case "messages_json":
		return s.handleClaudeStreamToNonStreaming(c, resp, time.Now(), "fixture")
	case "chat":
		return s.handleChatCompletionsStreamingFromAntigravity(c, resp, time.Now(), "fixture", true)
	case "chat_json":
		return s.handleChatCompletionsNonStreamingFromAntigravity(c, resp, time.Now(), "fixture")
	case "responses":
		return s.handleResponsesStreamingFromAntigravity(c, resp, time.Now(), "fixture")
	case "responses_json":
		return s.handleResponsesNonStreamingFromAntigravity(c, resp, time.Now(), "fixture")
	default:
		return s.streamUpstreamResponse(c, resp, time.Now()), nil
	}
}

func TestAntigravityTimingStandaloneParsers(t *testing.T) {
	for _, path := range []string{"gemini", "gemini_json", "messages", "messages_json", "chat", "chat_json", "responses", "responses_json", "upstream"} {
		t.Run(path, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(antigravityTimingWire(path, true)))}
			svc := newAntigravityTestService(&config.Config{})
			result, err := antigravityTimingConsume(svc, c, resp, path)
			require.NoError(t, err)
			require.Equal(t, 8, result.usage.InputTokens)
			require.Equal(t, 3, result.usage.OutputTokens)
		})
	}
}

func TestAntigravityTimingFinalAttemptIsolation(t *testing.T) {
	for _, path := range []string{"gemini", "gemini_json", "messages", "messages_json", "chat", "chat_json", "responses", "responses_json", "upstream"} {
		for _, finalComplete := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/final_complete=%v", path, finalComplete), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, antigravityTimingWire(path, r.URL.Query().Get("complete") == "true"))
				}))
				defer server.Close()
				collector := requesttiming.New(time.Now(), 0)
				ctx := requesttiming.With(t.Context(), collector)
				svc := newAntigravityTestService(&config.Config{})
				account := newAntigravityCompatAccount(AccountTypeOAuth)
				for _, complete := range []bool{!finalComplete, finalComplete} {
					req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/v1internal:streamGenerateContent?complete=%v", server.URL, complete), nil)
					require.NoError(t, err)
					resp, err := svc.doAntigravityUpstream(req, "", account, messagesTimingHTTPUpstream{client: server.Client()})
					require.NoError(t, err)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					// Recover the actual attempt from the response even when a
					// standalone consumer has no inbound HTTP request.
					_, err = antigravityTimingConsume(svc, c, resp, path)
					require.NoError(t, err)
					require.NoError(t, resp.Body.Close())
				}
				collector.Finish(http.StatusOK, false)
				collector.WhenFinished(func(s requesttiming.Snapshot) {
					require.Len(t, s.Attempts, 4)
					require.Nil(t, s.Attempts[0].TextStream)
					require.Nil(t, s.Attempts[2].TextStream)
					v := requesttiming.DeriveContentObservation(s)
					require.NotNil(t, v.TextStream.Chunks)
					require.Equal(t, 1, *v.TextStream.Chunks, "earlier attempts never contribute deltas")
					want := "partial"
					if finalComplete {
						want = "observed"
					}
					require.Equal(t, want, v.TextStream.Status)
				})
			})
		}
	}
}

func TestAntigravityTimingTimeoutAfterTerminal(t *testing.T) {
	for _, path := range []string{"gemini", "gemini_json", "messages", "messages_json", "chat", "chat_json", "responses", "responses_json", "upstream"} {
		t.Run(path, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, antigravityTimingWire(path, true))
				_ = http.NewResponseController(w).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			collector := requesttiming.New(time.Now(), 0)
			ctx, cancel := context.WithTimeout(requesttiming.With(t.Context(), collector), 6*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1internal:streamGenerateContent", nil)
			require.NoError(t, err)
			svc := newAntigravityTestService(&config.Config{Gateway: config.GatewayConfig{StreamDataIntervalTimeout: 1}})
			resp, err := svc.doAntigravityUpstream(req, "", newAntigravityCompatAccount(AccountTypeOAuth), messagesTimingHTTPUpstream{client: server.Client()})
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = req
			_, _ = antigravityTimingConsume(svc, c, resp, path)
			require.NoError(t, ctx.Err(), "the parser interval timeout must win over the outer deadline")
			require.NoError(t, resp.Body.Close())
			collector.Finish(c.Writer.Status(), false)
			collector.WhenFinished(func(s requesttiming.Snapshot) {
				require.Len(t, s.Attempts, 2)
				require.Equal(t, "completed", s.Attempts[1].Terminal)
				v := requesttiming.DeriveContentObservation(s)
				require.Equal(t, "incomplete_capture", v.TextStream.Status, "an earlier STOP never hides a later parser timeout")
				require.Nil(t, v.VisibleTextFirstMS)
			})
		})
	}
}

func TestAntigravityTimingCanceledAfterTerminalKeepsBilling(t *testing.T) {
	for _, path := range []string{"gemini", "messages", "chat", "responses", "upstream"} {
		t.Run(path, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				wire := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"last"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}}` + "\n\n"
				if path == "upstream" {
					wire = strings.ReplaceAll(antigravityTimingWire(path, true), `"text":"last"`, `"text":"first"`)
					wire = strings.ReplaceAll(wire, `"type":"message_stop"`, `"type":"message_stop","marker":"last"`)
				}
				_, _ = io.WriteString(w, wire)
				_ = http.NewResponseController(w).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			collector := requesttiming.New(time.Now(), 0)
			ctx, cancel := context.WithTimeout(requesttiming.With(t.Context(), collector), 6*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1internal:streamGenerateContent", nil)
			require.NoError(t, err)
			svc := newAntigravityTestService(&config.Config{})
			resp, err := svc.doAntigravityUpstream(req, "", newAntigravityCompatAccount(AccountTypeOAuth), messagesTimingHTTPUpstream{client: server.Client()})
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = req
			c.Writer = &geminiTimingWriter{ResponseWriter: c.Writer, collector: collector, cancel: cancel}
			result, err := antigravityTimingConsume(svc, c, resp, path)
			require.NoError(t, err)
			require.ErrorIs(t, ctx.Err(), context.Canceled, "a downstream write must cancel after the raw terminal is parsed")
			require.True(t, result.clientDisconnect)
			require.Equal(t, 8, result.usage.InputTokens)
			require.Equal(t, 3, result.usage.OutputTokens)
			require.NoError(t, resp.Body.Close())
			collector.Finish(c.Writer.Status(), true)
			collector.WhenFinished(func(s requesttiming.Snapshot) {
				require.Len(t, s.Attempts, 2)
				require.Equal(t, "completed", s.Attempts[1].Terminal)
				require.NotEqual(t, "observed", requesttiming.DeriveContentObservation(s).TextStream.Status)
			})
		})
	}
}

func (u antigravityTimingHTTPUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = u.target.Scheme, u.target.Host
	return (messagesTimingHTTPUpstream{client: u.client}).Do(req, proxy, id, concurrency)
}

func (u antigravityTimingHTTPUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func TestAntigravityTimingPublicForward(t *testing.T) {
	for _, path := range []string{"gemini", "messages", "chat", "responses", "upstream", "legacy_upstream"} {
		for _, stream := range []bool{false, true} {
			endings := []string{"STOP", "eof", "http_error", "canceled", "write_error", "malformed", "oversized"}
			if !stream && (path == "upstream" || path == "legacy_upstream") {
				endings = []string{"STOP", "http_error", "canceled", "write_error"}
			}
			for _, ending := range endings {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", path, stream, ending), func(t *testing.T) {
					passthrough := path == "upstream" || path == "legacy_upstream"
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						if ending == "http_error" {
							w.WriteHeader(http.StatusBadRequest)
							_, _ = io.WriteString(w, `{"error":{"message":"fixture rejected"}}`)
							return
						}
						if passthrough && !stream {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"first last"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"cache_read_input_tokens":4,"cache_creation_input_tokens":0,"output_tokens":3}}`)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						send := func(data string) {
							_, _ = io.WriteString(w, "data: "+data+"\n\n")
							_ = http.NewResponseController(w).Flush()
						}
						if passthrough {
							send(`{"type":"message_start","message":{"usage":{"input_tokens":8,"cache_read_input_tokens":4,"cache_creation_input_tokens":0}}}`)
							send(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"private reasoning"}}`)
							send(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"first"}}`)
						} else {
							send(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"private reasoning"}]}}]}}`)
							send(`{"response":{"candidates":[{"content":{"parts":[{"text":"first"}]}}]}}`)
						}
						time.Sleep(25 * time.Millisecond)
						_, _ = io.WriteString(w, ": heartbeat\n\n")
						_ = http.NewResponseController(w).Flush()
						time.Sleep(25 * time.Millisecond)
						if ending == "malformed" {
							send(`{"broken":`)
						}
						if passthrough {
							send(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"last"}}`)
							send(`{"type":"message_delta","usage":{"output_tokens":3}}`)
							if ending != "eof" {
								send(`{"type":"message_stop"}`)
							}
						} else {
							send(`{"response":{"candidates":[{"content":{"parts":[{"text":"last"}]}}]}}`)
							if ending != "eof" {
								send(`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"cachedContentTokenCount":4,"candidatesTokenCount":3}}}`)
							}
						}
						if ending == "oversized" {
							send(strings.Repeat("x", 128*1024))
						}
					}))
					defer server.Close()
					target, err := url.Parse(server.URL)
					require.NoError(t, err)
					svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: 70 * 1024}, antigravityTimingHTTPUpstream{client: server.Client(), target: target})
					account := newAntigravityCompatAccount(AccountTypeOAuth)
					account.Credentials["base_url"], account.Credentials["api_key"] = server.URL, "fixture-private-key"
					if passthrough {
						account.Type = AccountTypeUpstream
					}
					collector := requesttiming.New(time.Now(), 0)
					ctx, cancel := context.WithCancel(requesttiming.With(t.Context(), collector))
					defer cancel()
					if ending == "canceled" {
						cancel()
					}
					body := []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-5","stream":%v,"max_tokens":256,"messages":[{"role":"user","content":"fixture-prompt"}]}`, stream))
					c, _ := newAntigravityCompatContext(http.MethodPost, "/fixture", body)
					c.Request = c.Request.WithContext(ctx)
					c.Writer = &geminiTimingWriter{ResponseWriter: c.Writer, collector: collector, fail: ending == "write_error"}
					var result *ForwardResult
					switch path {
					case "gemini":
						result, err = svc.ForwardGemini(ctx, c, account, "gemini-3.1-pro-high", "generateContent", stream, []byte(`{"contents":[{"role":"user","parts":[{"text":"fixture-prompt"}]}]}`), false)
					case "messages", "legacy_upstream":
						result, err = svc.Forward(ctx, c, account, body, false)
					case "chat":
						result, err = svc.ForwardAsChatCompletions(ctx, c, account, body, nil)
					case "responses":
						result, err = svc.ForwardAsResponses(ctx, c, account, []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-5","stream":%v,"input":"fixture-prompt"}`, stream)), nil)
					case "upstream":
						result, err = svc.ForwardUpstream(ctx, c, account, body)
					}
					if ending == "STOP" || ending == "write_error" {
						require.NoError(t, err, "timing must preserve forwarding and billing")
						require.NotNil(t, result)
						require.Equal(t, 8, result.Usage.InputTokens)
						require.Equal(t, 3, result.Usage.OutputTokens)
					}
					if ending == "http_error" && passthrough {
						require.NoError(t, err, "the passthrough HTTP-error contract remains unchanged")
						require.Equal(t, http.StatusBadRequest, c.Writer.Status())
					}
					collector.Finish(c.Writer.Status(), ctx.Err() != nil)
					collector.WhenFinished(func(s requesttiming.Snapshot) {
						switch ending {
						case "http_error", "canceled":
							require.Equal(t, "failed", s.Outcome)
						case "STOP":
							require.Equal(t, "success", s.Outcome)
						}
						v := requesttiming.DeriveContentObservation(s)
						if ending == "canceled" {
							require.NotEqual(t, "observed", v.TextStream.Status)
							return
						}
						if ending == "http_error" {
							require.NotEqual(t, "observed", v.TextStream.Status)
							require.Nil(t, v.VisibleTextFirstMS)
							return
						}
						if passthrough && !stream {
							require.Nil(t, v.TextStream.Chunks, "a JSON response is not a text-delta stream")
							if ending == "STOP" {
								require.Equal(t, "observed", v.Cache.Status)
								require.Equal(t, "canonical_messages_cumulative_usage", v.Cache.Source)
								require.InDelta(t, 1.0/3, *v.Cache.ReadRatio, 1e-9)
							} else {
								require.Nil(t, v.Cache.ReadRatio)
							}
							return
						}
						require.Len(t, s.Attempts, 2, "logical and physical sends remain separate")
						require.Nil(t, s.Attempts[0].TextStream)
						require.NotEmpty(t, s.Attempts[1].RouteFingerprint)
						if ending == "malformed" || ending == "oversized" {
							require.Equal(t, "incomplete_capture", v.TextStream.Status)
							require.Nil(t, v.VisibleTextFirstMS)
						} else {
							require.NotNil(t, v.ReasoningFirstMS)
							require.NotNil(t, v.VisibleTextFirstMS)
							require.NotNil(t, v.TextStream.Chunks)
							require.Equal(t, 2, *v.TextStream.Chunks)
							require.GreaterOrEqual(t, *v.TextStream.MaxGapMS, 40.0)
							if ending == "STOP" {
								require.Equal(t, "observed", v.TextStream.Status)
							} else {
								require.Equal(t, "partial", v.TextStream.Status)
							}
						}
						source := "typed_gemini_content"
						if passthrough {
							source = "typed_messages_sse"
						}
						require.Equal(t, source, v.TextStream.Source)
						require.Nil(t, v.OutputRate.TokensPerSecond)
						if ending == "STOP" {
							require.Equal(t, "observed", v.Cache.Status)
							require.EqualValues(t, 12, *v.Cache.InputTokens)
							require.InDelta(t, 1.0/3, *v.Cache.ReadRatio, 1e-9)
						} else {
							require.Nil(t, v.Cache.ReadRatio)
						}
						raw, marshalErr := json.Marshal(s)
						require.NoError(t, marshalErr)
						for _, private := range []string{"private reasoning", "fixture-prompt", "fixture-private-key", "fresh-oauth-token"} {
							require.NotContains(t, string(raw), private)
						}
					})
				})
			}
		}
	}
}
