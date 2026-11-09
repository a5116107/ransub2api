//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type availabilityCoverageRepo struct {
	ScheduledTestPlanRepository
	plan         *ScheduledTestPlan
	ids          []string
	selected     []string
	sources      []string
	finished     int
	qualityCalls int
	action       string
	changeRule   bool
	changeNext   bool
	changeFault  bool
	faultVersion int64
}

func (r *availabilityCoverageRepo) GetByID(context.Context, int64) (*ScheduledTestPlan, error) {
	copy := *r.plan
	if r.changeRule {
		copy.UpdatedAt = copy.UpdatedAt.Add(time.Second)
	}
	if r.changeNext && copy.NextRunAt != nil {
		changed := copy.NextRunAt.Add(-time.Second)
		copy.NextRunAt = &changed
	}
	return &copy, nil
}

func (r *availabilityCoverageRepo) ClaimPelican(_ context.Context, p *ScheduledTestPlan, _, _, next time.Time) (bool, error) {
	if p.TriggerSource == AvailabilityCoverageSource && !next.Equal(*p.NextRunAt) {
		return false, errors.New("coverage changed the existing cron schedule")
	}
	if p.TriggerSource == AvailabilityCoverageSource && p.AvailabilityExpectedFault != r.faultVersion {
		return false, nil
	}
	if p.TriggerSource == "" {
		p.NextRunAt = &next
	}
	for _, id := range r.ids {
		excluded := false
		for _, seen := range p.AvailabilityExcludedConnections {
			excluded = excluded || seen == id
		}
		if !excluded {
			p.AvailabilityProbeConnectionID = id
			p.AvailabilityEligibleConnections = len(r.ids)
			p.AvailabilityFaultVersion = r.faultVersion
			r.selected = append(r.selected, id)
			r.sources = append(r.sources, p.TriggerSource)
			return true, nil
		}
	}
	return false, nil
}

func (r *availabilityCoverageRepo) ApplyAvailabilityOutcomes(_ context.Context, _ *ScheduledTestPlan, _ time.Time, _ []AvailabilityModelOutcome) (string, error) {
	if r.action != "" {
		return r.action, nil
	}
	return "observed", nil
}

func (r *availabilityCoverageRepo) ApplyQualityOutcome(context.Context, *ScheduledTestPlan, time.Time, string) (string, error) {
	r.qualityCalls++
	return "applied", nil
}

func (r *availabilityCoverageRepo) FinishPelican(context.Context, int64, time.Time, time.Time) error {
	r.finished++
	if r.changeFault {
		r.faultVersion++
	}
	return nil
}

func (r *availabilityCoverageRepo) FinishAvailabilityCoverage(context.Context, *ScheduledTestPlan, time.Time) error {
	r.finished++
	if r.changeFault {
		r.faultVersion++
	}
	return nil
}

