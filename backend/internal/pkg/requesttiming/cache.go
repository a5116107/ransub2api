package requesttiming

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// CacheUsage retains canonical wire counters from one physical generation.
// InputTokens is the total input, including read/write buckets. Messages also
// retains its independent uncached bucket and presence bits for partial updates.
// Billing compatibility coercions are deliberately not cache evidence.
type CacheUsage struct {
	Protocol       string `json:"protocol,omitempty"`
	InputTokens    int64  `json:"input_tokens"`
	ReadTokens     int64  `json:"read_tokens"`
	WriteTokens    int64  `json:"write_tokens"`
	WriteReported  bool   `json:"write_reported"`
	Status         string `json:"status"`
	UncachedTokens int64  `json:"uncached_tokens,omitempty"`
	InputReported  bool   `json:"input_reported,omitempty"`
	ReadReported   bool   `json:"read_reported,omitempty"`
	messageStart   bool
}

// CacheObservation describes an observed past response, not cache retention,
// cross-account sharing, or a prediction of the next request's hit rate.
type CacheObservation struct {
	InputTokens *int64   `json:"input_tokens"`
	ReadTokens  *int64   `json:"read_tokens"`
	WriteTokens *int64   `json:"write_tokens"`
	ReadRatio   *float64 `json:"read_ratio"`
	Hit         *bool    `json:"hit"`
	Status      string   `json:"status"`
	Source      string   `json:"source"`
	Scope       string   `json:"scope"`
}

func unknownCacheObservation() CacheObservation {
	return CacheObservation{Status: "not_collected", Source: "none", Scope: "final_physical_http_responses_terminal_usage"}
}

// RecordAccountRoute annotates the existing egress attempt or WS turn with the immutable
// credential/route digest used by continuation guards, never credentials.
func RecordAccountRoute(ctx context.Context, fingerprint [32]byte) {
	if fingerprint == [32]byte{} || ctx == nil {
		return
	}
	tr, _ := ctx.Value(attemptKey{}).(*Trace)
	if tr != nil {
		tr.update(func(a *Attempt) {
			if (a.Kind == "egress" || a.Kind == "ws_turn") && a.RouteFingerprint == "" {
				a.RouteFingerprint = hex.EncodeToString(fingerprint[:])
			}
		})
	}
}

func cacheObject(raw []byte) (map[string]json.RawMessage, bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	out := make(map[string]json.RawMessage)
	for d.More() {
		if len(out) >= 32 {
			return nil, false
		}
		token, err = d.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, false
		}
		if _, duplicate := out[key]; duplicate {
			return nil, false
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, false
		}
		out[key] = value
	}
	if _, err = d.Token(); err != nil {
		return nil, false
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, false
	}
	return out, true
}

func readCacheUsage(raw json.RawMessage, protocols ...string) *CacheUsage {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	out := &CacheUsage{Status: "invalid_counters"}
	inputKey, detailsKey := "input_tokens", "input_tokens_details"
	if len(protocols) > 0 && protocols[0] == "chat_completions" {
		out.Protocol = "chat_completions"
		inputKey, detailsKey = "prompt_tokens", "prompt_tokens_details"
	}
	if len(raw) > 64*1024 {
		out.Status = "diagnostic_limit"
		return out
	}
	usage, ok := cacheObject(raw)
	if !ok {
		return out
	}
	if out.Protocol == "chat_completions" {
		for _, alias := range []string{"input_tokens", "input_tokens_details", "cache_read_input_tokens", "cache_creation_input_tokens", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens"} {
			if _, present := usage[alias]; present {
				out.Status = "ambiguous_counters"
				return out
			}
		}
	}
	details, ok := cacheObject(usage[detailsKey])
	if !ok {
		if _, present := usage[detailsKey]; present && out.Protocol == "chat_completions" {
			return out
		}
		out.Status = "not_reported"
		return out
	}
	input, inputErr := strconv.ParseInt(string(usage[inputKey]), 10, 64)
	read, readErr := strconv.ParseInt(string(details["cached_tokens"]), 10, 64)
	if len(usage[inputKey]) == 0 || len(details["cached_tokens"]) == 0 {
		out.Status = "not_reported"
		return out
	}
	if inputErr != nil || readErr != nil || input < 0 || read < 0 || read > input || input > 1<<53-1 {
		return out
	}
	out.InputTokens, out.ReadTokens = input, read
	if rawWrite, reported := details["cache_write_tokens"]; reported {
		write, err := strconv.ParseInt(string(rawWrite), 10, 64)
		if err != nil || write < 0 || write > input-read {
			return out
		}
		out.WriteTokens, out.WriteReported = write, true
	}
	out.Status = "observed"
	return out
}

