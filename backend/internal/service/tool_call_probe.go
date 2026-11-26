package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const ToolCallQuestionKind = "tool_call"

type toolCallProbeKey struct{}

// ToolCallProbeResult is an independent capability observation, not text
// availability, model authenticity, intelligence or scheduling permission.
// Only allowlisted conclusions and actual usage are retained, never history.
type ToolCallProbeResult struct {
	Version           int                  `json:"version"`
	Status            string               `json:"status"`
	Reason            string               `json:"reason"`
	Protocol          string               `json:"protocol"`
	Model             string               `json:"model,omitempty"`
	TokenBudget       int                  `json:"token_budget"`
	ObservedTokens    int64                `json:"observed_tokens"`
	ArgumentsVerified bool                 `json:"arguments_verified"`
	ResultVerified    bool                 `json:"result_verified"`
	Rounds            []ToolCallProbeRound `json:"rounds"`
}

type ToolCallProbeRound struct {
	HTTPStatus   int    `json:"http_status"`
	LatencyMs    int64  `json:"latency_ms"`
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	CachedTokens *int64 `json:"cached_tokens"`
}

func isToolCallPlan(cfg *PelicanTestConfig) bool {
	return cfg != nil && cfg.QuestionKind == ToolCallQuestionKind
}

func validateToolCallPlan(plan *ScheduledTestPlan) error {
	cfg := plan.PelicanConfig
	if !isToolCallPlan(cfg) {
		if cfg.ToolCallTokenBudget != 0 {
			return fmt.Errorf("tool budget requires a tool-call test")
		}
		return nil
	}
	if cfg.Quality != nil || cfg.Availability != nil || plan.AutoRecover || cfg.TestChannel == "bps" || cfg.ParallelCount != 1 || len(cfg.ModelIDs) > 1 {
		return fmt.Errorf("tool-call test requires one model, one sample and no quality, availability or auto-recovery actions")
	}
	if cfg.ToolCallTokenBudget == 0 {
		cfg.ToolCallTokenBudget = 1024
	}
	if cfg.ToolCallTokenBudget < 1 || cfg.ToolCallTokenBudget > 1000000 {
		return fmt.Errorf("tool-call token budget must be 1–1000000")
	}
	cfg.Prompt, cfg.ActualPrompt = "", ""
	return nil
}

func (s *AccountTestService) runToolCallBackground(ctx context.Context, accountID int64, model string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
	started := time.Now()
	snapshot := *cfg
	snapshot.ModelID, snapshot.ActualPrompt = model, ""
	probe := &ToolCallProbeResult{Version: 1, Status: "inconclusive", Reason: "not_observed", Protocol: "responses_http", TokenBudget: cfg.ToolCallTokenBudget, Rounds: []ToolCallProbeRound{}}
	if probe.TokenBudget == 0 {
		probe.TokenBudget = 1024
	}
	snapshot.ToolCallResult = probe
	ctx, cancel := context.WithTimeout(context.WithValue(ctx, toolCallProbeKey{}, probe), 90*time.Second)
	defer cancel()
	w := &pelicanRecorder{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	err := s.TestPelicanAccountConnection(c, accountID, model, "", "low")
	if ctx.Err() != nil {
		probe.Status, probe.Reason = "inconclusive", "canceled"
		if ctx.Err() == context.DeadlineExceeded {
			probe.Reason = "timeout"
		}
	} else if err != nil && probe.Reason == "not_observed" {
		probe.Reason = "local_admission"
	}
	finished := time.Now()
	snapshot.AvailabilityResult = &AvailabilityProbeResult{Version: 1, Status: "inconclusive", Reason: "tool_probe_not_text", StartedAt: started, FinishedAt: finished}
	status, message := "failed", probe.Reason
	if probe.Status == "pass" {
		status, message = "success", ""
	}
	return &ScheduledTestResult{Status: status, ErrorMessage: message, LatencyMs: finished.Sub(started).Milliseconds(), StartedAt: started, FinishedAt: finished, PelicanConfig: &snapshot}, nil
}

func (s *AccountTestService) toolCallUnsupportedReason(ctx context.Context, account *Account, model string) string {
	if s.pluginManager != nil && s.pluginManager.ShouldRouteOpenAIOAuth(account) {
		return "metering_unavailable"
	}
	if account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey || account.IsCredentialShadow() || account.HasManagedOpenAIConnections() || account.OpenAIRequestProxyEnabled() || account.IsSyntheticUITest() || account.IsSIWCEnabled() || account.IsExcelBPSEnabled() || account.IsPrismBrowserEnabledForModel(model) || account.gatewayProtocolsPending() || isOpenAIImageModel(account.GetMappedModel(model)) || !openai_compat.ShouldUseResponsesAPI(account.Extra) {
		return "unsupported_route"
	}
	if s.openaiGatewayService != nil && s.openaiGatewayService.getOpenAIWSProtocolResolver().Resolve(account).Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		return "unsupported_route"
	}
	if ctx.Err() != nil {
		return "canceled"
	}
	return ""
}

