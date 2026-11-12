//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestAvailabilityAntigravityEmptyStreamFeedback(t *testing.T) {
	for _, entry := range []string{"gemini", "messages", "chat", "responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(entry+map[bool]string{false: "/aggregate", true: "/stream"}[stream], func(t *testing.T) {
				for _, tc := range []struct {
					name, body            string
					availability, quality int64
				}{
					{"empty", "", 1, 0},
					{"heartbeat", ": keepalive\n\ndata: [DONE]\n\n", 1, 0},
					{"empty_object", "data: {\"response\":{}}\n\n", 1, 0},
					{"empty_candidates", "data: {\"response\":{\"candidates\":[]}}\n\n", 1, 0},
					{"usage_only", "data: {\"response\":{\"responseId\":\"fixture\",\"usageMetadata\":{\"promptTokenCount\":8}}}\n\n", 1, 0},
					{"raw_empty", "{}\n", 1, 0},
					{"raw_service", "{\"error\":{\"code\":503}}\n", 1, 1},
					{"raw_request", "{\"error\":{\"code\":400}}\n", 0, 0},
					{"raw_unknown", "{\"error\":{\"message\":\"fixture\"}}\n", 0, 0},
					{"policy", "data: {\"response\":{\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}}\n\n", 0, 0},
					{"answer_after_empty", "data: {\"response\":{}}\n\ndata: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}}\n\n", 0, 0},
				} {
					t.Run(tc.name, func(t *testing.T) {
						for _, mode := range []string{"business", "probe", "canceled"} {
							t.Run(mode, func(t *testing.T) {
								ctx := t.Context()
								switch mode {
								case "probe":
									ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
								case "canceled":
									var cancel context.CancelFunc
									ctx, cancel = context.WithCancel(ctx)
									cancel()
								}
								repo := &geminiAvailabilityFaultRepo{}
								svc := newAntigravityCompatService(rawChatCompletionsTestConfig().Gateway, nil)
								svc.rateLimitService = &RateLimitService{qualityTrigger: &quality5xxTrigger{availability: repo, immediate: repo}}
								c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/"+entry, nil)
								c.Request = c.Request.WithContext(ctx)
								account := newAntigravityCompatAccount(AccountTypeOAuth)
								resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Request: c.Request,
									Body: &availabilityFaultBody{ReadCloser: io.NopCloser(strings.NewReader(tc.body)), ctx: ctx,
										account: *availabilityFaultIdentity(account), trigger: svc.rateLimitService.qualityTrigger}}
								var err error
								switch entry {
								case "gemini":
									if stream {
										_, err = svc.handleGeminiStreamingResponse(c, resp, time.Now())
									} else {
										_, err = svc.handleGeminiStreamToNonStreaming(c, resp, time.Now())
									}
								case "messages":
									if stream {
										_, err = svc.handleClaudeStreamingResponse(c, resp, time.Now(), "gemini-3.1-pro-high")
									} else {
										_, err = svc.handleClaudeStreamToNonStreaming(c, resp, time.Now(), "gemini-3.1-pro-high")
									}
								case "chat":
									if stream {
										_, err = svc.handleChatCompletionsStreamingFromAntigravity(c, resp, time.Now(), "gemini-3.1-pro-high", true)
									} else {
										_, err = svc.handleChatCompletionsNonStreamingFromAntigravity(c, resp, time.Now(), "gemini-3.1-pro-high")
									}
								case "responses":
									if stream {
										_, err = svc.handleResponsesStreamingFromAntigravity(c, resp, time.Now(), "gemini-3.1-pro-high")
									} else {
										_, err = svc.handleResponsesNonStreamingFromAntigravity(c, resp, time.Now(), "gemini-3.1-pro-high")
									}
								}
								if err != nil {
									var failover *UpstreamFailoverError
									require.ErrorAs(t, err, &failover, "preserve the existing empty-stream error contract")
								}
								wantAvailability, wantQuality := tc.availability, tc.quality
								if mode != "business" {
									wantAvailability, wantQuality = 0, 0
								}
								require.Equal(t, wantAvailability, repo.availability.Load())
								require.Equal(t, wantQuality, repo.quality.Load())
							})
						}
					})
				}
			})
		}
	}
}

