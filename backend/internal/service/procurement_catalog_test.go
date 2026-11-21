package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

const catalogCommandFixture = `{"version":1,"revision":0,"buckets":[{"code":"b1","name":"First","min_rate":"0.2","max_rate":"0.5"},{"code":"b2","name":"Second","min_rate":"0.5","max_rate":"0.6"}]}`

func TestProcurementEvaluatedGatesIntersection(t *testing.T) {
	at := time.Now().UTC()
	qualityUntil, availabilityUntil := at.Add(time.Minute), at.Add(2*time.Minute)
	for _, tc := range []struct {
		name, price, quality, availability, state, reason string
		passed, required                                  int
	}{
		{"all passed", "constraints_passed", "pass", "pass", "pass", "all_evaluated_gates_passed", 3, 3},
		{"price only", "constraints_passed", "not_required", "not_required", "pass", "all_evaluated_gates_passed", 1, 1},
		{"quality unknown", "constraints_passed", "unknown", "pass", "unknown", "answer_quality_unavailable", 2, 3},
		{"quality failed", "constraints_passed", "fail", "pass", "fail", "answer_quality_failed", 2, 3},
		{"availability unknown", "constraints_passed", "pass", "unknown", "unknown", "availability_unavailable", 2, 3},
		{"availability failed", "constraints_passed", "pass", "fail", "fail", "availability_failed", 2, 3},
		{"failure beats unknown", "constraints_passed", "unknown", "fail", "fail", "availability_failed", 1, 3},
		{"price blocked", "blocked", "pass", "pass", "fail", "price_constraints_failed", 2, 3},
		{"price disabled", "disabled", "not_required", "not_required", "unknown", "price_constraints_unavailable", 0, 1},
		{"not associated", "not_associated", "pass", "unknown", "fail", "price_constraints_failed", 1, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := &ProcurementPolicy{Quality: "current_answer", Availability: "current_state"}
			if tc.quality == "not_required" {
				policy.Quality = "not_required"
			}
			if tc.availability == "not_required" {
				policy.Availability = "not_required"
			}
			quality := &QualityScopeEvidence{Version: 1, Status: tc.quality, Reason: "all_rules_passed", EvaluatedAt: at, ValidUntil: &qualityUntil, RuleCount: 1, ConfirmedRules: 1, Samples: 1, ValidSamples: 1}
			availability := &AvailabilityScopeEvidence{Version: 1, Status: tc.availability, Reason: "all_rules_passed", EvaluatedAt: at, ValidUntil: &availabilityUntil, RuleCount: 1, ConfirmedRules: 1}
			projection := &ProcurementProductPreview{State: tc.price, PriceClass: ProfitPreviewClassRejectedThreshold, PriceValidUntil: probeTimePtr(at.Add(ProcurementPreviewTTL)),
				AccountControls: procurementAccountControlsAt(&Account{ID: 1, Status: StatusActive, Schedulable: true}, 1, at)}
			projection.AssessEvidence(policy, quality, availability, at)
			require.Equal(t, tc.state, projection.EvaluatedGates.State)
			require.Equal(t, tc.reason, projection.EvaluatedGates.Reason)
			require.Equal(t, tc.passed+1, projection.EvaluatedGates.PassedGates)
			require.Equal(t, tc.required+1, projection.EvaluatedGates.RequiredGates)
			require.Equal(t, at, projection.EvaluatedGates.EvaluatedAt)
			require.Equal(t, at.Add(ProcurementPreviewTTL), *projection.EvaluatedGates.ValidUntil)
		})
	}
}

