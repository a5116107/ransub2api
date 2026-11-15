package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"time"

	"github.com/tidwall/gjson"
)

const upstreamBillingApplicabilityKey = "rate_applicability"

func parseUpstreamBillingApplicabilityResponse(body []byte, data map[string]any, observedAt time.Time) {
	value := gjson.GetBytes(body, upstreamBillingApplicabilityKey)
	if !value.Exists() {
		return
	}
	// Explicit null, conflicting duplicate envelope fields and malformed
	// declarations remain distinct from a supplier with no such contract.
	data[upstreamBillingApplicabilityKey] = nil
	if _, valid := availabilityJSONObject(gjson.ParseBytes(body)); valid {
		declaration := decodeUpstreamBillingApplicability([]byte(value.Raw), observedAt)
		if declaration != nil {
			// Conclusions are generated once when observing the declaration,
			// not O(entries²) on every request that consumes the snapshot.
			for i := range declaration.Entries {
				entry := &declaration.Entries[i]
				entry.Conclusion = declaration.verdict(&AvailabilityRequestScope{
					Model: entry.Model, Protocol: entry.Protocol, ServiceTier: entry.ServiceTier,
					ReasoningEffort: entry.ReasoningEffort, Capability: "text",
				}, observedAt)
			}
		}
		data[upstreamBillingApplicabilityKey] = declaration
	}
}

// Applicability is a supplier declaration, not independent proof of the billed
// amount or model identity. Exact wire scopes never inherit another entry.
type upstreamBillingApplicability struct {
	SchemaVersion int                                 `json:"schema_version"`
	Source        string                              `json:"source"`
	SourceVersion string                              `json:"source_version"`
	ObservedAt    time.Time                           `json:"observed_at"`
	ExpiresAt     time.Time                           `json:"expires_at"`
	Entries       []upstreamBillingApplicabilityEntry `json:"entries"`
}

type upstreamBillingApplicabilityEntry struct {
	Model            string `json:"model"`
	Protocol         string `json:"protocol"`
	ServiceTier      string `json:"service_tier"`
	ReasoningEffort  string `json:"reasoning_effort"`
	Context          string `json:"context"`
	GroupRateApplies *bool  `json:"group_rate_applies"`
	Conclusion       string `json:"conclusion"`
}

func decodeUpstreamBillingApplicability(raw []byte, observedAt time.Time) *upstreamBillingApplicability {
	var declaration upstreamBillingApplicability
	value := gjson.ParseBytes(raw)
	if !gjson.ValidBytes(raw) || !decodeBillingApplicabilityFields(value, &declaration,
		"schema_version", "source_version", "observed_at", "expires_at") || declaration.SchemaVersion != 1 ||
		!billingDeclarationToken(declaration.SourceVersion, 128) || !declaration.ObservedAt.Equal(observedAt) ||
		!declaration.ExpiresAt.After(observedAt) || declaration.ExpiresAt.Sub(observedAt) > 24*time.Hour {
		return nil
	}
	entries := value.Get("entries")
	if !entries.IsArray() || len(entries.Array()) == 0 || len(entries.Array()) > 128 {
		return nil
	}
	for _, rawEntry := range entries.Array() {
		var entry upstreamBillingApplicabilityEntry
		if !decodeBillingApplicabilityFields(rawEntry, &entry, "model", "protocol", "service_tier", "reasoning_effort", "context", "group_rate_applies") ||
			!billingDeclarationToken(entry.Model, 200) || !billingDeclarationToken(entry.Protocol, 64) ||
			!billingDeclarationToken(entry.ServiceTier, 64) || !billingDeclarationToken(entry.ReasoningEffort, 64) ||
			!billingDeclarationToken(entry.Context, 64) {
			return nil
		}
		declaration.Entries = append(declaration.Entries, entry)
	}
	// Source is assigned locally; never persist an arbitrary supplier URL,
	// credential or unrecognized extra field from the response.
	declaration.Source = "sub2api.key_billing"
	return &declaration
}

// Select exact case-sensitive wire names before Go's case-insensitive struct
// decoder sees them. A differently cased alias must not override a canonical
// scope, boolean or version; unknown fields and supplied conclusions are dropped.
func decodeBillingApplicabilityFields(value gjson.Result, target any, names ...string) bool {
	fields, valid := availabilityJSONObject(value)
	if !valid {
		return false
	}
	selected := make(map[string]json.RawMessage, len(names))
	for _, name := range names {
		if field, present := fields[name]; present {
			selected[name] = json.RawMessage(field.Raw)
		}
	}
	raw, err := json.Marshal(selected)
	return err == nil && json.Unmarshal(raw, target) == nil
}