func TestAvailabilityAntigravityBusinessSemanticFaults(t *testing.T) {
	for _, entry := range []string{"gemini", "messages", "chat", "responses"} {
		t.Run(entry, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				t.Run(map[bool]string{false: "aggregate", true: "stream"}[stream], func(t *testing.T) {
					for _, tc := range []struct {
						name                  string
						chunks                []string
						availability, quality int64
					}{
						{"service", []string{`{"error":{"code":503,"status":"UNAVAILABLE"}}`}, 1, 1},
						{"authentication", []string{`{"error":{"code":401}}`}, 1, 0},
						{"quota", []string{`{"error":{"code":429}}`}, 1, 0},
						{"timeout", []string{`{"error":{"code":408}}`}, 1, 0},
						{"known_status", []string{`{"error":{"status":"UNAVAILABLE"}}`}, 1, 1},
						{"repeated", []string{`{"error":{"code":503}}`, `{"error":{"code":503}}`}, 1, 1},
						{"availability_then_quality", []string{`{"error":{"code":401}}`, `{"error":{"code":503}}`}, 2, 1},
						{"policy", []string{`{"promptFeedback":{"blockReason":"SAFETY"}}`}, 0, 0},
						{"request", []string{`{"error":{"code":400}}`}, 0, 0},
						{"unknown", []string{`{"error":{"message":"fixture"}}`}, 0, 0},
						{"duplicate", []string{`{"error":{"code":503,"code":400}}`}, 0, 0},
						{"normal", nil, 0, 0},
					} {
						t.Run(tc.name, func(t *testing.T) {
							for _, probe := range []bool{false, true} {
								ctx := t.Context()
								if probe {
									ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
								}
								payloads := []string{`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}`}
								payloads = append(payloads, tc.chunks...)
								payloads = append(payloads, `{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}`)
								upstreamBody := ""
								for _, payload := range payloads {
									upstreamBody += "data: {\"response\":" + payload + "}\n\n"
								}
								var sends atomic.Int32
								upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
									sends.Add(1)
									return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(upstreamBody)), Request: req}, nil
								}}
								svc := newAntigravityCompatService(rawChatCompletionsTestConfig().Gateway, upstream)
								repo := &geminiAvailabilityFaultRepo{}
								svc.rateLimitService = &RateLimitService{qualityTrigger: &quality5xxTrigger{availability: repo, immediate: repo}}
								account := newAntigravityCompatAccount(AccountTypeOAuth)
								request := map[string]any{"model": "gemini-3.1-pro-high", "stream": stream, "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "Reply ok"}}}
								path := "/v1/" + entry
								if entry == "responses" {
									delete(request, "messages")
									request["input"] = "Reply ok"
								}
								if entry == "gemini" {
									request = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "Reply ok"}}}}}
									path = "/v1beta/models/gemini-3.1-pro-high:streamGenerateContent"
								}
								body, err := json.Marshal(request)
								require.NoError(t, err)
								c, recorder := newAntigravityCompatContext(http.MethodPost, path, body)
								c.Request = c.Request.WithContext(ctx)
								var result *ForwardResult
								switch entry {
								case "gemini":
									result, err = svc.ForwardGemini(ctx, c, account, "gemini-3.1-pro-high", "streamGenerateContent", stream, body, false)
								case "messages":
									result, err = svc.Forward(ctx, c, account, body, false)
								case "chat":
									result, err = svc.ForwardAsChatCompletions(ctx, c, account, body, nil)
								case "responses":
									result, err = svc.ForwardAsResponses(ctx, c, account, body, nil)
								}
								require.NoError(t, err, "probe=%t", probe)
								require.NotNil(t, result)
								require.Equal(t, http.StatusOK, recorder.Code)
								require.Contains(t, recorder.Body.String(), "ok")
								require.EqualValues(t, 1, sends.Load(), "a semantic fault must not replay the current generation")
								require.Equal(t, 8, result.Usage.InputTokens)
								require.Equal(t, 3, result.Usage.OutputTokens)
								wantAvailability, wantQuality := tc.availability, tc.quality
								if probe {
									wantAvailability, wantQuality = 0, 0
								}
								require.Equal(t, wantAvailability, repo.availability.Load(), "probe=%t", probe)
								require.Equal(t, wantQuality, repo.quality.Load(), "probe=%t", probe)
							}
						})
					}
				})
			}
		})
	}
}

