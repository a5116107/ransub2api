package service

import (
	"context"
	"fmt"
	"time"
)

type AvailabilityModelState struct {
	ModelID string `json:"model_id"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	AvailabilityState
}

// AvailabilityReport is derived from one primary-database snapshot. It never
// contains credentials, route fingerprints or internal event counters.
type AvailabilityReport struct {
	PlanID      int64                    `json:"plan_id"`
	AccountID   int64                    `json:"account_id"`
	Configured  bool                     `json:"configured"`
	Enabled     bool                     `json:"enabled"`
	EvaluatedAt time.Time                `json:"evaluated_at"`
	Models      []AvailabilityModelState `json:"models"`
}

func (s *ScheduledTestService) ReadAvailability(ctx context.Context, planID int64) (*AvailabilityReport, error) {
	return s.planRepo.ReadAvailability(ctx, planID)
}

// AvailabilityPolicy opts a scheduled answer test into independent latest-state
// tracking. It never changes quality ownership or manual account settings.
type AvailabilityPolicy struct {
	ValidForSeconds int `json:"valid_for_seconds"`
}

func validateAvailabilityPolicy(cfg *PelicanTestConfig) error {
	if cfg.Availability == nil {
		return nil
	}
	if cfg.Availability.ValidForSeconds < 30 || cfg.Availability.ValidForSeconds > 86400 {
		return fmt.Errorf("availability validity must be 30–86400 seconds")
	}
	if isOpenAICodexStateProbePlan(cfg) || cfg.TestChannel == "bps" {
		return fmt.Errorf("availability requires an account answer test")
	}
	return nil
}

// AvailabilityModelOutcome is one complete model round, before quality grading.
type AvailabilityModelOutcome struct {
	ModelID string
	Probe   *AvailabilityProbeResult
}

// AvailabilityState keeps the latest observation and the last decisive result
// separately: an inconclusive observation cannot extend a previous pass.
type AvailabilityState struct {
	Observation *AvailabilityProbeResult `json:"observation"`
	Decision    *AvailabilityProbeResult `json:"decision,omitempty"`
	ValidUntil  *time.Time               `json:"valid_until,omitempty"`
}

func availabilityRoundOutcomes(plan *ScheduledTestPlan, results []*ScheduledTestResult) []AvailabilityModelOutcome {
	models := plan.PelicanConfig.ModelIDs
	if len(models) == 0 {
		models = []string{plan.ModelID}
	}
	outcomes := make([]AvailabilityModelOutcome, 0, len(models))
	for _, model := range models {
		probe := &AvailabilityProbeResult{Version: 1, Status: "inconclusive", Reason: "round_not_confirmed"}
		allPass, observed, samples := true, false, 0
		for _, result := range results {
			if result == nil || result.PelicanConfig == nil || result.PelicanConfig.ModelID != model {
				continue
			}
			samples++
			current := result.PelicanConfig.AvailabilityResult
			if current == nil || current.Version != 1 {
				allPass = false
				continue
			}
			observed = true
			if probe.StartedAt.IsZero() || current.StartedAt.Before(probe.StartedAt) {
				probe.StartedAt = current.StartedAt
			}
			if current.FinishedAt.After(probe.FinishedAt) {
				probe.FinishedAt = current.FinishedAt
			}
			probe.AttemptCount += current.AttemptCount
			if current.Status == "fail" {
				probe.Status, probe.Reason = "fail", current.Reason
				probe.HTTPStatus, probe.SemanticStatus = current.HTTPStatus, current.SemanticStatus
			}
			allPass = allPass && current.Status == "pass"
		}
		if observed && allPass && samples == plan.PelicanConfig.ParallelCount {
			probe.Status, probe.Reason = "pass", "completed_round"
		}
		outcomes = append(outcomes, AvailabilityModelOutcome{ModelID: model, Probe: probe})
	}
	return outcomes
}

func (s AvailabilityState) StatusAt(now time.Time) string {
	if s.Decision == nil || s.ValidUntil == nil {
		return "unknown"
	}
	if !now.Before(*s.ValidUntil) {
		return "stale"
	}
	return s.Decision.Status
}
