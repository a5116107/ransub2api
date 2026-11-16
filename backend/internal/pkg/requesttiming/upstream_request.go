package requesttiming

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/util/transportdiag"
	"github.com/tidwall/gjson"
)

// UpstreamRequestObservation describes the request actually read by the final
// transport, not a response model, billing alias or mutable account setting.
// It is metadata only: it attests neither model quality nor supplier grouping.
type UpstreamRequestObservation struct {
	Version              int    `json:"version"`
	Status               string `json:"status"`
	Source               string `json:"source"`
	Protocol             string `json:"protocol,omitempty"`
	Model                string `json:"model,omitempty"`
	ServiceTier          string `json:"service_tier,omitempty"`
	ReasoningEffort      string `json:"reasoning_effort,omitempty"`
	Speed                string `json:"speed,omitempty"`
	ThinkingMode         string `json:"thinking_mode,omitempty"`
	ThinkingBudgetTokens int64  `json:"thinking_budget_tokens,omitempty"`
	ThinkingLevel        string `json:"thinking_level,omitempty"`
	IncludeThoughts      string `json:"include_thoughts,omitempty"`
	Stream               bool   `json:"stream"`
}

const maxUpstreamRequestScopeBytes = 1024 * 1024

// ObserveUpstreamRequest does not pre-read, replay or replace request bytes.
// Only a complete bounded body consumed by the transport can supply metadata.
// The transient body buffer is discarded at EOF/Close and never serialized.
func ObserveUpstreamRequest(req *http.Request) *http.Request {
	if req == nil {
		return req
	}
	tr, _ := req.Context().Value(attemptKey{}).(*Trace)
	if tr == nil {
		return req
	}
	physical := false
	tr.update(func(a *Attempt) {
		physical = a.Kind == "http"
		if a.UpstreamRequest != nil {
			a.UpstreamRequest = &UpstreamRequestObservation{Version: 1, Status: "changed_request", Source: "http_transport_body"}
		}
	})
	if !physical || req.URL == nil || req.Method != http.MethodPost || req.Body == nil || req.ContentLength > maxUpstreamRequestScopeBytes || req.Header.Get("Content-Encoding") != "" {
		return req
	}
	protocol := transportdiag.HTTPRoute(req).Endpoint
	gemini := protocol == "gemini_generate_content" || protocol == "gemini_stream_generate_content"
	if protocol != "responses" && protocol != "responses_compact" && protocol != "chat_completions" && protocol != "messages" && !gemini {
		return req
	}
	model, envelope := "", false
	if gemini {
		prefix, _, _ := strings.Cut(req.URL.Path, ":")
		envelope = strings.HasSuffix(prefix, "/v1internal")
		if !envelope {
			index := strings.LastIndex(prefix, "/models/")
			if index < 0 || !geminiScopeModel(prefix[index+len("/models/"):]) {
				return req
			}
			model = strings.Clone(prefix[index+len("/models/"):])
		}
	}
	clone := new(http.Request)
	*clone = *req
	clone.Body = &upstreamRequestBody{ReadCloser: req.Body, ctx: req.Context(), protocol: protocol, expected: req.ContentLength, geminiModel: model, geminiEnvelope: envelope}
	return clone
}

type upstreamRequestBody struct {
	io.ReadCloser
	mu             sync.Mutex
	ctx            context.Context
	protocol       string
	expected       int64
	data           []byte
	done           bool
	readErr        bool
	geminiModel    string
	geminiEnvelope bool
}

func (b *upstreamRequestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return n, err
	}
	if n > maxUpstreamRequestScopeBytes-len(b.data) {
		b.done, b.data = true, nil
		return n, err
	}
	b.data = append(b.data, p[:n]...)
	b.readErr = b.readErr || err != nil && err != io.EOF
	if err == io.EOF || b.expected > 0 && int64(len(b.data)) == b.expected {
		b.finish()
	}
	return n, err
}

func (b *upstreamRequestBody) finish() {
	if !b.done && !b.readErr && (b.expected <= 0 || int64(len(b.data)) == b.expected) {
		if b.protocol == "gemini_generate_content" || b.protocol == "gemini_stream_generate_content" {
			recordUpstreamRequest(b.ctx, parseGeminiUpstreamRequest(b.protocol, b.geminiModel, b.geminiEnvelope, b.data))
		} else {
			recordUpstreamRequest(b.ctx, parseUpstreamRequest(b.protocol, b.data, false))
		}
	}
	b.done, b.data = true, nil
}

