package service

import "time"

// QualityEvidenceRule is request-local input from one authoritative read. It
// contains the latest registered round, not a caller-supplied passing verdict.
type QualityEvidenceRule struct {
	Plan    *ScheduledTestPlan
	Samples []*ScheduledTestResult
}

type QualityScopeRuleEvidence struct {
	PlanID       int64      `json:"plan_id"`
	Status       string     `json:"status"`
	Reason       string     `json:"reason"`
	Samples      int        `json:"samples"`
	ValidSamples int        `json:"valid_samples"`
	ValidUntil   *time.Time `json:"valid_until"`
}

// Answer evidence never certifies model identity, cache capability or product
// admission. Its scope is the actual wire scope, independent of price buckets.
type QualityScopeEvidence struct {
	Version         int                        `json:"version"`
	Status          string                     `json:"status"`
	Reason          string                     `json:"reason"`
	ModelIdentity   string                     `json:"model_identity"`
	EvaluatedAt     time.Time                  `json:"evaluated_at"`
	ValidUntil      *time.Time                 `json:"valid_until"`
	RuleCount       int                        `json:"rule_count"`
	ConfirmedRules  int                        `json:"confirmed_rules"`
	Samples         int                        `json:"samples"`
	ValidSamples    int                        `json:"valid_samples"`
	OtherScopeRules int                        `json:"other_scope_rules"`
	Rules           []QualityScopeRuleEvidence `json:"rules"`
}

func AssessQualityScopeEvidence(account *Account, scope AvailabilityRequestScope, rules []QualityEvidenceRule, at time.Time, limited bool) *QualityScopeEvidence {
	out := &QualityScopeEvidence{Version: 1, Status: "unknown", Reason: "evidence_not_observed", ModelIdentity: "unknown", EvaluatedAt: at, Rules: []QualityScopeRuleEvidence{}}
	if account == nil {
		out.Reason = "account_unavailable"
		return out
	}
	if limited {
		out.Reason = "coverage_limit"
		return out
	}
	if at.IsZero() || !billingApplicabilityScopeShapeKnown(scope) {
		out.Reason = "scope_unconfirmed"
		return out
	}
	if len(rules) == 0 {
		out.Status, out.Reason = "not_configured", "quality_not_configured"
		return out
	}
	seenRules := make(map[int64]bool, len(rules))
	for _, rule := range rules {
		p := rule.Plan
		if p == nil || p.ID <= 0 || seenRules[p.ID] || p.AccountID != account.ID || p.PelicanConfig == nil || p.PelicanConfig.Quality == nil {
			out.Reason = "invalid_rule"
			return out
		}
		seenRules[p.ID] = true
		seenSamples := make(map[int64]bool, len(rule.Samples))
		for _, sample := range rule.Samples {
			if sample != nil {
				if sample.ID <= 0 || sample.PlanID != p.ID || seenSamples[sample.ID] {
					out.Reason = "invalid_sample"
					return out
				}
				seenSamples[sample.ID] = true
			}
		}
	}
	for _, rule := range rules {
		p := rule.Plan
		if !isQualityAnswerQuestion(p.PelicanConfig.QuestionKind) {
			continue
		}
		target := false
		for _, model := range qualityPlanModels(p) {
			target = target || AvailabilityModelForAccount(account, model) == scope.Model
		}
		if !target {
			continue
		}
		r := QualityScopeRuleEvidence{PlanID: p.ID, Status: "unknown", Reason: "evidence_not_observed"}
		otherModel := func(sample *ScheduledTestResult) bool {
			if sample == nil || sample.PelicanConfig == nil || sample.PelicanConfig.QualityMeasurement == nil {
				return false
			}
			cfg, m := sample.PelicanConfig, sample.PelicanConfig.QualityMeasurement
			// Exclude only a genuinely different configured model. A target
			// model sent as another wire model is missing evidence, not an
			// unrelated sample that can be dropped from the denominator.
			return m.Scope != nil && m.Scope.Model != scope.Model && m.RequestedModel == cfg.ModelID &&
				billingDeclarationToken(cfg.ModelID, 200) && AvailabilityModelForAccount(account, cfg.ModelID) != scope.Model
		}
		otherScope, relevant := true, 0
		for _, sample := range rule.Samples {
			if sample == nil || sample.PelicanConfig == nil {
				otherScope = false
				continue
			}
			m := sample.PelicanConfig.QualityMeasurement
			if otherModel(sample) {
				continue
			}
			relevant++
			v := sample.QualityMeasurementValidity
			if m == nil || m.Scope == nil || m.Scope.Model != scope.Model || v == nil || v.Round == nil || v.Round.Status != "latest_complete" || v.Route == nil || v.Route.Status != "matching" || !m.RuleVersion.Equal(p.UpdatedAt) || !billingApplicabilityScopeShapeKnown(*m.Scope) || *m.Scope == scope {
				otherScope = false
			}
		}
		if relevant > 0 && otherScope {
			out.OtherScopeRules++
			continue
		}
		for _, sample := range rule.Samples {
			if otherModel(sample) {
				continue
			}
			r.Samples++
			status, reason, until := qualityScopeSampleVerdict(sample, p, scope, at)
			if status == "pass" || status == "fail" {
				r.ValidSamples++
				if r.ValidUntil == nil || until.Before(*r.ValidUntil) {
					r.ValidUntil = until
				}
			}
			if status == "fail" || r.Status != "fail" && status != "pass" {
				r.Status, r.Reason = status, reason
			}
		}
		if !p.Enabled {
			r.Status, r.Reason = "unknown", "rule_paused"
		} else if r.Samples > 0 && r.ValidSamples == r.Samples && r.Status != "fail" {
			r.Status, r.Reason = "pass", "all_samples_passed"
		}
		out.Rules = append(out.Rules, r)
		out.RuleCount++
		out.Samples += r.Samples
		out.ValidSamples += r.ValidSamples
		if r.Status == "pass" {
			out.ConfirmedRules++
		}
		if r.ValidUntil != nil && (out.ValidUntil == nil || r.ValidUntil.Before(*out.ValidUntil)) {
			out.ValidUntil = r.ValidUntil
		}
		if r.Status == "fail" {
			out.Status, out.Reason = "fail", "answer_incorrect"
		}
	}
	if out.Status != "fail" {
		if out.RuleCount > 0 && out.ConfirmedRules == out.RuleCount {
			out.Status, out.Reason = "pass", "all_rules_passed"
		} else if out.RuleCount > 0 {
			out.Reason = "rule_unconfirmed"
		} else {
			out.Reason = "scope_unobserved"
		}
	}
	return out
}

