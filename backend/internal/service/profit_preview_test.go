package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPreviewProfitAdmissionUsesAccountRatesAndPreinitializesModels(t *testing.T) {
	now := time.Now()
	group := profitControlTestGroup(50, 0.2, 0)
	group.Name = "VIP-preview"

	cheap := profitControlTestAccountWithRate(
		upstreamCostTestAccount(1, UpstreamBillingProbeStatusOK, 1.0, now.Add(-3*time.Hour), 30*time.Minute),
		0.5,
	)
	cheap.Name = "cheap"
	cheap.Extra[UpstreamBillingRateSyncEnabledExtraKey] = true
	cheap.Credentials = map[string]any{"model_mapping": map[string]any{"gpt-sol": "gpt-sol"}}

	boundary := profitControlTestAccountWithRate(
		upstreamCostTestAccount(2, UpstreamBillingProbeStatusOK, 0.8, now.Add(-time.Minute), 30*time.Minute),
		0.8,
	)
	boundary.Name = "boundary"
	boundary.Credentials = map[string]any{"model_mapping": map[string]any{
		"gpt-sol":  "gpt-sol",
		"gpt-luna": "gpt-luna",
	}}

	expensive := profitControlTestAccountWithRate(
		upstreamCostTestAccount(3, UpstreamBillingProbeStatusOK, 0.2, now.Add(-time.Minute), 30*time.Minute),
		1.0,
	)
	expensive.Name = "expensive"

	invalid := upstreamCostTestOAuthAccount(4)
	invalid.Name = "invalid"

	reports := PreviewProfitAdmission([]ProfitPreviewGroupInput{{
		Group:         group,
		Accounts:      []*Account{cheap, boundary, expensive, invalid},
		UserOverrides: map[int64]float64{40: 0.5},
		Models:        []string{"gpt-sol", "gpt-luna", "gpt-no-account"},
	}}, now)
	require.Len(t, reports, 1)
	report := reports[0]

	require.True(t, report.EffectiveGate)
	require.InDelta(t, 1.0, report.DefaultD, 1e-12)
	require.InDelta(t, 0.8, report.ThresholdDefault, 1e-12)
	require.InDelta(t, 0.5, report.MinEffectiveD, 1e-12)
	require.InDelta(t, 0.4, report.ThresholdMinD, 1e-12)

	byID := map[int64]ProfitPreviewAccountVerdict{}
	for _, verdict := range report.Verdicts {
		byID[verdict.AccountID] = verdict
	}
	require.Equal(t, ProfitPreviewClassAdmitted, byID[cheap.ID].Class)
	require.Equal(t, ProfitPreviewRateSourceManual, byID[cheap.ID].RateSource, "probe switch is off, so sync is inactive")
	require.Empty(t, byID[cheap.ID].Warnings)
	require.True(t, byID[cheap.ID].RejectedUnderMinD)

	require.Equal(t, ProfitPreviewClassAdmitted, byID[boundary.ID].Class, "U == 阈值按 epsilon 语义准入")
	require.Equal(t, ProfitPreviewClassRejectedThreshold, byID[expensive.ID].Class)
	require.Contains(t, byID[expensive.ID].Warnings, ProfitPreviewWarningManualRateOne)
	require.Equal(t, ProfitPreviewClassRejectedInvalidRate, byID[invalid.ID].Class)

	require.Equal(t, 2, report.RemainingByModel["gpt-sol"])
	require.Equal(t, 1, report.RemainingByModel["gpt-luna"])
	require.Equal(t, 0, report.RemainingByModel["gpt-no-account"])
	_, present := report.RemainingByModel["gpt-no-account"]
	require.True(t, present, "全部账号均不支持时也必须显式保留 0，供 CLI 发出警告")
}

