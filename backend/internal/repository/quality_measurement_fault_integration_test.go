//go:build integration

package repository

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func claimQualityFaultSample(t *testing.T, plan *service.ScheduledTestPlan, original *service.ScheduledTestResult) (*service.ScheduledTestPlan, *service.ScheduledTestResult) {
	t.Helper()
	cfg := *plan.PelicanConfig
	policy := *cfg.Quality
	policy.EvidenceValidForSeconds = 60
	cfg.Quality = &policy
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := integrationDB.ExecContext(t.Context(), `UPDATE scheduled_test_plans SET pelican_config=$2,next_run_at=$3 WHERE id=$1`, plan.ID, marshalPelicanConfig(&cfg), now.Add(-time.Minute))
	require.NoError(t, err)
	plans := NewScheduledTestPlanRepository(integrationDB)
	plan, err = plans.GetByID(t.Context(), plan.ID)
	require.NoError(t, err)
	claimed, err := plans.ClaimPelican(t.Context(), plan, now, now.Add(15*time.Minute), now.Add(5*time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	require.NotNil(t, plan.QualityFaultVersion)
	require.NotEmpty(t, plan.QualityFaultRound)
	out, frozen := *original, *original.PelicanConfig
	measurement, probe, evidence := *frozen.QualityMeasurement, *frozen.AvailabilityResult, *original.QualityRouteEvidence
	observed := time.Now().UTC().Truncate(time.Microsecond)
	probe.StartedAt, probe.FinishedAt = observed, observed
	measurement.RuleVersion, measurement.ObservedAt, measurement.CompletedAt = plan.UpdatedAt, observed, observed
	frozen.Quality, frozen.QualityMeasurement, frozen.AvailabilityResult = &policy, &measurement, &probe
	evidence.ObservedAt = observed
	version := *plan.QualityFaultVersion
	evidence.FaultVersion, evidence.FaultRound = &version, plan.QualityFaultRound
	out.StartedAt, out.FinishedAt, out.QualityRoundID = observed, observed, plan.QualityFaultRound
	out.PelicanConfig, out.QualityRouteEvidence = &frozen, &evidence
	return plan, &out
}

func TestQualityMeasurementFaultOnlyPolicyRecordsBusinessFault(t *testing.T) {
	for _, status := range []string{"business-error", "upstream-5xx"} {
		t.Run(status, func(t *testing.T) {
			plan, _ := qualityRouteFixture(t, false)
			plan.PelicanConfig.Quality.EvidenceValidForSeconds = 60
			next := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
			_, err := integrationDB.ExecContext(t.Context(), `UPDATE scheduled_test_plans SET pelican_config=$2,next_run_at=$3 WHERE id=$1`, plan.ID, marshalPelicanConfig(plan.PelicanConfig), next)
			require.NoError(t, err)
			accounts := &accountRepository{sql: integrationDB}
			if status == "upstream-5xx" {
				err = accounts.ApplyQuality5xx(t.Context(), plan.AccountID)
			} else {
				err = accounts.ApplyAvailabilityFault(t.Context(), plan.AccountID)
			}
			require.NoError(t, err)
			var version int64
			var after time.Time
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT availability_fault_version,next_run_at FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&version, &after))
			require.EqualValues(t, 1, version, "a quality-only evidence policy must register the observed business fault")
			require.Equal(t, next, after, "tracking a fault must not add paid probes or change the configured test cycle")
			var enabled bool
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT schedulable FROM accounts WHERE id=$1`, plan.AccountID).Scan(&enabled))
			require.True(t, enabled, "recording evidence is independent of account isolation")
		})
	}
}

func TestQualityMeasurementFaultCurrentness(t *testing.T) {
	for _, scenario := range []string{"unchanged", "business-error", "upstream-5xx", "before-save", "legacy", "negative", "future-counter", "missing-round", "wrong-round", "paused", "rule-changed", "undeclared", "route-changed", "missing-route", "other-account", "both-policies", "managed", "managed-stale-parent"} {
		t.Run(scenario, func(t *testing.T) {
			plan, sample := qualityRouteFixture(t, scenario == "managed" || scenario == "managed-stale-parent")
			plan, sample = claimQualityFaultSample(t, plan, sample)
			accounts := &accountRepository{sql: integrationDB}
			if scenario == "before-save" {
				require.NoError(t, accounts.ApplyAvailabilityFault(t.Context(), plan.AccountID))
			}
			results := NewScheduledTestResultRepository(integrationDB)
			created, err := results.Create(t.Context(), sample)
			require.NoError(t, err)
			want, reason := "unchanged", "no_new_registered_fault"
			exec := func(query string, args ...any) {
				_, queryErr := integrationDB.ExecContext(t.Context(), query, args...)
				require.NoError(t, queryErr)
			}
			switch scenario {
			case "business-error", "upstream-5xx", "both-policies":
				if scenario == "both-policies" {
					exec(`UPDATE scheduled_test_plans SET pelican_config=jsonb_set(pelican_config,'{availability}','{"valid_for_seconds":60}') WHERE id=$1`, plan.ID)
				}
				if scenario == "upstream-5xx" {
					err = accounts.ApplyQuality5xx(t.Context(), plan.AccountID)
				} else {
					err = accounts.ApplyAvailabilityFault(t.Context(), plan.AccountID)
				}
				require.NoError(t, err)
				var version int64
				require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT availability_fault_version FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&version))
				require.EqualValues(t, 1, version, "the shared counter advances once even when both evidence policies are present")
				want, reason = "invalidated", "business_fault_recorded"
			case "before-save":
				want, reason = "invalidated", "business_fault_recorded"
			case "legacy", "negative", "future-counter", "missing-round", "wrong-round":
				evidence := *sample.QualityRouteEvidence
				switch scenario {
				case "legacy":
					evidence.FaultVersion, evidence.FaultRound = nil, ""
				case "negative", "future-counter":
					version := int64(-1)
					if scenario == "future-counter" {
						version = 1
					}
					evidence.FaultVersion = &version
				case "missing-round":
					evidence.FaultRound = ""
				case "wrong-round":
					evidence.FaultRound = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
				}
				raw, marshalErr := json.Marshal(evidence)
				require.NoError(t, marshalErr)
				exec(`UPDATE scheduled_test_results SET pelican_config=jsonb_set(pelican_config,'{_quality_observed_route}',$2::jsonb) WHERE id=$1`, created.ID, string(raw))
				want, reason = "unknown", "invalid_fault_evidence"
				if scenario == "legacy" {
					reason = "fault_not_tracked"
				}
			case "paused", "rule-changed", "undeclared":
				if scenario == "paused" {
					exec(`UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, plan.ID)
				} else if scenario == "rule-changed" {
					exec(`UPDATE scheduled_test_plans SET updated_at=updated_at+interval '1 second' WHERE id=$1`, plan.ID)
				} else {
					exec(`UPDATE scheduled_test_results SET pelican_config=jsonb_set(pelican_config,'{quality,evidence_valid_for_seconds}','0') WHERE id=$1`, created.ID)
				}
				want, reason = "unknown", "fault_tracking_not_current"
			case "route-changed":
				exec(`UPDATE accounts SET credentials='{"api_key":"changed"}' WHERE id=$1`, plan.AccountID)
				want, reason = "unknown", "route_not_current"
			case "missing-route":
				exec(`UPDATE scheduled_test_results SET pelican_config=pelican_config-'_quality_observed_route' WHERE id=$1`, created.ID)
				want, reason = "unknown", "fault_not_tracked"
			case "other-account":
				other, otherSample := qualityRouteFixture(t, false)
				other, _ = claimQualityFaultSample(t, other, otherSample)
				require.NoError(t, accounts.ApplyAvailabilityFault(t.Context(), other.AccountID))
			case "managed", "managed-stale-parent":
				var parent [32]byte
				raw, decodeErr := hex.DecodeString(sample.QualityRouteEvidence.Fingerprint)
				require.NoError(t, decodeErr)
				copy(parent[:], raw)
				if scenario == "managed-stale-parent" {
					parent[0] ^= 1
				}
				prepared := &service.Account{ID: plan.AccountID, Type: service.AccountTypeAPIKey,
					AuthConnectionID: sample.QualityRouteEvidence.ConnectionID, AuthConnectionRevision: 7, AuthConnectionParentRoute: &parent}
				require.NoError(t, accounts.ApplyManagedAvailabilityFault(t.Context(), prepared, false))
				if scenario == "managed" {
					want, reason = "invalidated", "business_fault_recorded"
				}
			}
			read, err := NewScheduledTestResultRepository(integrationDB).GetResult(t.Context(), plan.ID, created.ID)
			require.NoError(t, err)
			require.Equal(t, &service.QualityMeasurementFaultValidity{Status: want, Reason: reason}, read.QualityMeasurementValidity.Fault)
			require.Equal(t, "failed", read.Status, "fault evidence never rewrites the original wrong answer")
			raw, err := json.Marshal(read)
			require.NoError(t, err)
			for _, private := range []string{"fault_version", "fault_round", "QualityFaultVersion", "_quality_observed_route"} {
				require.NotContains(t, string(raw), private)
			}
		})
	}
}

