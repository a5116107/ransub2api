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
	// Extra answer samples per scheduled run, not a monetary price or a cap on
	// retries inside each sample. Zero preserves single-connection behavior.
	MaxExtraSamples int `json:"max_extra_samples,omitempty"`
}

// AvailabilityCoverageSource identifies extra local scheduled rounds. It is
// execution-only and never accepts a target connection from an API request.
const AvailabilityCoverageSource = "availability_coverage"
const MaxAvailabilityCoverageSamples = 100

// Reserve 20% of the validity window for request duration and scheduling delay.
// This is an estimate, not a guarantee that slow or failed probes finish in time.
func availabilityCoverageRounds(plan *ScheduledTestPlan, eligible int, now time.Time) int {
	if plan == nil || plan.PelicanConfig == nil || plan.PelicanConfig.Availability == nil || eligible <= 1 {
		return 1
	}
	policy := plan.PelicanConfig.Availability
	if plan.PelicanConfig.Quality != nil && plan.PelicanConfig.Quality.Action == QualityActionOpenAITwoFAAdmission {
		return 1 // Its native finish operation changes the next-run schedule.
	}
	models := len(plan.PelicanConfig.ModelIDs)
	if models == 0 {
		models = 1
	}
	if models > MaxAvailabilityCoverageSamples || plan.PelicanConfig.ParallelCount < 1 || plan.PelicanConfig.ParallelCount > 8 {
		return 1
	}
	samples := models * plan.PelicanConfig.ParallelCount
	if policy.ValidForSeconds <= 0 || policy.MaxExtraSamples <= 0 || samples <= 0 {
		return 1
	}
	// All samples share the existing per-plan history limit. Keep enough room
	// for the base round so extra probes cannot immediately prune its evidence.
	budget := min(policy.MaxExtraSamples, MaxAvailabilityCoverageSamples-samples, plan.MaxResults-samples)
	if budget <= 0 {
		return 1
	}
	var first time.Time
	var err error
	if plan.NextRunAt != nil && plan.NextRunAt.After(now) {
		first = *plan.NextRunAt
	} else {
		first, err = computeNextRun(plan.CronExpression, now)
	}
	if err != nil {
		return 1
	}
	second, err := computeNextRun(plan.CronExpression, first)
	if err != nil || !second.After(first) {
		return 1
	}
	required := float64(eligible) * second.Sub(first).Seconds() / (float64(policy.ValidForSeconds) * 0.8)
	rounds := 1
	for rounds < eligible && rounds < 8 && rounds-1 < budget/samples && float64(rounds) < required {
		rounds++
	}
	return rounds
}

// TriggersOnUpstream5xx keeps availability recovery independent of quality
// actions while sharing the existing coalesced account probe worker.
func (c *PelicanTestConfig) TriggersOnUpstream5xx() bool {
	return c != nil && (c.Availability != nil || (c.Quality != nil && c.Quality.TriggerOnUpstream5xx))
}

func validateAvailabilityPolicy(cfg *PelicanTestConfig) error {
	if cfg.Availability == nil {
		return nil
	}
	if cfg.Availability.ValidForSeconds < 30 || cfg.Availability.ValidForSeconds > 86400 {
		return fmt.Errorf("availability validity must be 30–86400 seconds")
	}
	if cfg.Availability.MaxExtraSamples < 0 || cfg.Availability.MaxExtraSamples > 64 {
		return fmt.Errorf("availability extra sample budget must be 0–64")
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
		var scope *AvailabilityRequestScope
		sameScope := true
		for _, result := range results {
			if result == nil || result.PelicanConfig == nil || result.PelicanConfig.ModelID != model {
				continue
			}
			samples++
			current := result.PelicanConfig.AvailabilityResult
			if current == nil || current.Version != 1 {
				allPass = false
				sameScope = false
				continue
			}
			observed = true
			if current.Scope == nil {
				sameScope = false
			} else if scope == nil {
				copy := *current.Scope
				scope = &copy
			} else if *scope != *current.Scope {
				sameScope = false
			}
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
		// A failure or inconclusive round can be attributed only when every
		// configured sample attests to the same actual request scope as well.
		if observed && sameScope && samples == plan.PelicanConfig.ParallelCount {
			probe.Scope = scope
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