func TestAvailabilityAntigravityFaultBeforeStreamClosure(t *testing.T) {
	for _, entry := range []string{"gemini", "messages", "chat", "responses"} {
		t.Run(entry, func(t *testing.T) {
			release := make(chan struct{})
			var sends atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":3}}}\n\ndata: {\"response\":{\"error\":{\"code\":503}}}\n\n")
				flusher, ok := w.(http.Flusher)
				if !ok {
					t.Error("test server has no HTTP flusher")
					return
				}
				flusher.Flush()
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(server.Close)
			defer close(release)
			original := antigravity.BaseURLs
			antigravity.BaseURLs = []string{server.URL}
			t.Cleanup(func() { antigravity.BaseURLs = original })
			upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				return server.Client().Do(req)
			}}
			svc := newAntigravityCompatService(rawChatCompletionsTestConfig().Gateway, upstream)
			repo := &geminiAvailabilityFaultRepo{signal: make(chan struct{}, 1)}
			svc.rateLimitService = &RateLimitService{qualityTrigger: &quality5xxTrigger{availability: repo, immediate: repo}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			account := newAntigravityCompatAccount(AccountTypeOAuth)
			body := []byte(`{"model":"gemini-3.1-pro-high","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"Reply ok"}]}`)
			switch entry {
			case "responses":
				body = []byte(`{"model":"gemini-3.1-pro-high","stream":true,"input":"Reply ok"}`)
			case "gemini":
				body = []byte(`{"contents":[{"role":"user","parts":[{"text":"Reply ok"}]}]}`)
			}
			c, rec := newAntigravityCompatContext(http.MethodPost, "/v1/"+entry, body)
			c.Request = c.Request.WithContext(ctx)
			done := make(chan error, 1)
			go func() {
				var err error
				switch entry {
				case "gemini":
					_, err = svc.ForwardGemini(ctx, c, account, "gemini-3.1-pro-high", "streamGenerateContent", true, body, false)
				case "messages":
					_, err = svc.Forward(ctx, c, account, body, false)
				case "chat":
					_, err = svc.ForwardAsChatCompletions(ctx, c, account, body, nil)
				case "responses":
					_, err = svc.ForwardAsResponses(ctx, c, account, body, nil)
				}
				done <- err
			}()
			select {
			case <-repo.signal:
				require.EqualValues(t, 1, repo.availability.Load())
				require.EqualValues(t, 1, repo.quality.Load())
			case <-time.After(2 * time.Second):
				t.Error("the semantic fault must invalidate the old pass before the upstream connection closes")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Antigravity forwarding did not finish after cancellation")
			}
			require.EqualValues(t, 1, sends.Load())
			require.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

func TestAvailabilityGeminiScopeAndAdmission(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		name, path := "native", "/v1beta/models/gemini-3.7-pro:streamGenerateContent?alt=sse"
		if wrapped {
			name, path = "internal", "/v1internal:streamGenerateContent?alt=sse"
		}
		t.Run(name, func(t *testing.T) {
			body := createGeminiTestPayload("gemini-3.7-pro", "original question")
			body, err := sjson.SetBytes(body, "generationConfig.thinkingConfig.thinkingLevel", "high")
			require.NoError(t, err)
			if wrapped {
				body, err = (&AntigravityGatewayService{}).wrapV1InternalRequest("fixture-project", "gemini-3.7-pro", body)
				require.NoError(t, err)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, `{}`)
			}))
			t.Cleanup(server.Close)
			account := &Account{ID: 1, Platform: PlatformGemini, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
			mode := "gemini"
			if wrapped {
				account.Platform, mode = PlatformAntigravity, "antigravity"
			}
			newRequest := func(raw []byte, suffix string) *http.Request {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+suffix, bytes.NewReader(raw))
				require.NoError(t, err)
				return req
			}
			scope := availabilityScopeForRequest(newRequest(body, path), body)
			require.NotNil(t, scope, "the native wire request must have an observable scope")
			require.Equal(t, "gemini-3.7-pro", scope.Model)
			require.Equal(t, "high", scope.ReasoningEffort)
			require.True(t, scope.Stream)
			report := availabilityAdmissionPass(account.ID, scope.Model)
			report.PlanID = 1
			report.Models[0].Decision.Scope = scope
			reader := &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}
			limiter := &RateLimitService{availabilityReader: reader}
			replica := regionalTestReplica(t, filepath.Join(t.TempDir(), "state"), true)
			require.NoError(t, replica.PublishNext(t.Context(), "test", time.Minute, regionalAvailabilityDoc(t, account, reader.reports)))
			regionalReader, err := NewRegionalSchedulerReader(replica)
			require.NoError(t, err)
			regional := NewSchedulerSnapshotService(nil, nil, nil, nil, nil)
			regional.SetLocalSnapshotReader(regionalReader)
			upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				return server.Client().Do(req)
			}}
			prefix := ""
			if wrapped {
				prefix = "request."
			}
			for _, tc := range []struct {
				name, field string
				value       any
				allowed     bool
			}{
				{"same", "", nil, true},
				{"new question", "contents.0.parts.0.text", "different question", true},
				{"empty tools", "tools", []any{}, true},
				{"thinking level", "generationConfig.thinkingConfig.thinkingLevel", "low", false},
				{"thinking budget", "generationConfig.thinkingConfig", map[string]any{"thinkingBudget": 8192}, false},
				{"thought output", "generationConfig.thinkingConfig.includeThoughts", true, false},
				{"multiple candidates", "generationConfig.candidateCount", 2, false},
				{"image output", "generationConfig.responseModalities", []string{"TEXT", "IMAGE"}, false},
				{"JSON output", "generationConfig.responseMimeType", "application/json", false},
				{"tool", "tools", []any{map[string]any{"googleSearch": map[string]any{}}}, false},
				{"file", "contents.0.parts.0", map[string]any{"fileData": map[string]any{"fileUri": "fixture-file"}}, false},
				{"signed history", "contents.0.parts.0.thoughtSignature", "fixture-signature", false},
				{"cache object", "cachedContent", "cachedContents/fixture", false},
				{"unknown option", "futureOption", true, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					payload := bytes.Clone(body)
					if tc.field != "" {
						payload, err = sjson.SetBytes(payload, prefix+tc.field, tc.value)
						require.NoError(t, err)
					}
					before := calls.Load()
					response, err := gatewayAvailabilitySend(mode, newRequest(payload, path), account, limiter, nil, upstream)
					if response != nil {
						require.NoError(t, response.Body.Close())
					}
					if tc.allowed {
						require.NoError(t, err)
						require.Equal(t, before+1, calls.Load())
					} else {
						require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
						require.Equal(t, before, calls.Load())
					}
					before, reads := calls.Load(), reader.reads
					response, err = gatewayAvailabilitySend(mode, newRequest(payload, path), account, limiter, regional, upstream)
					if response != nil {
						require.NoError(t, response.Body.Close())
					}
					require.Equal(t, reads, reader.reads, "regional decisions must not read the primary")
					if tc.allowed {
						require.NoError(t, err)
						require.Equal(t, before+1, calls.Load())
					} else {
						require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
						require.Equal(t, before, calls.Load())
					}
				})
			}
			for _, suffix := range []string{
				strings.ReplaceAll(path, "streamGenerateContent?alt=sse", "generateContent"),
				path + "&alt=sse", path + "&unknown=1", strings.ReplaceAll(path, ":streamGenerateContent", ":countTokens"),
			} {
				err := limiter.admitAvailabilityRequest(newRequest(body, suffix), account, nil)
				require.True(t, IsOpenAITurnAdmissionError(err), "a different action/query must not inherit evidence: %s", suffix)
			}
			if wrapped {
				for _, field := range []string{"model", "project", "userAgent", "requestType"} {
					payload, err := sjson.SetBytes(body, field, "different")
					require.NoError(t, err)
					require.True(t, IsOpenAITurnAdmissionError(limiter.admitAvailabilityRequest(newRequest(payload, path), account, nil)))
				}
			}
		})
	}
}