func TestProcurementEvaluatedGatesExpiryAndNoCertification(t *testing.T) {
	at := time.Now().UTC()
	until := at.Add(time.Second)
	policy := &ProcurementPolicy{Quality: "current_answer"}
	quality := &QualityScopeEvidence{Version: 1, Status: "pass", Reason: "all_rules_passed", EvaluatedAt: at, ValidUntil: &until, RuleCount: 1, ConfirmedRules: 1, Samples: 1, ValidSamples: 1}
	p := &ProcurementProductPreview{State: "constraints_passed", PriceValidUntil: probeTimePtr(at.Add(ProcurementPreviewTTL))}
	p.AccountControls = procurementAccountControlsAt(&Account{ID: 1, Status: StatusActive, Schedulable: true}, 1, at)
	p.AssessEvidence(policy, quality, nil, at)
	require.Equal(t, "pass", p.EvaluatedGates.State)
	*p.EvaluatedGates.ValidUntil = until.Add(time.Hour)
	require.Equal(t, at.Add(time.Second), *quality.ValidUntil, "projection must not extend source evidence")
	p.AccountControls = procurementAccountControlsAt(&Account{ID: 1, Status: StatusActive, Schedulable: true}, 1, until)
	p.PriceValidUntil = probeTimePtr(until.Add(ProcurementPreviewTTL))
	p.AssessEvidence(policy, quality, nil, until)
	require.Equal(t, "unknown", p.EvaluatedGates.State)
	require.Equal(t, "answer_quality_unavailable", p.EvaluatedGates.Reason)
	require.Equal(t, until.Add(ProcurementPreviewTTL), *p.EvaluatedGates.ValidUntil)
	p.AccountControls = procurementAccountControlsAt(&Account{ID: 1, Status: StatusActive, Schedulable: true}, 1, at)
	p.PriceValidUntil = probeTimePtr(at.Add(ProcurementPreviewTTL))
	p.AssessEvidence(policy, nil, nil, at)
	require.Equal(t, "unknown", p.EvaluatedGates.State)
	for _, priceClass := range []string{ProfitPreviewClassUnknownCatalog, ProfitPreviewClassUnknownProcurement, ProfitPreviewClassRejectedUpstreamRate, ProfitPreviewClassRejectedInvalidRate, "future_class"} {
		p.State, p.PriceClass = "blocked", priceClass
		p.AssessEvidence(nil, nil, nil, at)
		require.Equal(t, "unknown", p.EvaluatedGates.State, priceClass)
		require.Equal(t, "price_constraints_unavailable", p.EvaluatedGates.Reason)
	}
	var absent *ProcurementProductPreview
	absent.AssessEvidence(policy, quality, nil, at)
}

func TestProcurementCatalogStrictCommand(t *testing.T) {
	c, err := ParseProcurementCatalogCommand([]byte(catalogCommandFixture))
	require.NoError(t, err)
	require.Len(t, c.Buckets, 2)
	for _, raw := range []string{
		`null`, `{}`, `[]`, strings.Replace(catalogCommandFixture, `"revision":0`, `"revision":null`, 1),
		strings.Replace(catalogCommandFixture, `"revision":0`, `"Revision":0`, 1),
		strings.Replace(catalogCommandFixture, `"revision":0`, `"revision":0,"revision":1`, 1),
		strings.Replace(catalogCommandFixture, `"revision":0`, `"revision":0,"revision":0`, 1),
		strings.Replace(catalogCommandFixture, `"code":"b1"`, `"code":"b1","code":"b2"`, 1),
		strings.Replace(catalogCommandFixture, `"code":"b1"`, `"code":"b1","code":"b1"`, 1),
		strings.Replace(catalogCommandFixture, `"min_rate":"0.2",`, ``, 1),
		strings.Replace(catalogCommandFixture, `"max_rate":"0.5"`, `"max_rate":"0.5","credential":"private"`, 1),
		`{"version":1,"revision":0,"buckets":null}`,
		catalogCommandFixture + ` {}`,
	} {
		_, err := ParseProcurementCatalogCommand([]byte(raw))
		require.ErrorIs(t, err, ErrProcurementCatalogInvalid, raw)
	}
}

