package service

import "time"

// This projection consumes the original latest-state authority, not historical
// success rates or quality scores. It grants no model/cache certification.
type AvailabilityScopeEvidence struct {
	Version        int        `json:"version"`
	Status         string     `json:"status"`
	Reason         string     `json:"reason"`
	EvaluatedAt    time.Time  `json:"evaluated_at"`
	ValidUntil     *time.Time `json:"valid_until"`
	RuleCount      int        `json:"rule_count"`
	ConfirmedRules int        `json:"confirmed_rules"`
}

func AssessAvailabilityScopeEvidence(account *Account, scope *AvailabilityRequestScope, reports []*AvailabilityReport, at time.Time) *AvailabilityScopeEvidence {
	out := &AvailabilityScopeEvidence{Version: 1, Status: "unknown", Reason: "availability_not_confirmed", EvaluatedAt: at}
	if account == nil || account.ID <= 0 {
		out.Reason = "account_unavailable"
		return out
	}
	configured := false
	for _, report := range reports {
		if report == nil || report.AccountID != account.ID {
			out.Reason = "availability_state_unavailable"
			return out
		}
		configured = configured || report.Configured
	}
	if !configured {
		out.Status, out.Reason = "not_configured", "availability_not_configured"
		return out
	}
	if scope == nil || at.IsZero() {
		out.Reason = "availability_scope_unconfirmed"
		return out
	}
	out.Status, out.Reason = "pass", "all_rules_passed"
	reject := func(status, reason string) {
		if out.Status == "pass" || status == "fail" && out.Status != "fail" {
			out.Status, out.Reason = status, reason
		}
	}
	for _, report := range reports {
		if !report.Configured {
			continue
		}
		if report.EvaluatedAt.IsZero() || report.EvaluatedAt.After(at) || len(report.Models) == 0 {
			out.RuleCount++
			reject("unknown", "availability_not_confirmed")
			continue
		}
		matched, passed := false, true
		for _, state := range report.Models {
			if AvailabilityModelForAccount(account, state.ModelID) != scope.Model {
				continue
			}
			matched = true
			if !report.Enabled {
				passed = false
				reject("unknown", "availability_rule_paused")
				continue
			}
			if state.Decision == nil || state.Decision.Version != 1 || state.Decision.StartedAt.IsZero() || state.Decision.FinishedAt.Before(state.Decision.StartedAt) || state.Decision.FinishedAt.After(at) {
				passed = false
				reject("unknown", "availability_not_confirmed")
				continue
			}
			if state.ValidUntil != nil && (out.ValidUntil == nil || state.ValidUntil.Before(*out.ValidUntil)) {
				until := *state.ValidUntil
				out.ValidUntil = &until
			}
			if state.Status != "pass" || state.StatusAt(at) != "pass" {
				passed = false
				status := "unknown"
				if state.Status == "fail" && state.StatusAt(at) == "fail" && state.Decision != nil && state.Decision.Scope != nil && *state.Decision.Scope == *scope {
					status = "fail"
				}
				reject(status, "availability_not_confirmed")
				continue
			}
			if state.Decision == nil || state.Decision.Scope == nil || *state.Decision.Scope != *scope {
				passed = false
				reject("unknown", "availability_scope_unconfirmed")
			}
		}
		if matched {
			out.RuleCount++
			if passed {
				out.ConfirmedRules++
			}
		}
	}
	if out.RuleCount == 0 {
		out.Status, out.Reason = "unknown", "availability_model_unobserved"
	}
	if out.Status == "fail" && (out.ValidUntil == nil || !out.ValidUntil.After(at)) {
		out.Status, out.Reason = "unknown", "availability_not_confirmed"
	}
	return out
}
