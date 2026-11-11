//go:build unit

package service

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type outputRateTimingHTTPUpstream struct{ antigravityTimingHTTPUpstream }

func (u outputRateTimingHTTPUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	resp, err := u.antigravityTimingHTTPUpstream.Do(req, proxy, id, concurrency)
	if err == nil {
		resp.Body = &timingSSEStepBody{ReadCloser: resp.Body, reader: bufio.NewReader(resp.Body), delay: 300 * time.Millisecond}
	}
	return resp, err
}

func (u outputRateTimingHTTPUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func TestProtocolOutputRatePublicForward(t *testing.T) {
	for _, path := range []string{"chat/completions", "responses", "messages", "native_messages", "gemini_native", "gemini_messages", "gemini_chat"} {
		for _, mode := range []string{"complete", "missing_reasoning", "write_error"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				protocol := "chat_completions"
				usage := `{"prompt_tokens":100,"completion_tokens":120,"completion_tokens_details":{"reasoning_tokens":20}}`
				frames := []string{
					`{"choices":[{"index":0,"delta":{"content":"private first"}}]}`,
					`{"choices":[{"index":0,"delta":{"content":"private last"}}]}`,
					`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
					`{"choices":[],"usage":` + usage + `}`,
				}
				if path == "native_messages" {
					protocol = "messages"
					frames = []string{
						`{"type":"message_start","message":{"id":"private-id","role":"assistant","model":"fixture","usage":{"input_tokens":100}}}`,
						`{"type":"content_block_delta","delta":{"type":"text_delta","text":"private first"}}`,
						`{"type":"content_block_delta","delta":{"type":"text_delta","text":"private last"}}`,
						`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":120,"output_tokens_details":{"thinking_tokens":20}}}`,
						`{"type":"message_stop"}`,
					}
				} else if strings.HasPrefix(path, "gemini_") {
					protocol = "gemini"
					frames = []string{
						`{"candidates":[{"content":{"parts":[{"text":"private first"}]}}]}`,
						`{"candidates":[{"content":{"parts":[{"text":"private last"}]}}]}`,
						`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":100,"thoughtsTokenCount":20}}`,
					}
				}
				wire := "data: " + strings.Join(frames, "\n\ndata: ") + "\n\n"
				if mode == "missing_reasoning" {
					for _, field := range []string{`,"completion_tokens_details":{"reasoning_tokens":20}`, `,"output_tokens_details":{"thinking_tokens":20}`, `,"thoughtsTokenCount":20`} {
						wire = strings.ReplaceAll(wire, field, "")
					}
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, wire)
				}))
				defer server.Close()
				target, err := url.Parse(server.URL)
				require.NoError(t, err)
				upstream := outputRateTimingHTTPUpstream{antigravityTimingHTTPUpstream{client: server.Client(), target: target}}
				collector := requesttiming.New(time.Now(), 0)
				ctx := requesttiming.With(t.Context(), collector)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/fixture", nil).WithContext(ctx)
				c.Writer = &geminiTimingWriter{ResponseWriter: c.Writer, collector: collector, fail: mode == "write_error"}
				cfg := rawChatCompletionsTestConfig()
				cfg.Security.URLAllowlist.AllowInsecureHTTP = true
				body := []byte(`{"model":"fixture","stream":true,"max_tokens":256,"messages":[{"role":"user","content":"fixture"}]}`)
				if strings.HasPrefix(path, "gemini_") {
					svc := &GeminiMessagesCompatService{cfg: cfg, httpUpstream: upstream}
					account := &Account{ID: 707, Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-secret", "base_url": server.URL}}
					switch path {
					case "gemini_native":
						_, err = svc.ForwardNative(ctx, c, account, "fixture", "streamGenerateContent", true, []byte(geminiTransportTestNativeBody))
					case "gemini_messages":
						_, err = svc.Forward(ctx, c, account, body)
					case "gemini_chat":
						_, err = svc.ForwardAsChatCompletions(ctx, c, account, body)
					}
				} else {
					svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: &RateLimitService{cfg: cfg, accountRepo: &errorPolicyRepoStub{}}}
					account := forceChatResponsesFallbackAccount()
					account.Credentials["base_url"] = server.URL
					switch path {
					case "chat/completions":
						_, err = svc.ForwardAsChatCompletions(ctx, c, account, body, "", "")
					case "responses":
						_, err = svc.Forward(ctx, c, account, []byte(`{"model":"fixture","stream":true,"input":"fixture"}`))
					case "messages":
						_, err = svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
					case "native_messages":
						account = nativeAnthropicTestAccount()
						account.Credentials["api_base_urls"] = map[string]any{APIProtocolAnthropic: server.URL}
						account.Credentials["base_url"] = server.URL
						_, err = svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
					}
				}
				if mode != "write_error" {
					require.NoError(t, err)
				}
				collector.Finish(c.Writer.Status(), false)
				collector.WhenFinished(func(s requesttiming.Snapshot) {
					require.NotEmpty(t, s.Outcome, "the production Forward boundary must provide its own outcome")
					rate := requesttiming.DeriveContentObservation(s).OutputRate
					if mode == "complete" {
						require.Equal(t, "success", s.Outcome)
						require.Equal(t, "estimated", rate.Status)
						require.Equal(t, "canonical_"+protocol+"_usage_and_text_events", rate.Source)
						require.NotNil(t, rate.TokensPerSecond)
						require.EqualValues(t, 100, *rate.NonReasoningTokens)
						require.EqualValues(t, 20, *rate.ReasoningTokens)
						require.InDelta(t, float64(100)/(*rate.WindowMS/1000), *rate.TokensPerSecond, 1e-9)
					} else {
						require.Nil(t, rate.TokensPerSecond)
						if mode == "missing_reasoning" {
							require.Equal(t, "not_reported", rate.Status)
						}
					}
					raw, marshalErr := json.Marshal(s)
					require.NoError(t, marshalErr)
					require.NotContains(t, string(raw), "private")
				})
			})
		}
	}
}