func TestAvailabilityGeminiMappedProbeModel(t *testing.T) {
	for _, account := range []*Account{
		{ID: 1, Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"alias-*": "gemini-3.7-pro"}}},
		{ID: 2, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{"gemini-3.7-flash": "gemini-3.7-flash", "gemini-3.7-flash-high": "gemini-3.7-flash-high"}}},
	} {
		model := "alias-pro"
		if account.Platform == PlatformAntigravity {
			model = "gemini-3.7-flash"
		}
		mapped := AvailabilityModelForAccount(account, model)
		if account.Platform == PlatformAntigravity {
			require.Equal(t, "gemini-3.7-flash-high", mapped)
		} else {
			require.Equal(t, "gemini-3.7-pro", mapped)
		}
		payload := createGeminiTestPayload(mapped, "question")
		path := "https://fixture.invalid/v1beta/models/" + mapped + ":streamGenerateContent?alt=sse"
		if account.Platform == PlatformAntigravity {
			var err error
			payload, err = (&AntigravityGatewayService{}).wrapV1InternalRequest("fixture-project", mapped, payload)
			require.NoError(t, err)
			path = "https://fixture.invalid/v1internal:streamGenerateContent?alt=sse"
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(payload))
		require.NoError(t, err)
		report := availabilityAdmissionPass(account.ID, model)
		report.Models[0].Decision.Scope = availabilityScopeForRequest(req, payload)
		require.NotNil(t, report.Models[0].Decision.Scope)
		limiter := &RateLimitService{availabilityReader: &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}}
		require.NoError(t, limiter.admitAvailabilityRequest(req, account, nil))
	}
}