func TestPreviewProfitAdmissionAssumeEnabled(t *testing.T) {
	now := time.Now()
	group := profitControlTestGroup(51, 0, 0)
	group.Platform = PlatformOpenAI
	group.RateMultiplier = 0.5
	group.ProfitControlEnabled = false

	cheapAccount := profitControlTestAccountWithRate(
		upstreamCostTestAccount(1, UpstreamBillingProbeStatusOK, 0.9, now.Add(-time.Minute), 30*time.Minute),
		0.2,
	)
	expensiveAccount := profitControlTestAccountWithRate(
		upstreamCostTestAccount(2, UpstreamBillingProbeStatusOK, 0.2, now.Add(-time.Minute), 30*time.Minute),
		0.9,
	)

	withoutAssume := PreviewProfitAdmission([]ProfitPreviewGroupInput{{
		Group:    group,
		Accounts: []*Account{cheapAccount, expensiveAccount},
		Models:   []string{"gpt-test"},
	}}, now)[0]
	require.False(t, withoutAssume.EffectiveGate)
	require.Equal(t, ProfitPreviewClassAdmitted, withoutAssume.Verdicts[0].Class)
	require.Equal(t, ProfitPreviewClassAdmitted, withoutAssume.Verdicts[1].Class)

	withAssume := PreviewProfitAdmission([]ProfitPreviewGroupInput{{
		Group:         group,
		Accounts:      []*Account{cheapAccount, expensiveAccount},
		Models:        []string{"gpt-test"},
		AssumeEnabled: true,
	}}, now)[0]
	require.True(t, withAssume.EffectiveGate)
	require.True(t, withAssume.AssumedEnabled)

	byID := map[int64]ProfitPreviewAccountVerdict{}
	for _, verdict := range withAssume.Verdicts {
		byID[verdict.AccountID] = verdict
	}
	require.Equal(t, ProfitPreviewClassAdmitted, byID[cheapAccount.ID].Class,
		"账号倍率 0.2 <= 阈值 0.5，探测快照的高倍率不参与准入判断")
	require.Equal(t, ProfitPreviewClassRejectedThreshold, byID[expensiveAccount.ID].Class,
		"账号倍率 0.9 > 阈值 0.5，探测快照的低倍率不能替代账号倍率")
}

func TestPreviewProfitAdmissionSupportsFivePlatforms(t *testing.T) {
	for i, platform := range []string{
		PlatformOpenAI,
		PlatformAnthropic,
		PlatformGemini,
		PlatformGrok,
		PlatformAntigravity,
	} {
		group := profitControlTestGroup(int64(100+i), 0, 0)
		group.Platform = platform
		rate := 0.2
		account := &Account{
			ID:             int64(200 + i),
			Platform:       platform,
			RateMultiplier: &rate,
		}
		report := PreviewProfitAdmission([]ProfitPreviewGroupInput{{
			Group:    group,
			Accounts: []*Account{account},
			Models:   []string{"model"},
		}}, time.Now())[0]
		require.True(t, report.EffectiveGate, platform)
		require.Equal(t, ProfitPreviewClassAdmitted, report.Verdicts[0].Class, platform)
	}
}

