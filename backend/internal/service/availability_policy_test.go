//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAvailabilityPolicyValidation(t *testing.T) {
	for _, seconds := range []int{0, 29, 30, 300, 86400, 86401} {
		plan := pelicanPlan()
		plan.PelicanConfig.Availability = &AvailabilityPolicy{ValidForSeconds: seconds}
		_, err := nextPlanRun(plan, time.Now())
		if seconds < 30 || seconds > 86400 {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	}
	plan := pelicanPlan()
	plan.AutoRecover = true
	plan.PelicanConfig.Availability = &AvailabilityPolicy{ValidForSeconds: 60}
	_, err := nextPlanRun(plan, time.Now())
	require.EqualError(t, err, "availability tracking cannot use connectivity auto_recover")
	cfg := stateProbePlanConfig()
	cfg.Availability = &AvailabilityPolicy{ValidForSeconds: 60}
	require.Error(t, validateAvailabilityPolicy(cfg))
	cfg = &PelicanTestConfig{TestChannel: "bps", Availability: &AvailabilityPolicy{ValidForSeconds: 60}}
	require.Error(t, validateAvailabilityPolicy(cfg))
}

func TestAvailabilityRoundSeparatesModelsAndRequiresAllSamples(t *testing.T) {
	plan := pelicanPlan()
	plan.ModelID = "a"
	plan.PelicanConfig.ModelIDs = []string{"a", "b"}
	for _, tc := range []struct {
		name, second, expected string
	}{
		{"all_pass", "pass", "pass"},
		{"mixed_failure", "fail", "fail"},
		{"uncertain", "inconclusive", "inconclusive"},
		{"missing", "", "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			sample := func(model, status string) *ScheduledTestResult {
				return &ScheduledTestResult{PelicanConfig: &PelicanTestConfig{ModelID: model, AvailabilityResult: &AvailabilityProbeResult{Version: 1, Status: status, StartedAt: now, FinishedAt: now.Add(time.Second), AttemptCount: 1}}}
			}
			results := []*ScheduledTestResult{sample("a", "pass"), sample("b", "pass"), sample("b", "pass")}
			if tc.second != "" {
				results = append(results, sample("a", tc.second))
			}
			outcomes := availabilityRoundOutcomes(plan, results)
			require.Len(t, outcomes, 2)
			require.Equal(t, tc.expected, outcomes[0].Probe.Status)
			require.Equal(t, "pass", outcomes[1].Probe.Status)
		})
	}
}

type availabilityRunnerRepo struct {
	pelicanPlanRepo
	observations []AvailabilityModelOutcome
}

func (r *availabilityRunnerRepo) ApplyAvailabilityOutcomes(_ context.Context, _ *ScheduledTestPlan, _ time.Time, observations []AvailabilityModelOutcome) (string, error) {
	r.observations = observations
	return "observed", nil
}

func TestAvailabilityRunnerWritesIndependentEvidence(t *testing.T) {
	plan := pelicanPlan()
	plan.PelicanConfig.Availability = &AvailabilityPolicy{ValidForSeconds: 60}
	repo := &availabilityRunnerRepo{}
	results := &pelicanResults{}
	runner := &ScheduledTestRunnerService{planRepo: repo, scheduledSvc: &ScheduledTestService{resultRepo: results}, runPelican: func(_ context.Context, _ int64, model string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
		now := time.Now()
		return &ScheduledTestResult{Status: "failed", StartedAt: now, FinishedAt: now, PelicanConfig: &PelicanTestConfig{ModelID: model, AvailabilityResult: &AvailabilityProbeResult{Version: 1, Status: "pass", StartedAt: now, FinishedAt: now}}}, nil
	}}
	require.True(t, runner.runPelicanPlan(t.Context(), plan), "the claimed round must finish even when answers fail")
	require.Len(t, repo.observations, 1)
	require.Equal(t, "pass", repo.observations[0].Probe.Status)
	require.Len(t, results.results, 2)
	for _, result := range results.results {
		require.Equal(t, "failed", result.Status, "availability must not replace the answer result")
	}
	require.True(t, repo.finished)
}

func TestAvailabilityStateExpiryBoundary(t *testing.T) {
	now := time.Now()
	state := AvailabilityState{}
	require.Equal(t, "unknown", state.StatusAt(now))
	until := now.Add(time.Minute)
	state.Decision, state.ValidUntil = &AvailabilityProbeResult{Status: "pass"}, &until
	require.Equal(t, "pass", state.StatusAt(now))
	require.Equal(t, "stale", state.StatusAt(until))
	state.Decision.Status = "fail"
	require.Equal(t, "fail", state.StatusAt(now))
}
