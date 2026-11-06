package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/tidwall/gjson"
)

// AvailabilityRequestScope describes only observed wire capabilities. It stores
// no prompt, URL, credential or arbitrary request option. Unknown capabilities
// are deliberately not inferred from a successful text probe.
type AvailabilityRequestScope struct {
	Version         int    `json:"version"`
	Protocol        string `json:"protocol"`
	Stream          bool   `json:"stream"`
	Model           string `json:"model"`
	ServiceTier     string `json:"service_tier"`
	ReasoningEffort string `json:"reasoning_effort"`
	Capability      string `json:"capability"`
}

type availabilityBPSSourceKey struct{}
type availabilityBPSSource struct{ scope *AvailabilityRequestScope }

func withAvailabilityBPSSource(ctx context.Context, body []byte, bridge *basispoints.Bridge) context.Context {
	var scope *AvailabilityRequestScope
	if bridge != nil && !bridge.HasClientTools() {
		request := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: "/v1/responses"}, Header: make(http.Header)}
		scope = availabilityScopeForRequest(request, body)
	}
	return context.WithValue(ctx, availabilityBPSSourceKey{}, availabilityBPSSource{scope})
}

func availabilityBPSScopeForRequest(request *http.Request, payload []byte) *AvailabilityRequestScope {
	source, _ := request.Context().Value(availabilityBPSSourceKey{}).(availabilityBPSSource)
	if source.scope == nil || request.URL.String() != basispoints.ResponsesURL ||
		(request.Host != "" && !strings.EqualFold(request.Host, request.URL.Host)) {
		return nil
	}
	fields, valid := availabilityJSONObject(gjson.ParseBytes(payload))
	if !valid || fields["model"].Type != gjson.String || fields["model"].String() != source.scope.Model ||
		fields["model_selection"].String() != "explicit" || fields["stream"].Type != gjson.True ||
		fields["store"].Type != gjson.False || !fields["input"].IsArray() || !availabilityTextInput(fields["input"], false) {
		return nil
	}
	management := fields["context_management"]
	if !management.IsArray() || len(management.Array()) != 1 {
		return nil
	}
	compact, valid := availabilityJSONObject(management.Array()[0])
	if !valid || len(compact) != 2 || compact["type"].String() != "compaction" ||
		compact["compact_threshold"].Type != gjson.Number || compact["compact_threshold"].Raw != "920000" {
		return nil
	}
	metadata, valid := availabilityJSONObject(fields["metadata"])
	if !valid || len(metadata) != 3 || metadata["task_id"].Type != gjson.String || metadata["turn_id"].Type != gjson.String ||
		metadata["agent_iteration"].Type != gjson.String || metadata["agent_iteration"].String() != "1" {
		return nil
	}
	if cacheKey := fields["prompt_cache_key"]; cacheKey.Exists() && cacheKey.Type != gjson.String {
		return nil
	}
	for name := range fields {
		switch name {
		case "model", "model_selection", "stream", "store", "input", "reasoning_effort", "context_management", "metadata", "prompt_cache_key":
		default:
			return nil
		}
	}
	effort, valid := availabilityOption(fields["reasoning_effort"], "low", "medium", "high", "xhigh")
	if !valid || effort == "unspecified" {
		return nil
	}
	scope := *source.scope
	scope.Protocol, scope.ReasoningEffort = "bps_responses", effort
	return &scope
}

func availabilityJSONObject(value gjson.Result) (map[string]gjson.Result, bool) {
	if !value.IsObject() {
		return nil, false
	}
	fields, valid := make(map[string]gjson.Result), true
	value.ForEach(func(key, item gjson.Result) bool {
		name := key.String()
		if name != strings.ToLower(name) {
			valid = false
			return false
		}
		if previous, ok := fields[name]; ok && previous.Raw != item.Raw {
			valid = false
			return false
		}
		fields[name] = item
		return true
	})
	return fields, valid
}

func availabilityOption(value gjson.Result, options ...string) (string, bool) {
	if !value.Exists() {
		return "unspecified", true
	}
	if value.Type == gjson.String {
		for _, option := range options {
			if value.String() == option {
				return option, true
			}
		}
	}
	return "", false
}

func availabilityTextContent(value gjson.Result) bool {
	if value.Type == gjson.String {
		return true
	}
	if !value.IsArray() {
		return false
	}
	valid := true
	value.ForEach(func(_, item gjson.Result) bool {
		fields, ok := availabilityJSONObject(item)
		if !ok || len(fields) != 2 || fields["text"].Type != gjson.String {
			valid = false
			return false
		}
		switch fields["type"].String() {
		case "text", "input_text", "output_text":
		default:
			valid = false
		}
		return valid
	})
	return valid
}

func availabilityTextInput(value gjson.Result, chat bool) bool {
	if !value.Exists() || (!chat && value.Type == gjson.String) {
		return true
	}
	if !value.IsArray() {
		return false
	}
	valid := true
	value.ForEach(func(_, item gjson.Result) bool {
		fields, ok := availabilityJSONObject(item)
		if !ok || !availabilityTextContent(fields["content"]) {
			valid = false
			return false
		}
		for name := range fields {
			if name != "role" && name != "content" && name != "type" {
				valid = false
				return false
			}
		}
		if kind := fields["type"]; kind.Exists() && (chat || kind.Type != gjson.String || kind.String() != "message") {
			valid = false
			return false
		}
		switch fields["role"].String() {
		case "system", "developer", "user", "assistant":
		default:
			valid = false
		}
		return valid
	})
	return valid
}