func billingDeclarationToken(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r > '~' || r == '*' {
			return false
		}
	}
	return true
}

func upstreamBillingApplicabilityFromData(data map[string]any) (*upstreamBillingApplicability, bool) {
	value, present := data[upstreamBillingApplicabilityKey]
	if !present {
		return nil, false
	}
	raw, err := json.Marshal(value)
	observed, _ := data["observed_at"].(string)
	at, parseErr := time.Parse(time.RFC3339Nano, observed)
	if err != nil || parseErr != nil {
		return nil, true
	}
	return decodeUpstreamBillingApplicability(raw, at), true
}

// Unknown is scoped to the requested combination, never to an entire account.
// Context-limited declarations stay unknown until a trustworthy token/context
// classifier exists; JSON byte size is not a context-token measurement.
func (d *upstreamBillingApplicability) verdict(scope *AvailabilityRequestScope, at time.Time) string {
	if d == nil {
		return "invalid"
	}
	if at.Before(d.ObservedAt) || !at.Before(d.ExpiresAt) {
		return "expired"
	}
	if scope == nil || scope.Capability != "text" || scope.ThinkingMode != "" || scope.ProtocolOptions != "" {
		return "unknown"
	}
	verdict, matched, limitedContext := "unknown", false, false
	for _, entry := range d.Entries {
		if entry.Model != scope.Model || entry.Protocol != scope.Protocol || entry.ServiceTier != scope.ServiceTier ||
			entry.ReasoningEffort != scope.ReasoningEffort {
			continue
		}
		if entry.Context != "all" {
			// A specific context exception can overlap an "all" entry. Without
			// measured context, neither may silently take precedence.
			limitedContext = true
			continue
		}
		current := "unknown"
		if entry.GroupRateApplies != nil {
			current = "excluded"
			if *entry.GroupRateApplies {
				current = "covered"
			}
		}
		if matched && current != verdict {
			return "conflict"
		}
		matched, verdict = true, current
	}
	if limitedContext {
		return "unknown"
	}
	return verdict
}

func guardUpstreamBillingApplicability(account *Account, scope *AvailabilityRequestScope) error {
	if !upstreamBillingHasApplicability(account) {
		return nil // Legacy routing remains compatible, not qualified for managed supply.
	}
	if _, reason := upstreamBillingScopeVerdictAt(account, scope, time.Now()); reason != "" {
		return denyOpenAITurn(reason)
	}
	return nil
}

func upstreamBillingEvidenceRequired(account *Account) bool {
	return account != nil && (upstreamBillingRateSyncEnabled(account) || upstreamBillingHasApplicability(account))
}

func upstreamBillingHasApplicability(account *Account) bool {
	if account == nil || account.Type != AccountTypeAPIKey {
		return false
	}
	if _, present := account.Extra[UpstreamBillingManualApplicabilityExtraKey]; present && !manualApplicabilityRevoked(account.Extra) {
		return true
	}
	if upstreamBillingRateSyncEnabled(account) {
		if snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra); snapshot != nil {
			_, present := snapshot.Data[upstreamBillingApplicabilityKey]
			return present
		}
	}
	return false
}

// The offline preview and final send consume one contract at one explicit
// instant. Legacy/manual routing compatibility is NOT strict scope evidence.
func upstreamBillingScopeVerdictAt(account *Account, scope *AvailabilityRequestScope, at time.Time) (string, string) {
	if account == nil || account.Type != AccountTypeAPIKey {
		return "not_declared", "profit_upstream_scope_not_declared"
	}
	if _, reason := profitControlAccountRateAt(account, at); reason != "" {
		return "unavailable", reason
	}
	var declaration *upstreamBillingApplicability
	present := false
	if upstreamBillingRateSyncEnabled(account) {
		if snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra); snapshot != nil {
			declaration, present = upstreamBillingApplicabilityFromData(snapshot.Data)
		}
	}
	// A manual attestation fills only missing supplier support. An explicit
	// supplier exclusion, invalid declaration or expiry never falls back to it.
	if !present {
		if _, manual := account.Extra[UpstreamBillingManualApplicabilityExtraKey]; !manual || manualApplicabilityRevoked(account.Extra) {
			return "not_declared", "profit_upstream_scope_not_declared"
		}
		stored, status := manualBillingApplicabilityAt(account, at)
		if status != "active" {
			return status, "profit_upstream_scope_" + status
		}
		declaration = &stored.upstreamBillingApplicability
	}
	status := declaration.verdict(scope, at)
	if status != "covered" {
		return status, "profit_upstream_scope_" + status
	}
	return status, ""
}