func (s *AccountTestService) runOpenAIToolCallProbe(c *gin.Context, template *http.Request, account *Account, proxy string, probe *ToolCallProbeResult) error {
	modelBody, ok := availabilityRequestPayload(template)
	if !ok {
		return s.sendErrorAndEnd(c, "local_admission")
	}
	probe.Model = gjson.GetBytes(modelBody, "model").String()
	if probe.Model == "" || probe.TokenBudget < 1 || probe.TokenBudget > 1000000 {
		return s.sendErrorAndEnd(c, "local_admission")
	}
	session := "tool-probe-" + uuid.NewString()
	question := json.RawMessage(`{"role":"user","content":[{"type":"input_text","text":"Use add_numbers to compute 7 + 11. After its result, answer with only the integer 18."}]}`)
	payload := map[string]any{
		"model": probe.Model, "input": []json.RawMessage{question}, "stream": true, "store": false,
		"instructions": "Complete the fixed function call and return its integer result.",
		"tools":        json.RawMessage(`[{"type":"function","name":"add_numbers","description":"Add two integers.","strict":true,"parameters":{"type":"object","properties":{"a":{"type":"integer","enum":[7]},"b":{"type":"integer","enum":[11]}},"required":["a","b"],"additionalProperties":false}}]`),
		"tool_choice":  map[string]string{"type": "function", "name": "add_numbers"}, "parallel_tool_calls": false,
		"max_output_tokens": 512, "reasoning": map[string]string{"effort": "low"}, "include": []string{"reasoning.encrypted_content"}, "prompt_cache_key": session,
	}
	fingerprint := AccountRouteFingerprint(account)
	finish := func(status, reason string) error {
		probe.Status, probe.Reason = status, reason
		return s.sendErrorAndEnd(c, reason)
	}
	for round := 0; round < 2; round++ {
		current, err := s.accountRepo.GetByID(template.Context(), account.ID)
		if err != nil || current == nil || AccountRouteFingerprint(current) != fingerprint {
			return finish("deferred", "route_changed")
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return finish("inconclusive", "local_admission")
		}
		req := template.Clone(WithHTTPUpstreamRedirectsDisabled(template.Context()))
		req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		req.Header.Set("Session-Id", session)
		start := time.Now()
		// No application retry, credential fallback or TLS retry. Every physical
		// call uses the original test transport, shared Key admission and proxy.
		resp, err := s.doOpenAIAccountTestUpstream(req, proxy, account, false, body)
		if err != nil {
			return finish("inconclusive", "transport_error")
		}
		if resp == nil || resp.Body == nil {
			return finish("inconclusive", "incomplete_response")
		}
		observation := ToolCallProbeRound{HTTPStatus: resp.StatusCode}
		stream, reason := func() (toolCallStream, string) {
			defer func() { _ = resp.Body.Close() }()
			stop := context.AfterFunc(req.Context(), func() { _ = resp.Body.Close() })
			defer stop()
			sentIdentity, sentOK := availabilityRequestIdentity(req)
			gotIdentity, gotOK := availabilityRequestIdentity(resp.Request)
			if !sentOK || !gotOK || sentIdentity != gotIdentity || resp.Request.URL == nil || resp.Request.URL.String() != req.URL.String() || resp.Request.Method != req.Method || resp.Request.Host != req.Host {
				return toolCallStream{}, "response_source_unconfirmed"
			}
			if resp.StatusCode != http.StatusOK {
				return toolCallStream{}, "request_rejected"
			}
			if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
				return toolCallStream{}, "invalid_stream"
			}
			return readToolCallStream(resp.Body)
		}()
		observation.LatencyMs = time.Since(start).Milliseconds()
		observation.InputTokens, observation.OutputTokens, observation.CachedTokens = toolCallUsage(stream.response)
		probe.Rounds = append(probe.Rounds, observation)
		if observation.InputTokens != nil && observation.OutputTokens != nil {
			probe.ObservedTokens += *observation.InputTokens + *observation.OutputTokens
		}
		if reason != "" {
			return finish("inconclusive", reason)
		}
		if round == 0 {
			call, valid := toolCallArguments(stream)
			if !valid {
				return finish("fail", "invalid_tool_call")
			}
			probe.ArgumentsVerified = true
			if observation.InputTokens == nil || observation.OutputTokens == nil {
				return finish("deferred", "usage_unavailable")
			}
			if probe.ObservedTokens >= int64(probe.TokenBudget) {
				return finish("deferred", "token_budget_reached")
			}
			input := []json.RawMessage{question}
			for _, item := range stream.response.Get("output").Array() {
				input = append(input, json.RawMessage(item.Raw))
			}
			result, _ := json.Marshal(map[string]string{"type": "function_call_output", "call_id": call.Get("call_id").String(), "output": `{"sum":18}`})
			payload["input"], payload["tool_choice"] = append(input, result), "none"
		} else {
			if !toolCallFinalAnswer(stream) {
				return finish("fail", "invalid_tool_result")
			}
			probe.ResultVerified = true
			if observation.InputTokens == nil || observation.OutputTokens == nil {
				return finish("inconclusive", "usage_unavailable")
			}
		}
	}
	probe.Status, probe.Reason = "pass", "completed"
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