func TestProcurementCatalogPreviewScopeAndPriceAuthority(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	catalog, err := ParseProcurementCatalogCommand([]byte(catalogCommandFixture))
	require.NoError(t, err)
	catalog = catalog.WithRevision(1)
	request := ProfitPreviewRequestScope{AccountID: 1, PublicModel: "gpt-fixture", Scope: AvailabilityRequestScope{Version: 2, Protocol: "responses_http", Model: "gpt-fixture", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}}
	for _, tc := range []struct {
		name        string
		mutate      func(*Account, *ProfitPreviewRequestScope)
		state, code string
	}{
		{"covered", func(*Account, *ProfitPreviewRequestScope) {}, "classified", "b1"},
		{"price rise", func(a *Account, _ *ProfitPreviewRequestScope) { v := .5; a.RateMultiplier = &v }, "classified", "b2"},
		{"gap", func(a *Account, _ *ProfitPreviewRequestScope) { v := .6; a.RateMultiplier = &v }, "unclassified", ""},
		{"wrong model", func(_ *Account, r *ProfitPreviewRequestScope) { r.Scope.Model = "other" }, "scope_unconfirmed", ""},
		{"unsupported public", func(_ *Account, r *ProfitPreviewRequestScope) { r.PublicModel = "other" }, "scope_unconfirmed", ""},
		{"priority", func(_ *Account, r *ProfitPreviewRequestScope) { r.Scope.ServiceTier = "priority" }, "scope_unconfirmed", ""},
		{"unknown scope", func(_ *Account, r *ProfitPreviewRequestScope) { r.Scope.Version = 99 }, "scope_unconfirmed", ""},
		{"manual no declaration", func(a *Account, _ *ProfitPreviewRequestScope) { a.Extra = map[string]any{} }, "scope_unconfirmed", ""},
		{"missing price", func(a *Account, _ *ProfitPreviewRequestScope) { a.RateMultiplier = nil }, "rate_unavailable", ""},
		{"stale", func(a *Account, _ *ProfitPreviewRequestScope) {
			s := decodeUpstreamBillingProbeSnapshot(a.Extra)
			until := at
			s.FreshUntil = &until
			a.Extra[UpstreamBillingProbeExtraKey] = s
		}, "rate_unavailable", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := billingApplicabilityAccount(t, at, billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", true)))
			a.ID = 1
			a.Credentials["model_mapping"] = map[string]any{"gpt-fixture": "gpt-fixture"}
			r := request
			tc.mutate(a, &r)
			result := ClassifyProcurementScope(catalog, a, r, at.Add(time.Second), nil)
			require.Equal(t, tc.state, result.State)
			require.Equal(t, "not_evaluated", result.Qualification)
			if tc.code == "" {
				require.Nil(t, result.Bucket)
			} else {
				require.Equal(t, tc.code, result.Bucket.Code)
				require.NotNil(t, result.EffectiveRate)
			}
			raw, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "credentials")
			require.NotContains(t, string(raw), "test-api-key")
		})
	}
	result := ClassifyProcurementScope(catalog, nil, request, at, nil)
	require.Equal(t, "account_unavailable", result.State)
	result = ClassifyProcurementScope(&domain.ProcurementCatalog{Version: 1, Buckets: []domain.ProcurementBucket{}}, nil, request, at, nil)
	require.Equal(t, "not_configured", result.State)
}

