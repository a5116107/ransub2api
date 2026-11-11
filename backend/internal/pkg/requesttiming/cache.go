package requesttiming

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
)

// CacheUsage retains only canonical Responses counters from one terminal
// event. Billing compatibility coercions are deliberately not cache evidence.
type CacheUsage struct {
	InputTokens   int64  `json:"input_tokens"`
	ReadTokens    int64  `json:"read_tokens"`
	WriteTokens   int64  `json:"write_tokens"`
	WriteReported bool   `json:"write_reported"`
	Status        string `json:"status"`
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

func readCacheUsage(raw json.RawMessage) *CacheUsage {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	out := &CacheUsage{Status: "invalid_counters"}
	if len(raw) > 64*1024 {
		out.Status = "diagnostic_limit"
		return out
	}
	usage, ok := cacheObject(raw)
	if !ok {
		return out
	}
	details, ok := cacheObject(usage["input_tokens_details"])
	if !ok {
		out.Status = "not_reported"
		return out
	}
	input, inputErr := strconv.ParseInt(string(usage["input_tokens"]), 10, 64)
	read, readErr := strconv.ParseInt(string(details["cached_tokens"]), 10, 64)
	if len(usage["input_tokens"]) == 0 || len(details["cached_tokens"]) == 0 {
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

func deriveCacheObservation(s Snapshot, a Attempt) CacheObservation {
	out := unknownCacheObservation()
	if raw := a.CacheUsage; raw != nil {
		out.Source = "canonical_responses_terminal_usage"
		out.Status = raw.Status
		if raw.Status != "observed" {
			return out
		}
		if _, gap := a.Events["typed_content_truncated"]; gap {
			out.Status = "incomplete_capture"
			return out
		}
		if raw.InputTokens < 0 || raw.InputTokens > 1<<53-1 || raw.ReadTokens < 0 || raw.ReadTokens > raw.InputTokens ||
			(raw.WriteReported && (raw.WriteTokens < 0 || raw.WriteTokens > raw.InputTokens-raw.ReadTokens)) {
			out.Status = "invalid_counters"
			return out
		}
		if a.Terminal != "completed" || a.Error != "" || a.Status < 200 || a.Status >= 300 ||
			s.Status < 200 || s.Status >= 300 || s.Canceled || s.ClientDisconnect || s.DownstreamError {
			out.Status = "partial"
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
