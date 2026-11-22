package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func qualityScopeFixture() (*Account, AvailabilityRequestScope, QualityEvidenceRule, time.Time) {
	plan, sample := qualityMeasurementFixture()
	plan.ID, plan.AccountID, plan.ModelID, plan.Enabled = 12, 7, "public-model", true
	plan.PelicanConfig.Quality.EvidenceValidForSeconds = 300
	scope := AvailabilityRequestScope{Version: 2, Protocol: "responses_http", Model: "wire-model", Stream: true, ServiceTier: "priority", ReasoningEffort: "high", Capability: "text"}
	plan.PelicanConfig.AvailabilityResult.Scope = &scope
	sample.ID, sample.PlanID = 21, plan.ID
	recordQualityMeasurement(plan, sample)
	at := sample.FinishedAt.Add(time.Second)
	sample.QualityMeasurementValidity = AssessQualityMeasurementValidity(sample, plan.UpdatedAt, true, at)
	sample.QualityMeasurementValidity.Round = &QualityMeasurementRoundValidity{Status: "latest_complete", Reason: "latest_complete"}
	sample.QualityMeasurementValidity.Route = &QualityMeasurementRouteValidity{Status: "matching", Reason: "route_matches"}
	sample.QualityMeasurementValidity.Fault = &QualityMeasurementFaultValidity{Status: "unchanged", Reason: "no_new_registered_fault"}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"public-model": "wire-model"}}}
	return account, scope, QualityEvidenceRule{Plan: plan, Samples: []*ScheduledTestResult{sample}}, at
}

func TestQualityReportedModelDoesNotAuthorizeOrReplaceAnswerEvidence(t *testing.T) {
	for _, declaration := range []string{"match", "mismatch", "unknown"} {
		for _, answer := range []string{"pass", "fail", "unknown"} {
			t.Run(declaration+"/"+answer, func(t *testing.T) {
				account, scope, rule, at := qualityScopeFixture()
				sample := rule.Samples[0]
				measurement := sample.PelicanConfig.QualityMeasurement
				measurement.Dimensions["reported_model"] = QualityDimensionMeasurement{Status: "unknown", Reason: "model_declaration_not_observed"}
				if declaration == "match" {
					measurement.Dimensions["reported_model"] = measuredQualityDimension("pass", "upstream_model_matches_request", 1)
				} else if declaration == "mismatch" {
					measurement.Dimensions["reported_model"] = measuredQualityDimension("fail", "upstream_model_differs_from_request", 0)
				}
				if answer == "fail" {
					measurement.Dimensions["answer_correctness"] = measuredQualityDimension("fail", "graded_incorrect", 0)
				} else if answer == "unknown" {
					measurement.Dimensions["answer_correctness"] = QualityDimensionMeasurement{Status: "unknown", Reason: "grading_inconclusive"}
				}
				out := AssessQualityScopeEvidence(account, scope, []QualityEvidenceRule{rule}, at, false)
				require.Equal(t, answer, out.Status, "declarations are diagnostic; the existing answer evidence contract remains authoritative")
				require.Equal(t, "unknown", out.ModelIdentity)
			})
		}
	}
}