func TestProcurementPreviewStrictInputAndResponsesVerbosity(t *testing.T) {
	raw := `{"requests":[{"account_id":1,"public_model":"gpt-fixture","scope":{"version":4,"protocol":"responses_http","stream":false,"model":"gpt-fixture","service_tier":"unspecified","reasoning_effort":"unspecified","capability":"text","text_verbosity":"medium"}}]}`
	r, err := ParseProcurementPreviewCommand([]byte(raw))
	require.NoError(t, err)
	require.Equal(t, "medium", r.Requests[0].Scope.TextVerbosity)
	require.Nil(t, r.ProductGroupID)
	withProduct := strings.Replace(raw, `{"requests":`, `{"product_group_id":42,"requests":`, 1)
	r, err = ParseProcurementPreviewCommand([]byte(withProduct))
	require.NoError(t, err)
	require.EqualValues(t, 42, *r.ProductGroupID)
	for _, value := range []string{`null`, `0`, `-1`, `1.1`, `"42"`, `9007199254740992`, `true`} {
		_, err = ParseProcurementPreviewCommand([]byte(strings.Replace(withProduct, `"product_group_id":42`, `"product_group_id":`+value, 1)))
		require.ErrorIs(t, err, ErrProcurementCatalogInvalid, value)
	}
	_, err = ParseProcurementPreviewCommand([]byte(strings.Replace(withProduct, `"product_group_id":42`, `"product_group_id":42,"product_group_id":42`, 1)))
	require.ErrorIs(t, err, ErrProcurementCatalogInvalid)
	for _, invalid := range []string{`{}`, `{"requests":null}`, `{"requests":[]}`, strings.Replace(raw, `"account_id":1`, `"account_id":0`, 1), strings.Replace(raw, `"public_model":"gpt-fixture"`, `"public_model":null`, 1), strings.Replace(raw, `"text_verbosity":"medium"`, `"text_verbosity":"future"`, 1), strings.Replace(raw, `"requests":`, `"confirm":true,"requests":`, 1), strings.Replace(raw, `"model":"gpt-fixture"`, `"model":"gpt-fixture","model":"other"`, 1), strings.Replace(raw, `"account_id":1`, `"account_id":1,"rate":0.02`, 1),
		strings.Replace(raw, `"account_id":1`, `"account_id":1,"account_id":1`, 1),
		strings.Replace(raw, `"model":"gpt-fixture"`, `"model":"gpt-fixture","model":"gpt-fixture"`, 1), strings.Replace(raw, `"requests":[`, `"requests":[],"requests":[`, 1)} {
		_, err := ParseProcurementPreviewCommand([]byte(invalid))
		require.ErrorIs(t, err, ErrProcurementCatalogInvalid, invalid)
	}
}

func TestProcurementAccountControlsRejectProtectedAccounts(t *testing.T) {
	at := time.Now().UTC()
	for _, tc := range []struct {
		name    string
		protect func(*Account)
	}{
		{"manual pause", func(a *Account) { a.Schedulable = false }},
		{"performance pause", func(a *Account) { a.PerformancePaused = true }},
		{"inactive", func(a *Account) { a.Status = StatusDisabled }},
		{"expired", func(a *Account) { a.AutoPauseOnExpired = true; a.ExpiresAt = &at }},
		{"overloaded", func(a *Account) { until := at.Add(time.Minute); a.OverloadUntil = &until }},
		{"rate limited", func(a *Account) { until := at.Add(time.Minute); a.RateLimitResetAt = &until }},
		{"temporary cooldown", func(a *Account) { until := at.Add(time.Minute); a.TempUnschedulableUntil = &until }},
		{"quota exhausted", func(a *Account) { a.Extra["quota_limit"] = 10.; a.Extra["quota_used"] = 10. }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := billingApplicabilityAccount(t, at, billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", true)))
			a.ID, a.Status, a.Schedulable, a.GroupIDs = 901, StatusActive, true, []int64{42}
			a.Credentials["model_mapping"] = map[string]any{"gpt-fixture": "gpt-fixture"}
			tc.protect(a)
			require.False(t, a.IsSchedulable())
			request := ProfitPreviewRequestScope{AccountID: a.ID, PublicModel: "gpt-fixture", Scope: AvailabilityRequestScope{Version: 2, Protocol: "responses_http", Model: "gpt-fixture", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}}
			group := &Group{ID: 42, Platform: PlatformOpenAI, Status: StatusActive, RateMultiplier: 1, ProfitControlEnabled: true}
			result := ClassifyProcurementScope(&domain.ProcurementCatalog{Version: 1, Buckets: []domain.ProcurementBucket{}}, a, request, at, group)
			require.Equal(t, "constraints_passed", result.Product.State, "price must remain independent of account protection")
			require.Equal(t, "fail", result.Product.EvaluatedGates.State, "protected accounts must not receive an aggregate pass")
			require.Equal(t, "not_evaluated", result.Qualification)
		})
	}
}

