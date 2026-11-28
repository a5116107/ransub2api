package service

import (
	"encoding/json"
	"math"
	"strconv"
	"time"
)

// The source registry, a supplier declaration and actual business routing are
// different proof levels. This read-only projection never grants dispatch.
type ProcurementSourceEvidence struct {
	Version    int                                `json:"version"`
	AssessedAt time.Time                          `json:"assessed_at"`
	Selection  ProcurementSourceSelectionEvidence `json:"selection"`
	Models     ProcurementSourceModelEvidence     `json:"models"`
}

type ProcurementSourceSelectionEvidence struct {
	Status     string     `json:"status"`
	Source     string     `json:"source"`
	GroupID    string     `json:"group_id"`
	ObservedAt *time.Time `json:"observed_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

type ProcurementSourceModelEvidence struct {
	Status        string                              `json:"status"`
	Source        string                              `json:"source"`
	SourceVersion string                              `json:"source_version"`
	ObservedAt    *time.Time                          `json:"observed_at"`
	ExpiresAt     *time.Time                          `json:"expires_at"`
	Entries       []upstreamBillingApplicabilityEntry `json:"entries"`
}

// BuildProcurementSourceEvidence consumes the account, binding and declaration
// from the same repository transaction. It does not contact the supplier, use a
// visible-group catalog as key ownership, or borrow manually entered coverage.
func BuildProcurementSourceEvidence(row *ProcurementSourceMapping, account *Account, binding *NewAPIAccountBinding, at time.Time) *ProcurementSourceEvidence {
	out := &ProcurementSourceEvidence{Version: 1, AssessedAt: at,
		Selection: ProcurementSourceSelectionEvidence{Status: "unavailable", Source: "none"},
		Models:    ProcurementSourceModelEvidence{Status: "unavailable", Entries: []upstreamBillingApplicabilityEntry{}},
	}
	if row == nil || account == nil || row.AccountID != account.ID || account.Type != AccountTypeAPIKey ||
		row.AccountID < 1 || row.ID < 1 || row.AccountDeleted || row.LocalGroupDeleted || !row.Enabled || !row.SourceGroupEnabled ||
		!row.InLocalGroup || row.RouteIdentity != "current" || row.AccountStatus != StatusActive || row.LocalGroupStatus != StatusActive ||
		row.Platform != account.Platform || row.Platform != row.AccountPlatform || row.Platform != row.LocalGroupPlatform {
		return out
	}
	snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra)
	if snapshot == nil {
		out.Selection.Status = "not_observed"
		return out
	}
	if snapshot.Status != UpstreamBillingProbeStatusOK {
		out.Selection.Status = "failed"
		return out
	}
	if snapshot.ReceivedAt == nil || snapshot.FreshUntil == nil || snapshot.LastAttemptAt.IsZero() ||
		at.Before(snapshot.LastAttemptAt) || at.Before(*snapshot.ReceivedAt) || !snapshot.FreshUntil.After(*snapshot.ReceivedAt) {
		out.Selection.Status = "invalid"
		return out
	}
	out.Selection.ObservedAt, out.Selection.ExpiresAt = snapshot.ReceivedAt, snapshot.FreshUntil
	if !at.Before(*snapshot.FreshUntil) {
		out.Selection.Status = "expired"
		return out
	}
	data := snapshot.Data
	if data["billing_mode"] == "multi_group" || !procurementStoredIDMatches(data["schema_version"], 1) {
		out.Selection.Status = "unverified"
		return out
	}
	rate, rateOK := resolveAccountExtraNumber(data, "resolved_rate_multiplier")
	if !rateOK || !newAPIFinite(rate) || rate < 0 || rate > 1e6 {
		out.Selection.Status = "invalid"
		return out
	}
	if data["provider"] == "new_api" {
		out.Selection.Source = "new_api.management"
		site, err := NewAPIAccountSite(account)
		if err != nil || binding == nil || binding.AccountID != account.ID || binding.Profile.ID <= 0 || binding.Profile.UserID <= 0 || binding.Profile.Revision <= 0 ||
			binding.Fingerprint != NewAPIAccountFingerprint(account) || site != binding.Profile.SiteURL {
			return out
		}
		group, _ := data["group"].(string)
		if validNewAPIGroupRate(group, rate) {
			out.Selection.GroupID = group
		}
		if group != row.ExternalGroupID || binding.Group != row.ExternalGroupID || !procurementStoredIDMatches(data["token_id"], binding.TokenID) {
			out.Selection.Status = "mismatch"
			return out
		}
		if data["object"] != "new_api.group_billing" || data["billing_scope"] != "group" || data["group_rate_source"] != "user_groups" || !validNewAPIGroupRate(group, rate) {
			out.Selection.Status = "unverified"
			return out
		}
		out.Selection.Status = "supplier_declared"
	} else {
		out.Selection.Source = "sub2api.key_billing"
		requested, err := upstreamBillingRequestGroup(account)
		if binding != nil || err != nil || requested <= 0 || data["object"] != "sub2api.key_billing" || data["billing_scope"] != "token" {
			out.Selection.Status = "unverified"
			return out
		}
		confirmed, _ := data["selected_group_id"].(string)
		out.Selection.GroupID = confirmed
		if row.ExternalGroupID != strconv.FormatInt(requested, 10) || !upstreamBillingRequestGroupConfirmed(account, data) {
			out.Selection.Status = "mismatch"
			return out
		}
		out.Selection.Status = "receipt_confirmed"
	}
	declaration, present := upstreamBillingApplicabilityFromData(data)
	if !present {
		out.Models.Status = "not_declared"
		return out
	}
	if declaration == nil {
		out.Models.Status = "invalid"
		return out
	}
	out.Models.Source, out.Models.SourceVersion = declaration.Source, declaration.SourceVersion
	out.Models.ObservedAt = probeTimePtr(declaration.ObservedAt)
	out.Models.ExpiresAt = probeTimePtr(declaration.ExpiresAt)
	if snapshot.FreshUntil.Before(*out.Models.ExpiresAt) {
		out.Models.ExpiresAt = snapshot.FreshUntil
	}
	if at.Before(declaration.ObservedAt) || !at.Before(*out.Models.ExpiresAt) {
		out.Models.Status = "expired"
		return out
	}
	declaration.setConclusions(at) // reuse the same exact-scope verdict as final sending
	out.Models.Status, out.Models.Entries = "declared", declaration.Entries
	return out
}

// Binding token IDs historically traversed JSON numbers. A rounded float64 is
// never an exact identity; the request-scoped external group ID is a string.
func procurementStoredIDMatches(value any, expected int64) bool {
	if expected <= 0 {
		return false
	}
	switch value := value.(type) {
	case int64:
		return value == expected
	case int:
		return int64(value) == expected
	case json.Number:
		return value.String() == strconv.FormatInt(expected, 10)
	case float64:
		return value > 0 && value <= 9007199254740991 && math.Trunc(value) == value && int64(value) == expected
	default:
		return false
	}
}
