//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func procurementQualityFixture(t *testing.T) (*service.ScheduledTestPlan, *service.ScheduledTestResult) {
	t.Helper()
	plan, sample := qualityRouteFixture(t, false)
	plan, sample = claimQualityFaultSample(t, plan, sample)
	scope := sample.PelicanConfig.QualityMeasurement.Scope
	scope.ServiceTier, scope.ReasoningEffort = "unspecified", "unspecified"
	m := sample.PelicanConfig.QualityMeasurement
	m.ValidForSeconds = 60
	until := m.ObservedAt.Add(time.Minute)
	m.ValidUntil = &until
	raw, err := json.Marshal([]any{1, sample.PelicanConfig.Quality, sample.PelicanConfig.ModelIDs, sample.PelicanConfig.ParallelCount})
	require.NoError(t, err)
	m.PolicyHash = fmt.Sprintf("%x", sha256.Sum256(raw))
	one := 1.0
	m.Dimensions = map[string]service.QualityDimensionMeasurement{
		"answer_correctness": {Status: "pass", Reason: "graded_correct", Score: &one, SampleCount: 1},
		"grading":            {Status: "pass", Reason: "grading_completed", Score: &one, SampleCount: 1},
		"transport":          {Status: "pass", Reason: "completed_response", Score: &one, SampleCount: 1},
	}
	sample.Status, sample.ErrorMessage, sample.QualityAction, sample.ResponseText = "success", "", "observed", "42"
	sample.QualityJudgment = &service.QualityJudgment{Verdict: "correct"}
	return plan, sample
}

func saveProcurementQualityRound(t *testing.T, plan *service.ScheduledTestPlan, sample *service.ScheduledTestResult) {
	t.Helper()
	_, err := NewScheduledTestResultRepository(integrationDB).Create(t.Context(), sample)
	require.NoError(t, err)
	lease, err := time.Parse(time.RFC3339Nano, sample.QualityRoundID)
	require.NoError(t, err)
	require.NoError(t, NewScheduledTestPlanRepository(integrationDB).FinishPelican(t.Context(), plan.ID, lease, time.Now().UTC().Truncate(time.Microsecond)))
}

func TestProcurementQualityEvidenceCurrentRound(t *testing.T) {
	plan, sample := procurementQualityFixture(t)
	saveProcurementQualityRound(t, plan, sample)
	repo := NewChannelRepository(integrationDB).(service.ProcurementCatalogRepository)
	preview, err := repo.PreviewProcurementClassification(t.Context(), &service.ProcurementPreviewCommand{Requests: []service.ProfitPreviewRequestScope{{AccountID: plan.AccountID, PublicModel: plan.ModelID, Scope: *sample.PelicanConfig.QualityMeasurement.Scope}}})
	require.NoError(t, err)
	require.Equal(t, "pass", preview.Results[0].Quality.Status)
	require.Equal(t, 1, preview.Results[0].Quality.ConfirmedRules)
	require.Equal(t, "unknown", preview.Results[0].Quality.ModelIdentity)
	require.Equal(t, "not_evaluated", preview.Results[0].Qualification)
	raw, err := json.Marshal(preview)
	require.NoError(t, err)
	for _, private := range []string{"private-route-fixture", "_quality_observed_route", "measurement_hash", "policy_hash", "fingerprint", "response_text", sample.QualityRouteEvidence.Fingerprint} {
		require.NotContains(t, string(raw), private)
	}
	require.NotContains(t, strings.ToLower(string(raw)), "route.invalid")
}