func TestProcurementAccountControlsDeadlinesAndUnknown(t *testing.T) {
	at := time.Date(2031, 1, 6, 0, 0, 0, 0, time.UTC)
	a := &Account{ID: 7, Status: StatusActive, Type: AccountTypeAPIKey, Schedulable: true}
	check := procurementAccountControlsAt(a, a.ID, at)
	require.Equal(t, "pass", check.State)
	require.Equal(t, "account_controls_passed", check.Reason)
	require.Equal(t, at.Add(30*time.Second), *check.ValidUntil)
	soon := at.Add(time.Second)
	a.AutoPauseOnExpired, a.ExpiresAt = true, &soon
	check = procurementAccountControlsAt(a, a.ID, at)
	require.Equal(t, soon, *check.ValidUntil)
	*check.ValidUntil = at.Add(time.Hour)
	require.Equal(t, at.Add(time.Second), *a.ExpiresAt, "projection must never mutate source deadlines")
	require.Equal(t, "account_expired", procurementAccountControlsAt(a, a.ID, soon).Reason)
	a.AutoPauseOnExpired, a.TempUnschedulableUntil = false, &soon
	check = procurementAccountControlsAt(a, a.ID, at)
	require.Equal(t, "account_cooldown", check.Reason)
	require.Equal(t, soon, *check.ValidUntil)
	require.Equal(t, "pass", procurementAccountControlsAt(a, a.ID, soon).State, "recovery requires a new evaluation")
	a.TempUnschedulableUntil = nil
	a.Extra = map[string]any{"quota_daily_limit": 1, "quota_daily_used": 1, "quota_daily_start": at.Add(-24*time.Hour + time.Second).Format(time.RFC3339Nano)}
	check = procurementAccountControlsAt(a, a.ID, at)
	require.Equal(t, "account_quota_exceeded", check.Reason)
	require.Equal(t, soon, *check.ValidUntil)
	a.Extra = map[string]any{"quota_daily_limit": 1, "quota_daily_used": 1, "quota_daily_start": at.Add(-9 * time.Hour).Format(time.RFC3339Nano), "quota_daily_reset_mode": "fixed", "quota_reset_timezone": "Asia/Hong_Kong"}
	check = procurementAccountControlsAt(a, a.ID, at.Add(-8*time.Hour-time.Second))
	require.True(t, at.Add(-8*time.Hour).Equal(*check.ValidUntil))
	for _, tc := range []struct {
		account *Account
		id      int64
	}{{nil, 7}, {a, 8}, {&Account{ID: 7, Status: "future-state"}, 7}} {
		unknown := procurementAccountControlsAt(tc.account, tc.id, at)
		require.Equal(t, "unknown", unknown.State)
		require.Nil(t, unknown.ValidUntil)
	}
	p := &ProcurementProductPreview{State: "constraints_passed", PriceValidUntil: probeTimePtr(at.Add(ProcurementPreviewTTL))}
	p.AssessEvidence(nil, nil, nil, at)
	require.Equal(t, "unknown", p.EvaluatedGates.State)
	require.Equal(t, "account_controls_unavailable", p.EvaluatedGates.Reason)
	p.AccountControls = procurementAccountControlsAt(a, a.ID, at)
	p.AssessEvidence(nil, nil, nil, at.Add(time.Nanosecond))
	require.Equal(t, "unknown", p.EvaluatedGates.State, "evidence from a different authority instant must not be reused")
}