func TestAvailabilityGeminiBackgroundRequiresCompleteAnswer(t *testing.T) {
	text := `{"candidates":[{"content":{"parts":[{"text":"21"}]}}]}`
	stop := `{"candidates":[{"finishReason":"STOP"}]}`
	for _, name := range []string{"native", "antigravity_gemini", "antigravity_claude"} {
		wrapped := name != "native"
		model := "gemini-3.7-pro"
		if name == "antigravity_claude" {
			model = "claude-sonnet-4-6"
		}
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct{ name, data, status string }{
				{"complete", text + "\n" + stop, "pass"},
				{"missing stop", text, "inconclusive"},
				{"done without stop", text + "\n[DONE]", "inconclusive"},
				{"token cutoff", text + "\n" + `{"candidates":[{"finishReason":"MAX_TOKENS"}]}`, "inconclusive"},
				{"blocked", text + "\n" + `{"candidates":[{"finishReason":"SAFETY"}]}`, "inconclusive"},
				{"thinking only", `{"candidates":[{"content":{"parts":[{"text":"21","thought":true}]},"finishReason":"STOP"}]}`, "inconclusive"},
				{"malformed", text + "\n{invalid\n" + stop, "inconclusive"},
				{"no answer", stop, "inconclusive"},
				{"upstream error", text + "\n" + `{"error":{"code":503,"message":"fixture error"}}`, "fail"},
				{"error with stop", `{"error":{"code":503,"message":"fixture error"},"candidates":[{"content":{"parts":[{"text":"21"}]},"finishReason":"STOP"}]}`, "fail"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					requests := make(chan []byte, 8)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						sent, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						requests <- sent
						w.Header().Set("Content-Type", "text/event-stream")
						for _, raw := range strings.Split(tc.data, "\n") {
							if wrapped && strings.HasPrefix(raw, "{") && json.Valid([]byte(raw)) {
								raw = `{"response":` + raw + `}`
							}
							_, _ = io.WriteString(w, "data: "+raw+"\n\n")
						}
					}))
					t.Cleanup(server.Close)
					account := &Account{ID: 1, Platform: PlatformGemini, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
						Credentials: map[string]any{"base_url": server.URL, "api_key": "fixture-key"}}
					upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
						return server.Client().Do(req)
					}}
					svc := pelicanClaudeTestService(account, upstream)
					svc.cfg = rawChatCompletionsTestConfig()
					if wrapped {
						original := antigravity.BaseURLs
						antigravity.BaseURLs = []string{server.URL}
						t.Cleanup(func() { antigravity.BaseURLs = original })
						account.Platform, account.Type = PlatformAntigravity, AccountTypeOAuth
						account.Credentials["access_token"] = "fixture-token"
						account.Credentials["project_id"] = "fixture-project"
						account.Credentials["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
						account.Credentials["model_mapping"] = map[string]any{model: model}
						svc.antigravityGatewayService = &AntigravityGatewayService{tokenProvider: &AntigravityTokenProvider{}, httpUpstream: upstream}
					}
					result, err := svc.RunPelicanBackground(t.Context(), account.ID, model, &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, ReasoningEffort: "high", ParallelCount: 1})
					require.NoError(t, err)
					var sent []byte
					select {
					case sent = <-requests:
					default:
						t.Fatal("the background test must send its original question")
					}
					require.Empty(t, requests, "one physical generation must produce exactly one logical probe attempt")
					probe := result.PelicanConfig.AvailabilityResult
					require.NotNil(t, probe)
					require.Equal(t, tc.status, probe.Status)
					require.NotNil(t, probe.Scope, "scope must come from the actual request, including failed responses")
					require.Equal(t, 1, probe.AttemptCount)
					prefix := ""
					if wrapped {
						prefix = "request."
					}
					require.Contains(t, gjson.GetBytes(sent, prefix+"contents.0.parts.0.text").String(), CandyPrompt)
					if name == "antigravity_claude" {
						require.Greater(t, gjson.GetBytes(sent, prefix+"generationConfig.maxOutputTokens").Int(), int64(1))
						require.Equal(t, "budget", probe.Scope.ThinkingMode)
						require.Equal(t, int64(-1), probe.Scope.ThinkingBudget)
						require.Equal(t, "unspecified", probe.Scope.ReasoningEffort, "the converter does not attest to an output_config effort label")
					} else {
						require.Equal(t, "high", gjson.GetBytes(sent, prefix+"generationConfig.thinkingConfig.thinkingLevel").String())
					}
					if tc.name == "complete" {
						report := availabilityAdmissionPass(account.ID, model)
						report.Models[0].Decision = probe
						reader := &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}
						limiter := &RateLimitService{availabilityReader: reader}
						path, mode := "/v1beta/models/gemini-3.7-pro:streamGenerateContent?alt=sse", "gemini"
						if wrapped {
							path, mode = "/v1internal:streamGenerateContent?alt=sse", "antigravity"
						}
						for _, state := range []string{"pass", "fail", "recovery", "expired"} {
							report.Models[0].Status = "pass"
							switch state {
							case "fail":
								report.Models[0].Status = "fail"
							case "expired":
								expired := time.Now().Add(-time.Second)
								report.Models[0].ValidUntil = &expired
							}
							req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+path, bytes.NewReader(sent))
							require.NoError(t, err)
							response, err := gatewayAvailabilitySend(mode, req, account, limiter, nil, upstream)
							if response != nil {
								require.NoError(t, response.Body.Close())
							}
							if state == "pass" || state == "recovery" {
								require.NoError(t, err, state)
							} else {
								require.True(t, IsOpenAITurnAdmissionError(err), state)
							}
						}
						// Only the initial probe and two admitted requests reached
						// the server; failed/expired decisions emitted no request.
						require.Len(t, requests, 2)
					}
					raw, err := json.Marshal(probe)
					require.NoError(t, err)
					for _, secret := range []string{"fixture-key", "fixture-token", "fixture-project", server.URL, CandyPrompt} {
						require.NotContains(t, string(raw), secret)
					}
				})
			}
		})
	}
}

