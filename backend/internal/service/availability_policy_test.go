//go:build unit

package service

import (
	"context"
	"sync/atomic"
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
	qualityCalls int
}

type availabilityTriggeredRepo struct {
	availabilityRunnerRepo
	plan *ScheduledTestPlan
}

func (r *availabilityRunnerRepo) ApplyQualityOutcome(context.Context, *ScheduledTestPlan, time.Time, string) (string, error) {
	r.qualityCalls++
	return "applied", nil
}

func (r *availabilityRunnerRepo) FinishTriggeredQuality(ctx context.Context, plan *ScheduledTestPlan, until, finished, next time.Time) error {
	return r.FinishPelican(ctx, plan.ID, until, finished)
}

func TestAvailabilityOnlyBusinessFaultDoesNotApplyQualityAction(t *testing.T) {
	plan := pelicanPlan()
	plan.TriggerSource = quality5xxSource
	plan.PelicanConfig.ModelID = plan.ModelID
	plan.PelicanConfig.QuestionKind = "candy"
	plan.PelicanConfig.Quality = &QualityPolicy{Action: QualityActionRemoveModel, TriggerOnUpstream5xx: false, ExpectedAnswer: "21", RemoveModels: []string{plan.ModelID}}
	plan.PelicanConfig.Availability = &AvailabilityPolicy{ValidForSeconds: 60}
	repo := &availabilityRunnerRepo{}
	results := &pelicanResults{}
	runner := &ScheduledTestRunnerService{planRepo: repo, scheduledSvc: NewScheduledTestService(repo, results), runPelican: func(_ context.Context, _ int64, model string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
		now := time.Now()
		return &ScheduledTestResult{Status: "failed", StartedAt: now, FinishedAt: now, PelicanConfig: &PelicanTestConfig{ModelID: model, AvailabilityResult: &AvailabilityProbeResult{Version: 1, Status: "pass", StartedAt: now, FinishedAt: now}}}, nil
	}}
	require.True(t, runner.runPelicanPlan(t.Context(), plan))
	require.Zero(t, repo.qualityCalls, "availability-only fault must not trigger the disabled quality action")
	require.Len(t, repo.observations, 1)
	require.Len(t, results.results, 2)
	for _, result := range results.results {
		require.Equal(t, "inconclusive", result.QualityAction)
	}
}

func (r *availabilityTriggeredRepo) ListByAccountID(context.Context, int64) ([]*ScheduledTestPlan, error) {
	return []*ScheduledTestPlan{r.plan}, nil
}

func TestAvailabilityBusinessFaultRunsOriginalAnswerRound(t *testing.T) {
	account := stateProbeAccount()
	plan := pelicanPlan()
	plan.AccountID = account.ID
	plan.PelicanConfig.Availability = &AvailabilityPolicy{ValidForSeconds: 60}
	// A preceding in-flight round can finish after the fault; its finish time
	// alone is not evidence that the new independent version was observed.
	observed := time.Now().Add(-time.Second)
	finished := time.Now()
	plan.LastRunAt = &finished
	future := time.Now().Add(time.Hour)
	plan.NextRunAt = &future
	plan.PelicanConfig.ModelID = plan.ModelID
	originalConfig := plan.PelicanConfig
	original := *plan.PelicanConfig
	repo := &availabilityTriggeredRepo{plan: plan}
	results := &pelicanResults{}
	var calls atomic.Int32
	runner := &ScheduledTestRunnerService{
		planRepo: repo, scheduledSvc: NewScheduledTestService(repo, results),
		accountTestSvc: &AccountTestService{accountRepo: &stateProbeAccountRepo{account: account}},
	}
	runner.runPelican = func(_ context.Context, _ int64, model string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
		calls.Add(1)
		require.Equal(t, original.QuestionKind, cfg.QuestionKind)
		require.Equal(t, original.Prompt, cfg.Prompt)
		require.Equal(t, original.ReasoningEffort, cfg.ReasoningEffort)
		require.Equal(t, original.ParallelCount, cfg.ParallelCount)
		now := time.Now()
		return &ScheduledTestResult{Status: "success", ResponseText: "answer", StartedAt: now, FinishedAt: now, PelicanConfig: &PelicanTestConfig{ModelID: model, AvailabilityResult: &AvailabilityProbeResult{Version: 1, Status: "pass", StartedAt: now, FinishedAt: now}}}, nil
	}
	require.NoError(t, runner.runQualityTriggeredAccount(t.Context(), account.ID, observed))
	require.EqualValues(t, original.ParallelCount, calls.Load())
	require.Len(t, repo.observations, 1)
	require.Equal(t, "pass", repo.observations[0].Probe.Status)
	require.Len(t, results.results, original.ParallelCount)
	require.True(t, repo.finished)
	require.Equal(t, original, *originalConfig)
}

func TestAvailabilityFaultConfigPreservesQualityQuestion(t *testing.T) {
	plan := pelicanPlan()
	plan.PelicanConfig.Availability = &AvailabilityPolicy{ValidForSeconds: 60}
	plan.PelicanConfig.Quality = &QualityPolicy{Action: QualityActionObserveOnly, ExpectedAnswer: "answer"}
	cfg, apply := quality5xxTestConfig(stateProbeAccount(), plan.ModelID, *plan.PelicanConfig)
	require.False(t, apply, "availability-only recovery must not apply a disabled quality action")
	require.Equal(t, *plan.PelicanConfig, cfg)
	require.True(t, cfg.TriggersOnUpstream5xx(), "availability is independent of the quality trigger switch")
	var absent *PelicanTestConfig
	require.False(t, absent.TriggersOnUpstream5xx())
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