func TestProcurementProductPreviewUsesOriginalGateWithoutAwardingQualification(t *testing.T) {
	at := time.Now()
	a := profitControlTestAccountWithRate(upstreamCostTestOAuthAccount(901), .025)
	a.GroupIDs = []int64{42}
	request := ProfitPreviewRequestScope{AccountID: a.ID, PublicModel: "fixture", Scope: AvailabilityRequestScope{Version: 2, Protocol: "responses_http", Model: "fixture", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}}
	catalog := &domain.ProcurementCatalog{Version: 1, Buckets: []domain.ProcurementBucket{}}
	group := &Group{ID: 42, Platform: PlatformOpenAI, Status: StatusActive, RateMultiplier: .15, ProfitControlEnabled: true, ProfitMinMargin: .2, ProcurementPolicy: &ProcurementPolicy{Version: 2, Revision: 3, Sources: &domain.ProcurementSources{AccountIDs: []int64{}}}}
	result := ClassifyProcurementScope(catalog, a, request, at, group)
	require.Equal(t, "not_configured", result.State)
	require.Equal(t, "blocked", result.Product.State)
	require.Equal(t, ProfitPreviewClassRejectedSource, result.Product.PriceClass)
	require.InDelta(t, .12, result.Product.Threshold, 1e-12)
	require.Equal(t, .15, result.Product.DefaultD)
	require.EqualValues(t, 3, result.Product.PolicyRevision)
	require.Equal(t, "group_default", result.Product.PriceBasis)
	require.Equal(t, "not_evaluated", result.Qualification)
	group.ProcurementPolicy.Sources.AccountIDs = []int64{901}
	group.ProfitControlEnabled = false
	result = ClassifyProcurementScope(catalog, a, request, at, group)
	require.Equal(t, "disabled", result.Product.State)
	group.ProfitControlEnabled = true
	group.Status = StatusDisabled
	result = ClassifyProcurementScope(catalog, a, request, at, group)
	require.Equal(t, "product_inactive", result.Product.State)
	group.Status = StatusActive
	a.GroupIDs = nil
	result = ClassifyProcurementScope(catalog, a, request, at, group)
	require.Equal(t, "not_associated", result.Product.State)
	result = ClassifyProcurementScope(catalog, nil, request, at, group)
	require.Equal(t, "not_evaluated", result.Product.State)
	require.Equal(t, "account_unavailable", result.Product.Reason)
	result = ClassifyProcurementScope(catalog, a, request, at, nil)
	require.Nil(t, result.Product, "old inventory-only commands retain their response")
}

func TestProcurementProductPriceDependenciesExpireBeforeControls(t *testing.T) {
	at := time.Date(2026, 11, 21, 1, 59, 58, 0, time.UTC)
	observed := at.Add(-time.Second)
	for _, name := range []string{"rate snapshot", "supplier applicability", "manual applicability", "peak boundary", "sales peak boundary"} {
		t.Run(name, func(t *testing.T) {
			a := billingApplicabilityAccount(t, observed, billingApplicabilityFixture(observed, billingApplicabilityEntry("responses_http", "unspecified", true)))
			a.ID, a.Status, a.Schedulable, a.GroupIDs = 7, StatusActive, true, []int64{42}
			snapshot := decodeUpstreamBillingProbeSnapshot(a.Extra)
			until := at.Add(2 * time.Second)
			switch name {
			case "rate snapshot":
				snapshot.FreshUntil = &until
			case "supplier applicability":
				declaration, present := upstreamBillingApplicabilityFromData(snapshot.Data)
				require.True(t, present)
				declaration.ExpiresAt = until
				snapshot.Data[upstreamBillingApplicabilityKey] = declaration
			case "manual applicability":
				delete(snapshot.Data, upstreamBillingApplicabilityKey)
				a.Extra[UpstreamBillingProbeExtraKey] = snapshot
				var command map[string]any
				require.NoError(t, json.Unmarshal(billingManualCommand(t, a, observed, billingApplicabilityEntry("responses_http", "unspecified", true)), &command))
				command["expires_at"] = until
				raw, err := json.Marshal(command)
				require.NoError(t, err)
				_, err = prepareUpstreamBillingManualApplicability(t.Context(), a, raw, observed)
				require.NoError(t, err)
			case "peak boundary":
				snapshot.Data["peak_rate_enabled"], snapshot.Data["peak_start"], snapshot.Data["peak_end"] = true, "02:00", "03:00"
				snapshot.Data["timezone"], snapshot.Data["peak_rate_multiplier"] = "UTC", 2.0
			}
			a.Extra[UpstreamBillingProbeExtraKey] = snapshot
			r := ProfitPreviewRequestScope{AccountID: a.ID, PublicModel: "gpt-fixture", Scope: AvailabilityRequestScope{Version: 2, Protocol: "responses_http", Model: "gpt-fixture", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}}
			catalog := &domain.ProcurementCatalog{Version: 1, Buckets: []domain.ProcurementBucket{}}
			group := &Group{ID: 42, Platform: PlatformOpenAI, Status: StatusActive, RateMultiplier: .6, ProfitControlEnabled: true}
			if name == "sales peak boundary" {
				group.SubscriptionType, group.PeakRateEnabled, group.PeakStart, group.PeakEnd, group.PeakRateMultiplier = "subscription", true, "02:00", "03:00", .5
			}
			before, err := json.Marshal(a.Extra)
			require.NoError(t, err)
			result := ClassifyProcurementScope(catalog, a, r, at, group)
			require.Equal(t, "constraints_passed", result.Product.State)
			require.Equal(t, "pass", result.Product.EvaluatedGates.State)
			require.True(t, until.Equal(*result.Product.EvaluatedGates.ValidUntil), "price evidence must expire before the account snapshot: got %s, want %s", result.Product.EvaluatedGates.ValidUntil, until)
			later := ClassifyProcurementScope(catalog, a, r, until, group)
			require.NotEqual(t, "pass", later.Product.EvaluatedGates.State, "expiry or a new peak price requires a fresh decision")
			after, err := json.Marshal(a.Extra)
			require.NoError(t, err)
			require.JSONEq(t, string(before), string(after), "a preview must not extend or rewrite supplier evidence")
		})
	}
}

