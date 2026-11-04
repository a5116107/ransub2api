//go:build unit

package repository

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestAvailabilityReadPostgresFreshnessAndScope(t *testing.T) {
	for _, change := range []string{"none", "paused", "rule", "route", "event", "unrelated", "expired", "removed_policy", "missing"} {
		t.Run(change, func(t *testing.T) {
			repo, plan := availabilityPostgresPlan(t)
			until := availabilityPostgresClaim(t, repo, plan)
			probe := availabilityPostgresObservation("pass")
			if change == "expired" {
				probe[0].Probe.StartedAt = time.Now().Add(-2 * time.Minute)
				probe[0].Probe.FinishedAt = time.Now().Add(-time.Minute - time.Second)
			}
			if change != "missing" {
				_, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, probe)
				require.NoError(t, err)
			}
			queries := map[string]string{
				"paused":         `UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`,
				"rule":           `UPDATE scheduled_test_plans SET updated_at=updated_at+interval '1 second' WHERE id=$1`,
				"route":          `UPDATE accounts SET credentials='{"api_key":"fixture-secret-not-in-report"}' WHERE id=(SELECT account_id FROM scheduled_test_plans WHERE id=$1)`,
				"event":          `UPDATE scheduled_test_plans SET availability_fault_version=availability_fault_version+1 WHERE id=$1`,
				"unrelated":      `UPDATE accounts SET concurrency=29,updated_at=now(),extra=extra||'{"usage":123}' WHERE id=(SELECT account_id FROM scheduled_test_plans WHERE id=$1)`,
				"removed_policy": `UPDATE scheduled_test_plans SET pelican_config=pelican_config-'availability' WHERE id=$1`,
			}
			if query := queries[change]; query != "" {
				_, err := repo.db.Exec(query, plan.ID)
				require.NoError(t, err)
			}
			report, err := repo.ReadAvailability(t.Context(), plan.ID)
			require.NoError(t, err)
			require.Equal(t, plan.AccountID, report.AccountID)
			require.False(t, report.EvaluatedAt.IsZero())
			if change == "removed_policy" {
				require.False(t, report.Configured)
				require.Empty(t, report.Models)
				return
			}
			require.True(t, report.Configured)
			require.Len(t, report.Models, 1)
			wantStatus, wantReason := "unknown", "not_observed"
			switch change {
			case "none", "unrelated":
				wantStatus, wantReason = "pass", "latest_decision"
			case "paused":
				wantReason = "rule_paused"
			case "rule":
				wantReason = "rule_changed"
			case "route":
				wantReason = "route_changed"
			case "event":
				wantReason = "new_fault_event"
			case "expired":
				wantStatus, wantReason = "stale", "expired"
			}
			require.Equal(t, wantStatus, report.Models[0].Status)
			require.Equal(t, wantReason, report.Models[0].Reason)
			raw, err := json.Marshal(report)
			require.NoError(t, err)
			for _, hidden := range []string{"route_fingerprint", "event_version", "fixture-secret-not-in-report", "credentials", "api_key"} {
				require.NotContains(t, string(raw), hidden)
			}
		})
	}
}

func TestAvailabilityReadPostgresNewEventInconclusiveCannotRevalidatePass(t *testing.T) {
	repo, plan := availabilityPostgresPlan(t)
	until := availabilityPostgresClaim(t, repo, plan)
	_, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
	require.NoError(t, err)
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	_, err = repo.db.Exec(`UPDATE scheduled_test_plans SET availability_fault_version=availability_fault_version+1 WHERE id=$1`, plan.ID)
	require.NoError(t, err)
	until = availabilityPostgresClaim(t, repo, plan)
	_, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("inconclusive"))
	require.NoError(t, err)
	report, err := repo.ReadAvailability(t.Context(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, "unknown", report.Models[0].Status, "an inconclusive new episode must not revalidate the pre-fault pass")
	require.Nil(t, report.Models[0].Decision)
}

