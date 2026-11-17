//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/modeltrace"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type modelTraceRepo struct {
	AccountRepository
	account   *Account
	mutations int
}

func (r *modelTraceRepo) GetByID(context.Context, int64) (*Account, error) { return r.account, nil }
func (r *modelTraceRepo) SetError(context.Context, int64, string) error    { r.mutations++; return nil }

func modelTraceNativeFixture(protocol string) (*Account, string) {
	text := strings.Repeat("17,83,247,", 100)
	encoded, _ := json.Marshal(text)
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":" + string(encoded) + "}\n\ndata: {\"type\":\"response.completed\"}\n\n"
	account := &Account{ID: 77, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "mock-only", "base_url": "https://example.com", "model_mapping": map[string]any{"public-model": "gpt-6-astra"}}}
	if protocol == "chat_completions" {
		account.Extra = map[string]any{"openai_responses_supported": false}
		stream = "data: {\"choices\":[{\"delta\":{\"content\":" + string(encoded) + "},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	}
	if protocol == "messages" {
		account.Platform = PlatformAnthropic
		account.Credentials["model_mapping"] = map[string]any{"public-model": "claude-sonnet-4-6"}
		stream = "data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":" + string(encoded) + "}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	return account, stream
}

func TestModelTraceNativeProtocolsAndThreeQuestionBudget(t *testing.T) {
	for _, protocol := range []string{"responses", "chat_completions", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			account, stream := modelTraceNativeFixture(protocol)
			repo := &modelTraceRepo{account: account}
			upstream := &queuedHTTPUpstream{}
			for range 3 {
				upstream.responses = append(upstream.responses, newJSONResponse(http.StatusOK, stream))
			}
			svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: &config.Config{}}
			c, recorder := newTestContext()
			require.NoError(t, svc.TestModelTraceAccountConnection(c, 77, "public-model", ""))
			require.Len(t, upstream.requests, 3)
			require.Zero(t, repo.mutations, "attribution must not isolate the account")
			var completed ModelTraceResult
			for _, line := range strings.Split(recorder.Body.String(), "\n") {
				var event struct {
					Type string          `json:"type"`
					Data json.RawMessage `json:"data"`
				}
				if data, ok := strings.CutPrefix(line, "data:"); ok && json.Unmarshal([]byte(data), &event) == nil && event.Type == "modeltrace_complete" {
					require.NoError(t, json.Unmarshal(event.Data, &completed))
				}
			}
			require.Equal(t, "complete", completed.Status)
			require.Len(t, completed.Samples, 3)
			require.Equal(t, 3, completed.Analysis.UsedOutputs)
			prompts := make(map[string]bool)
			for i, request := range upstream.requests {
				body, err := io.ReadAll(request.Body)
				require.NoError(t, err)
				var payload map[string]any
				require.NoError(t, json.Unmarshal(body, &payload))
				require.NotContains(t, payload, "system")
				require.NotContains(t, payload, "instructions")
				require.NotContains(t, payload, "temperature")
				require.NotContains(t, string(body), "HTML")
				require.Equal(t, protocol, completed.Samples[i].Environment.Protocol)
				require.NotEmpty(t, completed.Samples[i].MappedModel)
				prompts[completed.Samples[i].Challenge.Prompt] = true
			}
			require.Len(t, prompts, 3)
		})
	}
}

func TestModelTraceMonitoringBudgetProtocolsRecoveryAndSpoofing(t *testing.T) {
	for _, protocol := range []string{"responses", "chat_completions", "messages"} {
		for _, spoof := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/provider_spoof=%t", protocol, spoof), func(t *testing.T) {
				account, stream := modelTraceNativeFixture(protocol)
				accounts := &modelTraceRepo{account: account}
				upstream := &queuedHTTPUpstream{}
				for range 6 {
					if spoof {
						upstream.responses = append(upstream.responses, newJSONResponse(http.StatusInternalServerError, `{"error":{"message":"monitoring probe deferred: request_limit"}}`))
					} else {
						upstream.responses = append(upstream.responses, newJSONResponse(http.StatusOK, stream))
					}
				}
				budget := &monitoringBudgetRepoStub{}
				if !spoof {
					budget.denyAt, budget.err = 2, &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "key_minute", RetryAt: time.Now().UTC().Add(time.Minute)}
				}
				svc := &AccountTestService{accountRepo: accounts, httpUpstream: monitoringBudgetHTTP{base: upstream}, cfg: &config.Config{}}
				svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
				c, recorder := newTestContext()
				require.NoError(t, svc.TestModelTraceAccountConnection(c, 77, "public-model", ""))
				result, deferred := modelTraceBudgetResult(t, recorder.Body.String())
				require.Zero(t, accounts.mutations)
				if spoof {
					require.Nil(t, deferred, "provider strings must not impersonate a request-owned budget error")
					require.Len(t, result.Samples, 3)
					require.Len(t, upstream.requests, 3)
					for _, sample := range result.Samples {
						require.Equal(t, "generation_failed", sample.Error)
						require.False(t, sample.Accepted)
					}
					return
				}
				require.Equal(t, "key_minute", deferred.Scope)
				require.Equal(t, "partial", result.Status)
				require.Len(t, upstream.requests, 1)
				require.Len(t, result.Samples, 2)
				budget.denyAt = 0
				c, recorder = newTestContext()
				require.NoError(t, svc.TestModelTraceAccountConnection(c, 77, "public-model", ""), "the prior round must release its busy marker")
				result, deferred = modelTraceBudgetResult(t, recorder.Body.String())
				require.Nil(t, deferred)
				require.Equal(t, "complete", result.Status)
				require.Equal(t, 3, result.Analysis.UsedOutputs)
				require.Len(t, upstream.requests, 4)
				require.Len(t, budget.requests, 5)
				require.Equal(t, 4, budget.released)
				require.Zero(t, accounts.mutations)
			})
		}
	}
}

