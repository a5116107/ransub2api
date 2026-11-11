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
	Protocol        string `json:"protocol,omitempty"`
	Final           bool   `json:"final,omitempty"`
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

func readOutputUsage(raw json.RawMessage, protocols ...string) *OutputUsage {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	out := &OutputUsage{Status: "invalid_counters"}
	protocol := "responses"
	if len(protocols) > 0 && protocols[0] != "responses" {
		protocol, out.Protocol = protocols[0], protocols[0]
		// Older readers ignore protocol/final fields. A distinct content tag
		// keeps their Responses-only gate from estimating this new wire data.
		out.ContentStatus = protocol + "_text_and_reasoning"
	}
	if len(raw) > 64*1024 {
		out.Status = "diagnostic_limit"
		return out
	}
	usage, valid := cacheObject(raw)
	if !valid {
		return out
	}
	if protocol != "responses" {
		for key := range usage {
			wrong := (protocol != "gemini" && (key == "candidatesTokenCount" || key == "thoughtsTokenCount")) ||
				(protocol != "chat_completions" && (key == "completion_tokens" || key == "completion_tokens_details")) ||
				(protocol != "messages" && (key == "output_tokens" || key == "output_tokens_details"))
			if wrong {
				out.Status = "unsupported_usage"
				return out
			}
		}
	}
	if protocol == "gemini" {
		text, hasText, validText := cacheCounter(usage, "candidatesTokenCount")
		thoughts, hasThoughts, validThoughts := cacheCounter(usage, "thoughtsTokenCount")
		if !validText || !validThoughts || text > (1<<53-1)-thoughts {
			return out
		}
		if !hasText || !hasThoughts {
			out.Status = "not_reported"
			return out
		}
		if raw, present := usage["candidatesTokensDetails"]; present {
			var details []json.RawMessage
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &details) != nil || len(details) > 4096 {
				return out
			}
			var total int64
			for _, rawDetail := range details {
				detail, valid := cacheObject(rawDetail)
				var modality string
				n, reported, validCount := cacheCounter(detail, "tokenCount")
				if !valid || !validCount || !reported || total > (1<<53-1)-n {
					return out
				}
				if json.Unmarshal(detail["modality"], &modality) != nil || modality != "TEXT" {
					out.Status = "unsupported_usage"
					return out
				}
				total += n
			}
			if total != text {
				return out
			}
		}
		out.OutputTokens, out.ReasoningTokens, out.Status = text+thoughts, thoughts, "observed"
		return out
	}
	outputKey, detailsKey, reasoningKey := "output_tokens", "output_tokens_details", "reasoning_tokens"
	switch protocol {
	case "responses":
	case "chat_completions":
		outputKey, detailsKey = "completion_tokens", "completion_tokens_details"
	case "messages":
		reasoningKey = "thinking_tokens"
	default:
		return out
	}
	if len(usage[outputKey]) == 0 || len(usage[detailsKey]) == 0 {
		out.Status = "not_reported"
		return out
	}
	details, valid := cacheObject(usage[detailsKey])
	if !valid {
		return out
	}
	if len(details[reasoningKey]) == 0 {
		out.Status = "not_reported"
		return out
	}
	for key, raw := range details {
		if key == reasoningKey {
			continue
		}
		// Chat commonly reports explicit zero audio/prediction buckets. Any
		// nonzero or unknown bucket prevents a pure generated-text estimate.
		if protocol != "chat_completions" || (key != "audio_tokens" && key != "accepted_prediction_tokens" && key != "rejected_prediction_tokens") {
			out.Status = "unsupported_usage"
			return out
		}
		n, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil || n < 0 || n > 1<<53-1 {
			return out
		}
		if n != 0 {
			out.Status = "unsupported_usage"
			return out
		}
	}
	output, outputErr := strconv.ParseInt(string(usage[outputKey]), 10, 64)
	reasoning, reasoningErr := strconv.ParseInt(string(details[reasoningKey]), 10, 64)
	if outputErr != nil || reasoningErr != nil || output < 0 || output > 1<<53-1 || reasoning < 0 || reasoning > output {
		return out
	}
	out.OutputTokens, out.ReasoningTokens, out.Status = output, reasoning, "observed"
	return out
}