func TestQualityScopeEvidenceConservativeIntersection(t *testing.T) {
	for _, scenario := range []string{"pass", "incorrect", "expired", "paused", "rule-changed", "running", "partial-round", "route-changed", "new-fault", "wrong-scope", "unknown-sample", "other-model", "invalid-plan", "wrong-plan", "duplicate-rule", "duplicate-sample", "future-read", "limited", "absent-account", "not-configured"} {
		t.Run(scenario, func(t *testing.T) {
			account, scope, rule, at := qualityScopeFixture()
			rules := []QualityEvidenceRule{rule}
			sample := rule.Samples[0]
			want := "unknown"
			switch scenario {
			case "pass":
				want = "pass"
			case "incorrect":
				sample.PelicanConfig.QualityMeasurement.Dimensions["answer_correctness"] = measuredQualityDimension("fail", "graded_incorrect", 0)
				want = "fail"
			case "expired":
				at = *sample.PelicanConfig.QualityMeasurement.ValidUntil
			case "paused":
				rule.Plan.Enabled = false
			case "rule-changed":
				rule.Plan.UpdatedAt = rule.Plan.UpdatedAt.Add(time.Second)
			case "running", "partial-round":
				sample.QualityMeasurementValidity.Round.Status = scenario
			case "route-changed":
				sample.QualityMeasurementValidity.Route.Status = "changed"
			case "new-fault":
				sample.QualityMeasurementValidity.Fault.Status = "invalidated"
			case "wrong-scope":
				scope.ReasoningEffort = "low"
			case "unknown-sample":
				rules[0].Samples = append(rules[0].Samples, nil)
			case "other-model":
				scope.Model = "another-model"
			case "invalid-plan":
				rule.Plan.AccountID++
			case "wrong-plan":
				sample.PlanID++
			case "duplicate-rule":
				rules = append(rules, rule)
			case "duplicate-sample":
				rules[0].Samples = append(rules[0].Samples, sample)
			case "future-read":
				sample.QualityMeasurementValidity.EvaluatedAt = at.Add(time.Second)
			case "absent-account":
				account = nil
			case "not-configured":
				rules, want = nil, "not_configured"
			}
			out := AssessQualityScopeEvidence(account, scope, rules, at, scenario == "limited")
			require.Equal(t, want, out.Status)
			require.Equal(t, "unknown", out.ModelIdentity)
			require.Equal(t, at, out.EvaluatedAt)
			if scenario == "pass" {
				require.Equal(t, 1, out.ConfirmedRules)
				require.Equal(t, 1, out.ValidSamples)
				require.Equal(t, sample.PelicanConfig.QualityMeasurement.ValidUntil, out.ValidUntil)
			}
		})
	}
}

func TestQualityScopeEvidenceMultipleRulesAndOptions(t *testing.T) {
	for _, scenario := range []string{"all-pass", "one-fails", "one-unknown", "one-paused", "other-scope", "other-model", "state-probe", "min-expiry", "stream", "protocol", "tier", "effort", "summary", "verbosity", "encrypted"} {
		t.Run(scenario, func(t *testing.T) {
			account, scope, first, at := qualityScopeFixture()
			_, _, second, _ := qualityScopeFixture()
			second.Plan.ID++
			second.Samples[0].PlanID = second.Plan.ID
			second.Samples[0].QualityMeasurementValidity.EvaluatedAt = at
			want := "unknown"
			switch scenario {
			case "all-pass", "min-expiry":
				want = "pass"
			case "one-fails":
				second.Samples[0].PelicanConfig.QualityMeasurement.Dimensions["answer_correctness"] = measuredQualityDimension("fail", "graded_incorrect", 0)
				want = "fail"
			case "one-unknown":
				second.Samples[0].QualityMeasurementValidity.Fault.Status = "unknown"
			case "one-paused":
				second.Plan.Enabled = false
			case "other-scope":
				second.Samples[0].PelicanConfig.QualityMeasurement.Scope.ServiceTier = "default"
				want = "pass"
			case "other-model":
				second.Plan.ModelID = "another-model"
				want = "pass"
			case "state-probe":
				second.Plan.PelicanConfig.QuestionKind = OpenAICodexStateProbeQuestionKind
				want = "pass"
			case "stream":
				scope.Stream = false
			case "protocol":
				scope.Protocol = "responses_ws_v2"
			case "tier":
				scope.ServiceTier = "default"
			case "effort":
				scope.ReasoningEffort = "low"
			case "summary":
				scope.Version, scope.ReasoningSummary = 3, "auto"
			case "verbosity":
				scope.Version, scope.TextVerbosity = 4, "high"
			case "encrypted":
				scope.Version, scope.IncludeEncryptedContent = 3, true
			}
			out := AssessQualityScopeEvidence(account, scope, []QualityEvidenceRule{first, second}, at, false)
			require.Equal(t, want, out.Status)
			if scenario == "min-expiry" {
				require.Equal(t, first.Samples[0].PelicanConfig.QualityMeasurement.ValidUntil, out.ValidUntil)
			}
			require.Equal(t, "unknown", out.ModelIdentity)
		})
	}
}

