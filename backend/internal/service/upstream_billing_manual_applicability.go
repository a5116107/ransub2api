package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"time"
	"unicode"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const UpstreamBillingManualApplicabilityExtraKey = "upstream_billing_manual_applicability"

var ErrUpstreamBillingManualApplicability = infraerrors.BadRequest("INVALID_MANUAL_RATE_APPLICABILITY", "manual rate applicability requires a current API-key binding, reason, version, exact scopes and expiry within 24 hours")
var ErrUpstreamBillingManualApplicabilityConflict = infraerrors.Conflict("MANUAL_RATE_APPLICABILITY_CHANGED", "manual rate applicability changed; reload before confirming or revoking")

type upstreamBillingManualApplicability struct {
	upstreamBillingApplicability
	Revision    string  `json:"revision"`
	Reason      string  `json:"reason"`
	Binding     string  `json:"binding"`
	Rate        float64 `json:"rate_multiplier"`
	Invalidated bool    `json:"invalidated,omitempty"`
	Revoked     bool    `json:"revoked,omitempty"`
}

type manualApplicabilityUpdateKey struct{}
type manualApplicabilityUpdate struct{ expectedRevision string }

func manualApplicabilityRevision(extra map[string]any) string {
	raw, _ := json.Marshal(extra[UpstreamBillingManualApplicabilityExtraKey])
	return gjson.GetBytes(raw, "revision").String()
}

func manualApplicabilityRevoked(extra map[string]any) bool {
	raw, _ := json.Marshal(extra[UpstreamBillingManualApplicabilityExtraKey])
	var stored upstreamBillingManualApplicability
	return json.Unmarshal(raw, &stored) == nil && stored.Source == "manual.admin" && stored.Revoked && stored.Revision != ""
}

// Bind the attestation to this account's purchased route and effective U, not
// to a later account configuration. No raw credential is stored in the result.
func manualApplicabilityBinding(account *Account, rate float64) string {
	copyAccount := *account
	copyAccount.Proxy = nil // Hydration must not change this configuration identity.
	var group map[string]any
	if upstreamBillingRateSyncEnabled(account) {
		if snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra); snapshot != nil {
			group = make(map[string]any)
			for _, key := range []string{"object", "billing_scope", "group", "token_id", "selected_group_id"} {
				group[key] = snapshot.Data[key]
			}
		}
	}
	raw, err := json.Marshal(struct {
		AccountID int64
		Route     [32]byte
		Probe     string
		Rate      float64
		Automatic bool
		Group     map[string]any
	}{account.ID, AccountRouteFingerprint(&copyAccount), NewAPIAccountFingerprint(account), rate, upstreamBillingRateSyncEnabled(account), group})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func manualApplicabilityTarget(account *Account) *http.Request {
	base := upstreamBillingProbeBaseURL(account)
	if base == "" && account.Platform == PlatformOpenAI {
		base = "https://api.openai.com"
	}
	request, _ := http.NewRequest(http.MethodPost, buildOpenAIEndpointURL(base, "/v1/responses"), nil)
	return request
}