func TestQualityMeasurementFaultLateResultAndRecovery(t *testing.T) {
	plans := NewScheduledTestPlanRepository(integrationDB)
	plan, sample := qualityRouteFixture(t, false)
	plan, old := claimQualityFaultSample(t, plan, sample)
	accounts := &accountRepository{sql: integrationDB}
	require.NoError(t, accounts.ApplyAvailabilityFault(t.Context(), plan.AccountID))
	lease, err := time.Parse(time.RFC3339Nano, plan.QualityFaultRound)
	require.NoError(t, err)
	require.NoError(t, plans.FinishPelican(t.Context(), plan.ID, lease, time.Now()))
	plan, fresh := claimQualityFaultSample(t, plan, sample)
	require.EqualValues(t, 1, *plan.QualityFaultVersion)
	results := NewScheduledTestResultRepository(integrationDB)
	newResult, err := results.Create(t.Context(), fresh)
	require.NoError(t, err)
	late, err := results.Create(t.Context(), old)
	require.NoError(t, err)
	for _, check := range []struct {
		id     int64
		status string
	}{{newResult.ID, "unchanged"}, {late.ID, "invalidated"}} {
		read, readErr := NewScheduledTestResultRepository(integrationDB).GetResult(t.Context(), plan.ID, check.id)
		require.NoError(t, readErr)
		require.Equal(t, check.status, read.QualityMeasurementValidity.Fault.Status, "late insertion cannot replace a frozen claim version")
	}
	// A failed second claim cannot retain a caller's old execution fence.
	claimed, err := plans.ClaimPelican(t.Context(), plan, time.Now(), time.Now().Add(15*time.Minute), time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.False(t, claimed)
	require.Nil(t, plan.QualityFaultVersion)
	require.Empty(t, plan.QualityFaultRound)
}

func TestQualityMeasurementFaultConcurrentRegistrationAndRollback(t *testing.T) {
	plan, sample := qualityRouteFixture(t, false)
	plan, sample = claimQualityFaultSample(t, plan, sample)
	accounts := &accountRepository{sql: integrationDB}
	const count = 8
	errors := make(chan error, count)
	for range count {
		go func() { errors <- accounts.ApplyAvailabilityFault(t.Context(), plan.AccountID) }()
	}
	for range count {
		require.NoError(t, <-errors)
	}
	var version int64
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT availability_fault_version FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&version))
	require.EqualValues(t, count, version, "plan locks must preserve concurrent fault increments")
	_, err := integrationDB.ExecContext(t.Context(), `CREATE FUNCTION reject_quality_fault_fixture() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'quality fault fixture write rejected'; END $$;
 CREATE TRIGGER reject_quality_fault_fixture BEFORE UPDATE OF availability_fault_version ON scheduled_test_plans FOR EACH ROW EXECUTE FUNCTION reject_quality_fault_fixture()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS reject_quality_fault_fixture ON scheduled_test_plans; DROP FUNCTION IF EXISTS reject_quality_fault_fixture()`)
	})
	require.ErrorContains(t, accounts.ApplyAvailabilityFault(t.Context(), plan.AccountID), "quality fault fixture write rejected")
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT availability_fault_version FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&version))
	require.EqualValues(t, count, version, "a failed fence transaction must not partially advance the counter")
	_, err = integrationDB.ExecContext(t.Context(), `DROP TRIGGER reject_quality_fault_fixture ON scheduled_test_plans; DROP FUNCTION reject_quality_fault_fixture()`)
	require.NoError(t, err)
	require.NoError(t, accounts.ApplyAvailabilityFault(t.Context(), plan.AccountID))
	created, err := NewScheduledTestResultRepository(integrationDB).Create(t.Context(), sample)
	require.NoError(t, err)
	read, err := NewScheduledTestResultRepository(integrationDB).GetResult(t.Context(), plan.ID, created.ID)
	require.NoError(t, err)
	require.Equal(t, "invalidated", read.QualityMeasurementValidity.Fault.Status)
}