func TestPreviewProfitAdmissionFinalScopes(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	for _, tc := range []struct {
		name, status, reason string
		mutate               func(*Account, *AvailabilityRequestScope)
		admitted             bool
	}{
		{"standard covered", "covered", "", func(*Account, *AvailabilityRequestScope) {}, true},
		{"fast excluded", "excluded", "profit_upstream_scope_excluded", func(_ *Account, s *AvailabilityRequestScope) { s.ServiceTier = "priority" }, false},
		{"other effort unknown", "unknown", "profit_upstream_scope_unknown", func(_ *Account, s *AvailabilityRequestScope) { s.ReasoningEffort = "high" }, false},
		{"unknown tier is not a wire scope", "unknown", "profit_upstream_scope_unknown", func(_ *Account, s *AvailabilityRequestScope) { s.ServiceTier = "future-tier" }, false},
		{"tools not borrowed", "unknown", "profit_upstream_scope_unknown", func(_ *Account, s *AvailabilityRequestScope) { s.Capability = "tools" }, false},
		{"legacy not qualified", "not_declared", "profit_upstream_scope_not_declared", func(a *Account, _ *AvailabilityRequestScope) {
			snap := decodeUpstreamBillingProbeSnapshot(a.Extra)
			delete(snap.Data, upstreamBillingApplicabilityKey)
			a.Extra[UpstreamBillingProbeExtraKey] = snap
		}, false},
		{"manual not qualified", "not_declared", "profit_upstream_scope_not_declared", func(a *Account, _ *AvailabilityRequestScope) { a.Extra[UpstreamBillingRateSyncEnabledExtraKey] = false }, false},
		{"expired independently", "expired", "profit_upstream_scope_expired", func(a *Account, _ *AvailabilityRequestScope) {
			snap := decodeUpstreamBillingProbeSnapshot(a.Extra)
			snap.Data[upstreamBillingApplicabilityKey].(map[string]any)["expires_at"] = at.Add(30 * time.Second).Format(time.RFC3339Nano)
			a.Extra[UpstreamBillingProbeExtraKey] = snap
		}, false},
		{"unknown public alias", "covered", "model_not_supported", func(_ *Account, s *AvailabilityRequestScope) {}, false},
		{"public mapping changed", "covered", "model_mapping_unconfirmed", func(a *Account, _ *AvailabilityRequestScope) {
			a.Credentials["model_mapping"].(map[string]any)["public-fixture"] = "another-wire-model"
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := billingApplicabilityAccount(t, at, billingApplicabilityFixture(at,
				billingApplicabilityEntry("responses_http", "unspecified", true), billingApplicabilityEntry("responses_http", "priority", false)))
			account.Credentials["model_mapping"] = map[string]any{"public-fixture": "gpt-fixture", "gpt-fixture": "must-not-map-twice"}
			scope := AvailabilityRequestScope{Version: availabilityRequestScopeVersion, Protocol: "responses_http", Model: "gpt-fixture", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}
			tc.mutate(account, &scope)
			publicModel := "public-fixture"
			if tc.name == "unknown public alias" {
				publicModel = "not-permitted"
			}
			group := profitControlTestGroup(80, 0, 0)
			group.RateMultiplier = 0.5
			report := PreviewProfitAdmission([]ProfitPreviewGroupInput{{Group: group, Accounts: []*Account{account}, Models: []string{"public-fixture"}, UserOverrides: map[int64]float64{7: 0.3}, RequestScopes: []ProfitPreviewRequestScope{{AccountID: account.ID, PublicModel: publicModel, Scope: scope}}}}, at.Add(time.Minute))[0]
			require.Equal(t, "account_price_only", report.ModelCountsScope)
			require.Len(t, report.ScopeVerdicts, 1)
			result := report.ScopeVerdicts[0]
			require.Equal(t, tc.status, result.Applicability)
			require.Equal(t, tc.reason, result.Reason)
			require.Equal(t, tc.admitted, result.Admitted)
			require.False(t, result.AdmittedUnderMinD, "scope coverage must not override the lower selling-price threshold")
			require.Equal(t, "gpt-fixture", result.Scope.Model, "the explicitly supplied final model is not mapped a second time")
			require.Equal(t, 1, report.RemainingByModel["public-fixture"], "legacy count remains explicitly account-price-only")
		})
	}
}

func TestPreviewProfitAdmissionScopeGateAndProtocolBoundaries(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	account := billingApplicabilityAccount(t, at, billingApplicabilityFixture(at, billingApplicabilityEntry("responses_ws_v2", "unspecified", true)))
	scope := AvailabilityRequestScope{Version: availabilityRequestScopeVersion, Protocol: "responses_ws_v2", Stream: true, Model: "gpt-fixture", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}
	group := profitControlTestGroup(81, 0, 0)
	group.ProfitControlEnabled = false
	input := ProfitPreviewGroupInput{Group: group, Accounts: []*Account{account}, RequestScopes: []ProfitPreviewRequestScope{{AccountID: account.ID, PublicModel: "gpt-fixture", Scope: scope}}}
	result := PreviewProfitAdmission([]ProfitPreviewGroupInput{input}, at.Add(time.Minute))[0].ScopeVerdicts[0]
	require.Equal(t, "covered", result.Applicability)
	require.Equal(t, "profit_gate_disabled", result.Reason)
	require.False(t, result.Admitted)
	input.AssumeEnabled = true
	result = PreviewProfitAdmission([]ProfitPreviewGroupInput{input}, at.Add(time.Minute))[0].ScopeVerdicts[0]
	require.True(t, result.Admitted)
	input.RequestScopes[0].Scope.Protocol = "responses_http"
	result = PreviewProfitAdmission([]ProfitPreviewGroupInput{input}, at.Add(time.Minute))[0].ScopeVerdicts[0]
	require.Equal(t, "unknown", result.Applicability, "HTTP does not borrow a WS declaration")
	require.False(t, result.Admitted)
	input.RequestScopes[0].AccountID = 999
	result = PreviewProfitAdmission([]ProfitPreviewGroupInput{input}, at.Add(time.Minute))[0].ScopeVerdicts[0]
	require.Equal(t, "account_unavailable", result.Reason)
	require.False(t, result.Admitted)
}

