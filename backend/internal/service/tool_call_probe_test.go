//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func toolProbeWire(round int, usage string) string {
	if round == 1 {
		return "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"arguments\":\"{\\\"a\\\":7,\\\"b\\\":11}\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"add_numbers\",\"arguments\":\"{\\\"a\\\":7,\\\"b\\\":11}\"}]" + usage + "}}\n\n"
	}
	return "data: {\"type\":\"response.output_text.delta\",\"delta\":\"18\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"18\"}]}]" + usage + "}}\n\n"
}

const toolProbeUsage = `,"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":4}}`

func TestToolCallProbeRoundtripAndBudget(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, wantStatus, reason string
		budget, calls                           int
	}{
		{"complete", toolProbeWire(1, toolProbeUsage), toolProbeWire(2, toolProbeUsage), "pass", "completed", 1024, 2},
		{"budget_reached", toolProbeWire(1, toolProbeUsage), "", "deferred", "token_budget_reached", 12, 1},
		{"inflight_overage", toolProbeWire(1, toolProbeUsage), "", "deferred", "token_budget_reached", 1, 1},
		{"missing_usage", toolProbeWire(1, ""), "", "deferred", "usage_unavailable", 1024, 1},
		{"invalid_usage", toolProbeWire(1, `,"usage":{"input_tokens":-1,"output_tokens":2}`), "", "deferred", "usage_unavailable", 1024, 1},
		{"missing_terminal", "data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{}\"}\n\n", "", "inconclusive", "incomplete_response", 1024, 1},
		{"wrong_arguments", strings.ReplaceAll(toolProbeWire(1, toolProbeUsage), `\"a\":7`, `\"a\":8`), "", "fail", "invalid_tool_call", 1024, 1},
		{"duplicate_terminal", toolProbeWire(1, toolProbeUsage) + toolProbeWire(1, toolProbeUsage), "", "inconclusive", "invalid_stream", 1024, 1},
		{"text_is_not_tool", toolProbeWire(2, toolProbeUsage), "", "fail", "invalid_tool_call", 1024, 1},
		{"wrong_final", toolProbeWire(1, toolProbeUsage), strings.ReplaceAll(toolProbeWire(2, toolProbeUsage), "18", "19"), "fail", "invalid_tool_result", 1024, 2},
		{"second_missing_usage", toolProbeWire(1, toolProbeUsage), toolProbeWire(2, ""), "inconclusive", "usage_unavailable", 1024, 2},
		{"error_after_complete", toolProbeWire(1, toolProbeUsage) + "data: {\"type\":\"error\",\"message\":\"fixture-secret\"}\n\n", "", "inconclusive", "invalid_stream", 1024, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bodies [][]byte
			var sessions []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				bodies = append(bodies, body)
				sessions = append(sessions, r.Header.Get("Session-Id"))
				require.Equal(t, "/v1/responses", r.URL.Path)
				require.Equal(t, "Bearer fixture-key", r.Header.Get("Authorization"))
				require.Equal(t, "mapped-model", gjson.GetBytes(body, "model").String())
				require.Equal(t, int64(512), gjson.GetBytes(body, "max_output_tokens").Int())
				w.Header().Set("Content-Type", "text/event-stream")
				if len(bodies) == 1 {
					_, _ = io.WriteString(w, tc.first)
				} else {
					_, _ = io.WriteString(w, tc.second)
				}
			}))
			defer server.Close()
			account := &Account{ID: 91, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{"base_url": server.URL, "api_key": "fixture-key", "model_mapping": map[string]any{"public-model": "mapped-model"}}, Extra: map[string]any{"api_protocol": "responses"}}
			upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
				return server.Client().Do(req)
			}}
			svc := pelicanClaudeTestService(account, upstream)
			svc.cfg = rawChatCompletionsTestConfig()
			result, err := svc.RunPelicanBackground(t.Context(), account.ID, "public-model", &PelicanTestConfig{QuestionKind: ToolCallQuestionKind, ReasoningEffort: "low", ParallelCount: 1, ToolCallTokenBudget: tc.budget})
			require.NoError(t, err)
			require.NotNil(t, result)
			probe := result.PelicanConfig.ToolCallResult
			require.NotNil(t, probe)
			require.Equal(t, tc.wantStatus, probe.Status)
			require.Equal(t, tc.reason, probe.Reason)
			require.Equal(t, tc.calls, len(bodies))
			require.Equal(t, tc.calls, len(probe.Rounds))
			require.NotEmpty(t, sessions[0])
			if tc.calls == 2 {
				require.Equal(t, sessions[0], sessions[1])
				require.Equal(t, gjson.GetBytes(bodies[0], "prompt_cache_key").String(), gjson.GetBytes(bodies[1], "prompt_cache_key").String())
				require.Equal(t, "call_1", gjson.GetBytes(bodies[1], "input.2.call_id").String())
				require.Equal(t, `{"sum":18}`, gjson.GetBytes(bodies[1], "input.2.output").String())
			}
			require.Equal(t, "inconclusive", result.PelicanConfig.AvailabilityResult.Status, "tool result must not grant text admission")
			raw, err := json.Marshal(probe)
			require.NoError(t, err)
			for _, secret := range []string{"fixture-key", server.URL, "call_1", "fixture-secret"} {
				require.NotContains(t, string(raw), secret)
			}
			if tc.wantStatus == "pass" {
				require.Equal(t, "success", result.Status)
				require.True(t, probe.ArgumentsVerified)
				require.True(t, probe.ResultVerified)
				require.EqualValues(t, 24, probe.ObservedTokens)
			} else {
				require.NotEqual(t, "success", result.Status)
			}
		})
	}
}