// Only the dedicated admin-account field creates an attestation. Generic extra
// updates are stripped, and the repository repeats revision CAS under its row lock.
func prepareUpstreamBillingManualApplicability(ctx context.Context, account *Account, raw json.RawMessage, at time.Time) (context.Context, error) {
	if len(raw) == 0 {
		return ctx, nil
	}
	value := gjson.ParseBytes(raw)
	fields, valid := availabilityJSONObject(value)
	if len(raw) > 64*1024 || !gjson.ValidBytes(raw) || !valid {
		return ctx, ErrUpstreamBillingManualApplicability
	}
	var command struct {
		Revoke                bool      `json:"revoke"`
		ExpectedRevision      string    `json:"expected_revision"`
		ConfirmCurrentBinding bool      `json:"confirm_current_binding"`
		SourceVersion         string    `json:"source_version"`
		Reason                string    `json:"reason"`
		ExpiresAt             time.Time `json:"expires_at"`
	}
	allowed := map[string]bool{"revoke": true, "expected_revision": true, "confirm_current_binding": true, "source_version": true, "reason": true, "expires_at": true, "entries": true}
	for name := range fields {
		if !allowed[name] {
			return ctx, ErrUpstreamBillingManualApplicability
		}
	}
	if fields["expected_revision"].Type != gjson.String || !decodeBillingApplicabilityFields(value, &command, "revoke", "expected_revision", "confirm_current_binding", "source_version", "reason", "expires_at") {
		return ctx, ErrUpstreamBillingManualApplicability
	}
	if command.ExpectedRevision != manualApplicabilityRevision(account.Extra) {
		return ctx, ErrUpstreamBillingManualApplicabilityConflict
	}
	account.Extra = maps.Clone(account.Extra)
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	ctx = context.WithValue(ctx, manualApplicabilityUpdateKey{}, manualApplicabilityUpdate{command.ExpectedRevision})
	if command.Revoke {
		if len(fields) != 2 || fields["expected_revision"].Type != gjson.String {
			return ctx, ErrUpstreamBillingManualApplicability
		}
		// Keep a minimal revision tombstone. Removing the key would let a form
		// from before initial creation replay expected_revision="" (ABA).
		account.Extra[UpstreamBillingManualApplicabilityExtraKey] = map[string]any{
			"schema_version": 1, "source": "manual.admin", "revision": uuid.NewString(), "revoked": true,
		}
		return ctx, nil
	}
	command.Reason = strings.TrimSpace(command.Reason)
	key, keyIsString := account.Credentials["api_key"].(string)
	if account.ID <= 0 || account.Type != AccountTypeAPIKey || account.ParentAccountID != nil || account.HasManagedOpenAIConnections() ||
		!keyIsString || strings.TrimSpace(key) == "" ||
		!command.ConfirmCurrentBinding || len(command.Reason) == 0 || len(command.Reason) > 512 || strings.IndexFunc(command.Reason, unicode.IsControl) >= 0 {
		return ctx, ErrUpstreamBillingManualApplicability
	}
	if _, err := upstreamBillingRequestGroup(account); err != nil || !upstreamBillingRequestTargetConfirmed(account, manualApplicabilityTarget(account)) {
		return ctx, ErrUpstreamBillingManualApplicability
	}
	rate, reason := profitControlAccountRateAt(account, at)
	if reason != "" {
		return ctx, ErrUpstreamBillingManualApplicability
	}
	declarationRaw, _ := json.Marshal(map[string]any{"schema_version": 1, "source_version": command.SourceVersion,
		"observed_at": at, "expires_at": command.ExpiresAt, "entries": json.RawMessage(value.Get("entries").Raw)})
	declaration := decodeUpstreamBillingApplicability(declarationRaw, at)
	if declaration == nil {
		return ctx, ErrUpstreamBillingManualApplicability
	}
	for _, entry := range declaration.Entries {
		if !billingApplicabilityScopeShapeKnown(AvailabilityRequestScope{Version: availabilityRequestScopeVersion, Protocol: entry.Protocol,
			Stream: entry.Protocol == "responses_ws_v2", Model: entry.Model, ServiceTier: entry.ServiceTier, ReasoningEffort: entry.ReasoningEffort, Capability: "text"}) {
			return ctx, ErrUpstreamBillingManualApplicability
		}
	}
	declaration.Source = "manual.admin"
	account.Extra[UpstreamBillingManualApplicabilityExtraKey] = upstreamBillingManualApplicability{
		upstreamBillingApplicability: *declaration, Revision: uuid.NewString(), Reason: command.Reason, Rate: rate, Binding: manualApplicabilityBinding(account, rate),
	}
	return ctx, nil
}

// Invalidate on an observed binding edit, so reverting the old key/route does
// not revive the old attestation. Expiry alone also never extends its lifetime.
func InvalidateUpstreamBillingManualApplicability(extra map[string]any) {
	value, present := extra[UpstreamBillingManualApplicabilityExtraKey]
	if !present {
		return
	}
	raw, err := json.Marshal(value)
	var stored map[string]any
	if err == nil && json.Unmarshal(raw, &stored) == nil && stored != nil {
		stored["invalidated"] = true
		extra[UpstreamBillingManualApplicabilityExtraKey] = stored
	}
}