func TestQualityScopeEvidenceAllRulesAndScopes(t *testing.T) {
	account, scope, first, at := qualityScopeFixture()
	_, _, second, _ := qualityScopeFixture()
	second.Plan.ID++
	second.Samples[0].ID++
	second.Samples[0].PlanID = second.Plan.ID
	second.Samples[0].QualityMeasurementValidity.EvaluatedAt = at
	for _, status := range []string{"pass", "fail", "unknown"} {
		t.Run(status, func(t *testing.T) {
			second.Samples[0].PelicanConfig.QualityMeasurement.Dimensions["answer_correctness"] = measuredQualityDimension(status, map[string]string{"pass": "graded_correct", "fail": "graded_incorrect", "unknown": "grading_not_observed"}[status], map[string]float64{"pass": 1}[status])
			out := AssessQualityScopeEvidence(account, scope, []QualityEvidenceRule{first, second}, at, false)
			require.Equal(t, status, out.Status, "one passing rule must not conceal another configured rule")
			require.Equal(t, 2, out.RuleCount)
		})
	}
	second.Samples[0].PelicanConfig.QualityMeasurement.Scope.ReasoningEffort = "low"
	out := AssessQualityScopeEvidence(account, scope, []QualityEvidenceRule{first, second}, at, false)
	require.Equal(t, "pass", out.Status, "a confirmed different actual scope must not borrow or veto this scope's answer")
	require.Equal(t, 1, out.OtherScopeRules)
	second.Samples[0].QualityMeasurementValidity.Route.Status = "changed"
	out = AssessQualityScopeEvidence(account, scope, []QualityEvidenceRule{first, second}, at, false)
	require.Equal(t, "unknown", out.Status, "changed routes cannot certify a different scope")
}

func TestQualityScopeEvidenceMultipleModelsKeepExactDenominator(t *testing.T) {
	account, scope, rule, at := qualityScopeFixture()
	rule.Plan.PelicanConfig.ModelIDs = []string{"public-model", "other-model"}
	first := rule.Samples[0]
	recordQualityMeasurement(rule.Plan, first)
	second := *first
	second.ID++
	cfg, probe := *first.PelicanConfig, *first.PelicanConfig.AvailabilityResult
	other := scope
	other.Model = "other-model"
	probe.Scope, cfg.ModelID, cfg.AvailabilityResult = &other, "other-model", &probe
	second.PelicanConfig = &cfg
	recordQualityMeasurement(rule.Plan, &second)
	rule.Samples = append(rule.Samples, &second)
	for _, sample := range rule.Samples {
		sample.QualityMeasurementValidity = AssessQualityMeasurementValidity(sample, rule.Plan.UpdatedAt, true, at)
		sample.QualityMeasurementValidity.Round = &QualityMeasurementRoundValidity{Status: "latest_complete"}
		sample.QualityMeasurementValidity.Route = &QualityMeasurementRouteValidity{Status: "matching"}
		sample.QualityMeasurementValidity.Fault = &QualityMeasurementFaultValidity{Status: "unchanged"}
	}
	out := AssessQualityScopeEvidence(account, scope, []QualityEvidenceRule{rule}, at, false)
	require.Equal(t, "pass", out.Status)
	require.Equal(t, 1, out.Samples, "a different configured model must not pollute the requested model's denominator")
	require.Equal(t, 1, out.ValidSamples)
}
