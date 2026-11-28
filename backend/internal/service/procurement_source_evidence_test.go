package service

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func procurementEvidenceFixture(t *testing.T) (ProcurementSourceMapping, *Account, *NewAPIAccountBinding, time.Time) {
	t.Helper()
	at := time.Now().UTC().Add(-time.Minute)
	row := ProcurementSourceMapping{ID: 1, AccountID: 9, Platform: PlatformOpenAI, AccountPlatform: PlatformOpenAI, LocalGroupPlatform: PlatformOpenAI, Enabled: true, SourceGroupEnabled: true, InLocalGroup: true, RouteIdentity: "current", AccountStatus: StatusActive, LocalGroupStatus: StatusActive, ExternalGroupID: "vip"}
	a := &Account{ID: 9, Type: AccountTypeAPIKey, Platform: PlatformOpenAI, Credentials: map[string]any{"api_key": "fixture-private-key", "base_url": "https://fixture-private-site.example/v1"}}
	b := &NewAPIAccountBinding{AccountID: a.ID, TokenID: 3, Group: "vip", Profile: NewAPISiteAuthorization{ID: 7, UserID: 4, Revision: 2, SiteURL: "https://fixture-private-site.example"}, Fingerprint: NewAPIAccountFingerprint(a)}
	data := &newAPIData{ratios: map[string]float64{"vip": .5}}
	snap := newAPISnapshot(NewAPIToken{ID: 3, Group: "vip"}, data, 30, at)
	applyNewAPIBillingApplicability(snap.Data, mustJSONForEvidence(t, newAPIPricingApplicabilityFixture(t, at)), "vip", .5, at)
	a.Extra = map[string]any{UpstreamBillingProbeExtraKey: snap}
	return row, a, b, at.Add(time.Minute)
}

func mustJSONForEvidence(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}

func TestProcurementSourceEvidenceLinksFixedDeclarationAndExactModelScopes(t *testing.T) {
	row, a, binding, now := procurementEvidenceFixture(t)
	before := mustJSONForEvidence(t, a)
	got := BuildProcurementSourceEvidence(&row, a, binding, now)
	require.Equal(t, "supplier_declared", got.Selection.Status)
	require.Equal(t, "new_api.management", got.Selection.Source)
	require.Equal(t, "vip", got.Selection.GroupID)
	require.Equal(t, "declared", got.Models.Status)
	require.Equal(t, "new_api.pricing", got.Models.Source)
	require.Len(t, got.Models.Entries, 2)
	require.Equal(t, "covered", got.Models.Entries[0].Conclusion)
	require.Equal(t, "excluded", got.Models.Entries[1].Conclusion)
	require.Equal(t, "priority", got.Models.Entries[1].ServiceTier)
	require.Equal(t, before, mustJSONForEvidence(t, a), "projection must not rewrite stored declarations or extend their TTL")
	encoded := string(mustJSONForEvidence(t, got))
	for _, secret := range []string{"fixture-private-key", "fixture-private-site", binding.Fingerprint} {
		require.NotContains(t, encoded, secret)
	}
}