func TestModelTraceCancellationDoesNotStartRemainingQuestions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls, emissions := 0, 0
	challenges := make([]modeltrace.Challenge, 3)
	err := runModelTraceSamples(ctx, challenges, func(ctx context.Context, _ modeltrace.Challenge) ModelTraceSample {
		calls++
		_, hasDeadline := ctx.Deadline()
		require.True(t, hasDeadline)
		cancel()
		return ModelTraceSample{}
	}, func(ModelTraceSample) { emissions++ })
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
	require.Zero(t, emissions)
}

func TestModelTraceMonitoringBudgetDeferralStopsRound(t *testing.T) {
	for _, denyAt := range []int{1, 2} {
		for _, reason := range []string{"request_limit", "recovery_reserve", "concurrency_limit", "unavailable"} {
			t.Run(fmt.Sprintf("%s/attempt=%d", reason, denyAt), func(t *testing.T) {
				denial := &MonitoringProbeBudgetDenial{Reason: reason, Scope: "global_day", RetryAt: time.Now().UTC().Add(time.Hour)}
				var cause error = denial
				if reason == "unavailable" {
					cause = errors.New("PRIVATE_DATABASE_FAILURE")
				}
				repo := &monitoringBudgetRepoStub{denyAt: denyAt, err: cause}
				text := strings.Repeat("17,83,247,", 100)
				svc, upstream := qualityBudgetTestService(repo, qualityBudgetReply(text), qualityBudgetReply(text), qualityBudgetReply(text))
				c, recorder := newTestContext()
				require.NoError(t, svc.TestModelTraceAccountConnection(c, 2, "public-model", ""))
				result, deferred := modelTraceBudgetResult(t, recorder.Body.String())
				require.Len(t, upstream.requests, denyAt-1, "a local deferral stops this round before another paid request")
				require.Len(t, repo.requests, denyAt)
				require.Equal(t, denyAt-1, repo.released)
				require.Len(t, result.Samples, denyAt)
				require.Equal(t, "monitoring_budget_deferred", result.Samples[denyAt-1].Error)
				require.False(t, result.Samples[denyAt-1].Accepted)
				require.NotNil(t, deferred, "completed SSE must explain the local pause structurally")
				if reason == "unavailable" {
					require.Equal(t, "budget_unavailable", deferred.Reason)
					require.Equal(t, "system", deferred.Scope)
				} else {
					require.Equal(t, denial, deferred)
				}
				require.NotContains(t, recorder.Body.String(), "PRIVATE_DATABASE_FAILURE")
				if denyAt == 1 {
					require.Equal(t, "insufficient", result.Status)
					require.Nil(t, result.Analysis)
				} else {
					require.Equal(t, "partial", result.Status)
					require.Equal(t, 1, result.Analysis.UsedOutputs, "the earlier completed answer remains usable, not a passing three-question round")
				}
				require.NoError(t, c.Request.Context().Err())
			})
		}
	}
}

func modelTraceBudgetResult(t *testing.T, stream string) (ModelTraceResult, *MonitoringProbeBudgetDenial) {
	t.Helper()
	for _, line := range strings.Split(stream, "\n") {
		var event struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if data, ok := strings.CutPrefix(line, "data:"); ok && json.Unmarshal([]byte(data), &event) == nil && event.Type == "modeltrace_complete" {
			var result ModelTraceResult
			var extra struct {
				BudgetDeferral *MonitoringProbeBudgetDenial `json:"budget_deferral"`
			}
			require.NoError(t, json.Unmarshal(event.Data, &result))
			require.NoError(t, json.Unmarshal(event.Data, &extra))
			return result, extra.BudgetDeferral
		}
	}
	t.Fatal("ModelTrace completed event missing")
	return ModelTraceResult{}, nil
}

