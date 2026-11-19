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

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestProcurementQualityAdmissionReadsCurrentSQL(t *testing.T) {
	plan, sample := procurementQualityFixture(t)
	saveProcurementQualityRound(t, plan, sample)
	accounts := NewAccountRepository(integrationEntClient, integrationDB, nil)
	selected, err := accounts.GetByID(t.Context(), plan.AccountID)
	require.NoError(t, err)
	reader := NewGroupRepository(integrationEntClient, integrationDB).(service.QualityScopeEvidenceReader)
	scope := *sample.PelicanConfig.QualityMeasurement.Scope
	policy := &service.ProcurementPolicy{Version: 4, Revision: 1, Sources: &domain.ProcurementSources{}, Buckets: &domain.ProcurementBuckets{}, Quality: "current_answer"}
	assertState := func(want string) {
		t.Helper()
		evidence, err := reader.ReadQualityScopeEvidence(t.Context(), selected, scope)
		require.NoError(t, err)
		out := service.ProcurementAnswerQuality(policy, evidence, evidence.EvaluatedAt)
		require.True(t, out.Required)
		require.Equal(t, want, out.State)
		require.Equal(t, "unknown", evidence.ModelIdentity)
	}
	assertState("pass")
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, plan.ID)
	require.NoError(t, err)
	assertState("unknown")
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE scheduled_test_plans SET enabled=true WHERE id=$1`, plan.ID)
	require.NoError(t, err)
	assertState("pass")
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE scheduled_test_plans SET availability_fault_version=availability_fault_version+1 WHERE id=$1`, plan.ID)
	require.NoError(t, err)
	assertState("unknown")
	// A previously selected object cannot apply its answer evidence to a new key.
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE accounts SET credentials=jsonb_set(credentials,'{api_key}','"changed-local-fixture"') WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	evidence, err := reader.ReadQualityScopeEvidence(t.Context(), selected, scope)
	require.Error(t, err)
	require.Nil(t, evidence)
	selected, err = accounts.GetByID(t.Context(), plan.AccountID)
	require.NoError(t, err)
	assertState("unknown")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	evidence, err = reader.ReadQualityScopeEvidence(ctx, selected, scope)
	require.Error(t, err)
	require.Nil(t, evidence)
	assertState("unknown")
	var samples int
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM scheduled_test_results WHERE plan_id=$1`, plan.ID).Scan(&samples))
	require.Equal(t, 1, samples, "admission is read-only and must never send a probe or create a sample")
}

func TestProcurementQualityAdmissionKeepsManagedConnectionIdentity(t *testing.T) {
	plan, sample := procurementQualityFixture(t, true)
	saveProcurementQualityRound(t, plan, sample)
	selected, err := NewAccountRepository(integrationEntClient, integrationDB, nil).GetByID(t.Context(), plan.AccountID)
	require.NoError(t, err)
	reader := NewGroupRepository(integrationEntClient, integrationDB).(service.QualityScopeEvidenceReader)
	scope := *sample.PelicanConfig.QualityMeasurement.Scope
	_, err = reader.ReadQualityScopeEvidence(t.Context(), selected, scope)
	require.Error(t, err, "unprepared parent has no confirmed connection")
	parent := service.AccountRouteFingerprint(selected)
	selected.AuthConnectionParentRoute = &parent
	selected.AuthConnectionID = sample.QualityRouteEvidence.ConnectionID
	selected.AuthConnectionRevision = sample.QualityRouteEvidence.ConnectionRevision
	read := func(want string) {
		t.Helper()
		evidence, readErr := reader.ReadQualityScopeEvidence(t.Context(), selected, scope)
		require.NoError(t, readErr)
		require.Equal(t, want, evidence.Status)
	}
	read("pass")
	selected.AuthConnectionID = "another-local-connection"
	read("unknown")
	selected.AuthConnectionID = sample.QualityRouteEvidence.ConnectionID
	selected.AuthConnectionRevision++
	read("unknown")
	selected.AuthConnectionRevision--
	read("pass")
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE openai_auth_connections SET revision=revision+1 WHERE id=$1`, selected.AuthConnectionID)
	require.NoError(t, err)
	read("unknown")
}