func readTerminalUsage(data []byte) (*CacheUsage, *OutputUsage) {
	outer, ok := cacheObject(data)
	if !ok {
		return &CacheUsage{Status: "invalid_counters"}, &OutputUsage{Status: "invalid_counters"}
	}
	envelope := outer
	usage := outer["usage"]
	if response, exists := outer["response"]; exists {
		if !bytes.Equal(bytes.TrimSpace(response), []byte("null")) {
			r, valid := cacheObject(response)
			if !valid {
				return &CacheUsage{Status: "invalid_counters"}, &OutputUsage{Status: "invalid_counters"}
			}
			if reported, present := r["usage"]; present {
				usage = reported
			} else {
				// Keep the existing cache compatibility fallback, but do not
				// combine output counters and text from different envelopes.
				return readCacheUsage(usage), &OutputUsage{Status: "not_reported"}
			}
			envelope = r
		}
	}
	output := readOutputUsage(usage)
	if output != nil {
		output.ContentStatus = outputContentStatus(envelope)
	}
	return readCacheUsage(usage), output
}

func cacheCounter(usage map[string]json.RawMessage, name string) (int64, bool, bool) {
	raw, present := usage[name]
	if !present {
		return 0, false, true
	}
	v, err := strconv.ParseInt(string(raw), 10, 64)
	return v, true, err == nil && v >= 0 && v <= 1<<53-1
}

func readMessagesCacheUsage(data []byte, eventType string) *CacheUsage {
	out := &CacheUsage{Protocol: "messages", Status: "invalid_counters", messageStart: eventType == "message_start" || eventType == "message"}
	event, ok := cacheObject(data)
	if !ok {
		return out
	}
	if eventType == "message_start" {
		event, ok = cacheObject(event["message"])
		if !ok {
			return out
		}
	}
	if eventType == "message_delta" {
		if rawDelta, present := event["delta"]; present && !bytes.Equal(bytes.TrimSpace(rawDelta), []byte("null")) {
			if _, valid := cacheObject(rawDelta); !valid {
				return out
			}
		}
	}
	raw, present := event["usage"]
	if !present {
		if out.messageStart {
			out.Status = "not_reported"
			return out
		}
		return nil
	}
	if len(raw) > 64*1024 {
		out.Status = "diagnostic_limit"
		return out
	}
	usage, ok := cacheObject(raw)
	if !ok {
		return out
	}
	// Some compatible providers redefine input_tokens when prompt aliases
	// are present. Their billing normalization is not Anthropic wire evidence.
	for _, name := range []string{"prompt_tokens", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens", "prompt_tokens_details"} {
		if _, present := usage[name]; present {
			out.Status = "ambiguous_counters"
			return out
		}
	}
	var valid bool
	out.UncachedTokens, out.InputReported, valid = cacheCounter(usage, "input_tokens")
	if !valid {
		return out
	}
	out.ReadTokens, out.ReadReported, valid = cacheCounter(usage, "cache_read_input_tokens")
	if !valid {
		return out
	}
	out.WriteTokens, out.WriteReported, valid = cacheCounter(usage, "cache_creation_input_tokens")
	if !valid {
		return out
	}
	if raw, reported := usage["cache_creation"]; reported {
		details, ok := cacheObject(raw)
		if !ok {
			return out
		}
		five, hasFive, validFive := cacheCounter(details, "ephemeral_5m_input_tokens")
		hour, hasHour, validHour := cacheCounter(details, "ephemeral_1h_input_tokens")
		if !validFive || !validHour || (out.WriteReported &&
			((hasFive && five > out.WriteTokens) || (hasHour && hour > out.WriteTokens) ||
				(hasFive && hasHour && five+hour != out.WriteTokens))) {
			return out
		}
	}
	out.Status = "not_reported"
	return out
}

func readGeminiCacheUsage(data []byte) *CacheUsage {
	out := &CacheUsage{Protocol: "gemini", Status: "invalid_counters"}
	event, ok := cacheObject(data)
	if !ok {
		return out
	}
	if inner, wrapped := event["response"]; wrapped && !bytes.Equal(bytes.TrimSpace(inner), []byte("null")) {
		event, ok = cacheObject(inner)
		if !ok {
			return out
		}
	}
	raw, present := event["usageMetadata"]
	if !present {
		return nil
	}
	if len(raw) > 64*1024 {
		out.Status = "diagnostic_limit"
		return out
	}
	usage, ok := cacheObject(raw)
	if !ok {
		return out
	}
	input, hasInput, validInput := cacheCounter(usage, "promptTokenCount")
	read, hasRead, validRead := cacheCounter(usage, "cachedContentTokenCount")
	if !validInput || !validRead || (hasInput && hasRead && read > input) {
		return out
	}
	if !hasInput || !hasRead {
		out.Status = "not_reported"
		return out
	}
	out.InputTokens, out.ReadTokens, out.Status = input, read, "observed"
	return out
}

