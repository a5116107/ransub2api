//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestQualityMeasurementValidityReadsDatabaseTimeAndRule(t *testing.T) {
	ctx := t.Context()
	var account int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status) VALUES('validity-fixture','openai','apikey','active') RETURNING id`).Scan(&account))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id=$1`, account)
	})
	plans, results := NewScheduledTestPlanRepository(integrationDB), NewScheduledTestResultRepository(integrationDB)
	svc := service.NewScheduledTestService(plans, results)
	for _, scenario := range []string{"within", "expired", "paused", "edited", "unknown-answer"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := &service.PelicanTestConfig{QuestionKind: "candy", ModelID: "fixture-model", Prompt: "42", ReasoningEffort: "high", ParallelCount: 1,
				Quality: &service.QualityPolicy{Action: service.QualityActionObserveOnly, ExpectedAnswer: "42", EvidenceValidForSeconds: 60}}
			plan, err := svc.CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: account, ModelID: cfg.ModelID, Enabled: true, CronExpression: "*/5 * * * *", MaxResults: 20, PelicanConfig: cfg})
			require.NoError(t, err)
			t.Cleanup(func() {
				_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM scheduled_test_plans WHERE id=$1`, plan.ID)
			})
			now := time.Now().UTC()
			observed := now.Add(-10 * time.Second)
			if scenario == "expired" {
				observed = now.Add(-61 * time.Second)
			}
			started, completed := observed.Add(-time.Second), observed.Add(time.Second)
			deadline := observed.Add(time.Minute)
			cfg = plan.PelicanConfig
			cfg.AvailabilityResult = &service.AvailabilityProbeResult{Version: 1, Status: "pass", StartedAt: started, FinishedAt: observed}
			policyJSON, err := json.Marshal([]any{1, cfg.Quality, cfg.ModelIDs, cfg.ParallelCount})
			require.NoError(t, err)
			zero, one := 0.0, 1.0
			measurement := &service.QualityMeasurement{Version: 1, MeasurementHash: strings.Repeat("a", 64), PolicyHash: fmt.Sprintf("%x", sha256.Sum256(policyJSON)), RuleVersion: plan.UpdatedAt,
				RequestedModel: cfg.ModelID, QuestionKind: cfg.QuestionKind, ObservedAt: observed, CompletedAt: completed, ValidForSeconds: 60, ValidUntil: &deadline,
				Dimensions: map[string]service.QualityDimensionMeasurement{
					"answer_correctness": {Status: "fail", Reason: "graded_incorrect", Score: &zero, SampleCount: 1},
					"grading":            {Status: "pass", Reason: "grading_completed", Score: &one, SampleCount: 1},
				}}
			want, reason := "within_window", "within_window"
			switch scenario {
			case "expired":
				want, reason = "expired", "expired"
			case "paused":
				_, err = integrationDB.ExecContext(ctx, `UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, plan.ID)
				want, reason = "unknown", "rule_paused"
			case "edited":
				_, err = integrationDB.ExecContext(ctx, `UPDATE scheduled_test_plans SET updated_at=updated_at+interval '1 microsecond' WHERE id=$1`, plan.ID)
				want, reason = "unknown", "rule_changed"
			case "unknown-answer":
				measurement.Dimensions["answer_correctness"] = service.QualityDimensionMeasurement{Status: "unknown", Reason: "grading_inconclusive"}
				measurement.ValidUntil = nil
				want, reason = "unknown", "answer_not_observed"
			}
			require.NoError(t, err)
			cfg.QualityMeasurement = measurement
			created, err := results.Create(ctx, &service.ScheduledTestResult{PlanID: plan.ID, Status: "failed", ErrorMessage: "answer_mismatch", StartedAt: started, FinishedAt: completed, PelicanConfig: cfg})
			require.NoError(t, err)
			read, err := NewScheduledTestResultRepository(integrationDB).GetResult(ctx, plan.ID, created.ID)
			require.NoError(t, err)
			require.Equal(t, want, read.QualityMeasurementValidity.Status, "reason=%s observed=%s completed=%s sql-start=%s sql-finish=%s", read.QualityMeasurementValidity.Reason, observed.Format(time.RFC3339Nano), completed.Format(time.RFC3339Nano), read.StartedAt.Format(time.RFC3339Nano), read.FinishedAt.Format(time.RFC3339Nano))
			require.Equal(t, reason, read.QualityMeasurementValidity.Reason, "observed=%s completed=%s sql-start=%s sql-finish=%s", observed.Format(time.RFC3339Nano), completed.Format(time.RFC3339Nano), read.StartedAt.Format(time.RFC3339Nano), read.FinishedAt.Format(time.RFC3339Nano))
			require.WithinDuration(t, time.Now(), read.QualityMeasurementValidity.EvaluatedAt, 3*time.Second)
			require.Equal(t, measurement, read.PelicanConfig.QualityMeasurement, "read-only assessment preserves frozen JSON")
			require.Equal(t, "failed", read.Status, "an unexpired wrong answer stays wrong")
			var raw []byte
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT pelican_config FROM scheduled_test_results WHERE id=$1`, created.ID).Scan(&raw))
			require.NotContains(t, string(raw), "quality_measurement_validity", "read assessments are never persisted as evidence")
		})
	}
}

func TestQualityMeasurementPersistenceAndLegacyHistory(t *testing.T) {
	ctx := t.Context()
	var account int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status)
 VALUES('measurement-fixture','openai','apikey','active') RETURNING id`).Scan(&account))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id=$1`, account)
	})
	plans := NewScheduledTestPlanRepository(integrationDB)
	results := NewScheduledTestResultRepository(integrationDB)
	svc := service.NewScheduledTestService(plans, results)
	config := &service.PelicanTestConfig{Prompt: "fixture", ReasoningEffort: "high", ParallelCount: 1,
		QualityMeasurement: &service.QualityMeasurement{Version: 999, MeasurementHash: "forged"}}
	plan, err := svc.CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: account, ModelID: "public-model",
		CronExpression: "*/5 * * * *", MaxResults: 20, PelicanConfig: config})
	require.NoError(t, err)
	require.Nil(t, plan.PelicanConfig.QualityMeasurement, "management inputs cannot configure measured results")
	storedPlan, err := plans.GetByID(ctx, plan.ID)
	require.NoError(t, err)
	require.Nil(t, storedPlan.PelicanConfig.QualityMeasurement)
	now := time.Now().UTC().Truncate(time.Microsecond)
	zero, one := 0.0, 1.0
	measurement := &service.QualityMeasurement{Version: 1, MeasurementHash: strings.Repeat("a", 64), PolicyHash: strings.Repeat("b", 64),
		RuleVersion: now.Add(-time.Hour), RequestedModel: "public-model", QuestionKind: "candy",
		ObservedAt: now, CompletedAt: now.Add(time.Second),
		Scope: &service.AvailabilityRequestScope{Version: 4, Model: "wire-model", Protocol: "responses", ServiceTier: "priority", ReasoningEffort: "high", Capability: "text"},
		Dimensions: map[string]service.QualityDimensionMeasurement{
			"answer_correctness": {Status: "fail", Reason: "graded_incorrect", Score: &zero, SampleCount: 1},
			"transport":          {Status: "pass", Reason: "completed_response", Score: &one, SampleCount: 1},
			"grading":            {Status: "pass", Reason: "grading_completed", Score: &one, SampleCount: 1},
			"model_identity":     {Status: "unknown", Reason: "independent_baseline_not_observed"},
		}}
	cfg := *plan.PelicanConfig
	cfg.Quality = &service.QualityPolicy{Action: service.QualityActionObserveOnly, ExpectedAnswer: "42"}
	cfg.ModelID, cfg.QualityMeasurement = "public-model", measurement
	result, err := results.Create(ctx, &service.ScheduledTestResult{PlanID: plan.ID, Status: "failed", ResponseText: "41",
		ErrorMessage: "answer_mismatch", StartedAt: now.Add(-time.Second), FinishedAt: measurement.CompletedAt,
		PelicanConfig: &cfg, QualityJudgment: &service.QualityJudgment{Verdict: "incorrect"}})
	require.NoError(t, err)
	// Reconstruct the repository to exercise persisted JSON, not the original pointers.
	restarted := NewScheduledTestResultRepository(integrationDB)
	detail, err := restarted.GetResult(ctx, plan.ID, result.ID)
	require.NoError(t, err)
	require.Equal(t, measurement, detail.PelicanConfig.QualityMeasurement)
	require.Equal(t, "incorrect", detail.QualityJudgment.Verdict)
	require.Equal(t, "failed", detail.Status)
	require.Nil(t, detail.PelicanConfig.QualityMeasurement.ValidUntil)
	summaries, err := restarted.ListByPlanID(ctx, plan.ID, 20, false)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	require.Empty(t, summaries[0].ResponseText)
	require.Equal(t, measurement, summaries[0].PelicanConfig.QualityMeasurement)
	operations, err := restarted.ListQualityHistory(ctx, 0, 100)
	require.NoError(t, err)
	var operationFound bool
	for _, operation := range operations {
		if operation.ID == result.ID {
			operationFound = true
			require.Equal(t, measurement, operation.PelicanConfig.QualityMeasurement)
		}
	}
	require.True(t, operationFound)
	cfg.QualityMeasurement = nil
	legacy, err := results.Create(ctx, &service.ScheduledTestResult{PlanID: plan.ID, Status: "success", ResponseText: "42",
		StartedAt: now, FinishedAt: now, PelicanConfig: &cfg, QualityJudgment: &service.QualityJudgment{Verdict: "correct"}})
	require.NoError(t, err)
	old, err := restarted.GetResult(ctx, plan.ID, legacy.ID)
	require.NoError(t, err)
	require.Nil(t, old.PelicanConfig.QualityMeasurement, "old history is never backfilled from current config")
	require.Equal(t, "correct", old.QualityJudgment.Verdict)
}

func TestQualityMeasurementRoundCurrentness(t *testing.T) {
	for _, scenario := range []string{"complete", "wrong-answer", "partial", "missing-model", "duplicate-model", "conflicting-config", "running", "expired-lease", "pending-comparison", "late-old-result", "legacy", "future-marker", "legacy-finish", "untracked-finish", "untracked-running", "untracked-result", "finished-before-samples", "paused", "edited", "pruned", "quota-skip", "lost-claim"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			var account int64
			require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status) VALUES('round-fixture','openai','apikey','active') RETURNING id`).Scan(&account))
			t.Cleanup(func() {
				_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id=$1`, account)
			})
			plans, results := NewScheduledTestPlanRepository(integrationDB), NewScheduledTestResultRepository(integrationDB)
			cfg := &service.PelicanTestConfig{QuestionKind: "candy", ModelID: "model-a", Prompt: "42", ReasoningEffort: "high", ParallelCount: 1,
				Quality: &service.QualityPolicy{Action: service.QualityActionObserveOnly, ExpectedAnswer: "42", EvidenceValidForSeconds: 60}}
			if scenario == "partial" || scenario == "conflicting-config" || scenario == "pruned" {
				cfg.ParallelCount = 2
			}
			if scenario == "missing-model" || scenario == "duplicate-model" {
				cfg.ModelIDs = []string{"model-a", "model-b"}
			}
			plan, err := service.NewScheduledTestService(plans, results).CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: account, ModelID: cfg.ModelID, Enabled: true, CronExpression: "*/5 * * * *", MaxResults: 20, PelicanConfig: cfg})
			require.NoError(t, err)
			now := time.Now().UTC().Truncate(time.Microsecond)
			until := now.Add(15 * time.Minute)
			exec := func(query string, args ...any) {
				t.Helper()
				_, err := integrationDB.ExecContext(ctx, query, args...)
				require.NoError(t, err)
			}
			claim := func(at, lease time.Time) {
				t.Helper()
				due := at.Add(-time.Second)
				exec(`UPDATE scheduled_test_plans SET running_until=NULL,next_run_at=$2 WHERE id=$1`, plan.ID, due)
				plan.NextRunAt = &due
				ok, err := plans.ClaimPelican(ctx, plan, at, lease, at.Add(30*time.Minute))
				require.NoError(t, err)
				require.True(t, ok)
			}
			claim(now.Add(-20*time.Second), until)
			save := func(round time.Time, verdict, action string) *service.ScheduledTestResult {
				t.Helper()
				config := *plan.PelicanConfig
				observed := now.Add(-5 * time.Second)
				started, completed := observed.Add(-time.Second), observed.Add(time.Second)
				deadline := observed.Add(time.Minute)
				config.AvailabilityResult = &service.AvailabilityProbeResult{Version: 1, Status: "pass", StartedAt: started, FinishedAt: observed}
				raw, err := json.Marshal([]any{1, config.Quality, config.ModelIDs, config.ParallelCount})
				require.NoError(t, err)
				score, status, reason := 1.0, "pass", "graded_correct"
				if verdict == "incorrect" {
					score, status, reason = 0, "fail", "graded_incorrect"
				}
				one := 1.0
				config.QualityMeasurement = &service.QualityMeasurement{Version: 1, MeasurementHash: strings.Repeat("a", 64), PolicyHash: fmt.Sprintf("%x", sha256.Sum256(raw)), RuleVersion: plan.UpdatedAt,
					RequestedModel: config.ModelID, QuestionKind: config.QuestionKind, ObservedAt: observed, CompletedAt: completed, ValidForSeconds: 60, ValidUntil: &deadline,
					Dimensions: map[string]service.QualityDimensionMeasurement{"answer_correctness": {Status: status, Reason: reason, Score: &score, SampleCount: 1}, "grading": {Status: "pass", Reason: "grading_completed", Score: &one, SampleCount: 1}}}
				result := &service.ScheduledTestResult{PlanID: plan.ID, Status: "success", StartedAt: started, FinishedAt: completed, PelicanConfig: &config, QualityRoundID: round.Format(time.RFC3339Nano), QualityAction: action, QualityJudgment: &service.QualityJudgment{Verdict: verdict}}
				if verdict == "incorrect" {
					result.Status, result.ErrorMessage = "failed", "answer_mismatch"
				}
				created, err := results.Create(ctx, result)
				require.NoError(t, err)
				return created
			}
			verdict := "correct"
			if scenario == "wrong-answer" {
				verdict = "incorrect"
			}
			action := "no_change"
			if scenario == "pending-comparison" {
				action = "comparison_pending"
			}
			created := save(until, verdict, action)
			want, reason, count, expected := "latest_complete", "latest_complete", int64(1), int64(1)
			if scenario != "running" && scenario != "expired-lease" && scenario != "legacy-finish" {
				require.NoError(t, plans.FinishPelican(ctx, plan.ID, until, now.Add(-time.Second)))
			}
			switch scenario {
			case "partial", "missing-model":
				want, reason, expected = "incomplete", "round_incomplete", 2
			case "duplicate-model":
				save(until, "correct", "no_change")
				want, reason, count, expected = "incomplete", "round_incomplete", 2, 2
			case "conflicting-config":
				plan.PelicanConfig.ParallelCount = 1
				save(until, "correct", "no_change")
				want, reason, count, expected = "incomplete", "round_incomplete", 2, 0
			case "running":
				want, reason = "running", "round_running"
			case "expired-lease":
				// Preserve the same registered identity, but make its lease expire.
				exec(`UPDATE scheduled_test_plans SET running_until=$2,pelican_config=jsonb_set(pelican_config,'{_quality_latest_round,lease_until}',to_jsonb($2::timestamptz)) WHERE id=$1`, plan.ID, now.Add(-time.Second))
				exec(`UPDATE scheduled_test_results SET quality_round_id=$2 WHERE id=$1`, created.ID, now.Add(-time.Second).Format(time.RFC3339Nano))
				want, reason = "incomplete", "lease_expired"
			case "pending-comparison":
				want, reason = "incomplete", "comparison_pending"
			case "late-old-result":
				newer := until.Add(time.Second)
				claim(now.Add(-10*time.Second), newer)
				newResult := save(newer, "incorrect", "inconclusive")
				require.NoError(t, plans.FinishTriggeredQuality(ctx, plan, newer, now.Add(-time.Second), now.Add(time.Minute)))
				created = save(until, "correct", "no_change")
				require.Greater(t, created.ID, newResult.ID, "old result arrives after the new failure")
				fresh, err := results.GetResult(ctx, plan.ID, newResult.ID)
				require.NoError(t, err)
				require.Equal(t, "latest_complete", fresh.QualityMeasurementValidity.Round.Status)
				require.Equal(t, "within_window", fresh.QualityMeasurementValidity.Status)
				require.Equal(t, "incorrect", fresh.QualityJudgment.Verdict)
				want, reason, count, expected = "superseded", "different_claim", 2, 1
			case "legacy":
				exec(`UPDATE scheduled_test_plans SET pelican_config=pelican_config-'_quality_latest_round' WHERE id=$1`, plan.ID)
				want, reason = "unknown", "round_not_tracked"
			case "future-marker":
				exec(`UPDATE scheduled_test_plans SET pelican_config=jsonb_set(pelican_config,'{_quality_latest_round,version}','99') WHERE id=$1`, plan.ID)
				want, reason = "unknown", "round_not_tracked"
			case "legacy-finish", "untracked-finish":
				exec(`UPDATE scheduled_test_plans SET running_until=NULL,last_run_at=$2 WHERE id=$1`, plan.ID, now)
				want, reason = "incomplete", "round_not_finished"
				if scenario == "untracked-finish" {
					want, reason = "unknown", "untracked_run"
				}
			case "paused":
				exec(`UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, plan.ID)
				want, reason = "unknown", "rule_paused"
			case "untracked-running", "untracked-result":
				exec(`UPDATE scheduled_test_plans SET running_until=$2 WHERE id=$1`, plan.ID, until.Add(time.Second))
				if scenario == "untracked-result" {
					created = save(until.Add(time.Second), "correct", "no_change")
				}
				want, reason = "unknown", "untracked_run"
			case "finished-before-samples":
				exec(`UPDATE scheduled_test_plans SET last_run_at=$2,pelican_config=jsonb_set(pelican_config,'{_quality_latest_round,completed_at}',to_jsonb($2::timestamptz)) WHERE id=$1`, plan.ID, now.Add(-10*time.Second))
				want, reason = "unknown", "untracked_run"
			case "edited":
				exec(`UPDATE scheduled_test_plans SET updated_at=updated_at+interval '1 microsecond' WHERE id=$1`, plan.ID)
				want, reason = "unknown", "rule_changed"
			case "pruned":
				save(until, "correct", "no_change")
				require.NoError(t, results.PruneOldResults(ctx, plan.ID, 1))
				latest, listErr := results.ListByPlanID(ctx, plan.ID, 1)
				require.NoError(t, listErr)
				created = latest[0]
				want, reason, expected = "incomplete", "round_incomplete", 2
			case "quota-skip":
				newer := until.Add(time.Second)
				claim(now.Add(-10*time.Second), newer)
				require.NoError(t, plans.(service.QualityQuotaSkipRepository).FinishQuotaSkippedPelican(ctx, plan, newer, now.Add(-time.Second)))
				want, reason = "superseded", "different_claim"
			case "lost-claim":
				plan.NextRunAt = &now
				ok, claimErr := plans.ClaimPelican(ctx, plan, now, until.Add(time.Second), now.Add(time.Hour))
				require.NoError(t, claimErr)
				require.False(t, ok)
			}
			read, err := NewScheduledTestResultRepository(integrationDB).GetResult(ctx, plan.ID, created.ID)
			require.NoError(t, err)
			require.NotNil(t, read.QualityMeasurementValidity.Round)
			round := read.QualityMeasurementValidity.Round
			require.Equal(t, want, round.Status)
			require.Equal(t, reason, round.Reason)
			require.Equal(t, count, round.SampleCount)
			if expected == 0 {
				require.Nil(t, round.ExpectedSampleCount)
			} else {
				require.Equal(t, &expected, round.ExpectedSampleCount)
			}
			require.Equal(t, created.PelicanConfig.QualityMeasurement, read.PelicanConfig.QualityMeasurement, "read assessment must not rewrite frozen evidence")
			storedPlan, err := plans.GetByID(ctx, plan.ID)
			require.NoError(t, err)
			raw, err := json.Marshal(storedPlan)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "_quality_latest_round", "claim marker is repository-private, not rule input/output")
			var saved []byte
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT pelican_config FROM scheduled_test_results WHERE id=$1`, created.ID).Scan(&saved))
			require.NotContains(t, string(saved), "quality_measurement_validity")
			require.NotContains(t, string(saved), "_quality_latest_round")
		})
	}
}

func TestQualityMeasurementConcurrentClaims(t *testing.T) {
	ctx := t.Context()
	var account int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status) VALUES('claim-fixture','openai','apikey','active') RETURNING id`).Scan(&account))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id=$1`, account)
	})
	plans := NewScheduledTestPlanRepository(integrationDB)
	now := time.Now().UTC().Truncate(time.Microsecond)
	due := now.Add(-time.Minute)
	plan, err := plans.Create(ctx, &service.ScheduledTestPlan{AccountID: account, ModelID: "fixture", Enabled: true, NextRunAt: &due, CronExpression: "*/5 * * * *", MaxResults: 20,
		PelicanConfig: &service.PelicanTestConfig{ParallelCount: 1, Quality: &service.QualityPolicy{Action: service.QualityActionObserveOnly}}})
	require.NoError(t, err)
	type claimResult struct {
		until time.Time
		ok    bool
		err   error
	}
	start, outcomes := make(chan struct{}), make(chan claimResult, 2)
	for index := range 2 {
		go func() {
			local := *plan
			until := now.Add(15*time.Minute + time.Duration(index)*time.Microsecond)
			<-start
			ok, err := plans.ClaimPelican(ctx, &local, now, until, now.Add(time.Hour))
			outcomes <- claimResult{until, ok, err}
		}()
	}
	close(start)
	var winner, loser time.Time
	winners := 0
	for range 2 {
		result := <-outcomes
		require.NoError(t, result.err)
		if result.ok {
			winner = result.until
			winners++
		} else {
			loser = result.until
		}
	}
	require.Equal(t, 1, winners)
	// A stale completion must not finish or replace the winning claim marker.
	require.NoError(t, plans.FinishPelican(ctx, plan.ID, loser, now))
	var lease time.Time
	var completed bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT (pelican_config->'_quality_latest_round'->>'lease_until')::timestamptz,
 pelican_config->'_quality_latest_round' ? 'completed_at' FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&lease, &completed))
	require.Equal(t, winner, lease)
	require.False(t, completed)
	require.NoError(t, plans.FinishPelican(ctx, plan.ID, winner, now))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT (pelican_config->'_quality_latest_round'->>'completed_at')::timestamptz=last_run_at
 FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&completed))
	require.True(t, completed)
}

func TestPelicanSchedulePersistenceLeaseAndRetention(t *testing.T) {
	ctx := context.Background()
	var account int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts (name,platform,type,status) VALUES ('pelican-integration','openai','apikey','active') RETURNING id`).Scan(&account))
	defer func() { _, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id=$1`, account) }()
	plans := NewScheduledTestPlanRepository(integrationDB)
	results := NewScheduledTestResultRepository(integrationDB)
	svc := service.NewScheduledTestService(plans, results)
	config := &service.PelicanTestConfig{RuleName: "Saved test rule", Prompt: "<pelican>", ReasoningEffort: "medium", ParallelCount: 2}
	plan, err := svc.CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: account, ModelID: "gpt-6-astra", CronExpression: "*/30 * * * *", Enabled: true, MaxResults: 2, PelicanConfig: config})
	require.NoError(t, err)
	require.Equal(t, config, plan.PelicanConfig)
	second, err := svc.CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: account, ModelID: "gpt-6-astra", CronExpression: "*/30 * * * *", Enabled: true, PelicanConfig: config})
	require.NoError(t, err, "multiple cron plans per account are supported")
	legacy, err := svc.CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: account, CronExpression: "*/30 * * * *", Enabled: true})
	require.NoError(t, err)
	require.Nil(t, legacy.PelicanConfig)
	now := time.Now().Truncate(time.Microsecond)
	_, err = integrationDB.ExecContext(ctx, `UPDATE scheduled_test_plans SET next_run_at=$2 WHERE id=$1`, plan.ID, now.Add(-time.Minute))
	require.NoError(t, err)
	stalePlan := *plan
	plan, err = plans.GetByID(ctx, plan.ID)
	require.NoError(t, err)
	until := now.Add(15 * time.Minute)
	claimed, err := plans.ClaimPelican(ctx, &stalePlan, now, until, now.Add(30*time.Minute))
	require.NoError(t, err)
	require.False(t, claimed, "a changed due time invalidates the stale plan snapshot")
	claimed, err = plans.ClaimPelican(ctx, plan, now, until, now.Add(30*time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	_, err = integrationDB.ExecContext(ctx, `UPDATE scheduled_test_plans SET next_run_at=$2 WHERE id=$1`, second.ID, now.Add(-time.Minute))
	require.NoError(t, err)
	second, err = plans.GetByID(ctx, second.ID)
	require.NoError(t, err)
	claimed, err = plans.ClaimPelican(ctx, second, now, until, now.Add(30*time.Minute))
	require.NoError(t, err)
	require.False(t, claimed, "another plan for the same account must wait for the active lease")

	claimed, err = plans.ClaimPelican(ctx, plan, now, until, now.Add(30*time.Minute))
	require.NoError(t, err)
	require.False(t, claimed)
	plan.Enabled = false
	_, err = svc.UpdatePlan(ctx, plan)
	require.NoError(t, err)
	require.NoError(t, plans.FinishPelican(ctx, plan.ID, until, now))
	stored, err := plans.GetByID(ctx, plan.ID)
	require.NoError(t, err)
	require.False(t, stored.Enabled)
	require.Nil(t, stored.RunningUntil)
	for i := 0; i < 3; i++ {
		require.NoError(t, svc.SaveResult(ctx, plan.ID, 2, &service.ScheduledTestResult{Status: "success", ResponseText: "<html></html>", StartedAt: now, FinishedAt: now, PelicanConfig: config}))
	}
	saved, err := results.ListByPlanID(ctx, plan.ID, 50)
	require.NoError(t, err)
	require.Len(t, saved, 2)
	require.Equal(t, config, saved[0].PelicanConfig)
	summaries, err := results.ListByPlanID(ctx, plan.ID, 50, false)
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	require.Empty(t, summaries[0].ResponseText)
	detail, err := results.GetResult(ctx, plan.ID, summaries[0].ID)
	require.NoError(t, err)
	require.Equal(t, "<html></html>", detail.ResponseText)
	_, err = results.GetResult(ctx, legacy.ID, summaries[0].ID)
	require.Error(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE scheduled_test_results SET created_at=$2 WHERE plan_id=$1`, plan.ID, now.Add(-8*24*time.Hour))
	require.NoError(t, err)
	_, err = results.Create(ctx, &service.ScheduledTestResult{PlanID: legacy.ID, Status: "success", StartedAt: now, FinishedAt: now})
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE scheduled_test_results SET created_at=$2 WHERE plan_id=$1`, legacy.ID, now.Add(-8*24*time.Hour))
	require.NoError(t, err)
	require.NoError(t, results.PruneExpiredPelican(ctx, now.Add(-7*24*time.Hour)))
	saved, err = results.ListByPlanID(ctx, plan.ID, 50)
	require.NoError(t, err)
	require.Empty(t, saved)
	saved, err = results.ListByPlanID(ctx, legacy.ID, 50)
	require.NoError(t, err)
	require.Len(t, saved, 1, "do not expire connectivity history")
}

func TestPelicanHistoryReturnsEveryRetainedOutputAcrossAccounts(t *testing.T) {
	ctx := context.Background()
	plans := NewScheduledTestPlanRepository(integrationDB)
	results := NewScheduledTestResultRepository(integrationDB)
	svc := service.NewScheduledTestService(plans, results)
	var accountIDs []int64
	defer func() {
		for _, id := range accountIDs {
			_, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id=$1`, id)
		}
	}()
	var ids []int64
	now := time.Now()
	for i := 0; i < 2; i++ {
		var accountID int64
		require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts (name,platform,type,status) VALUES ('pelican-history','openai','apikey','active') RETURNING id`).Scan(&accountID))
		accountIDs = append(accountIDs, accountID)
		plan, err := svc.CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: accountID, ModelID: "gpt-6-astra", CronExpression: "*/3 * * * *", Enabled: false, MaxResults: 6, PelicanConfig: &service.PelicanTestConfig{Prompt: "draw", ReasoningEffort: "medium", ParallelCount: 1}})
		require.NoError(t, err)
		count := 6
		if i == 1 {
			count = 1
		}
		for j := 0; j < count; j++ {
			status := "success"
			if j == 5 {
				status = "failed"
			}
			result, err := results.Create(ctx, &service.ScheduledTestResult{PlanID: plan.ID, Status: status, ResponseText: "<html>saved</html>", StartedAt: now, FinishedAt: now, PelicanConfig: plan.PelicanConfig})
			require.NoError(t, err)
			ids = append(ids, result.ID)
		}
		// Ordinary connection records must not leak into Pelican history.
		legacy, err := svc.CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: accountID, CronExpression: "*/3 * * * *"})
		require.NoError(t, err)
		_, err = results.Create(ctx, &service.ScheduledTestResult{PlanID: legacy.ID, Status: "success", StartedAt: now, FinishedAt: now})
		require.NoError(t, err)
	}
	collect := func(accountID int64) []*service.PelicanHistoryResult {
		var found []*service.PelicanHistoryResult
		cursor := int64(0)
		for {
			page, err := svc.ListPelicanHistory(ctx, cursor, 2, accountID)
			require.NoError(t, err)
			found = append(found, page.Items...)
			if page.NextCursor == 0 {
				break
			}
			if cursor > 0 {
				require.Less(t, page.NextCursor, cursor)
			}
			cursor = page.NextCursor
		}
		return found
	}
	// The IQ test's records window pages one account with the same cursor.
	for i, want := range [][]int64{ids[:6], ids[6:]} {
		scoped := collect(accountIDs[i])
		scopedIDs := make([]int64, 0, len(scoped))
		for _, result := range scoped {
			require.Equal(t, accountIDs[i], result.AccountID)
			scopedIDs = append(scopedIDs, result.ID)
		}
		require.ElementsMatch(t, want, scopedIDs)
	}
	found := collect(0)
	require.Len(t, found, 7)
	actual := make([]int64, 0)
	failed := 0
	for _, result := range found {
		actual = append(actual, result.ID)
		require.Empty(t, result.ResponseText)
		require.Equal(t, "pelican-history", result.AccountName)
		if result.Status == "failed" {
			failed++
		}
	}
	require.ElementsMatch(t, ids, actual)
	require.Equal(t, 1, failed)
}
