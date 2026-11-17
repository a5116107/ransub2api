package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/typesafe"
)

// Reported distinguishes complete zero usage from absent/invalid usage. Tokens
// may be a known lower bound when a stream returned only some usage fields.
type MonitoringProbeTokenUsage struct {
	Tokens   int64
	Reported bool
}

type monitoringProbeTokenKey struct{}

const monitoringProbeUsageMaxBytes = 1024 * 1024
const monitoringProbeTokenMax = int64(1<<53 - 1)

type monitoringProbeTokenCollector struct {
	mu                     sync.Mutex
	stream, overflow       bool
	imageJSON              *monitoringProbeImageJSONFilter
	buffer, event          []byte
	input, output, total   int64
	read, write            int64
	hasInput, hasOutput    bool
	hasTotal, hasAnthropic bool
	invalid                bool
	complete               bool
	gemini                 bool
}

// Image data is already consumed by the real adapter. Remove only safe string
// content from b64_json values before the existing bounded usage parser sees
// it; preserve keys, escapes and control bytes so malformed JSON stays invalid.
// This is a lexical filter, not a second usage or JSON validation algorithm.
type monitoringProbeImageJSONFilter struct {
	inString, escaped, discard bool
	field, value               bool
	unicode                    int
	start                      int
}

func (f *monitoringProbeImageJSONFilter) append(dst, data []byte) ([]byte, bool) {
	for _, b := range data {
		if f.inString && f.discard && !f.escaped && f.unicode == 0 && b >= 0x20 && b != '"' && b != '\\' {
			continue
		}
		if len(dst) >= monitoringProbeUsageMaxBytes {
			return nil, true
		}
		dst = append(dst, b)
		if f.inString {
			switch {
			case f.unicode > 0:
				f.unicode--
			case f.escaped:
				f.escaped = false
				if b == 'u' {
					f.unicode = 4
				}
			case b == '\\':
				f.escaped = true
			case b == '"':
				f.inString, f.field = false, false
				if !f.discard && len(dst)-f.start <= 128 {
					var name string
					f.field = json.Unmarshal(dst[f.start:], &name) == nil && name == "b64_json"
				}
				f.discard = false
			}
		} else if b == '"' {
			f.inString, f.discard, f.start = true, f.value, len(dst)-1
			f.field, f.value = false, false
		} else if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			f.value = b == ':' && f.field
			f.field = false
		}
	}
	return dst, false
}

func monitoringProbeGenerationPath(path string) bool {
	path = strings.TrimRight(path, "/")
	return strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, "/chat/completions") ||
		strings.HasSuffix(path, "/responses/compact") || strings.HasSuffix(path, "/completions") ||
		strings.HasSuffix(path, "/messages") || strings.HasSuffix(path, ":generateContent") ||
		strings.HasSuffix(path, ":streamGenerateContent") || strings.HasSuffix(path, ":rawPredict") ||
		strings.HasSuffix(path, ":streamRawPredict") ||
		strings.Contains(path, "/model/") && (strings.HasSuffix(path, "/invoke") || strings.HasSuffix(path, "/invoke-with-response-stream")) ||
		strings.HasSuffix(path, "/images/generations") ||
		strings.HasSuffix(path, "/images/edits") || strings.HasSuffix(path, typesafe.SystemOnePath)
}

func (c *monitoringProbeTokenCollector) setStream(stream bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stream = stream
}

// Observe bytes already read by the real caller. Never pre-read, replay, or
// retain response text after parsing; bounds also apply to individual SSE events.
func (c *monitoringProbeTokenCollector) feed(data []byte, eof bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.overflow {
		return
	}
	if !c.stream {
		if c.imageJSON != nil {
			c.buffer, c.overflow = c.imageJSON.append(c.buffer, data)
		} else if len(c.buffer)+len(data) > monitoringProbeUsageMaxBytes {
			c.overflow = true
		} else {
			c.buffer = append(c.buffer, data...)
		}
		if c.overflow {
			c.invalid, c.buffer = true, nil
			return
		}
		if eof {
			c.observe(c.buffer)
			c.buffer = nil
		}
		return
	}
	for _, b := range data {
		if b != '\n' {
			if len(c.buffer)+len(c.event) >= monitoringProbeUsageMaxBytes {
				c.overflow, c.invalid, c.buffer, c.event = true, true, nil, nil
				return
			}
			c.buffer = append(c.buffer, b)
			continue
		}
		line := bytes.TrimSuffix(c.buffer, []byte{'\r'})
		if len(line) == 0 {
			c.observe(c.event)
			c.event = nil
		} else if bytes.HasPrefix(line, []byte("data:")) {
			if len(c.event) > 0 {
				c.event = append(c.event, '\n')
			}
			c.event = append(c.event, bytes.TrimPrefix(line[5:], []byte{' '})...)
		}
		c.buffer = nil
	}
	// An incomplete SSE frame is not a returned usage event, even at EOF.
	if eof && c.gemini && len(c.buffer) == 0 && len(c.event) == 0 {
		c.complete = true
	}
}

// Reject duplicate fields and coercions rather than treating malformed counters
// as free detection. This parser is private to actual monitoring responses.
func monitoringProbeUsageObject(raw []byte) (map[string]json.RawMessage, bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return nil, false
	}
	out := make(map[string]json.RawMessage)
	for d.More() {
		name, err := d.Token()
		key, ok := name.(string)
		if err != nil || !ok || len(out) >= 128 {
			return nil, false
		}
		if _, exists := out[key]; exists {
			return nil, false
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, false
		}
		out[key] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, false
	}
	_, err = d.Token()
	return out, err == io.EOF
}