func guardUpstreamBillingHTTPApplicability(account *Account, request *http.Request) error {
	if !upstreamBillingHasApplicability(account) {
		return nil
	}
	payload, confirmed := availabilityRequestPayload(request)
	var scope *AvailabilityRequestScope
	if confirmed {
		scope = availabilityScopeForRequest(request, payload)
	}
	return guardUpstreamBillingApplicability(account, scope)
}

func guardUpstreamBillingWSApplicability(ctx context.Context, account *Account, binding *openAIWSTurnBinding, payload any) error {
	if !gatewayProfitControlGateActive(ctx) || account == nil || account.Type != AccountTypeAPIKey || !upstreamBillingEvidenceRequired(account) {
		return nil
	}
	if !upstreamBillingHasApplicability(account) {
		return nil
	}
	var scope *AvailabilityRequestScope
	if binding != nil {
		scope = binding.availability.scope(account, payload)
	}
	return guardUpstreamBillingApplicability(account, scope)
}

// A late supplier response must not roll back an already observed declaration.
// Repository CAS independently fences concurrent probe, route and key changes.
func upstreamBillingApplicabilityReplay(previous, next map[string]any) bool {
	_, oldPresent := previous[upstreamBillingApplicabilityKey]
	_, newPresent := next[upstreamBillingApplicabilityKey]
	if !oldPresent && !newPresent {
		return false
	}
	oldAt, _ := time.Parse(time.RFC3339Nano, stringMapValue(previous, "observed_at"))
	newAt, _ := time.Parse(time.RFC3339Nano, stringMapValue(next, "observed_at"))
	if newAt.Before(oldAt) || oldPresent && !newPresent {
		return true
	}
	if newAt.Equal(oldAt) {
		oldDeclaration, _ := upstreamBillingApplicabilityFromData(previous)
		newDeclaration, _ := upstreamBillingApplicabilityFromData(next)
		if !reflect.DeepEqual(oldDeclaration, newDeclaration) {
			return true
		}
		// Applicability and the price it qualifies are one observation. The
		// same timestamp must not silently acquire another price or group pin.
		for _, key := range []string{"group_rate_multiplier", "user_rate_multiplier", "resolved_rate_multiplier", "peak_rate_enabled", "peak_start", "peak_end", "peak_rate_multiplier", "applied_peak_multiplier", "effective_rate_multiplier", "timezone", "selected_group_id"} {
			if !reflect.DeepEqual(previous[key], next[key]) {
				return true
			}
		}
	}
	return false
}

func stringMapValue(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return value
}

// Validate the supplied descriptor with the existing wire parser, rather than
// keeping a second list of accepted tier/effort options. This constructs no
// generation request and supplies no evidence of a real upstream send.
func billingApplicabilityScopeShapeKnown(scope AvailabilityRequestScope) bool {
	body := map[string]any{"model": scope.Model, "stream": scope.Stream}
	method, scheme, path := http.MethodPost, "https", "/v1/responses"
	switch scope.Protocol {
	case "responses_http":
	case "responses_ws_v2":
		method, scheme = http.MethodGet, "wss"
		body["type"] = "response.create"
	case "chat_http":
		path = "/v1/chat/completions"
	default:
		return false
	}
	if scope.ServiceTier != "unspecified" {
		body["service_tier"] = scope.ServiceTier
	}
	if scope.ReasoningEffort != "unspecified" {
		if scope.Protocol == "chat_http" {
			body["reasoning_effort"] = scope.ReasoningEffort
		} else {
			body["reasoning"] = map[string]any{"effort": scope.ReasoningEffort}
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return false
	}
	parsed := availabilityScopeForRequest(&http.Request{Method: method, URL: &url.URL{Scheme: scheme, Host: "preview.invalid", Path: path}, Header: make(http.Header)}, payload)
	return parsed != nil && *parsed == scope
}
