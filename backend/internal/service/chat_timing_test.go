//go:build unit

package service

import (
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
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

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
