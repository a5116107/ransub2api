package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/tidwall/gjson"
)

// Version 2 requires upstream identity association as well as address and body
// evidence. Version 1 observations must be reprobed before they authorize sends.
const availabilityRequestScopeVersion = 2

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
	ThinkingMode    string `json:"thinking_mode,omitempty"`
	ThinkingBudget  int64  `json:"thinking_budget,omitempty"`
	ProtocolOptions string `json:"protocol_options,omitempty"`
	// Stored separately inside the repository's private state, never in API or history JSON.
	ConnectionScope string `json:"-"`
}

// AvailabilityConnectionScope identifies an immutable managed grant revision
// without exposing its identifier or credentials in stored availability data.
func AvailabilityConnectionScope(account *Account) string {
	if account == nil || account.AuthConnectionID == "" || account.AuthConnectionRevision <= 0 {
		return ""
	}
	sum := sha256.Sum256([]byte("availability-connection-v1\x00" + account.AuthConnectionID + "\x00" + strconv.FormatInt(account.AuthConnectionRevision, 10)))
	return hex.EncodeToString(sum[:])
}

// AvailabilityProbeTargetKey is stable across credential revisions and is used
// only as a bounded, non-secret round-robin cursor in the existing state row.
func AvailabilityProbeTargetKey(accountID int64, connectionID string) string {
	if accountID <= 0 || connectionID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("availability-probe-target-v1\x00" + strconv.FormatInt(accountID, 10) + "\x00" + connectionID))
	return hex.EncodeToString(sum[:])
}

// Bind observed capability to the independently authorized grant and its
// revision. A parent-only or legacy observation must not authorize that grant.
func availabilityScopeForAccount(scope *AvailabilityRequestScope, request *http.Request, account *Account) *AvailabilityRequestScope {
	if scope == nil || account == nil {
		return scope
	}
	if account.AuthConnectionID == "" {
		if account.HasManagedOpenAIConnections() {
			return nil
		}
		return scope
	}
	connectionScope := AvailabilityConnectionScope(account)
	if connectionScope == "" {
		return nil
	}
	credential := account.GetCredential("access_token")
	if account.Type == AccountTypeAPIKey {
		credential = account.GetCredential("api_key")
	}
	authorization, valid := availabilitySingleAuthorization(request)
	if credential == "" || !valid || authorization != "Bearer "+credential {
		return nil
	}
	copy := *scope
	copy.ConnectionScope = connectionScope
	return &copy
}

func availabilitySingleAuthorization(request *http.Request) (string, bool) {
	if request == nil {
		return "", false
	}
	var values []string
	for key, headers := range request.Header {
		if strings.EqualFold(key, "Authorization") {
			values = append(values, headers...)
		}
	}
	if len(values) != 1 || values[0] == "" {
		return "", false
	}
	return values[0], true
}