func readProtocolOutputUsage(data []byte, protocol, eventType string) *OutputUsage {
	invalid := &OutputUsage{Protocol: protocol, Status: "invalid_counters"}
	event, valid := cacheObject(data)
	if !valid {
		return invalid
	}
	if protocol == "gemini" {
		if raw, wrapped := event["response"]; wrapped && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			event, valid = cacheObject(raw)
		}
	} else if eventType == "message_start" {
		event, valid = cacheObject(event["message"])
	}
	if !valid {
		return invalid
	}
	key := "usage"
	if protocol == "gemini" {
		key = "usageMetadata"
	}
	return readOutputUsage(event[key], protocol)
}

// Keep invalid counters sticky and never fill missing final fields with an
// earlier cumulative report. Only a protocol completion boundary can seal them.
func updateProtocolOutput(a *Attempt, next *OutputUsage, final bool) {
	if next == nil {
		return
	}
	if previous := a.OutputUsage; previous != nil {
		if previous.Status != "observed" && previous.Status != "not_reported" {
			return
		}
		if previous.Protocol != next.Protocol {
			previous.Status = "invalid_counters"
			return
		}
		if previous.Status == "observed" && next.Status == "observed" &&
			(next.OutputTokens < previous.OutputTokens || next.ReasoningTokens < previous.ReasoningTokens) {
			previous.Status = "invalid_counters"
			return
		}
	}
	next.Final = next.Final || final
	a.OutputUsage = next
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
	if a.Kind == "ws_turn" && s.TTFTMode == "ws_turn" {
		out.Scope = "physical_websocket_turn_responses_text_at_parser"
	}
	raw := a.OutputUsage
	if raw == nil {
		return out
	}
	out.Source = "canonical_responses_terminal_usage_and_text_events"
	protocol := raw.Protocol
	if protocol == "" {
		protocol = "responses" // Original JSON predates protocol tags.
	}
	streamProtocol := "responses"
	if a.TextStream != nil && a.TextStream.Protocol != "" {
		streamProtocol = a.TextStream.Protocol
	}
	if protocol != streamProtocol || (protocol != "responses" && protocol != "messages" && protocol != "gemini" && protocol != "chat_completions") ||
		(a.Kind == "ws_turn" && protocol != "responses") {
		out.Status, out.Source = "invalid_protocol", "none"
		return out
	}
	if protocol != "responses" {
		out.Source = "canonical_" + protocol + "_usage_and_text_events"
		out.Scope = "final_physical_http_" + protocol + "_text_at_parser"
	}
	out.Status = raw.Status
	if raw.Status != "observed" {
		return out
	}
	if raw.OutputTokens < 0 || raw.OutputTokens > 1<<53-1 || raw.ReasoningTokens < 0 || raw.ReasoningTokens > raw.OutputTokens {
		out.Status = "invalid_counters"
		return out
	}
	if _, gap := a.Events["typed_content_truncated"]; gap || (a.TextStream != nil && a.TextStream.Truncated) {
		out.Status = "incomplete_capture"
		return out
	}
	if !contentAttemptSucceeded(s, a) || s.Terminal != "completed" {
		out.Status = "partial"
		return out
	}
	if a.TextStream != nil && a.TextStream.Invalid {
		out.Status = "invalid_boundary"
		return out
	}
	if protocol != "responses" && !raw.Final {
		out.Status = "not_reported"
		return out
	}
	if _, multiple := a.Events["typed_output_multiple_candidates"]; multiple {
		out.Status = "unsupported_output"
		return out
	}
	contentStatus := "text_and_reasoning"
	if protocol != "responses" {
		contentStatus = protocol + "_text_and_reasoning"
	}
	if raw.ContentStatus != contentStatus {
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