func TestModelTraceMonitoringBudgetLeaseLossExcludesPartialAnswer(t *testing.T) {
	for _, phase := range []string{"read", "close"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				response := qualityBudgetReply(strings.Repeat("17,83,247,", 100))
				loseLease := func() { time.Sleep(41 * time.Second); synctest.Wait() }
				body := qualityBudgetCloseBody{ReadCloser: response.Body, close: func() {}}
				if phase == "read" {
					body.read = loseLease
				} else {
					body.close = loseLease
				}
				response.Body = body
				repo := &monitoringBudgetRepoStub{renew: func(context.Context) (bool, error) { return false, nil }}
				svc, upstream := qualityBudgetTestService(repo, response)
				c, recorder := newTestContext()
				c.Request = c.Request.WithContext(t.Context())
				require.NoError(t, svc.TestModelTraceAccountConnection(c, 2, "public-model", ""))
				result, deferred := modelTraceBudgetResult(t, recorder.Body.String())
				require.Len(t, upstream.requests, 1)
				require.Len(t, repo.requests, 1)
				require.Equal(t, 1, repo.released)
				require.Len(t, result.Samples, 1)
				require.Equal(t, "monitoring_budget_deferred", result.Samples[0].Error)
				require.False(t, result.Samples[0].Accepted)
				require.Nil(t, result.Analysis)
				require.NotNil(t, deferred)
				require.Equal(t, "budget_unavailable", deferred.Reason)
				require.NoError(t, c.Request.Context().Err())
			})
		})
	}
}

func TestModelTraceIncompleteAnswersAndRefusalsAreExcluded(t *testing.T) {
	for _, protocol := range []string{"responses", "chat_completions", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			c, rec := newTestContext()
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), modelTraceContextKey{}, modelTraceEnvironment{Protocol: protocol}))
			svc := &AccountTestService{}
			var err error
			switch protocol {
			case "responses":
				err = svc.processOpenAIStream(c, strings.NewReader("data: {\"type\":\"response.done\",\"response\":{\"status\":\"incomplete\"}}\n\n"))
			case "chat_completions":
				err = svc.processOpenAIChatCompletionsStream(c, strings.NewReader("data: {\"choices\":[{\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"), nil)
			case "messages":
				c.Request = c.Request.WithContext(withPelicanTestOptions(c.Request.Context(), pelicanTestOptions{}))
				err = svc.processClaudeStream(c, strings.NewReader("data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"))
			}
			require.Error(t, err)
			_, message := parsePelicanOutput(rec.Body.String())
			require.NotEmpty(t, message)
		})
	}
	result := ModelTraceResult{Samples: []ModelTraceSample{
		{Accepted: true, Text: strings.Repeat("17,", 200), Challenge: modeltrace.Challenge{ExpectedCount: 300}},
		{Accepted: false, Text: strings.Repeat("83,", 300), Error: "generation_failed"},
		{Accepted: false, Text: "refused", Error: "too_few_numbers"},
	}}
	completeModelTraceResult(&result)
	require.Equal(t, "partial", result.Status)
	require.Equal(t, 1, result.Analysis.UsedOutputs)
	completeModelTraceResult(&ModelTraceResult{})
}

func TestModelTraceBareEOFIsNotAnAnthropicCompletion(t *testing.T) {
	c, rec := newTestContext()
	ctx := context.WithValue(c.Request.Context(), modelTraceContextKey{}, modelTraceEnvironment{Protocol: "messages"})
	c.Request = c.Request.WithContext(withPelicanTestOptions(ctx, pelicanTestOptions{}))
	stream := "data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"17,83,247\"}}\n\n"
	err := (&AccountTestService{}).processClaudeStream(c, strings.NewReader(stream))
	require.Error(t, err)
	_, message := parsePelicanOutput(rec.Body.String())
	require.Contains(t, message, "Stream ended before message_stop")
}

func TestModelTraceOAuthInstructionsAreRetainedAndOtherRoutesRejected(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		account := &Account{Platform: platform, Type: AccountTypeOAuth}
		env, err := modelTraceEnvironmentForAccount(account, "gpt-6-astra")
		require.NoError(t, err)
		ctx := context.WithValue(context.Background(), modelTraceContextKey{}, env)
		payload := map[string]any{"system": "required", "instructions": "required", "temperature": 1}
		applyModelTracePayload(ctx, payload)
		require.Equal(t, "required", payload["system"])
		require.Equal(t, "required", payload["instructions"])
		require.NotEqual(t, "none", env.SystemPrompt)
	}
	_, err := modelTraceEnvironmentForAccount(&Account{Platform: PlatformGemini, Type: AccountTypeAPIKey}, "gemini")
	require.Error(t, err)
	svc := &AccountTestService{accountRepo: &modelTraceRepo{account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}}}
	svc.modelTraceAccounts.Store(int64(77), struct{}{})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	require.Error(t, svc.TestModelTraceAccountConnection(c, 77, "gpt-6-astra", ""))
	require.Empty(t, rec.Body.String(), "busy validation must precede SSE headers")
}
