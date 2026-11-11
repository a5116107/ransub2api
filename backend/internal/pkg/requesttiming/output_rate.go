package requesttiming

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// OutputUsage is canonical terminal usage, separate from billing coercions.
// ContentStatus admits only text and reasoning, whose reported tokens can be
// separated. It retains no generated content.
type OutputUsage struct {
	OutputTokens    int64  `json:"output_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens"`
	Status          string `json:"status"`
	ContentStatus   string `json:"content_status"`
}

// OutputRateObservation is an estimate over parser observations, not a model
// token clock, a client delivery rate, or throughput of hidden reasoning.
type OutputRateObservation struct {
	TokensPerSecond    *float64 `json:"tokens_per_second"`
	WindowMS           *float64 `json:"window_ms"`
	OutputTokens       *int64   `json:"output_tokens"`
	ReasoningTokens    *int64   `json:"reasoning_tokens"`
	NonReasoningTokens *int64   `json:"non_reasoning_tokens"`
	Status             string   `json:"status"`
	Source             string   `json:"source"`
	Scope              string   `json:"scope"`
	Method             string   `json:"method"`
}

func unknownOutputRate() OutputRateObservation {
	return OutputRateObservation{Status: "not_collected", Source: "none",
		Scope:  "final_physical_http_responses_text_at_parser",
		Method: "non_reasoning_tokens_over_first_text_to_terminal"}
}

func readOutputUsage(raw json.RawMessage) *OutputUsage {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	out := &OutputUsage{Status: "invalid_counters"}
	if len(raw) > 64*1024 {
		out.Status = "diagnostic_limit"
		return out
	}
	usage, valid := cacheObject(raw)
	if !valid {
		return out
	}
	if len(usage["output_tokens"]) == 0 || len(usage["output_tokens_details"]) == 0 {
		out.Status = "not_reported"
		return out
	}
	details, valid := cacheObject(usage["output_tokens_details"])
	if !valid {
		return out
	}
	if len(details["reasoning_tokens"]) == 0 {
		out.Status = "not_reported"
		return out
	}
	if len(details) != 1 {
		out.Status = "unsupported_usage"
		return out
	}
	output, outputErr := strconv.ParseInt(string(usage["output_tokens"]), 10, 64)
	reasoning, reasoningErr := strconv.ParseInt(string(details["reasoning_tokens"]), 10, 64)
	if outputErr != nil || reasoningErr != nil || output < 0 || output > 1<<53-1 || reasoning < 0 || reasoning > output {
		return out
	}
	out.OutputTokens, out.ReasoningTokens, out.Status = output, reasoning, "observed"
	return out
}

func outputContentStatus(envelope map[string]json.RawMessage) string {
	if status, exists := envelope["status"]; exists {
		var value string
		if json.Unmarshal(status, &value) != nil || value != "completed" {
			return "invalid_terminal"
		}
	}
	if len(envelope["output"]) == 0 {
		return "not_reported"
	}
	if len(envelope["output"]) > 1024*1024 {
		return "diagnostic_limit"
	}
	var items []json.RawMessage
	if json.Unmarshal(envelope["output"], &items) != nil || len(items) > 4096 {
		return "unsupported_output"
	}
	text := false
	for _, raw := range items {
		item, valid := cacheObject(raw)
		if !valid {
			return "unsupported_output"
		}
		var kind string
		if json.Unmarshal(item["type"], &kind) != nil {
			return "unsupported_output"
		}
		if status, present := item["status"]; present {
			var value string
			if json.Unmarshal(status, &value) != nil || value != "completed" {
				return "invalid_terminal"
			}
		}
		if kind == "reasoning" {
			continue
		}
		var content []json.RawMessage
		if kind != "message" || json.Unmarshal(item["content"], &content) != nil || len(content) > 4096 {
			return "unsupported_output"
		}
		for _, rawPart := range content {
			part, valid := cacheObject(rawPart)
			var partKind, value string
			if !valid || json.Unmarshal(part["type"], &partKind) != nil || partKind != "output_text" ||
				json.Unmarshal(part["text"], &value) != nil {
				return "unsupported_output"
			}
			text = text || value != ""
		}
	}
	if !text {
		return "no_text_output"
	}
	return "text_and_reasoning"
}

func deriveOutputRate(s Snapshot, a Attempt, stream TextStreamObservation) OutputRateObservation {
	out := unknownOutputRate()
	raw := a.OutputUsage
	if raw == nil {
		return out
	}
	out.Source = "canonical_responses_terminal_usage_and_text_events"
	out.Status = raw.Status
	if raw.Status != "observed" {
		return out
	}
	if raw.OutputTokens < 0 || raw.OutputTokens > 1<<53-1 || raw.ReasoningTokens < 0 || raw.ReasoningTokens > raw.OutputTokens {
		out.Status = "invalid_counters"
		return out
	}
	if _, gap := a.Events["typed_content_truncated"]; gap {
		out.Status = "incomplete_capture"
		return out
	}
	if a.Terminal != "completed" || a.Error != "" || a.Status < 200 || a.Status >= 300 ||
		s.Status < 200 || s.Status >= 300 || s.Terminal != "completed" || s.Canceled || s.ClientDisconnect || s.DownstreamError ||
		(s.Outcome != "" && s.Outcome != "success") {
		out.Status = "partial"
		return out
	}
	if raw.ContentStatus != "text_and_reasoning" {
		out.Status = raw.ContentStatus
		if out.Status == "" {
			out.Status = "not_collected"
		}
		return out
	}
	output, reasoning := raw.OutputTokens, raw.ReasoningTokens
	nonReasoning := output - reasoning
	out.OutputTokens, out.ReasoningTokens, out.NonReasoningTokens = &output, &reasoning, &nonReasoning
	if stream.Status != "observed" || a.TextStream == nil {
		out.Status = "insufficient_stream"
		if stream.Status == "invalid_boundary" {
			out.Status = "invalid_boundary"
		}
		return out
	}
	if first, present := a.Events["first_visible_text"]; !present || first != a.TextStream.FirstMS {
		out.Status = "incomplete_stream"
		return out
	}
	if a.TextStream.NonTextOutput {
		out.Status = "unsupported_output"
		return out
	}
	if nonReasoning < 32 {
		out.Status = "short_output"
		return out
	}
	if a.TextStream.Chunks < 2 {
		out.Status = "insufficient_stream"
		return out
	}
	window := a.TextStream.TerminalMS - a.TextStream.FirstMS
	if !finite(window) || window < 250 || stream.SpanMS == nil || *stream.SpanMS < 250 {
		out.Status = "insufficient_window"
		return out
	}
	rate := float64(nonReasoning) / (window / 1000)
	if !finite(rate) || rate <= 0 {
		out.Status = "invalid_boundary"
		return out
	}
	out.WindowMS, out.TokensPerSecond, out.Status = &window, &rate, "estimated"
	return out
}