// Invalid observations remain invalid for the generation. Missing Gemini
// counters are not filled from a previous chunk; Messages deltas, in contrast,
// explicitly update independent cumulative buckets, including reported zeros.
func updateProtocolCache(a *Attempt, next *CacheUsage) {
	if next == nil {
		return
	}
	previous := a.CacheUsage
	if previous != nil && previous.Status != "observed" && previous.Status != "not_reported" {
		return
	}
	if previous != nil && (previous.Protocol != next.Protocol || next.messageStart) {
		previous.Status = "invalid_counters"
		return
	}
	if next.Protocol != "messages" || (next.Status != "not_reported" && next.Status != "observed") {
		a.CacheUsage = next
		return
	}
	if previous == nil {
		previous = &CacheUsage{Protocol: "messages"}
		a.CacheUsage = previous
	}
	if next.InputReported {
		previous.UncachedTokens, previous.InputReported = next.UncachedTokens, true
	}
	if next.ReadReported {
		previous.ReadTokens, previous.ReadReported = next.ReadTokens, true
	}
	if next.WriteReported {
		previous.WriteTokens, previous.WriteReported = next.WriteTokens, true
	}
	previous.Status = "not_reported"
	if previous.InputReported && previous.ReadReported && previous.WriteReported {
		total := previous.UncachedTokens + previous.ReadTokens + previous.WriteTokens
		if total > 1<<53-1 {
			previous.Status = "invalid_counters"
			return
		}
		previous.InputTokens, previous.Status = total, "observed"
	}
}

func deriveCacheObservation(s Snapshot, a Attempt) CacheObservation {
	out := unknownCacheObservation()
	protocol := "responses"
	if a.TextStream != nil && a.TextStream.Protocol != "" {
		protocol = a.TextStream.Protocol
	} else if a.CacheUsage != nil && a.CacheUsage.Protocol != "" {
		protocol = a.CacheUsage.Protocol
	} else if a.CacheUsage == nil && a.Route.Endpoint == "messages" {
		protocol = "messages"
	} else if a.CacheUsage == nil && strings.HasPrefix(a.Route.Endpoint, "gemini_") {
		protocol = "gemini"
	}
	source := "canonical_responses_terminal_usage"
	switch protocol {
	case "messages":
		source, out.Scope = "canonical_messages_cumulative_usage", "final_physical_http_messages_cumulative_usage"
	case "gemini":
		source, out.Scope = "canonical_gemini_usage_metadata", "final_physical_http_gemini_usage_metadata"
	case "chat_completions":
		source, out.Scope = "canonical_chat_completions_usage", "final_physical_http_chat_completions_usage"
	}
	if a.Kind == "ws_turn" && s.TTFTMode == "ws_turn" {
		out.Scope = "physical_websocket_turn_responses_terminal_usage"
	}
	if raw := a.CacheUsage; raw != nil {
		out.Source = source
		out.Status = raw.Status
		rawProtocol := raw.Protocol
		if rawProtocol == "" {
			rawProtocol = "responses" // JSON written before protocol tagging.
		}
		if rawProtocol != protocol || (protocol != "responses" && protocol != "messages" && protocol != "gemini" && protocol != "chat_completions") ||
			(protocol != "responses" && (a.TextStream == nil || a.TextStream.Protocol != protocol)) ||
			(a.Kind == "ws_turn" && protocol != "responses") {
			out.Status = "invalid_protocol"
			return out
		}
		if raw.Status != "observed" {
			return out
		}
		if _, gap := a.Events["typed_content_truncated"]; gap || (a.TextStream != nil && a.TextStream.Truncated) {
			out.Status = "incomplete_capture"
			return out
		}
		if raw.InputTokens < 0 || raw.InputTokens > 1<<53-1 || raw.ReadTokens < 0 || raw.ReadTokens > raw.InputTokens ||
			(raw.WriteReported && (raw.WriteTokens < 0 || raw.WriteTokens > raw.InputTokens-raw.ReadTokens)) {
			out.Status = "invalid_counters"
			return out
		}
		if protocol == "messages" && (!raw.InputReported || !raw.ReadReported || !raw.WriteReported ||
			raw.UncachedTokens < 0 || raw.UncachedTokens > raw.InputTokens ||
			raw.UncachedTokens != raw.InputTokens-raw.ReadTokens-raw.WriteTokens) {
			out.Status = "invalid_counters"
			return out
		}
		if protocol == "gemini" && raw.WriteReported {
			out.Status = "invalid_counters"
			return out
		}
		if !contentAttemptSucceeded(s, a) {
			out.Status = "partial"
			return out
		}
		if a.TextStream != nil && a.TextStream.Invalid {
			out.Status = "invalid_boundary"
			return out
		}
		if raw.InputTokens == 0 {
			out.Status = "empty_input"
			return out
		}
		input, read := raw.InputTokens, raw.ReadTokens
		ratio, hit := float64(read)/float64(input), read > 0
		out.InputTokens, out.ReadTokens, out.ReadRatio, out.Hit = &input, &read, &ratio, &hit
		if raw.WriteReported {
			write := raw.WriteTokens
			out.WriteTokens = &write
		}
	}
	return out
}