func (c *monitoringProbeTokenCollector) observe(data []byte) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	// Nonstream Gemini adapters may return an array of streamed envelopes.
	if data[0] == '[' {
		var frames []json.RawMessage
		if json.Unmarshal(data, &frames) != nil || len(frames) > 1024 {
			c.invalid = true
			return
		}
		for _, frame := range frames {
			if bytes.HasPrefix(bytes.TrimSpace(frame), []byte{'['}) {
				c.invalid = true
				return
			}
			c.observe(frame)
		}
		return
	}
	outer, ok := monitoringProbeUsageObject(data)
	if !ok {
		c.invalid = true
		return
	}
	var kind string
	_ = json.Unmarshal(outer["type"], &kind)
	if kind == "response.completed" || kind == "response.done" || kind == "response.failed" || kind == "response.incomplete" || kind == "message_stop" {
		c.complete = true
	}
	for _, name := range []string{"response", "message"} {
		if raw, exists := outer[name]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			inner, valid := monitoringProbeUsageObject(raw)
			if !valid {
				c.invalid = true
				return
			}
			outer = inner
			break
		}
	}
	usageRaw, hasUsage := outer["usage"]
	gemini := false
	if raw, exists := outer["usageMetadata"]; exists {
		if hasUsage {
			c.invalid = true
			return
		}
		usageRaw, hasUsage, gemini = raw, true, true
	}
	if !hasUsage || bytes.Equal(bytes.TrimSpace(usageRaw), []byte("null")) {
		return
	}
	if _, chat := outer["choices"]; chat {
		c.complete = true
	}
	u, ok := monitoringProbeUsageObject(usageRaw)
	if !ok {
		c.invalid = true
		return
	}
	values := make(map[string]int64)
	for _, name := range []string{"input_tokens", "prompt_tokens", "output_tokens", "completion_tokens", "total_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "totalTokenCount", "promptTokenCount", "candidatesTokenCount", "thoughtsTokenCount", "toolUsePromptTokenCount"} {
		if raw, exists := u[name]; exists {
			value, err := strconv.ParseInt(string(raw), 10, 64)
			if err != nil || value < 0 || value > monitoringProbeTokenMax {
				c.invalid = true
				return
			}
			values[name] = value
		}
	}
	if gemini {
		c.gemini = true
		if input, exists := values["promptTokenCount"]; exists {
			c.input, c.hasInput = slices.Max([]int64{c.input, input}), true
		}
		output := values["candidatesTokenCount"] + values["thoughtsTokenCount"] + values["toolUsePromptTokenCount"]
		c.output = slices.Max([]int64{c.output, output})
		if _, exists := values["candidatesTokenCount"]; exists {
			c.hasOutput = true
		}
		if total, exists := values["totalTokenCount"]; exists {
			c.total, c.hasTotal = slices.Max([]int64{c.total, total}), true
		}
		return
	}
	for _, pair := range [][2]string{{"input_tokens", "prompt_tokens"}, {"output_tokens", "completion_tokens"}} {
		_, a := values[pair[0]]
		_, b := values[pair[1]]
		if a && b {
			c.invalid = true
			return
		}
	}
	if kind == "message_start" || kind == "message_delta" || len(outer["stop_reason"]) > 0 || len(u["cache_read_input_tokens"]) > 0 || len(u["cache_creation_input_tokens"]) > 0 {
		c.hasAnthropic = true
	}
	for _, name := range []string{"input_tokens", "prompt_tokens"} {
		if value, exists := values[name]; exists {
			c.input, c.hasInput = slices.Max([]int64{c.input, value}), true
		}
	}
	for _, name := range []string{"output_tokens", "completion_tokens"} {
		if value, exists := values[name]; exists {
			c.output, c.hasOutput = slices.Max([]int64{c.output, value}), true
		}
	}
	c.read = slices.Max([]int64{c.read, values["cache_read_input_tokens"]})
	c.write = slices.Max([]int64{c.write, values["cache_creation_input_tokens"]})
	if value, exists := values["total_tokens"]; exists {
		c.total, c.hasTotal = slices.Max([]int64{c.total, value}), true
	}
}

func (c *monitoringProbeTokenCollector) snapshot() MonitoringProbeTokenUsage {
	if c == nil {
		return MonitoringProbeTokenUsage{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stream && len(c.buffer) > 0 {
		// json.Decoder callers may close after one complete object without
		// issuing the extra Read that would return EOF.
		c.observe(c.buffer)
	}
	c.buffer, c.event = nil, nil
	tokens := c.input + c.output
	if c.hasAnthropic {
		tokens += c.read + c.write
	}
	tokens = slices.Max([]int64{tokens, c.total})
	complete := !c.stream || c.complete
	consistent := !c.hasTotal || !c.hasInput || !c.hasOutput || c.total >= c.input+c.output
	return MonitoringProbeTokenUsage{Tokens: tokens, Reported: !c.invalid && complete && consistent && (c.hasTotal || c.hasInput && c.hasOutput)}
}

func observeMonitoringProbeWSUsage(ctx context.Context, data []byte) {
	if c, _ := ctx.Value(monitoringProbeTokenKey{}).(*monitoringProbeTokenCollector); c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(data) > monitoringProbeUsageMaxBytes {
			c.invalid = true
			return
		}
		c.observe(data)
	}
}