func TestProcurementSourceEvidenceNeverBorrowsAnotherGroupOrStaleIdentity(t *testing.T) {
	for _, test := range []struct {
		name, want string
		change     func(*ProcurementSourceMapping, *Account, *NewAPIAccountBinding)
	}{
		{"registered group mismatch", "mismatch", func(r *ProcurementSourceMapping, _ *Account, _ *NewAPIAccountBinding) { r.ExternalGroupID = "other" }},
		{"binding group mismatch", "mismatch", func(_ *ProcurementSourceMapping, _ *Account, b *NewAPIAccountBinding) { b.Group = "other" }},
		{"bound token mismatch", "mismatch", func(_ *ProcurementSourceMapping, _ *Account, b *NewAPIAccountBinding) { b.TokenID = 4 }},
		{"credential rotated", "unavailable", func(_ *ProcurementSourceMapping, a *Account, _ *NewAPIAccountBinding) {
			a.Credentials["api_key"] = "rotated"
		}},
		{"site mismatch", "unavailable", func(_ *ProcurementSourceMapping, _ *Account, b *NewAPIAccountBinding) {
			b.Profile.SiteURL = "https://other.example"
		}},
		{"route changed", "unavailable", func(r *ProcurementSourceMapping, _ *Account, _ *NewAPIAccountBinding) { r.RouteIdentity = "changed" }},
		{"vendor mismatch", "unavailable", func(r *ProcurementSourceMapping, _ *Account, _ *NewAPIAccountBinding) { r.Platform = PlatformGemini }},
		{"removed membership", "unavailable", func(r *ProcurementSourceMapping, _ *Account, _ *NewAPIAccountBinding) { r.InLocalGroup = false }},
		{"paused declaration", "unavailable", func(r *ProcurementSourceMapping, _ *Account, _ *NewAPIAccountBinding) { r.Enabled = false }},
		{"deleted account", "unavailable", func(r *ProcurementSourceMapping, _ *Account, _ *NewAPIAccountBinding) { r.AccountDeleted = true }},
		{"public reference only", "unverified", func(_ *ProcurementSourceMapping, a *Account, _ *NewAPIAccountBinding) {
			s := decodeUpstreamBillingProbeSnapshot(a.Extra)
			s.Data["group_rate_source"] = "public_pricing"
			a.Extra[UpstreamBillingProbeExtraKey] = s
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			row, a, binding, now := procurementEvidenceFixture(t)
			test.change(&row, a, binding)
			got := BuildProcurementSourceEvidence(&row, a, binding, now)
			require.Equal(t, test.want, got.Selection.Status)
			require.Equal(t, "unavailable", got.Models.Status)
			require.Empty(t, got.Models.Entries)
		})
	}
}

func TestProcurementSourceEvidenceFreshnessAndModelUnknowns(t *testing.T) {
	row, a, binding, now := procurementEvidenceFixture(t)
	snap := decodeUpstreamBillingProbeSnapshot(a.Extra)
	got := BuildProcurementSourceEvidence(&row, a, binding, *snap.FreshUntil)
	require.Equal(t, "expired", got.Selection.Status)
	require.Empty(t, got.Models.Entries)
	snap.Status = UpstreamBillingProbeStatusFailed
	a.Extra[UpstreamBillingProbeExtraKey] = snap
	got = BuildProcurementSourceEvidence(&row, a, binding, now)
	require.Equal(t, "failed", got.Selection.Status)
	require.Empty(t, got.Models.Entries)
	snap.Status = UpstreamBillingProbeStatusOK
	delete(snap.Data, upstreamBillingApplicabilityKey)
	got = BuildProcurementSourceEvidence(&row, a, binding, now)
	require.Equal(t, "not_declared", got.Models.Status)
	snap.Data[upstreamBillingApplicabilityKey] = nil
	got = BuildProcurementSourceEvidence(&row, a, binding, now)
	require.Equal(t, "invalid", got.Models.Status)
	declaration := billingApplicabilityFixture(*snap.ReceivedAt, billingApplicabilityEntry("responses_http", "priority", true), billingApplicabilityEntry("responses_http", "priority", false))
	declaration["source"] = "private-injected-source"
	declaration["expires_at"] = now.Add(time.Minute)
	snap.Data["observed_at"] = snap.ReceivedAt.Format(time.RFC3339Nano)
	snap.Data[upstreamBillingApplicabilityKey] = declaration
	got = BuildProcurementSourceEvidence(&row, a, binding, now)
	require.Equal(t, "declared", got.Models.Status)
	require.Equal(t, "conflict", got.Models.Entries[0].Conclusion)
	require.Equal(t, "new_api.pricing", got.Models.Source)
}