func TestProcurementPriceDeadlinePrecedenceAndNoExtension(t *testing.T) {
	at := time.Date(2026, 11, 21, 1, 59, 58, 123456789, time.UTC)
	observed := at.Add(-time.Second)
	a := billingApplicabilityAccount(t, observed, billingApplicabilityFixture(observed, billingApplicabilityEntry("responses_http", "unspecified", true)))
	manualUntil := at.Add(time.Second)
	var command map[string]any
	require.NoError(t, json.Unmarshal(billingManualCommand(t, a, observed, billingApplicabilityEntry("responses_http", "unspecified", true)), &command))
	command["expires_at"] = manualUntil
	raw, err := json.Marshal(command)
	require.NoError(t, err)
	_, err = prepareUpstreamBillingManualApplicability(t.Context(), a, raw, observed)
	require.NoError(t, err)
	require.True(t, at.Add(ProcurementPreviewTTL).Equal(*procurementPriceDeadlineAt(a, nil, at)), "an unused manual fallback must not expire supplier evidence")
	snapshot := decodeUpstreamBillingProbeSnapshot(a.Extra)
	snapshot.Data[upstreamBillingApplicabilityKey] = nil
	a.Extra[UpstreamBillingProbeExtraKey] = snapshot
	scope := AvailabilityRequestScope{Version: 2, Protocol: "responses_http", Model: "gpt-fixture", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}
	status, _ := upstreamBillingScopeVerdictAt(a, &scope, at)
	require.NotEqual(t, "covered", status, "invalid supplier data never falls back to manual")
	delete(snapshot.Data, upstreamBillingApplicabilityKey)
	a.Extra[UpstreamBillingProbeExtraKey] = snapshot
	until := procurementPriceDeadlineAt(a, nil, at)
	require.True(t, manualUntil.Equal(*until))
	*until = at.Add(time.Hour)
	manual, state := manualBillingApplicabilityAt(a, at)
	require.Equal(t, "active", state)
	require.True(t, manualUntil.Equal(manual.ExpiresAt), "projection changes must not extend source evidence")
	// A local minute boundary is conservative, including fractional seconds
	// and non-hour timezones. It does not predict a price beyond that minute.
	snapshot.Data["peak_rate_enabled"], snapshot.Data["peak_start"], snapshot.Data["peak_end"] = true, "08:00", "09:00"
	snapshot.Data["timezone"], snapshot.Data["peak_rate_multiplier"] = "Asia/Kathmandu", 2.0
	snapshot.Data[upstreamBillingApplicabilityKey] = billingApplicabilityFixture(observed, billingApplicabilityEntry("responses_http", "unspecified", true))
	a.Extra[UpstreamBillingProbeExtraKey] = snapshot
	require.True(t, at.Truncate(time.Minute).Add(time.Minute).Equal(*procurementPriceDeadlineAt(a, nil, at)))
	require.True(t, at.Add(ProcurementPreviewTTL).Equal(*procurementPriceDeadlineAt(nil, nil, at)))
	require.Nil(t, procurementPriceDeadlineAt(a, nil, time.Time{}))
}