func TestPreviewProfitAdmissionResponsesOutputExactScope(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	entry := billingApplicabilityEntry("responses_http", "unspecified", true)
	entry["reasoning_summary"], entry["include_encrypted_content"] = "auto", true
	declaration := billingApplicabilityFixture(at, entry)
	declaration["schema_version"] = 2
	account := billingApplicabilityAccount(t, at, declaration)
	scope := AvailabilityRequestScope{Version: availabilityResponsesOutputScopeVersion, Protocol: "responses_http", Model: "gpt-fixture", ServiceTier: "unspecified", ReasoningEffort: "unspecified", ReasoningSummary: "auto", IncludeEncryptedContent: true, Capability: "text"}
	for _, tc := range []struct {
		name   string
		mutate func(*AvailabilityRequestScope)
		want   string
	}{
		{"exact", func(*AvailabilityRequestScope) {}, "covered"},
		{"old version", func(s *AvailabilityRequestScope) { s.Version = availabilityRequestScopeVersion }, "unknown"},
		{"different summary", func(s *AvailabilityRequestScope) { s.ReasoningSummary = "none" }, "unknown"},
		{"encrypted output absent", func(s *AvailabilityRequestScope) { s.IncludeEncryptedContent = false }, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requestScope := scope
			tc.mutate(&requestScope)
			group := profitControlTestGroup(7, 0.5, 0)
			input := ProfitPreviewGroupInput{Group: group, Accounts: []*Account{account}, RequestScopes: []ProfitPreviewRequestScope{{AccountID: account.ID, PublicModel: "gpt-fixture", Scope: requestScope}}}
			verdict := PreviewProfitAdmission([]ProfitPreviewGroupInput{input}, at.Add(time.Second))[0].ScopeVerdicts[0]
			require.Equal(t, tc.want, verdict.Applicability)
			require.Equal(t, tc.want == "covered", verdict.Admitted)
		})
	}
}

func TestPreviewProfitAdmissionResponsesVerbosityExactScope(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	entry := billingApplicabilityEntry("responses_http", "unspecified", true)
	entry["text_verbosity"] = "medium"
	declaration := billingApplicabilityFixture(at, entry)
	declaration["schema_version"] = 3
	account := billingApplicabilityAccount(t, at, declaration)
	for _, tc := range []struct {
		verbosity string
		version   int
		want      string
	}{
		{"medium", availabilityResponsesTextScopeVersion, "covered"},
		{"medium", availabilityResponsesOutputScopeVersion, "unknown"},
		{"low", availabilityResponsesTextScopeVersion, "unknown"},
		{"high", availabilityResponsesTextScopeVersion, "unknown"},
		{"", availabilityRequestScopeVersion, "unknown"},
	} {
		t.Run(fmt.Sprintf("%s/v%d", tc.verbosity, tc.version), func(t *testing.T) {
			scope := AvailabilityRequestScope{Version: tc.version, Protocol: "responses_http", Model: "gpt-fixture", ServiceTier: "unspecified", ReasoningEffort: "unspecified", TextVerbosity: tc.verbosity, Capability: "text"}
			input := ProfitPreviewGroupInput{Group: profitControlTestGroup(7, .5, 0), Accounts: []*Account{account}, RequestScopes: []ProfitPreviewRequestScope{{AccountID: account.ID, PublicModel: "gpt-fixture", Scope: scope}}}
			verdict := PreviewProfitAdmission([]ProfitPreviewGroupInput{input}, at.Add(time.Second))[0].ScopeVerdicts[0]
			require.Equal(t, tc.want, verdict.Applicability)
			require.Equal(t, tc.want == "covered", verdict.Admitted)
		})
	}
}