func TestChatTimingBridges(t *testing.T) {
	for _, endpoint := range []string{"chat/completions", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, ending := range []string{"complete", "missing_read", "length", "write_error", "eof", "done_only", "malformed", "late_error", "partial_tail", "read_error"} {
				if !stream && (ending == "eof" || ending == "done_only" || ending == "partial_tail" || ending == "read_error") {
					continue
				}
				t.Run(fmt.Sprintf("%s/stream=%v/%s", endpoint, stream, ending), func(t *testing.T) {
					usage := `{"prompt_tokens":100,"completion_tokens":120,"prompt_tokens_details":{"cached_tokens":90}}`
					if ending == "missing_read" {
						usage = `{"prompt_tokens":100,"completion_tokens":120}`
					}
					finish := "stop"
					if ending == "length" {
						finish = "length"
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						if !stream {
							w.Header().Set("Content-Type", "application/json")
							extra := ""
							switch ending {
							case "malformed":
								extra = `,"choices":[]`
							case "late_error":
								extra = `,"error":{"message":"synthetic"}`
							}
							_, _ = io.WriteString(w, `{"id":"private-chat-id","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"private first last","reasoning_content":"private reasoning"},"finish_reason":"`+finish+`"}],"usage":`+usage+extra+`}`)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						if ending == "read_error" {
							w.Header().Set("Content-Length", "999999")
						}
						send := func(data string) { _, _ = io.WriteString(w, "data: "+data+"\n\n") }
						send(`{"object":"chat.completion.chunk","choices":[{"index":1,"delta":{"content":"secondary"},"finish_reason":"stop"}]}`)
						send(`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"private reasoning"}}]}`)
						send(`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"private first"}}]}`)
						_, _ = io.WriteString(w, ": keepalive\n\n")
						if ending == "malformed" {
							send(`{"choices":[{"index":0,"delta":{"content":42}}]}`)
						}
						send(`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" last"}}]}`)
						if ending != "eof" && ending != "done_only" {
							send(`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"` + finish + `"}]}`)
						}
						send(`{"object":"chat.completion.chunk","choices":[],"usage":` + usage + `}`)
						if ending == "late_error" {
							send(`{"error":{"message":"synthetic"}}`)
						}
						if ending == "partial_tail" {
							_, _ = io.WriteString(w, "data: {")
						} else if ending != "eof" && ending != "read_error" {
							send("[DONE]")
						}
					}))
					defer server.Close()
					collector := requesttiming.New(time.Now(), 0)
					body := []byte(fmt.Sprintf(`{"model":"gpt-5.4","stream":%v,"max_tokens":256,"messages":[{"role":"user","content":"fixture"}]}`, stream))
					if endpoint == "responses" {
						body = []byte(fmt.Sprintf(`{"model":"gpt-5.4","stream":%v,"input":"fixture"}`, stream))
					}
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					ctx := requesttiming.With(t.Context(), collector)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, strings.NewReader(string(body))).WithContext(ctx)
					c.Writer = &geminiTimingWriter{ResponseWriter: c.Writer, collector: collector, fail: ending == "write_error"}
					target, err := url.Parse(server.URL)
					require.NoError(t, err)
					account := forceChatResponsesFallbackAccount()
					account.Credentials["base_url"] = server.URL
					cfg := rawChatCompletionsTestConfig()
					svc := &OpenAIGatewayService{cfg: cfg, rateLimitService: &RateLimitService{cfg: cfg, accountRepo: &errorPolicyRepoStub{}}, httpUpstream: antigravityTimingHTTPUpstream{client: server.Client(), target: target}}
					var result *OpenAIForwardResult
					switch endpoint {
					case "chat/completions":
						result, err = svc.ForwardAsChatCompletions(ctx, c, account, body, "", "")
					case "responses":
						result, err = svc.Forward(ctx, c, account, body)
					case "messages":
						result, err = svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
					}
					if ending == "complete" || ending == "missing_read" || ending == "length" {
						require.NoError(t, err)
						require.NotNil(t, result)
						require.Equal(t, 120, result.Usage.OutputTokens, "canonical observation must preserve billing")
					}
					collector.Finish(c.Writer.Status(), false)
					collector.WhenFinished(func(s requesttiming.Snapshot) {
						require.NotEmpty(t, s.Outcome, "production forwarding must record its own outcome")
						require.Len(t, s.Attempts, 2)
						require.Nil(t, s.Attempts[0].TextStream, "logical attempt must not duplicate physical chunks")
						require.NotEmpty(t, s.Attempts[1].RouteFingerprint)
						v := requesttiming.DeriveContentObservation(s)
						if ending == "complete" {
							require.Equal(t, "success", s.Outcome)
							require.Equal(t, "observed", v.Cache.Status)
							require.Equal(t, "canonical_chat_completions_usage", v.Cache.Source)
							require.InDelta(t, 0.9, *v.Cache.ReadRatio, 1e-9)
							require.NotNil(t, v.VisibleTextFirstMS)
							require.NotNil(t, v.ReasoningFirstMS)
							if stream {
								require.Equal(t, "observed", v.TextStream.Status)
								require.Equal(t, "typed_chat_completions_sse", v.TextStream.Source)
								require.Equal(t, 2, *v.TextStream.Chunks)
							} else {
								require.Nil(t, v.TextStream.Chunks, "JSON must not invent deltas")
							}
						} else {
							require.NotEqual(t, "observed", v.Cache.Status)
							require.Nil(t, v.Cache.ReadRatio)
						}
						require.Nil(t, v.OutputRate.TokensPerSecond, "Chat output counters need independent reasoning evidence")
						raw, marshalErr := json.Marshal(s)
						require.NoError(t, marshalErr)
						for _, private := range []string{"private first", "private reasoning", "private-chat-id", "secondary"} {
							require.NotContains(t, string(raw), private)
						}
					})
				})
			}
		}
	}
}