// Upstream identity is compared only during this request. Values never enter a
// scope or its persisted option digest; missing credentials remain distinct
// from empty or ambiguous headers, including differently cased duplicates.
func availabilityRequestIdentity(request *http.Request) ([7]string, bool) {
	var identity [7]string
	if request == nil {
		return identity, false
	}
	for index, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Chatgpt-Account-Id", "X-Openai-Account-Id", "OpenAI-Organization", "OpenAI-Project"} {
		var values []string
		for key, value := range request.Header {
			if strings.EqualFold(key, name) {
				values = append(values, value...)
			}
		}
		if len(values) > 1 || len(values) == 1 && values[0] == "" {
			return [7]string{}, false
		}
		if len(values) == 1 {
			identity[index] = values[0]
		}
	}
	return identity, true
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
		fields["store"].Type != gjson.False || !fields["input"].IsArray() || !availabilityTextInput(fields["input"], "responses_http") {
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
		// Keys stay case-sensitive. Each protocol validates its exact names;
		// Gemini uses camelCase while OpenAI and Messages use snake_case.
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

func availabilityTextContent(value gjson.Result, messages bool) bool {
	if value.Type == gjson.String {
		return true
	}
	if !value.IsArray() {
		return false
	}
	valid := true
	value.ForEach(func(_, item gjson.Result) bool {
		fields, ok := availabilityJSONObject(item)
		if !ok || fields["text"].Type != gjson.String {
			valid = false
			return false
		}
		if cache, exists := fields["cache_control"]; exists && messages {
			options, ok := availabilityJSONObject(cache)
			if !ok || len(options) != 1 || options["type"].Type != gjson.String || options["type"].String() != "ephemeral" {
				valid = false
				return false
			}
			delete(fields, "cache_control")
		}
		if len(fields) != 2 || messages && fields["type"].String() != "text" {
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

func availabilityTextInput(value gjson.Result, protocol string) bool {
	if !value.Exists() {
		return protocol != "messages_http"
	}
	if protocol == "responses_http" && value.Type == gjson.String {
		return true
	}
	if !value.IsArray() {
		return false
	}
	valid := true
	value.ForEach(func(_, item gjson.Result) bool {
		fields, ok := availabilityJSONObject(item)
		if !ok || !availabilityTextContent(fields["content"], protocol == "messages_http") {
			valid = false
			return false
		}
		for name := range fields {
			if name != "role" && name != "content" && name != "type" {
				valid = false
				return false
			}
		}
		if kind := fields["type"]; kind.Exists() && (protocol != "responses_http" || kind.Type != gjson.String || kind.String() != "message") {
			valid = false
			return false
		}
		switch fields["role"].String() {
		case "system", "developer":
			valid = protocol != "messages_http"
		case "user", "assistant":
		default:
			valid = false
		}
		return valid
	})
	return valid
}

func availabilityScopeForRequest(request *http.Request, payload []byte) *AvailabilityRequestScope {
	if request == nil || request.URL == nil || request.URL.Fragment != "" || request.Header.Get("Content-Encoding") != "" || !gjson.ValidBytes(payload) {
		return nil
	}
	if _, valid := availabilityRequestIdentity(request); !valid {
		return nil
	}
	if strings.HasSuffix(request.URL.Path, ":generateContent") || strings.HasSuffix(request.URL.Path, ":streamGenerateContent") {
		return availabilityGeminiScope(request, payload)
	}
	messages := request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/messages")
	if request.URL.RawQuery != "" && (!messages || request.URL.RawQuery != "beta=true") {
		return nil
	}
	websocket := request.Method == http.MethodGet && (request.URL.Scheme == "ws" || request.URL.Scheme == "wss") && strings.HasSuffix(request.URL.Path, "/responses")
	if !websocket && request.Method != http.MethodPost {
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
	scope := &AvailabilityRequestScope{Version: availabilityRequestScopeVersion, Model: model, Capability: "text", Stream: websocket}
	if stream := fields["stream"]; stream.Exists() {
		if stream.Type != gjson.True && stream.Type != gjson.False {
			return nil
		}
		scope.Stream = stream.Bool()
	}
	switch {
	case websocket:
		if fields["type"].Type != gjson.String || fields["type"].String() != "response.create" {
			return nil
		}
		scope.Protocol = "responses_ws_v2"
	case strings.HasSuffix(request.URL.Path, "/responses"):
		scope.Protocol = "responses_http"
	case strings.HasSuffix(request.URL.Path, "/chat/completions"):
		scope.Protocol = "chat_http"
	case messages:
		return availabilityMessagesScope(request, fields, scope)
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
		case "type":
			if !websocket {
				return nil
			}
		case "generate":
			// A text answer does not attest to generate=false prewarming.
			if !websocket || value.Type != gjson.True {
				return nil
			}
		case "model", "service_tier", "stream", "stream_options", "max_tokens", "max_completion_tokens", "max_output_tokens", "temperature", "top_p", "top_k", "presence_penalty", "frequency_penalty", "seed", "store", "metadata", "user", "safety_identifier", "prompt_cache_key", "prompt_cache_retention", "truncation":
		case "instructions":
			if value.Type != gjson.String {
				return nil
			}
		case "input":
			if (scope.Protocol != "responses_http" && !websocket) || !availabilityTextInput(value, "responses_http") {
				return nil
			}
		case "messages":
			if scope.Protocol != "chat_http" || !availabilityTextInput(value, "chat_http") {
				return nil
			}
		case "reasoning_effort":
			if scope.Protocol != "chat_http" {
				return nil
			}
		case "reasoning":
			reasoning, ok := availabilityJSONObject(value)
			if !ok || (scope.Protocol != "responses_http" && !websocket) {
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

// Messages observations attest only to text plus the exact thinking and public
// protocol options sent by the native probe. Tool, media and cache-TTL features
// need their own evidence and are not inferred from a text answer.
func availabilityMessagesScope(request *http.Request, fields map[string]gjson.Result, scope *AvailabilityRequestScope) *AvailabilityRequestScope {
	scope.Protocol, scope.ReasoningEffort = "messages_http", "unspecified"
	var valid bool
	scope.ServiceTier, valid = availabilityOption(fields["service_tier"], "auto", "standard_only")
	if !valid || !availabilityTextInput(fields["messages"], scope.Protocol) || len(fields["messages"].Array()) == 0 {
		return nil
	}
	options := request.URL.RawQuery
	for _, name := range []string{"Anthropic-Version", "Anthropic-Beta"} {
		var values []string
		for key, value := range request.Header {
			if strings.EqualFold(key, name) {
				values = append(values, value...)
			}
		}
		if len(values) > 1 || len(values) == 1 && len(values[0]) > 4096 {
			return nil
		}
		options += "\x00" + strings.Join(values, "")
	}
	sum := sha256.Sum256([]byte(options))
	scope.ProtocolOptions = hex.EncodeToString(sum[:])
	for name, value := range fields {
		switch name {
		case "model", "messages", "stream", "service_tier", "max_tokens", "temperature", "top_p", "top_k", "metadata", "stop_sequences":
		case "system":
			if !availabilityTextContent(value, true) {
				return nil
			}
		case "thinking":
			thinking, ok := availabilityJSONObject(value)
			if !ok || thinking["type"].Type != gjson.String {
				return nil
			}
			scope.ThinkingMode = thinking["type"].String()
			switch scope.ThinkingMode {
			case "disabled", "adaptive":
				if len(thinking) != 1 {
					return nil
				}
			case "enabled":
				budget, err := strconv.ParseInt(thinking["budget_tokens"].Raw, 10, 64)
				if len(thinking) != 2 || err != nil || budget <= 0 {
					return nil
				}
				scope.ThinkingBudget = budget
			default:
				return nil
			}
		case "output_config":
			output, ok := availabilityJSONObject(value)
			if !ok || len(output) != 1 {
				return nil
			}
			scope.ReasoningEffort, valid = availabilityOption(output["effort"], "low", "medium", "high", "xhigh", "max", "ultra")
			if !valid || scope.ReasoningEffort == "unspecified" {
				return nil
			}
		case "tools":
			if !value.IsArray() || len(value.Array()) != 0 {
				return nil
			}
		case "tool_choice":
			choice, ok := availabilityJSONObject(value)
			if !ok || len(choice) != 1 || choice["type"].Type != gjson.String || choice["type"].String() != "none" {
				return nil
			}
		default:
			return nil
		}
	}
	return scope
}

// A binding remembers only a target and an irreversible grant revision. The
// actual handshake's credentials are checked at construction, never persisted.
type availabilityWSBinding struct {
	target          *url.URL
	connectionScope string
	requestHash     [32]byte
}

func bindAvailabilityWS(account *Account, target string, headers http.Header) *availabilityWSBinding {
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil || request.URL.Host == "" || (request.URL.Scheme != "ws" && request.URL.Scheme != "wss") || headers.Get("Content-Encoding") != "" {
		return nil
	}
	request.Header = headers
	scope := availabilityScopeForAccount(&AvailabilityRequestScope{}, request, account)
	if scope == nil {
		return nil
	}
	requestHash := availabilityWSRequestHash(request)
	if requestHash == ([32]byte{}) {
		return nil
	}
	return &availabilityWSBinding{target: request.URL, connectionScope: scope.ConnectionScope, requestHash: requestHash}
}

// coder/websocket uses HTTP(S) on the handshake response. Canonicalize only the
// scheme; redirect, Host or credential changes must not produce a wire proof.
func availabilityWSRequestHash(request *http.Request) [32]byte {
	if request == nil || request.URL == nil || request.Method != http.MethodGet || request.Response != nil {
		return [32]byte{}
	}
	address := *request.URL
	switch address.Scheme {
	case "ws":
		address.Scheme = "http"
	case "wss":
		address.Scheme = "https"
	case "http", "https":
	default:
		return [32]byte{}
	}
	identity, valid := availabilityRequestIdentity(request)
	if !valid || identity[0] == "" {
		return [32]byte{}
	}
	host := request.Host
	if host == "" {
		host = address.Host
	}
	return sha256.Sum256([]byte(address.String() + "\x00" + host + "\x00" + strings.Join(identity[:], "\x00")))
}

func (b *availabilityWSBinding) scope(account *Account, payload any) *AvailabilityRequestScope {
	if b == nil || account == nil || b.target == nil || b.connectionScope != AvailabilityConnectionScope(account) || (account.HasManagedOpenAIConnections() && account.AuthConnectionID == "") {
		return nil
	}
	var raw []byte
	switch value := payload.(type) {
	case []byte:
		raw = value
	case json.RawMessage:
		raw = value
	case openAIWSPreparedJSON:
		raw = value
	default:
		var err error
		raw, err = json.Marshal(value)
		if err != nil {
			return nil
		}
	}
	scope := availabilityScopeForRequest(&http.Request{Method: http.MethodGet, URL: b.target, Header: make(http.Header)}, raw)
	if scope != nil {
		scope.ConnectionScope = b.connectionScope
	}
	return scope
}

func (s *OpenAIGatewayService) admitAvailabilityWS(ctx context.Context, account *Account, binding *openAIWSTurnBinding, payload any) error {
	return admitAvailabilityScope(ctx, account, s.availabilityReader, s.schedulerSnapshot, func() *AvailabilityRequestScope {
		if binding == nil {
			return nil
		}
		return binding.availability.scope(account, payload)
	})
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

func observeAvailabilityProbeRequest(request *http.Request, trace *requesttiming.Trace, account *Account) func(*http.Response) {
	observation, _ := request.Context().Value(availabilityProbeSnapshotKey{}).(*availabilityProbeObservation)
	if observation == nil || trace.Number() == 0 {
		return func(*http.Response) {}
	}
	observation.scope, observation.scopeAttempt, observation.semanticStatus = nil, trace.Number(), 0
	identity, valid := availabilityRequestIdentity(request)
	if !valid {
		return func(*http.Response) {}
	}
	payload, confirmed := availabilityRequestPayload(request)
	scope := availabilityScopeForAccount(availabilityScopeForRequest(request, payload), request, account)
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
		actualIdentity, valid := availabilityRequestIdentity(actual)
		if !valid || actualIdentity != identity {
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
		if scope.ConnectionScope != "" {
			if actualScope != nil {
				actualScope.ConnectionScope = scope.ConnectionScope
			}
		}
		if err == nil && closeErr == nil && bytes.Equal(sent, payload) && actualScope != nil && *scope == *actualScope {
			observation.scope = scope
		}
	}
}