func TestQualityMeasurementFaultSharedBatchRestart(t *testing.T) {
	plans := NewScheduledTestPlanRepository(integrationDB).(*scheduledTestPlanRepository)
	plan, sample := qualityRouteFixture(t, false)
	plan.PelicanConfig.QuestionKind = service.QualityRandomQuestionKind
	plan, sample = claimQualityFaultSample(t, plan, sample)
	now := time.Now().UTC().Truncate(time.Microsecond)
	batch := &service.QualitySharedBatch{ID: "fault-" + uuid.NewString(), StartedAt: now.Add(-time.Minute),
		StartDeadline: now.Add(time.Minute), ClosesAt: now.Add(10 * time.Minute),
		Questions: []service.QualityBatchQuestion{{Prompt: "Return 42", Expected: "42",
			Question: &service.QualityRandomQuestion{ID: "fault-fixture", Family: "paths", Version: "fixture"}}}}
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM quality_probe_batches WHERE id=$1`, batch.ID)
	})
	lease, err := time.Parse(time.RFC3339Nano, plan.QualityFaultRound)
	require.NoError(t, err)
	_, err = plans.StartQualityBatch(t.Context(), batch, plan, lease)
	require.NoError(t, err)
	sample.PelicanConfig.QuestionKind = service.QualityRandomQuestionKind
	sample.PelicanConfig.QualityMeasurement.QuestionKind = service.QualityRandomQuestionKind
	sample.PelicanConfig.RandomQuestion = batch.Questions[0].Question
	sample.PelicanConfig.QualityBatch = &service.QualityBatchEvidence{ID: batch.ID, StartedAt: batch.StartedAt,
		StartDeadline: batch.StartDeadline, ClosesAt: batch.ClosesAt, Decision: "pending", Route: "fixture", MappedModel: plan.ModelID}
	sample.QualityAction = "comparison_pending"
	run := &service.QualityBatchRun{Plan: plan, Until: lease, Results: []*service.ScheduledTestResult{sample}}
	require.NoError(t, plans.CollectQualityBatch(t.Context(), run))
	require.NoError(t, plans.CollectQualityBatch(t.Context(), run), "collecting twice must not replace the frozen fault version")
	require.NoError(t, (&accountRepository{sql: integrationDB}).ApplyAvailabilityFault(t.Context(), plan.AccountID))
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE quality_probe_batches SET start_deadline=clock_timestamp()-interval '1 second' WHERE id=$1`, batch.ID)
	require.NoError(t, err)
	restarted := NewScheduledTestPlanRepository(integrationDB).(*scheduledTestPlanRepository)
	require.NoError(t, restarted.FinalizeQualityBatches(t.Context()))
	require.NoError(t, restarted.FinalizeQualityBatches(t.Context()), "finalizing twice must not backfill a newer fault version")
	var raw []byte
	var resultID int64
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT id,pelican_config->'_quality_observed_route'
 FROM scheduled_test_results WHERE plan_id=$1`, plan.ID).Scan(&resultID, &raw))
	var frozen service.QualityRouteEvidence
	require.NoError(t, json.Unmarshal(raw, &frozen))
	require.NotNil(t, frozen.FaultVersion)
	require.Zero(t, *frozen.FaultVersion)
	require.Equal(t, plan.QualityFaultRound, frozen.FaultRound)
	read, err := NewScheduledTestResultRepository(integrationDB).GetResult(t.Context(), plan.ID, resultID)
	require.NoError(t, err)
	require.Equal(t, "invalidated", read.QualityMeasurementValidity.Fault.Status)
	require.Equal(t, "failed", read.Status)
}