func TestAvailabilityGeminiProbeAssociation(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		for _, redirect := range []bool{false, true} {
			name := "native"
			if wrapped {
				name = "internal"
			}
			if redirect {
				name += "/redirect"
			} else {
				name += "/direct"
			}
			t.Run(name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if redirect && !strings.HasPrefix(req.URL.Path, "/redirected/") {
						http.Redirect(w, req, "/redirected"+req.URL.RequestURI(), http.StatusTemporaryRedirect)
						return
					}
					w.WriteHeader(http.StatusOK)
				}))
				t.Cleanup(server.Close)
				collector := requesttiming.New(time.Now(), 0)
				observation := &availabilityProbeObservation{}
				ctx := context.WithValue(requesttiming.With(t.Context(), collector), availabilityProbeSnapshotKey{}, observation)
				payload := createGeminiTestPayload("gemini-3.7-pro", "question")
				path := "/v1beta/models/gemini-3.7-pro:streamGenerateContent?alt=sse"
				if wrapped {
					var err error
					payload, err = (&AntigravityGatewayService{}).wrapV1InternalRequest("fixture-project", "gemini-3.7-pro", payload)
					require.NoError(t, err)
					path = "/v1internal:streamGenerateContent?alt=sse"
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, bytes.NewReader(payload))
				require.NoError(t, err)
				upstream := timedAccountTestUpstream(&codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
					return server.Client().Do(req)
				}}, nil)
				response, err := upstream.Do(req, "", 1, 1)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				if redirect {
					require.Nil(t, observation.scope)
				} else {
					require.NotNil(t, observation.scope)
				}
			})
		}
	}
}