func TestProcurementSourceEvidenceSub2APIRequiresTheRequestedExternalGroup(t *testing.T) {
	row, a, _, now := procurementEvidenceFixture(t)
	row.ExternalGroupID = "9007199254740993"
	a.Credentials["header_override_enabled"] = true
	a.Credentials["header_overrides"] = map[string]any{APIKeyGroupHeader: row.ExternalGroupID}
	snap := decodeUpstreamBillingProbeSnapshot(a.Extra)
	snap.Data = map[string]any{"object": "sub2api.key_billing", "schema_version": 1, "billing_scope": "token", "selected_group_id": row.ExternalGroupID, "resolved_rate_multiplier": 0, "observed_at": snap.ReceivedAt.Format(time.RFC3339Nano), upstreamBillingApplicabilityKey: billingApplicabilityFixture(*snap.ReceivedAt, billingApplicabilityEntry("responses_http", "unspecified", true))}
	a.Extra[UpstreamBillingProbeExtraKey] = snap
	got := BuildProcurementSourceEvidence(&row, a, nil, now)
	require.Equal(t, "receipt_confirmed", got.Selection.Status)
	require.Equal(t, row.ExternalGroupID, got.Selection.GroupID, "external IDs never pass through float64")
	require.Equal(t, "declared", got.Models.Status)
	delete(a.Credentials, "header_override_enabled")
	got = BuildProcurementSourceEvidence(&row, a, nil, now)
	require.Equal(t, "unverified", got.Selection.Status, "an unpinned default must not inherit a receipt or a local Group ID")
	require.Empty(t, got.Models.Entries)
	a.Credentials["header_override_enabled"] = true
	row.ExternalGroupID = "17"
	got = BuildProcurementSourceEvidence(&row, a, nil, now)
	require.Equal(t, "mismatch", got.Selection.Status)
	require.Empty(t, got.Models.Entries)
	row.ExternalGroupID = "9007199254740993"
	row.Platform, row.AccountPlatform, row.LocalGroupPlatform, a.Platform = PlatformGemini, PlatformGemini, PlatformGemini, PlatformGemini
	got = BuildProcurementSourceEvidence(&row, a, nil, now)
	require.Equal(t, "unverified", got.Selection.Status, "Gemini does not send the configured header override; a stored receipt is not routing support")
	require.Empty(t, got.Models.Entries)
}

func TestProcurementSourceEvidenceRejectsRoundedIDsAndFutureOrInvalidSnapshots(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*UpstreamBillingProbeSnapshot)
	}{
		{"future observation", func(s *UpstreamBillingProbeSnapshot) { s.ReceivedAt = probeTimePtr(time.Now().Add(time.Hour)) }},
		{"future attempt", func(s *UpstreamBillingProbeSnapshot) { s.LastAttemptAt = time.Now().Add(time.Hour) }},
		{"missing expiry", func(s *UpstreamBillingProbeSnapshot) { s.FreshUntil = nil }},
		{"unknown billing schema", func(s *UpstreamBillingProbeSnapshot) { s.Data["schema_version"] = 2 }},
		{"nonfinite rate", func(s *UpstreamBillingProbeSnapshot) { s.Data["resolved_rate_multiplier"] = math.Inf(1) }},
		{"negative rate", func(s *UpstreamBillingProbeSnapshot) { s.Data["resolved_rate_multiplier"] = -1 }},
		{"rounded token ID", func(s *UpstreamBillingProbeSnapshot) { s.Data["token_id"] = float64(9007199254740992) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			row, a, b, now := procurementEvidenceFixture(t)
			s := decodeUpstreamBillingProbeSnapshot(a.Extra)
			test.mutate(s)
			a.Extra[UpstreamBillingProbeExtraKey] = s
			e := BuildProcurementSourceEvidence(&row, a, b, now)
			require.NotContains(t, []string{"supplier_declared", "receipt_confirmed"}, e.Selection.Status)
			require.Empty(t, e.Models.Entries)
		})
	}
}