func HasUpstreamBillingManualApplicabilityUpdate(ctx context.Context) bool {
	_, explicit := ctx.Value(manualApplicabilityUpdateKey{}).(manualApplicabilityUpdate)
	return explicit
}

// MergeUpstreamBillingManualApplicability is called with the same locked row
// already used for billing/probe state. An ordinary stale form never owns this key.
func MergeUpstreamBillingManualApplicability(ctx context.Context, current, desired map[string]any) (map[string]any, error) {
	command, explicit := ctx.Value(manualApplicabilityUpdateKey{}).(manualApplicabilityUpdate)
	if explicit && command.expectedRevision != manualApplicabilityRevision(current) {
		return nil, ErrUpstreamBillingManualApplicabilityConflict
	}
	if !explicit {
		desired = maps.Clone(desired)
		delete(desired, UpstreamBillingManualApplicabilityExtraKey)
		if existing, present := current[UpstreamBillingManualApplicabilityExtraKey]; present {
			if desired == nil {
				desired = make(map[string]any)
			}
			desired[UpstreamBillingManualApplicabilityExtraKey] = existing
		}
	}
	return desired, nil
}

func manualBillingApplicabilityAt(account *Account, at time.Time) (*upstreamBillingManualApplicability, string) {
	raw, err := json.Marshal(account.Extra[UpstreamBillingManualApplicabilityExtraKey])
	var stored upstreamBillingManualApplicability
	if err != nil || json.Unmarshal(raw, &stored) != nil || stored.Source != "manual.admin" || stored.Revision == "" {
		return nil, "invalid"
	}
	if stored.Revoked {
		return &stored, "revoked"
	}
	if stored.Reason == "" {
		return nil, "invalid"
	}
	parsed := decodeUpstreamBillingApplicability(raw, stored.ObservedAt)
	if parsed == nil {
		return nil, "invalid"
	}
	parsed.Source = "manual.admin"
	stored.upstreamBillingApplicability = *parsed
	if stored.Invalidated {
		return &stored, "binding_changed"
	}
	if at.Before(stored.ObservedAt) || !at.Before(stored.ExpiresAt) {
		return &stored, "expired"
	}
	rate, reason := profitControlAccountRateAt(account, at)
	if reason != "" {
		return &stored, "unavailable"
	}
	if stored.Binding == "" || stored.Binding != manualApplicabilityBinding(account, rate) || stored.Rate != rate {
		return &stored, "binding_changed"
	}
	return &stored, "active"
}

// UpstreamBillingManualApplicabilityView is a safe admin-only projection. The
// private binding is never returned; conclusions are recomputed, never trusted.
func UpstreamBillingManualApplicabilityView(account *Account, at time.Time) map[string]any {
	if account == nil {
		return nil
	}
	if _, present := account.Extra[UpstreamBillingManualApplicabilityExtraKey]; !present {
		return nil
	}
	stored, status := manualBillingApplicabilityAt(account, at)
	if stored == nil {
		return map[string]any{"status": status, "revision": manualApplicabilityRevision(account.Extra)}
	}
	if status == "revoked" {
		return map[string]any{"status": status, "revision": stored.Revision, "source": "manual.admin"}
	}
	if upstreamBillingRateSyncEnabled(account) {
		if snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra); snapshot != nil {
			if _, present := snapshot.Data[upstreamBillingApplicabilityKey]; present {
				status = "superseded"
			}
		}
	}
	entries := append([]upstreamBillingApplicabilityEntry(nil), stored.Entries...)
	for i := range entries {
		entry := &entries[i]
		entry.Conclusion = status
		if status == "active" {
			entry.Conclusion = stored.verdict(&AvailabilityRequestScope{Model: entry.Model, Protocol: entry.Protocol,
				ServiceTier: entry.ServiceTier, ReasoningEffort: entry.ReasoningEffort, Capability: "text"}, at)
		}
	}
	return map[string]any{"schema_version": 1, "source": "manual.admin", "source_version": stored.SourceVersion, "observed_at": stored.ObservedAt,
		"expires_at": stored.ExpiresAt, "entries": entries, "revision": stored.Revision, "reason": stored.Reason, "rate_multiplier": stored.Rate, "status": status}
}