func availabilityScopeForRequest(request *http.Request, payload []byte) *AvailabilityRequestScope {
	if request == nil || request.URL == nil || request.Method != http.MethodPost || request.URL.RawQuery != "" || request.Header.Get("Content-Encoding") != "" || !gjson.ValidBytes(payload) {
		return nil
	}
	_, isBPS := request.Context().Value(availabilityBPSSourceKey{}).(availabilityBPSSource)
	if isBPS || request.URL.String() == basispoints.ResponsesURL {
		return availabilityBPSScopeForRequest(request, payload)
	}
	fields, valid := availabilityJSONObject(gjson.ParseBytes(payload))
	if !valid || fields["model"].Type != gjson.String {
		return nil
	}
	model := fields["model"].String()
	if len(model) == 0 || len(model) > 200 {
		return nil
	}
	for _, r := range model {
		if r <= ' ' || r > '~' {
			return nil
		}
	}
	scope := &AvailabilityRequestScope{Version: 1, Model: model, Capability: "text"}
	if stream := fields["stream"]; stream.Exists() {
		if stream.Type != gjson.True && stream.Type != gjson.False {
			return nil
		}
		scope.Stream = stream.Bool()
	}
	switch {
	case strings.HasSuffix(request.URL.Path, "/responses"):
		scope.Protocol = "responses_http"
	case strings.HasSuffix(request.URL.Path, "/chat/completions"):
		scope.Protocol = "chat_http"
	default:
		return nil
	}
	scope.ServiceTier, valid = availabilityOption(fields["service_tier"], "auto", "default", "priority", "flex", "scale")
	if !valid {
		return nil
	}
	scope.ReasoningEffort, valid = availabilityOption(fields["reasoning_effort"], "none", "minimal", "low", "medium", "high", "xhigh", "max")
	if !valid {
		return nil
	}
	for name, value := range fields {
		switch name {
		case "model", "service_tier", "stream", "stream_options", "max_tokens", "max_completion_tokens", "max_output_tokens", "temperature", "top_p", "top_k", "presence_penalty", "frequency_penalty", "seed", "store", "metadata", "user", "safety_identifier", "prompt_cache_key", "prompt_cache_retention", "truncation":
		case "instructions":
			if value.Type != gjson.String {
				return nil
			}
		case "input":
			if scope.Protocol != "responses_http" || !availabilityTextInput(value, false) {
				return nil
			}
		case "messages":
			if scope.Protocol != "chat_http" || !availabilityTextInput(value, true) {
				return nil
			}
		case "reasoning_effort":
			if scope.Protocol != "chat_http" {
				return nil
			}
		case "reasoning":
			reasoning, ok := availabilityJSONObject(value)
			if !ok || scope.Protocol != "responses_http" {
				return nil
			}
			for option := range reasoning {
				if option != "effort" {
					return nil
				}
			}
			scope.ReasoningEffort, valid = availabilityOption(reasoning["effort"], "none", "minimal", "low", "medium", "high", "xhigh", "max")
			if !valid {
				return nil
			}
		case "tools", "include":
			if !value.IsArray() || len(value.Array()) != 0 {
				return nil
			}
		case "tool_choice":
			if value.Type != gjson.String || value.String() != "none" {
				return nil
			}
		default:
			return nil
		}
	}
	return scope
}

// Read the exact body being sent and preserve its original Close lifecycle.
// GetBody is independently checked because retries use it rather than Body.
func availabilityRequestPayload(request *http.Request) ([]byte, bool) {
	if request.GetBody == nil || request.Body == nil {
		return nil, false
	}
	body, err := request.GetBody()
	if err != nil || body == nil {
		if body != nil {
			_ = body.Close()
		}
		return nil, false
	}
	replay, err := io.ReadAll(body)
	closeErr := body.Close()
	if err != nil || closeErr != nil {
		return nil, false
	}
	original := request.Body
	payload, err := io.ReadAll(original)
	request.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.MultiReader(bytes.NewReader(payload), original), Closer: original}
	return payload, err == nil && bytes.Equal(payload, replay)
}

func observeAvailabilityProbeRequest(request *http.Request, trace *requesttiming.Trace) func(*http.Response) {
	observation, _ := request.Context().Value(availabilityProbeSnapshotKey{}).(*availabilityProbeObservation)
	if observation == nil || trace.Number() == 0 {
		return func(*http.Response) {}
	}
	observation.scope, observation.scopeAttempt = nil, trace.Number()
	payload, confirmed := availabilityRequestPayload(request)
	scope := availabilityScopeForRequest(request, payload)
	if !confirmed || scope == nil {
		return func(*http.Response) {}
	}
	method, host, address := request.Method, request.Host, request.URL.String()
	return func(response *http.Response) {
		if response == nil || response.Request == nil {
			return
		}
		actual := response.Request
		// A redirect or a rewriting adapter does not attest to the probe we
		// constructed. No scope is published without this final association.
		if actual.URL == nil || actual.URL.String() != address || actual.Host != host || actual.Method != method || actual.Response != nil || actual.GetBody == nil {
			return
		}
		body, err := actual.GetBody()
		if err != nil || body == nil {
			if body != nil {
				_ = body.Close()
			}
			return
		}
		sent, err := io.ReadAll(body)
		closeErr := body.Close()
		actualScope := availabilityScopeForRequest(actual, sent)
		if err == nil && closeErr == nil && bytes.Equal(sent, payload) && actualScope != nil && *scope == *actualScope {
			observation.scope = scope
		}
	}
}