func (b *upstreamRequestBody) Close() error {
	// Do not hold the observation mutex while closing the transport body: Close
	// must remain able to unblock a concurrent Read.
	err := b.ReadCloser.Close()
	b.mu.Lock()
	b.done, b.data = true, nil
	b.mu.Unlock()
	return err
}

func requestScopeFields(raw gjson.Result) (map[string]gjson.Result, bool) {
	if !raw.IsObject() {
		return nil, false
	}
	fields, valid := make(map[string]gjson.Result), true
	raw.ForEach(func(k, v gjson.Result) bool {
		if _, exists := fields[k.Str]; exists {
			valid = false
			return false
		}
		fields[k.Str] = v
		return true
	})
	return fields, valid
}

func requestScopeOption(value gjson.Result, allowed ...string) (string, bool) {
	if !value.Exists() {
		return "unspecified", true
	}
	if value.Type == gjson.String {
		for _, option := range allowed {
			if value.Str == option {
				return option, true
			}
		}
	}
	return "", false
}

// Keep only a model identifier, never a Vertex project/location resource path.
func geminiScopeModel(model string) bool {
	if model == "" || len(model) > 200 {
		return false
	}
	for _, r := range model {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return model != "." && model != ".."
}

func parseGeminiUpstreamRequest(protocol, model string, envelope bool, payload []byte) *UpstreamRequestObservation {
	if len(payload) > maxUpstreamRequestScopeBytes || !gjson.ValidBytes(payload) {
		return nil
	}
	fields, valid := requestScopeFields(gjson.ParseBytes(payload))
	if !valid {
		return nil
	}
	source := "http_transport_url_body"
	if envelope {
		if fields["model"].Type != gjson.String || !geminiScopeModel(fields["model"].Str) || fields["generationConfig"].Exists() || fields["stream"].Exists() {
			return nil
		}
		model, source = strings.Clone(fields["model"].Str), "http_transport_body"
		fields, valid = requestScopeFields(fields["request"])
		if !valid {
			return nil
		}
	}
	for _, key := range []string{"model", "request", "stream", "thinking", "thinkingConfig", "reasoning", "reasoning_effort", "service_tier", "speed", "output_config"} {
		if fields[key].Exists() {
			return nil
		}
	}
	out := &UpstreamRequestObservation{Version: 1, Status: "observed", Source: source, Protocol: protocol, Model: model,
		ServiceTier: "unspecified", ReasoningEffort: "unspecified", ThinkingMode: "unspecified", IncludeThoughts: "unspecified", Stream: protocol == "gemini_stream_generate_content"}
	if config := fields["generationConfig"]; config.Exists() {
		values, ok := requestScopeFields(config)
		if !ok {
			return nil
		}
		if thinking := values["thinkingConfig"]; thinking.Exists() {
			options, ok := requestScopeFields(thinking)
			if !ok {
				return nil
			}
			for key := range options {
				if key != "thinkingBudget" && key != "thinkingLevel" && key != "includeThoughts" {
					return nil
				}
			}
			if thoughts := options["includeThoughts"]; thoughts.Exists() {
				if thoughts.Type != gjson.True && thoughts.Type != gjson.False {
					return nil
				}
				out.IncludeThoughts = "disabled"
				if thoughts.Bool() {
					out.IncludeThoughts = "enabled"
				}
			}
			budget, level := options["thinkingBudget"], options["thinkingLevel"]
			if budget.Exists() && level.Exists() {
				return nil
			}
			if budget.Exists() {
				n, err := strconv.ParseInt(budget.Raw, 10, 64)
				if budget.Type != gjson.Number || err != nil || n < -1 || n > 1<<53-1 {
					return nil
				}
				switch n {
				case -1:
					out.ThinkingMode = "budget_dynamic"
				case 0:
					out.ThinkingMode = "budget_zero"
				default:
					out.ThinkingMode, out.ThinkingBudgetTokens = "budget_fixed", n
				}
			} else if level.Exists() {
				out.ThinkingLevel, valid = requestScopeOption(level, "minimal", "low", "medium", "high", "MINIMAL", "LOW", "MEDIUM", "HIGH")
				if !valid {
					return nil
				}
				out.ThinkingMode = "level"
			}
		}
	}
	return out
}

func parseUpstreamRequest(protocol string, payload []byte, ws bool) *UpstreamRequestObservation {
	if len(payload) > maxUpstreamRequestScopeBytes || !gjson.ValidBytes(payload) {
		return nil
	}
	fields, valid := requestScopeFields(gjson.ParseBytes(payload))
	model := fields["model"]
	if !valid || model.Type != gjson.String || len(model.Str) == 0 || len(model.Str) > 200 {
		return nil
	}
	for _, r := range model.Str {
		if r <= ' ' || r > '~' {
			return nil
		}
	}
	out := &UpstreamRequestObservation{Version: 1, Status: "observed", Source: "http_transport_body", Protocol: protocol, Model: strings.Clone(model.Str), Stream: ws}
	if ws {
		if fields["type"].Type != gjson.String || fields["type"].Str != "response.create" {
			return nil
		}
		out.Source = "websocket_generation_payload"
	}
	if stream := fields["stream"]; stream.Exists() {
		if stream.Type != gjson.True && stream.Type != gjson.False {
			return nil
		}
		out.Stream = stream.Bool()
	}
	if protocol == "messages" {
		if ws || fields["reasoning"].Exists() || fields["reasoning_effort"].Exists() {
			return nil
		}
		out.ServiceTier, valid = requestScopeOption(fields["service_tier"], "auto", "standard_only")
		if !valid {
			return nil
		}
		out.Speed, valid = requestScopeOption(fields["speed"], "standard", "fast")
		if !valid {
			return nil
		}
		out.ThinkingMode = "unspecified"
		if thinking := fields["thinking"]; thinking.Exists() {
			values, ok := requestScopeFields(thinking)
			if !ok {
				return nil
			}
			out.ThinkingMode, ok = requestScopeOption(values["type"], "enabled", "disabled", "adaptive")
			if !ok || out.ThinkingMode == "unspecified" {
				return nil
			}
			budget := values["budget_tokens"]
			if out.ThinkingMode == "enabled" {
				n, err := strconv.ParseInt(budget.Raw, 10, 64)
				if budget.Type != gjson.Number || err != nil || n <= 0 || n > 1<<53-1 {
					return nil
				}
				out.ThinkingBudgetTokens = n
			} else if budget.Exists() {
				return nil
			}
		}
		effort := gjson.Result{}
		if config := fields["output_config"]; config.Exists() {
			values, ok := requestScopeFields(config)
			if !ok {
				return nil
			}
			effort = values["effort"]
		}
		out.ReasoningEffort, valid = requestScopeOption(effort, "low", "medium", "high", "max")
		if !valid {
			return nil
		}
		return out
	}
	out.ServiceTier, valid = requestScopeOption(fields["service_tier"], "auto", "default", "priority", "flex", "scale")
	if !valid {
		return nil
	}
	effort := fields["reasoning_effort"]
	if protocol != "chat_completions" {
		if effort.Exists() {
			return nil
		}
		if reasoning := fields["reasoning"]; reasoning.Exists() {
			values, ok := requestScopeFields(reasoning)
			if !ok {
				return nil
			}
			effort = values["effort"]
		}
	} else if fields["reasoning"].Exists() {
		return nil
	}
	out.ReasoningEffort, valid = requestScopeOption(effort, "none", "minimal", "low", "medium", "high", "xhigh", "max")
	if !valid {
		return nil
	}
	return out
}

func recordUpstreamRequest(ctx context.Context, observation *UpstreamRequestObservation) {
	if observation == nil {
		return
	}
	tr, _ := ctx.Value(attemptKey{}).(*Trace)
	if tr == nil {
		return
	}
	tr.update(func(a *Attempt) {
		if a.EndMS != nil {
			return
		}
		if a.UpstreamRequest == nil {
			a.UpstreamRequest = observation
		} else if *a.UpstreamRequest != *observation {
			a.UpstreamRequest = &UpstreamRequestObservation{Version: 1, Status: "changed_request", Source: observation.Source}
		}
	})
}

// RecordRequest receives the final generation payload immediately before the
// existing socket write, independently for every physical WS generation.
func (w *WSTurnCapture) RecordRequest(payload []byte) {
	if w != nil {
		recordUpstreamRequest(w.ctx, parseUpstreamRequest("responses", payload, true))
	}
}