func TestAvailabilityReadPostgresRejectsMalformedDecision(t *testing.T) {
	repo, plan := availabilityPostgresPlan(t)
	until := availabilityPostgresClaim(t, repo, plan)
	_, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
	require.NoError(t, err)
	_, err = repo.db.Exec(`UPDATE account_availability_states SET state=jsonb_set(state,'{decision,version}','99') WHERE plan_id=$1`, plan.ID)
	require.NoError(t, err)
	report, err := repo.ReadAvailability(t.Context(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, "unknown", report.Models[0].Status)
	require.Equal(t, "invalid_evidence", report.Models[0].Reason)
}

func TestAvailabilityReadPostgresFaultVersionSurvivesQualityOwnershipReset(t *testing.T) {
	repo, plan := availabilityPostgresPlan(t)
	until := availabilityPostgresClaim(t, repo, plan)
	_, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
	require.NoError(t, err)
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	for i := 0; i < 2; i++ {
		changed, err := repo.applyImmediateQuality5xx(t.Context(), plan.ID, plan.AccountID)
		require.NoError(t, err)
		require.True(t, changed)
	}
	var version int64
	require.NoError(t, repo.db.QueryRow(`SELECT availability_fault_version FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&version))
	require.EqualValues(t, 2, version)
	_, err = repo.db.Exec(`DELETE FROM account_quality_states WHERE plan_id=$1`, plan.ID)
	require.NoError(t, err)
	report, err := repo.ReadAvailability(t.Context(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, "unknown", report.Models[0].Status)
	require.Equal(t, "new_fault_event", report.Models[0].Reason, "quality counter reset must not revalidate a pre-fault pass")
	plan, err = repo.GetByID(t.Context(), plan.ID)
	require.NoError(t, err)
	until = availabilityPostgresClaim(t, repo, plan)
	require.EqualValues(t, 2, plan.AvailabilityFaultVersion)
	_, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("inconclusive"))
	require.NoError(t, err)
	report, err = repo.ReadAvailability(t.Context(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, "unknown", report.Models[0].Status)
	require.Nil(t, report.Models[0].Decision)
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	until = availabilityPostgresClaim(t, repo, plan)
	_, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
	require.NoError(t, err)
	report, err = repo.ReadAvailability(t.Context(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, "pass", report.Models[0].Status, "only the new completed probe restores availability")
}

func TestAvailabilityReadPostgresIndependentFaultRejectsOldRun(t *testing.T) {
	repo, plan := availabilityPostgresPlan(t)
	until := availabilityPostgresClaim(t, repo, plan)
	changed, err := repo.applyImmediateQuality5xx(t.Context(), plan.ID, plan.AccountID)
	require.NoError(t, err)
	require.True(t, changed)
	_, err = repo.db.Exec(`DELETE FROM account_quality_states WHERE plan_id=$1`, plan.ID)
	require.NoError(t, err)
	action, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
	require.NoError(t, err)
	require.Equal(t, "stale_run", action, "the independent fault version must survive a reset of quality ownership")
}

func TestAvailabilityReadPostgresQualityRestoreKeepsNewAvailabilityPass(t *testing.T) {
	repo, plan := availabilityPostgresPlan(t)
	changed, err := repo.applyImmediateQuality5xx(t.Context(), plan.ID, plan.AccountID)
	require.NoError(t, err)
	require.True(t, changed)
	plan, err = repo.GetByID(t.Context(), plan.ID)
	require.NoError(t, err)
	until := availabilityPostgresClaim(t, repo, plan)
	_, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
	require.NoError(t, err)
	action, err := repo.ApplyQualityOutcome(t.Context(), plan, until, "passed")
	require.NoError(t, err)
	require.Equal(t, "restored", action)
	var states int
	require.NoError(t, repo.db.QueryRow(`SELECT count(*) FROM account_quality_states WHERE plan_id=$1`, plan.ID).Scan(&states))
	require.Zero(t, states)
	report, err := repo.ReadAvailability(t.Context(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, "pass", report.Models[0].Status, "releasing quality ownership must not invalidate a newly verified availability pass")
}

func TestAvailabilityReadPostgresProxyAndParentIdentity(t *testing.T) {
	for _, target := range []string{"proxy", "parent"} {
		t.Run(target, func(t *testing.T) {
			repo, plan := availabilityPostgresPlan(t)
			var relatedID int64
			if target == "proxy" {
				_, err := repo.db.Exec(`INSERT INTO proxies(id,protocol,host,port) VALUES(99,'http','127.0.0.1',9876)`)
				require.NoError(t, err)
				_, err = repo.db.Exec(`UPDATE accounts SET proxy_id=99 WHERE id=$1`, plan.AccountID)
				require.NoError(t, err)
			} else {
				require.NoError(t, repo.db.QueryRow(`INSERT INTO accounts(type,credentials) VALUES('apikey','{"api_key":"fixture-parent-original"}') RETURNING id`).Scan(&relatedID))
				_, err := repo.db.Exec(`UPDATE accounts SET parent_account_id=$2 WHERE id=$1`, plan.AccountID, relatedID)
				require.NoError(t, err)
			}
			until := availabilityPostgresClaim(t, repo, plan)
			_, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
			require.NoError(t, err)
			report, err := repo.ReadAvailability(t.Context(), plan.ID)
			require.NoError(t, err, "read-only fingerprint must not acquire FOR UPDATE/SHARE locks")
			require.Equal(t, "pass", report.Models[0].Status)
			if target == "proxy" {
				_, err = repo.db.Exec(`UPDATE proxies SET port=9877 WHERE id=99`)
			} else {
				_, err = repo.db.Exec(`UPDATE accounts SET credentials='{"api_key":"fixture-parent-changed"}' WHERE id=$1`, relatedID)
			}
			require.NoError(t, err)
			report, err = repo.ReadAvailability(t.Context(), plan.ID)
			require.NoError(t, err)
			require.Equal(t, "unknown", report.Models[0].Status)
			require.Equal(t, "route_changed", report.Models[0].Reason)
		})
	}
}

func TestAvailabilityReadPostgresLegacyEvidenceAndFaultMigrationRollback(t *testing.T) {
	repo, plan := availabilityPostgresPlan(t)
	until := availabilityPostgresClaim(t, repo, plan)
	_, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
	require.NoError(t, err)
	_, err = repo.db.Exec(`UPDATE account_availability_states SET state=state-'fault_version' WHERE plan_id=$1`, plan.ID)
	require.NoError(t, err)
	report, err := repo.ReadAvailability(t.Context(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, "unknown", report.Models[0].Status)
	require.Equal(t, "evidence_version_missing", report.Models[0].Reason)
	// Dropping the monotonic counter requires discarding its derived evidence;
	// keeping old rows while recreating a zero counter would permit an ABA.
	_, err = repo.db.Exec(`DELETE FROM account_availability_states; ALTER TABLE scheduled_test_plans DROP COLUMN availability_fault_version`)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("326_availability_fault_version.sql")
	require.NoError(t, err)
	_, err = repo.db.Exec(string(migration))
	require.NoError(t, err)
	report, err = repo.ReadAvailability(t.Context(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, "unknown", report.Models[0].Status)
}