func TestProcurementQualityEvidenceStateMatrix(t *testing.T) {
	for _, scenario := range []string{"pass", "incorrect", "expired", "undeclared", "paused", "rule-changed", "running", "pending-comparison", "missing-sample", "legacy-marker", "future-marker", "new-fault", "changed-key", "legacy-route", "missing-measurement", "wrong-model", "wrong-protocol", "fast", "effort", "stream", "summary", "verbosity", "encrypted", "deleted-account", "timezone-round"} {
		t.Run(scenario, func(t *testing.T) {
			plan, sample := procurementQualityFixture(t)
			request := service.ProfitPreviewRequestScope{AccountID: plan.AccountID, PublicModel: plan.ModelID, Scope: *sample.PelicanConfig.QualityMeasurement.Scope}
			want, reason := "unknown", ""
			exec := func(query string, args ...any) {
				t.Helper()
				_, err := integrationDB.ExecContext(t.Context(), query, args...)
				require.NoError(t, err)
			}
			switch scenario {
			case "pass", "timezone-round":
				want = "pass"
				if scenario == "timezone-round" {
					lease, err := time.Parse(time.RFC3339Nano, sample.QualityRoundID)
					require.NoError(t, err)
					sample.QualityRoundID = lease.In(time.FixedZone("HKT", 8*3600)).Format(time.RFC3339Nano)
					sample.QualityRouteEvidence.FaultRound = sample.QualityRoundID
				}
			case "incorrect":
				zero := 0.0
				sample.PelicanConfig.QualityMeasurement.Dimensions["answer_correctness"] = service.QualityDimensionMeasurement{Status: "fail", Reason: "graded_incorrect", Score: &zero, SampleCount: 1}
				sample.QualityJudgment.Verdict, sample.Status, sample.ErrorMessage = "incorrect", "failed", "answer_mismatch"
				want, reason = "fail", "answer_incorrect"
			case "expired":
				m, probe, evidence := sample.PelicanConfig.QualityMeasurement, sample.PelicanConfig.AvailabilityResult, sample.QualityRouteEvidence
				past := sample.FinishedAt.Add(-2 * time.Minute)
				m.ObservedAt, m.CompletedAt, probe.StartedAt, probe.FinishedAt, evidence.ObservedAt = past, past, past, past, past
				sample.StartedAt, sample.FinishedAt = past, past
				expires := past.Add(time.Minute)
				m.ValidUntil = &expires
				reason = "expired"
			case "undeclared":
				sample.PelicanConfig.QualityMeasurement.ValidUntil = nil
				sample.PelicanConfig.QualityMeasurement.ValidForSeconds = 0
				reason = "validity_not_declared"
			case "pending-comparison":
				sample.QualityAction = "comparison_pending"
				reason = "comparison_pending"
			case "missing-sample":
				sample.PelicanConfig.ParallelCount = 2
				reason = "invalid_measurement"
			case "legacy-route":
				sample.QualityRouteEvidence = nil
				reason = "route_not_observed"
			case "missing-measurement":
				sample.PelicanConfig.QualityMeasurement = nil
				reason = "evidence_not_observed"
			case "wrong-model":
				request.Scope.Model = "another-model"
			case "wrong-protocol":
				request.Scope.Protocol = "chat_http"
			case "fast":
				request.Scope.ServiceTier = "priority"
			case "effort":
				request.Scope.ReasoningEffort = "high"
			case "stream":
				request.Scope.Stream = true
			case "summary":
				request.Scope.Version, request.Scope.ReasoningSummary = 3, "detailed"
			case "verbosity":
				request.Scope.Version, request.Scope.TextVerbosity = 4, "medium"
			case "encrypted":
				request.Scope.Version, request.Scope.IncludeEncryptedContent = 3, true
			}
			saveProcurementQualityRound(t, plan, sample)
			switch scenario {
			case "paused":
				exec(`UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, plan.ID)
				reason = "rule_paused"
			case "rule-changed":
				exec(`UPDATE scheduled_test_plans SET updated_at=updated_at+interval '1 second' WHERE id=$1`, plan.ID)
				reason = "rule_changed"
			case "running":
				plan, _ = claimQualityFaultSample(t, plan, sample)
				reason = "evidence_not_observed"
			case "legacy-marker":
				exec(`UPDATE scheduled_test_plans SET pelican_config=pelican_config-'_quality_latest_round' WHERE id=$1`, plan.ID)
				reason = "evidence_not_observed"
			case "future-marker":
				exec(`UPDATE scheduled_test_plans SET pelican_config=jsonb_set(pelican_config,'{_quality_latest_round,version}','99') WHERE id=$1`, plan.ID)
				reason = "evidence_not_observed"
			case "new-fault":
				require.NoError(t, (&accountRepository{sql: integrationDB}).ApplyAvailabilityFault(t.Context(), plan.AccountID))
				reason = "business_fault_recorded"
			case "changed-key":
				exec(`UPDATE accounts SET credentials=jsonb_set(credentials,'{api_key}','"new-key"') WHERE id=$1`, plan.AccountID)
				reason = "route_changed"
			case "deleted-account":
				exec(`UPDATE accounts SET deleted_at=clock_timestamp() WHERE id=$1`, plan.AccountID)
			}
			preview, err := NewChannelRepository(integrationDB).(service.ProcurementCatalogRepository).PreviewProcurementClassification(t.Context(), &service.ProcurementPreviewCommand{Requests: []service.ProfitPreviewRequestScope{request}})
			require.NoError(t, err)
			q := preview.Results[0].Quality
			require.Equal(t, want, q.Status)
			require.Equal(t, "unknown", q.ModelIdentity)
			require.Equal(t, preview.ObservedAt, q.EvaluatedAt)
			require.Equal(t, "not_evaluated", preview.Results[0].Qualification)
			if reason != "" {
				require.Len(t, q.Rules, 1)
				require.Equal(t, reason, q.Rules[0].Reason)
			}
		})
	}
}

func TestProcurementQualityEvidenceSnapshotAndRecovery(t *testing.T) {
	for _, scenario := range []string{"fault", "key", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			plan, sample := procurementQualityFixture(t)
			saveProcurementQualityRound(t, plan, sample)
			db, err := sql.Open("postgres", integrationPostgresDSN)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)
			var readerPID int
			require.NoError(t, db.QueryRow(`SELECT pg_backend_pid()`).Scan(&readerPID))
			gate, err := integrationDB.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = gate.Rollback() }()
			_, err = gate.Exec(`LOCK TABLE scheduled_test_plans IN ACCESS EXCLUSIVE MODE`)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			command := &service.ProcurementPreviewCommand{Requests: []service.ProfitPreviewRequestScope{{AccountID: plan.AccountID, PublicModel: plan.ModelID, Scope: *sample.PelicanConfig.QualityMeasurement.Scope}}}
			type outcome struct {
				view *service.ProcurementClassificationPreview
				err  error
			}
			finished := make(chan outcome, 1)
			repo := NewChannelRepository(db).(service.ProcurementCatalogRepository)
			go func() { view, e := repo.PreviewProcurementClassification(ctx, command); finished <- outcome{view, e} }()
			require.Eventually(t, func() bool {
				var waiting bool
				e := integrationDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND query LIKE '%FROM scheduled_test_plans%')`, readerPID).Scan(&waiting)
				return e == nil && waiting
			}, 5*time.Second, 10*time.Millisecond)
			var changedAt time.Time
			if scenario == "cancel" {
				cancel()
			} else {
				query := `UPDATE scheduled_test_plans SET availability_fault_version=availability_fault_version+1 WHERE id=$1 RETURNING clock_timestamp()`
				id := plan.ID
				if scenario == "key" {
					query, id = `UPDATE accounts SET credentials=jsonb_set(credentials,'{api_key}','"changed-during-preview"') WHERE id=$1 RETURNING clock_timestamp()`, plan.AccountID
				}
				require.NoError(t, gate.QueryRow(query, id).Scan(&changedAt))
				require.NoError(t, gate.Commit())
			}
			var result outcome
			select {
			case result = <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("preview did not finish after release/cancel")
			}
			if scenario == "cancel" {
				require.Error(t, result.err)
				require.Nil(t, result.view)
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				require.NoError(t, gate.Rollback())
			} else {
				require.NoError(t, result.err)
				require.Equal(t, "pass", result.view.Results[0].Quality.Status, "all reads must share the original snapshot")
				require.False(t, result.view.ObservedAt.Before(changedAt), "database time must follow the blocked read")
			}
			view, err := repo.PreviewProcurementClassification(t.Context(), command)
			require.NoError(t, err, "the same pool must recover after cancellation")
			want := "unknown"
			if scenario == "cancel" {
				want = "pass"
			}
			require.Equal(t, want, view.Results[0].Quality.Status)
		})
	}
}