func procurementQualityFixture(t *testing.T, managed ...bool) (*service.ScheduledTestPlan, *service.ScheduledTestResult) {
	t.Helper()
	plan, sample := qualityRouteFixture(t, len(managed) > 0 && managed[0])
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

func TestProcurementAvailabilityPreviewSQLSameSnapshot(t *testing.T) {
	plan, sample := qualityRouteFixture(t, false)
	plans := NewScheduledTestPlanRepository(integrationDB)
	plan.PelicanConfig.Availability = &service.AvailabilityPolicy{ValidForSeconds: 60}
	next := time.Now().UTC().Add(-time.Second)
	plan.NextRunAt = &next
	var err error
	plan, err = plans.Update(t.Context(), plan)
	require.NoError(t, err)
	scope := *sample.PelicanConfig.QualityMeasurement.Scope
	scope.ServiceTier, scope.ReasoningEffort = "unspecified", "unspecified"
	command := &service.ProcurementPreviewCommand{Requests: []service.ProfitPreviewRequestScope{{AccountID: plan.AccountID, PublicModel: plan.ModelID, Scope: scope}}}
	policy := &service.ProcurementPolicy{Version: 5, Revision: 1, Sources: &domain.ProcurementSources{}, Buckets: &domain.ProcurementBuckets{}, Quality: "not_required", Availability: "current_state"}
	product, err := integrationEntClient.Group.Create().SetName("latest-availability-preview").SetPlatform("openai").SetRateMultiplier(.15).SetProfitControlEnabled(true).SetProcurementPolicy(policy).Save(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM groups WHERE id=$1`, product.ID)
	})
	command.ProductGroupID = &product.ID
	repo := NewChannelRepository(integrationDB).(service.ProcurementCatalogRepository)
	publish := func(status string) {
		t.Helper()
		plan, err = plans.GetByID(t.Context(), plan.ID)
		require.NoError(t, err)
		require.NotNil(t, plan.NextRunAt)
		now := time.Now().UTC().Truncate(time.Microsecond)
		until := now.Add(time.Minute)
		claimed, claimErr := plans.ClaimPelican(t.Context(), plan, now, until, *plan.NextRunAt)
		require.NoError(t, claimErr)
		require.True(t, claimed)
		var finished time.Time
		require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT clock_timestamp()-interval '1 second'`).Scan(&finished))
		probe := &service.AvailabilityProbeResult{Version: 1, Status: status, StartedAt: finished.Add(-time.Second), FinishedAt: finished, Scope: &scope}
		action, writeErr := plans.ApplyAvailabilityOutcomes(t.Context(), plan, until, []service.AvailabilityModelOutcome{{ModelID: plan.ModelID, Probe: probe}})
		require.NoError(t, writeErr)
		require.Equal(t, "observed", action)
		require.NoError(t, plans.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	}
	assertState := func(want string) {
		t.Helper()
		result, readErr := repo.PreviewProcurementClassification(t.Context(), command)
		require.NoError(t, readErr)
		require.Len(t, result.Results, 1)
		row := result.Results[0]
		require.Equal(t, want, row.Availability.Status)
		require.Equal(t, result.ObservedAt, row.Availability.EvaluatedAt)
		require.Equal(t, want, row.Product.Availability.State)
		require.Equal(t, "not_required", row.Product.AnswerQuality.State)
		require.Equal(t, "unknown", row.Quality.ModelIdentity)
		require.Equal(t, "not_evaluated", row.Qualification)
		raw, encodeErr := json.Marshal(result)
		require.NoError(t, encodeErr)
		for _, private := range []string{"private-route-fixture", "route.invalid", "connection_id", "fingerprint", "response_text"} {
			require.NotContains(t, string(raw), private)
		}
	}
	assertState("unknown")
	publish("pass")
	assertState("pass")
	publish("fail")
	assertState("fail")
	publish("pass")
	assertState("pass")
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, plan.ID)
	require.NoError(t, err)
	assertState("unknown")
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE scheduled_test_plans SET enabled=true WHERE id=$1`, plan.ID)
	require.NoError(t, err)
	assertState("pass")
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE scheduled_test_plans SET availability_fault_version=availability_fault_version+1 WHERE id=$1`, plan.ID)
	require.NoError(t, err)
	assertState("unknown")
	publish("pass")
	assertState("pass")
	// Multiple request scopes share one authoritative account read, never a
	// duplicated sample or an automatically issued paid detection.
	original := command.Requests[0]
	for len(command.Requests) < 100 {
		command.Requests = append(command.Requests, original)
	}
	preview, err := repo.PreviewProcurementClassification(t.Context(), command)
	require.NoError(t, err)
	require.Len(t, preview.Results, 100)
	for _, row := range preview.Results {
		require.Equal(t, "pass", row.Availability.Status)
		require.Equal(t, preview.ObservedAt, row.Availability.EvaluatedAt)
	}
	var count int
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM scheduled_test_results WHERE plan_id=$1`, plan.ID).Scan(&count))
	require.Zero(t, count)
}