type toolCallStream struct {
	response  gjson.Result
	arguments []gjson.Result
	text      strings.Builder
}

func readToolCallStream(body io.Reader) (toolCallStream, string) {
	stream := toolCallStream{}
	limited := &io.LimitedReader{R: body, N: (2 << 20) + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 32<<10), 1<<20)
	var data []string
	completed, ended := false, false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			continue
		}
		if line != "" || len(data) == 0 {
			continue
		}
		raw := strings.Join(data, "\n")
		data = nil
		if ended {
			return stream, "invalid_stream"
		}
		if raw == "[DONE]" {
			ended = true
			continue
		}
		if !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
			return stream, "invalid_stream"
		}
		event := gjson.Parse(raw)
		switch event.Get("type").String() {
		case "error", "response.failed", "response.incomplete":
			return stream, "invalid_stream"
		case "response.completed":
			if completed {
				return stream, "invalid_stream"
			}
			completed, stream.response = true, event.Get("response")
			if stream.response.Get("status").String() != "completed" || !stream.response.Get("output").IsArray() {
				return stream, "invalid_stream"
			}
		case "response.function_call_arguments.done":
			if completed {
				return stream, "invalid_stream"
			}
			stream.arguments = append(stream.arguments, event)
		case "response.output_text.delta":
			if completed {
				return stream, "invalid_stream"
			}
			if _, err := stream.text.WriteString(event.Get("delta").String()); err != nil {
				return stream, "invalid_stream"
			}
		}
	}
	if scanner.Err() != nil || limited.N == 0 || len(data) != 0 {
		return stream, "invalid_stream"
	}
	if !completed {
		return stream, "incomplete_response"
	}
	return stream, ""
}

func toolCallArguments(stream toolCallStream) (gjson.Result, bool) {
	var call gjson.Result
	for _, item := range stream.response.Get("output").Array() {
		if !item.IsObject() {
			return call, false
		}
		if item.Get("type").String() == "function_call" {
			if call.Exists() {
				return call, false
			}
			call = item
		} else if item.Get("type").String() != "reasoning" {
			return call, false
		}
	}
	validArguments := func(raw string) bool {
		if !gjson.Valid(raw) {
			return false
		}
		fields, valid := availabilityJSONObject(gjson.Parse(raw))
		count := 0
		gjson.Parse(raw).ForEach(func(_, _ gjson.Result) bool { count++; return true })
		return valid && len(fields) == 2 && count == 2 && fields["a"].Raw == "7" && fields["b"].Raw == "11"
	}
	return call, call.Get("name").String() == "add_numbers" && call.Get("call_id").String() != "" && call.Get("id").String() != "" && validArguments(call.Get("arguments").String()) && len(stream.arguments) == 1 && stream.arguments[0].Get("item_id").String() == call.Get("id").String() && validArguments(stream.arguments[0].Get("arguments").String())
}

func toolCallFinalAnswer(stream toolCallStream) bool {
	var text strings.Builder
	for _, item := range stream.response.Get("output").Array() {
		if !item.IsObject() {
			return false
		}
		switch item.Get("type").String() {
		case "reasoning":
		case "message":
			if item.Get("role").String() != "assistant" || !item.Get("content").IsArray() {
				return false
			}
			for _, part := range item.Get("content").Array() {
				if part.Get("type").String() != "output_text" {
					return false
				}
				if _, err := text.WriteString(part.Get("text").String()); err != nil {
					return false
				}
			}
		default:
			return false
		}
	}
	return len(stream.arguments) == 0 && strings.TrimSpace(text.String()) == "18" && strings.TrimSpace(stream.text.String()) == "18"
}

func toolCallUsage(response gjson.Result) (*int64, *int64, *int64) {
	integer := func(value gjson.Result) *int64 {
		if value.Type != gjson.Number {
			return nil
		}
		n, err := strconv.ParseInt(value.Raw, 10, 64)
		if err != nil || n < 0 || n > 1<<50 {
			return nil
		}
		return &n
	}
	input, output := integer(response.Get("usage.input_tokens")), integer(response.Get("usage.output_tokens"))
	cached := integer(response.Get("usage.input_tokens_details.cached_tokens"))
	if cached != nil && (input == nil || *cached > *input) {
		cached = nil
	}
	return input, output, cached
}