func TestProcurementQualityEvidenceMultipleRulesAndLateResults(t *testing.T) {
	for _, scenario := range []string{"both-pass", "second-fails", "second-paused", "other-scope", "late-old-pass", "missing-peer-model", "duplicate-sample"} {
		t.Run(scenario, func(t *testing.T) {
			plan, original := procurementQualityFixture(t)
			saveProcurementQualityRound(t, plan, original)
			request := service.ProfitPreviewRequestScope{AccountID: plan.AccountID, PublicModel: plan.ModelID, Scope: *original.PelicanConfig.QualityMeasurement.Scope}
			want := "unknown"
			if scenario == "missing-peer-model" || scenario == "duplicate-sample" {
				if scenario == "duplicate-sample" {
					_, err := NewScheduledTestResultRepository(integrationDB).Create(t.Context(), original)
					require.NoError(t, err)
				} else {
					_, err := integrationDB.Exec(`UPDATE scheduled_test_results SET pelican_config=jsonb_set(pelican_config,'{model_ids}','["fixture-model","absent-peer"]') WHERE plan_id=$1`, plan.ID)
					require.NoError(t, err)
				}
			} else {
				plans := NewScheduledTestPlanRepository(integrationDB)
				second := plan
				if scenario != "late-old-pass" {
					cfg := *plan.PelicanConfig
					cfg.QualityMeasurement, cfg.AvailabilityResult = nil, nil
					policy := *cfg.Quality
					policy.Action = "disable_scheduling"
					cfg.Quality = &policy
					var err error
					second, err = plans.Create(t.Context(), &service.ScheduledTestPlan{AccountID: plan.AccountID, ModelID: plan.ModelID, Enabled: true, CronExpression: "*/5 * * * *", MaxResults: 20, PelicanConfig: &cfg})
					require.NoError(t, err)
				}
				second, sample := claimQualityFaultSample(t, second, original)
				sample.PlanID = second.ID
				until := sample.PelicanConfig.QualityMeasurement.ObservedAt.Add(time.Minute)
				sample.PelicanConfig.QualityMeasurement.ValidUntil = &until
				raw, err := json.Marshal([]any{1, sample.PelicanConfig.Quality, sample.PelicanConfig.ModelIDs, sample.PelicanConfig.ParallelCount})
				require.NoError(t, err)
				sample.PelicanConfig.QualityMeasurement.PolicyHash = fmt.Sprintf("%x", sha256.Sum256(raw))
				if scenario == "second-fails" || scenario == "late-old-pass" {
					zero := 0.0
					sample.PelicanConfig.QualityMeasurement.Dimensions["answer_correctness"] = service.QualityDimensionMeasurement{Status: "fail", Reason: "graded_incorrect", Score: &zero, SampleCount: 1}
					sample.QualityJudgment = &service.QualityJudgment{Verdict: "incorrect"}
					sample.Status, sample.ErrorMessage = "failed", "answer_mismatch"
					want = "fail"
				} else if scenario == "both-pass" || scenario == "other-scope" {
					want = "pass"
					if scenario == "other-scope" {
						scope := *sample.PelicanConfig.QualityMeasurement.Scope
						scope.ServiceTier = "priority"
						probe, m := *sample.PelicanConfig.AvailabilityResult, *sample.PelicanConfig.QualityMeasurement
						probe.Scope, m.Scope = &scope, &scope
						sample.PelicanConfig.AvailabilityResult, sample.PelicanConfig.QualityMeasurement = &probe, &m
					}
				}
				saveProcurementQualityRound(t, second, sample)
				if scenario == "second-paused" {
					_, err := integrationDB.Exec(`UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, second.ID)
					require.NoError(t, err)
				}
				if scenario == "late-old-pass" {
					_, err := NewScheduledTestResultRepository(integrationDB).Create(t.Context(), original)
					require.NoError(t, err)
				}
			}
			requests := make([]service.ProfitPreviewRequestScope, 100)
			for i := range requests {
				requests[i] = request
			}
			preview, err := NewChannelRepository(integrationDB).(service.ProcurementCatalogRepository).PreviewProcurementClassification(t.Context(), &service.ProcurementPreviewCommand{Requests: requests})
			require.NoError(t, err)
			require.Len(t, preview.Results, 100)
			for _, result := range preview.Results {
				require.Equal(t, want, result.Quality.Status)
				require.Equal(t, preview.ObservedAt, result.Quality.EvaluatedAt)
			}
		})
	}
}

func TestProcurementQualityEvidenceBoundedRead(t *testing.T) {
	for _, scenario := range []string{"history", "samples", "null-quality-rule"} {
		t.Run(scenario, func(t *testing.T) {
			plan, sample := procurementQualityFixture(t)
			saveProcurementQualityRound(t, plan, sample)
			if scenario == "null-quality-rule" {
				_, err := integrationDB.Exec(`INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,pelican_config)
 SELECT account_id,model_id,cron_expression,enabled,'{"quality":null}'::jsonb FROM scheduled_test_plans WHERE id=$1`, plan.ID)
				require.NoError(t, err)
			} else {
				count, round := qualityScopeMaxHistory, "old-round"
				if scenario == "samples" {
					count, round = qualityScopeMaxSamples, sample.QualityRoundID
				}
				_, err := integrationDB.Exec(`INSERT INTO scheduled_test_results(plan_id,status,started_at,finished_at,quality_round_id,pelican_config)
 SELECT $1,'success',clock_timestamp(),clock_timestamp(),$2,'{}'::jsonb FROM generate_series(1,$3)`, plan.ID, round, count)
				require.NoError(t, err)
			}
			preview, err := NewChannelRepository(integrationDB).(service.ProcurementCatalogRepository).PreviewProcurementClassification(t.Context(), &service.ProcurementPreviewCommand{Requests: []service.ProfitPreviewRequestScope{{AccountID: plan.AccountID, PublicModel: plan.ModelID, Scope: *sample.PelicanConfig.QualityMeasurement.Scope}}})
			require.NoError(t, err)
			q := preview.Results[0].Quality
			if scenario == "null-quality-rule" {
				require.Equal(t, "pass", q.Status, "native quality:null plans are not paused answer rules")
			} else {
				require.Equal(t, "unknown", q.Status)
				require.Equal(t, "coverage_limit", q.Reason)
				require.Empty(t, q.Rules, "truncated evidence must not acquire a passing subset")
			}
		})
	}
}

func TestProcurementQualityEvidenceRejectsPartiallyMatchingModel(t *testing.T) {
	plan, sample := procurementQualityFixture(t)
	request := service.ProfitPreviewRequestScope{AccountID: plan.AccountID, PublicModel: plan.ModelID, Scope: *sample.PelicanConfig.QualityMeasurement.Scope}
	// A full two-sample round must not become a one-sample green projection
	// when only one physical send used the configured model.
	sample.PelicanConfig.ParallelCount = 2
	raw, err := json.Marshal([]any{1, sample.PelicanConfig.Quality, sample.PelicanConfig.ModelIDs, 2})
	require.NoError(t, err)
	sample.PelicanConfig.QualityMeasurement.PolicyHash = fmt.Sprintf("%x", sha256.Sum256(raw))
	_, err = NewScheduledTestResultRepository(integrationDB).Create(t.Context(), sample)
	require.NoError(t, err)
	scope := *sample.PelicanConfig.QualityMeasurement.Scope
	scope.Model = "different-wire-model"
	measurement, probe := *sample.PelicanConfig.QualityMeasurement, *sample.PelicanConfig.AvailabilityResult
	measurement.Scope, probe.Scope = &scope, &scope
	sample.PelicanConfig.QualityMeasurement, sample.PelicanConfig.AvailabilityResult = &measurement, &probe
	saveProcurementQualityRound(t, plan, sample)
	preview, err := NewChannelRepository(integrationDB).(service.ProcurementCatalogRepository).PreviewProcurementClassification(t.Context(), &service.ProcurementPreviewCommand{Requests: []service.ProfitPreviewRequestScope{request}})
	require.NoError(t, err)
	require.Equal(t, "unknown", preview.Results[0].Quality.Status)
	require.Equal(t, 2, preview.Results[0].Quality.Samples)
	require.Equal(t, 1, preview.Results[0].Quality.ValidSamples)
}