func TestReadGeminiTestStreamStopsBeforeConnectionClose(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	completed := make(chan error, 1)
	go func() {
		completed <- readGeminiTestStream(t.Context(), reader, nil, func(TestEvent) {})
	}()
	_, err := io.WriteString(writer, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"answer\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	require.NoError(t, err)
	select {
	case err := <-completed:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("a complete answer must not wait for EOF")
	}
}

func TestAvailabilityGeminiProbeCredentialAssociation(t *testing.T) {
	for _, name := range []string{"Authorization", "X-Goog-Api-Key"} {
		for _, mutation := range []string{"same", "rewrite", "remove", "duplicate"} {
			t.Run(name+"/"+mutation, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
				t.Cleanup(server.Close)
				collector := requesttiming.New(time.Now(), 0)
				observation := &availabilityProbeObservation{}
				ctx := context.WithValue(requesttiming.With(t.Context(), collector), availabilityProbeSnapshotKey{}, observation)
				payload := createGeminiTestPayload("gemini-3.7-pro", "question")
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1beta/models/gemini-3.7-pro:streamGenerateContent?alt=sse", bytes.NewReader(payload))
				require.NoError(t, err)
				req.Header.Set(name, "fixture-credential")
				if mutation == "duplicate" {
					req.Header[strings.ToLower(name)] = []string{"fixture-other-credential"}
				}
				upstream := timedAccountTestUpstream(&codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
					actual := req.Clone(req.Context())
					switch mutation {
					case "rewrite":
						actual.Header.Set(name, "fixture-other-credential")
					case "remove":
						actual.Header.Del(name)
					}
					return server.Client().Do(actual)
				}}, nil)
				response, err := upstream.Do(req, "", 1, 1)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				if mutation == "same" {
					require.NotNil(t, observation.scope)
				} else {
					require.Nil(t, observation.scope, "changed or ambiguous credentials cannot attest to the constructed request")
				}
			})
		}
	}
}

func TestReadGeminiTestStreamConnectionAndAnswerBudgets(t *testing.T) {
	for _, mode := range []string{"connection", "answer", "availability"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			switch mode {
			case "answer":
				ctx = withPelicanTestOptions(ctx, pelicanTestOptions{prompt: "question"})
			case "availability":
				ctx = context.WithValue(ctx, availabilityProbeSnapshotKey{}, &availabilityProbeObservation{})
			}
			stream := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"answer\"}]},\"finishReason\":\"MAX_TOKENS\"}]}\n\n"
			err := readGeminiTestStream(ctx, strings.NewReader(stream), nil, func(TestEvent) {})
			if mode == "connection" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, pelicanErrMaxTokens)
			}
		})
	}
}