func qualityScopeSampleVerdict(sample *ScheduledTestResult, plan *ScheduledTestPlan, scope AvailabilityRequestScope, at time.Time) (string, string, *time.Time) {
	unknown := func(reason string) (string, string, *time.Time) { return "unknown", reason, nil }
	if sample == nil || sample.PelicanConfig == nil || sample.PelicanConfig.QualityMeasurement == nil || sample.QualityMeasurementValidity == nil {
		return unknown("evidence_not_observed")
	}
	m, v := sample.PelicanConfig.QualityMeasurement, sample.QualityMeasurementValidity
	// Reuse the original rule/window validator. A request-local read projection
	// must not make an edited rule or a sample from another read current.
	current := AssessQualityMeasurementValidity(sample, plan.UpdatedAt, plan.Enabled, at)
	if current.Status != "within_window" {
		return unknown(current.Reason)
	}
	if v.Status != "within_window" || !v.EvaluatedAt.Equal(at) {
		return unknown("read_not_current")
	}
	if v.Round == nil || v.Round.Status != "latest_complete" {
		if v.Round != nil {
			return unknown(v.Round.Reason)
		}
		return unknown("round_not_tracked")
	}
	if v.Route == nil || v.Route.Status != "matching" {
		if v.Route != nil {
			return unknown(v.Route.Reason)
		}
		return unknown("route_not_observed")
	}
	if v.Fault == nil || v.Fault.Status != "unchanged" {
		if v.Fault != nil {
			return unknown(v.Fault.Reason)
		}
		return unknown("fault_not_tracked")
	}
	if m.Scope == nil || *m.Scope != scope {
		return unknown("scope_unconfirmed")
	}
	transport := m.Dimensions["transport"]
	if !qualityMeasuredAnswer(m) || transport.Status != "pass" || transport.Reason != "completed_response" || transport.SampleCount != 1 || transport.Score == nil || *transport.Score != 1 {
		return unknown("answer_not_observed")
	}
	if m.Dimensions["answer_correctness"].Status == "fail" {
		return "fail", "answer_incorrect", m.ValidUntil
	}
	return "pass", "graded_correct", m.ValidUntil
}