func TestAvailabilityCoverageRunnerUsesIndependentBoundedRounds(t *testing.T) {
	for _, tc := range []struct {
		name            string
		budget          int
		ids             []string
		action          string
		changeRule      bool
		changeNext      bool
		changeFault     bool
		missingScope    bool
		rateLimited     bool
		wantConnections []string
	}{
		{"default", 0, []string{"a", "b", "c"}, "", false, false, false, false, false, []string{"a"}},
		{"full_batch", 2, []string{"a", "b", "c"}, "", false, false, false, false, false, []string{"a", "b", "c"}},
		{"bounded_budget", 1, []string{"a", "b", "c"}, "", false, false, false, false, false, []string{"a", "b"}},
		{"no_duplicate", 8, []string{"a", "b"}, "", false, false, false, false, false, []string{"a", "b"}},
		{"unattributed_failure", 8, []string{"a", "b", "c"}, "unattributed_failure", false, false, false, false, false, []string{"a"}},
		{"missing_scope", 8, []string{"a", "b", "c"}, "", false, false, false, true, false, []string{"a"}},
		{"rate_limited", 8, []string{"a", "b", "c"}, "", false, false, false, false, true, []string{"a"}},
		{"rule_changed", 8, []string{"a", "b", "c"}, "", true, false, false, false, false, []string{"a"}},
		{"fault_rescheduled", 8, []string{"a", "b", "c"}, "", false, true, false, false, false, []string{"a"}},
		{"fault_same_schedule", 8, []string{"a", "b", "c"}, "", false, false, true, false, false, []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := pelicanPlan()
			plan.UpdatedAt = time.Now().Add(-time.Hour)
			future := time.Now().Add(-time.Minute)
			plan.NextRunAt = &future
			plan.CronExpression = "0 0 1 1 *"
			plan.PelicanConfig.ParallelCount = 1
			plan.PelicanConfig.Availability = &AvailabilityPolicy{ValidForSeconds: 30, MaxExtraSamples: tc.budget}
			repo := &availabilityCoverageRepo{plan: plan, ids: tc.ids, action: tc.action, changeRule: tc.changeRule, changeNext: tc.changeNext, changeFault: tc.changeFault}
			results := &pelicanResults{}
			var mu sync.Mutex
			var sent []string
			runner := &ScheduledTestRunnerService{planRepo: repo, scheduledSvc: NewScheduledTestService(repo, results)}
			runner.runPelican = func(ctx context.Context, _ int64, model string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
				target, ok := ctx.Value(availabilityProbeConnectionKey{}).(availabilityProbeConnection)
				require.True(t, ok)
				mu.Lock()
				sent = append(sent, target.connectionID)
				mu.Unlock()
				now := time.Now()
				probe := &AvailabilityProbeResult{Version: 1, Status: "pass", StartedAt: now, FinishedAt: now}
				if tc.rateLimited {
					probe.Status, probe.HTTPStatus = "fail", 429
				}
				if !tc.missingScope {
					probe.Scope = &AvailabilityRequestScope{Version: 1, ConnectionScope: target.connectionID}
				}
				return &ScheduledTestResult{Status: "success", StartedAt: now, FinishedAt: now, PelicanConfig: &PelicanTestConfig{ModelID: model, AvailabilityResult: probe}}, nil
			}
			require.True(t, runner.runOnePlan(t.Context(), plan))
			require.Equal(t, tc.wantConnections, repo.selected)
			require.Equal(t, tc.wantConnections, sent)
			require.Equal(t, len(tc.wantConnections), repo.finished)
			for _, source := range repo.sources[1:] {
				require.Equal(t, AvailabilityCoverageSource, source)
			}
		})
	}
}

func TestAvailabilityCoverageCapsTotalSamples(t *testing.T) {
	plan := pelicanPlan()
	plan.CronExpression = "0 0 1 1 *"
	plan.PelicanConfig.ParallelCount = 8
	plan.MaxResults = 100
	plan.PelicanConfig.Availability = &AvailabilityPolicy{ValidForSeconds: 30, MaxExtraSamples: 64}
	for i := 0; i < 10; i++ {
		plan.PelicanConfig.ModelIDs = append(plan.PelicanConfig.ModelIDs, fmt.Sprint(i))
	}
	require.Equal(t, 1, availabilityCoverageRounds(plan, 10, time.Now()), "80 base samples leave no budget for another 80-sample round")
	plan.PelicanConfig.ModelIDs = plan.PelicanConfig.ModelIDs[:5]
	require.Equal(t, 2, availabilityCoverageRounds(plan, 10, time.Now()), "40 base plus 40 extra stays within the total cap")
	plan.MaxResults = 60
	require.Equal(t, 1, availabilityCoverageRounds(plan, 10, time.Now()), "the history limit must leave room for the base round")
	plan.MaxResults = 80
	require.Equal(t, 2, availabilityCoverageRounds(plan, 10, time.Now()), "a complete extra round fits without pruning the base round")
}