func TestToolCallPlanValidation(t *testing.T) {
	valid := func() *ScheduledTestPlan {
		return &ScheduledTestPlan{ModelID: "public-model", CronExpression: "*/30 * * * *", MaxResults: 20, PelicanConfig: &PelicanTestConfig{QuestionKind: ToolCallQuestionKind, ReasoningEffort: "low", ParallelCount: 1, ToolCallTokenBudget: 1024}}
	}
	plan := valid()
	plan.PelicanConfig.ToolCallResult = &ToolCallProbeResult{Status: "pass"}
	_, err := nextPlanRun(plan, time.Now())
	require.NoError(t, err)
	require.Nil(t, plan.PelicanConfig.ToolCallResult)
	for _, mutate := range []func(*ScheduledTestPlan){
		func(p *ScheduledTestPlan) { p.AutoRecover = true },
		func(p *ScheduledTestPlan) { p.PelicanConfig.Availability = &AvailabilityPolicy{ValidForSeconds: 120} },
		func(p *ScheduledTestPlan) { p.PelicanConfig.Quality = &QualityPolicy{Action: QualityActionObserveOnly} },
		func(p *ScheduledTestPlan) { p.PelicanConfig.ParallelCount = 2 },
		func(p *ScheduledTestPlan) { p.PelicanConfig.ModelIDs = []string{"a", "b"} },
		func(p *ScheduledTestPlan) { p.PelicanConfig.ToolCallTokenBudget = -1 },
		func(p *ScheduledTestPlan) { p.PelicanConfig.ToolCallTokenBudget = 1000001 },
		func(p *ScheduledTestPlan) { p.PelicanConfig.TestChannel = "bps" },
	} {
		p := valid()
		mutate(p)
		_, err := nextPlanRun(p, time.Now())
		require.Error(t, err)
	}
}

func TestToolCallProbeUnsupportedAndCanceledNeverSend(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformGemini, PlatformOpenAI} {
		t.Run(platform, func(t *testing.T) {
			account := &Account{ID: 91, Platform: platform, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "fixture-key"}}
			calls := 0
			upstream := &codexModelsHTTPUpstreamStub{do: func(*http.Request, string, int64, int) (*http.Response, error) { calls++; return nil, context.Canceled }}
			svc := pelicanClaudeTestService(account, upstream)
			result, err := svc.RunPelicanBackground(t.Context(), account.ID, "target", &PelicanTestConfig{QuestionKind: ToolCallQuestionKind, ReasoningEffort: "low", ParallelCount: 1, ToolCallTokenBudget: 1024})
			require.NoError(t, err)
			require.Zero(t, calls)
			require.Equal(t, "deferred", result.PelicanConfig.ToolCallResult.Status)
		})
	}
}

func TestToolCallSchedulePersistsEvidenceWithoutQualityActions(t *testing.T) {
	plans, results := &pelicanPlanRepo{}, &pelicanResults{}
	runner := &ScheduledTestRunnerService{planRepo: plans, scheduledSvc: NewScheduledTestService(plans, results)}
	runner.judgeQuality = func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment {
		t.Fatal("tool monitoring must not invoke an intelligence judge")
		return nil
	}
	calls := 0
	runner.runPelican = func(_ context.Context, _ int64, model string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
		calls++
		snapshot := *cfg
		snapshot.ModelID = model
		snapshot.ToolCallResult = &ToolCallProbeResult{Version: 1, Status: "deferred", Reason: "token_budget_reached", ObservedTokens: 12}
		return &ScheduledTestResult{Status: "failed", ErrorMessage: "token_budget_reached", PelicanConfig: &snapshot}, nil
	}
	plan := &ScheduledTestPlan{ID: 1, AccountID: 91, ModelID: "target", Enabled: true, MaxResults: 20, CronExpression: "*/30 * * * *", PelicanConfig: &PelicanTestConfig{QuestionKind: ToolCallQuestionKind, ReasoningEffort: "low", ParallelCount: 1, ToolCallTokenBudget: 12}}
	require.True(t, runner.runOnePlan(t.Context(), plan))
	require.True(t, plans.finished)
	require.Equal(t, 1, calls, "budget deferral must not enter answer retries")
	require.Len(t, results.results, 1)
	saved := results.results[0]
	require.Nil(t, saved.QualityJudgment)
	require.Empty(t, saved.QualityAction)
	require.Equal(t, "target", saved.PelicanConfig.ModelID)
	require.Equal(t, "deferred", saved.PelicanConfig.ToolCallResult.Status)
	require.Nil(t, plan.PelicanConfig.ToolCallResult, "result evidence must not mutate the plan")
}

func TestToolCallStreamBoundaries(t *testing.T) {
	for _, wire := range []string{
		toolProbeWire(1, toolProbeUsage) + "data: {}",
		toolProbeWire(1, toolProbeUsage) + "data: [DONE]\n\ndata: {}\n\n",
		strings.Repeat(": heartbeat\n\n", 180000),
		"data: " + strings.Repeat("a", 1<<20) + "\n\n",
	} {
		_, reason := readToolCallStream(strings.NewReader(wire))
		require.NotEmpty(t, reason, "truncation, trailing data and size limits must not produce a pass")
	}
}