func TestAvailabilityCoverageDoesNotJudgeOrApplyQuality(t *testing.T) {
	plan := pelicanPlan()
	plan.CronExpression = "0 0 1 1 *"
	plan.PelicanConfig.QuestionKind = "candy"
	plan.PelicanConfig.ParallelCount = 1
	plan.PelicanConfig.Availability = &AvailabilityPolicy{ValidForSeconds: 30, MaxExtraSamples: 1}
	plan.PelicanConfig.Quality = &QualityPolicy{Action: QualityActionObserveOnly, ExpectedAnswer: "21", Judge: &QualityJudgeConfig{GroupID: 9, ModelID: "judge", Prompt: "grade"}}
	quality := plan.PelicanConfig.Quality
	future := time.Now().Add(time.Hour)
	plan.NextRunAt = &future
	plan.TriggerSource = AvailabilityCoverageSource
	repo := &availabilityCoverageRepo{plan: plan, ids: []string{"a"}}
	results := &pelicanResults{}
	var judges atomic.Int32
	runner := &ScheduledTestRunnerService{planRepo: repo, scheduledSvc: NewScheduledTestService(repo, results)}
	runner.runPelican = func(_ context.Context, _ int64, model string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
		now := time.Now()
		return &ScheduledTestResult{Status: "success", ResponseText: "21", StartedAt: now, FinishedAt: now, QualityJudgment: &QualityJudgment{Verdict: "correct"}, PelicanConfig: &PelicanTestConfig{ModelID: model, AvailabilityResult: &AvailabilityProbeResult{Version: 1, Status: "pass", StartedAt: now, FinishedAt: now}}}, nil
	}
	runner.judgeQuality = func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment {
		judges.Add(1)
		return &QualityJudgment{Verdict: "correct"}
	}
	require.True(t, runner.runPelicanPlan(t.Context(), plan))
	require.Zero(t, judges.Load())
	require.Zero(t, repo.qualityCalls)
	require.Len(t, results.results, 1)
	saved := results.results[0]
	require.Empty(t, saved.QualityAction)
	require.Empty(t, saved.QualityRoundID)
	require.Nil(t, saved.QualityJudgment)
	require.Nil(t, saved.PelicanConfig.Quality)
	require.Empty(t, saved.PelicanConfig.QualityModelActions)
	require.Empty(t, saved.PelicanConfig.QualityModelOutcomes)
	require.Nil(t, saved.PelicanConfig.QualityRestoreDiagnostic)
	require.NotNil(t, saved.PelicanConfig.AvailabilityResult)
	require.Equal(t, AvailabilityCoverageSource, saved.PelicanConfig.TriggerSource)
	require.Same(t, quality, plan.PelicanConfig.Quality)
	encoded, err := json.Marshal(saved.PelicanConfig)
	require.NoError(t, err)
	var snapshot map[string]any
	require.NoError(t, json.Unmarshal(encoded, &snapshot))
	require.NotContains(t, snapshot, "quality", "quality history and statistics select by this JSON field")
	require.Contains(t, snapshot, "availability_result")
}

func TestAvailabilityCoverageStopsAfterAnyRateLimitedSample(t *testing.T) {
	plan := pelicanPlan()
	plan.CronExpression = "* * * * *"
	plan.PelicanConfig.ParallelCount = 2
	plan.PelicanConfig.Availability = &AvailabilityPolicy{ValidForSeconds: 30, MaxExtraSamples: 4}
	due := time.Now().Add(-time.Minute)
	plan.NextRunAt = &due
	repo := &availabilityCoverageRepo{plan: plan, ids: []string{"a", "b", "c"}}
	results := &pelicanResults{}
	var calls atomic.Int32
	runner := &ScheduledTestRunnerService{planRepo: repo, scheduledSvc: NewScheduledTestService(repo, results)}
	runner.runPelican = func(ctx context.Context, _ int64, model string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
		target, ok := ctx.Value(availabilityProbeConnectionKey{}).(availabilityProbeConnection)
		require.True(t, ok)
		now := time.Now()
		status := 503
		if calls.Add(1) == 1 {
			status = 429
		}
		probe := &AvailabilityProbeResult{Version: 1, Status: "fail", HTTPStatus: status, StartedAt: now, FinishedAt: now, Scope: &AvailabilityRequestScope{Version: 1, ConnectionScope: target.connectionID}}
		return &ScheduledTestResult{Status: "failed", StartedAt: now, FinishedAt: now, PelicanConfig: &PelicanTestConfig{ModelID: model, AvailabilityResult: probe}}, nil
	}
	require.True(t, runner.runOnePlan(t.Context(), plan))
	require.EqualValues(t, 2, calls.Load())
	require.Equal(t, []string{"a"}, repo.selected)
}
