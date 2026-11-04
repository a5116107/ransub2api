//go:build unit

package repository

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestAvailabilityReadPostgresIndependentBusinessFault(t *testing.T) {
	for _, quality := range []string{"absent", "disabled_trigger", "observe_only"} {
		t.Run(quality, func(t *testing.T) {
			repo, plan := availabilityPostgresPlan(t)
			switch quality {
			case "absent":
				plan.PelicanConfig.Quality = nil
			case "disabled_trigger":
				plan.PelicanConfig.Quality.TriggerOnUpstream5xx = false
			case "observe_only":
				plan.PelicanConfig.Quality.Action = service.QualityActionObserveOnly
			}
			var err error
			due := time.Now().Add(-time.Minute)
			plan.NextRunAt = &due
			plan, err = repo.Update(t.Context(), plan)
			require.NoError(t, err)
			until := time.Now().Add(time.Minute).Truncate(time.Microsecond)
			claimed, err := repo.ClaimPelican(t.Context(), plan, time.Now(), until, time.Now().Add(time.Hour))
			require.NoError(t, err)
			require.True(t, claimed)
			_, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
			require.NoError(t, err)
			accounts := &accountRepository{sql: repo.db}
			require.NoError(t, accounts.ApplyQuality5xx(t.Context(), plan.AccountID))
			report, err := repo.ReadAvailability(t.Context(), plan.ID)
			require.NoError(t, err)
			require.Equal(t, "new_fault_event", report.Models[0].Reason)
			require.Equal(t, "unknown", report.Models[0].Status)
			var version int64
			var running time.Time
			var next time.Time
			require.NoError(t, repo.db.QueryRow(`SELECT availability_fault_version,running_until,next_run_at FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&version, &running, &next))
			require.EqualValues(t, 1, version)
			require.True(t, running.Equal(until), "faults must not steal an active lease")
			require.False(t, next.After(time.Now()), "database fallback must schedule a new probe")
			action, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
			require.NoError(t, err)
			require.Equal(t, "stale_run", action)
			var owners, concurrency int
			require.NoError(t, repo.db.QueryRow(`SELECT count(*) FROM account_quality_states`).Scan(&owners))
			require.Zero(t, owners)
			require.NoError(t, repo.db.QueryRow(`SELECT concurrency FROM accounts WHERE id=$1`, plan.AccountID).Scan(&concurrency))
			require.Equal(t, 17, concurrency)
			require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
			plan, err = repo.GetByID(t.Context(), plan.ID)
			require.NoError(t, err)
			// A queued event must still claim a future timer even when an older
			// in-flight run finished after the signal but failed its version fence.
			future := time.Now().Add(time.Hour)
			_, err = repo.db.Exec(`UPDATE scheduled_test_plans SET next_run_at=$2 WHERE id=$1`, plan.ID, future)
			require.NoError(t, err)
			plan.NextRunAt = &future
			observed := time.Now().Add(-time.Second)
			plan.TriggerObservedAt = &observed
			until = availabilityPostgresClaim(t, repo, plan)
			require.EqualValues(t, 1, plan.AvailabilityFaultVersion)
			_, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
			require.NoError(t, err)
			report, err = repo.ReadAvailability(t.Context(), plan.ID)
			require.NoError(t, err)
			require.Equal(t, "pass", report.Models[0].Status)
		})
	}
}

func TestAvailabilityReadPostgresFaultPreservesManualAndQualityOwnership(t *testing.T) {
	for _, change := range []string{"manual_status", "manual_unscheduled", "own_quality_action", "other_quality_owner", "paused_plan", "removed_policy", "deleted_account"} {
		t.Run(change, func(t *testing.T) {
			repo, plan := availabilityPostgresPlan(t)
			if change == "manual_unscheduled" {
				plan.PelicanConfig.Quality = nil
				var err error
				plan, err = repo.Update(t.Context(), plan)
				require.NoError(t, err)
			}
			until := availabilityPostgresClaim(t, repo, plan)
			_, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
			require.NoError(t, err)
			queries := map[string]string{
				"manual_status":      `UPDATE accounts SET status='disabled' WHERE id=$1`,
				"manual_unscheduled": `UPDATE accounts SET schedulable=false WHERE id=$1`,
				"deleted_account":    `UPDATE accounts SET deleted_at=now() WHERE id=$1`,
			}
			if query := queries[change]; query != "" {
				_, err = repo.db.Exec(query, plan.AccountID)
				require.NoError(t, err)
			}
			switch change {
			case "own_quality_action":
				_, err = repo.db.Exec(`INSERT INTO account_quality_states(plan_id,state) VALUES($1,'{"action":"disable_scheduling","manual":"kept"}')`, plan.ID)
			case "other_quality_owner":
				_, err = repo.db.Exec(`WITH peer AS (INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,max_results,auto_recover,created_at,updated_at) VALUES($1,'peer','0 * * * *',false,10,false,now(),now()) RETURNING id) INSERT INTO account_quality_states(plan_id,state) SELECT id,'{"action":"disable_scheduling","manual":"kept"}' FROM peer`, plan.AccountID)
			case "paused_plan":
				_, err = repo.db.Exec(`UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, plan.ID)
			case "removed_policy":
				_, err = repo.db.Exec(`UPDATE scheduled_test_plans SET pelican_config='{}' WHERE id=$1`, plan.ID)
			}
			require.NoError(t, err)
			var before, after string
			require.NoError(t, repo.db.QueryRow(`SELECT jsonb_build_array(status,schedulable,concurrency,extra)::text FROM accounts WHERE id=$1`, plan.AccountID).Scan(&before))
			var ownerBefore, ownerAfter string
			require.NoError(t, repo.db.QueryRow(`SELECT COALESCE(jsonb_agg(state),'[]')::text FROM account_quality_states`).Scan(&ownerBefore))
			changed, err := repo.applyImmediateQuality5xx(t.Context(), plan.ID, plan.AccountID)
			require.NoError(t, err)
			want := change != "paused_plan" && change != "removed_policy" && change != "deleted_account"
			require.Equal(t, want, changed)
			var version int64
			require.NoError(t, repo.db.QueryRow(`SELECT availability_fault_version FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&version))
			if want {
				require.EqualValues(t, 1, version)
			} else {
				require.Zero(t, version)
			}
			require.NoError(t, repo.db.QueryRow(`SELECT jsonb_build_array(status,schedulable,concurrency,extra)::text FROM accounts WHERE id=$1`, plan.AccountID).Scan(&after))
			require.Equal(t, before, after)
			require.NoError(t, repo.db.QueryRow(`SELECT COALESCE(jsonb_agg(state),'[]')::text FROM account_quality_states`).Scan(&ownerAfter))
			require.Equal(t, ownerBefore, ownerAfter)
		})
	}
}

func TestAvailabilityReadPostgresBusinessFaultDatabaseFallback(t *testing.T) {
	repo, plan := availabilityPostgresPlan(t)
	plan.PelicanConfig.Quality = nil
	var err error
	plan, err = repo.Update(t.Context(), plan)
	require.NoError(t, err)
	accounts := &accountRepository{sql: repo.db}
	require.NoError(t, accounts.ApplyQuality5xx(t.Context(), plan.AccountID))
	// No Redis and no event source: the normal scheduled worker must discover
	// the durable due time and capture the independent fault version.
	due, err := repo.ListDue(t.Context(), time.Now())
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Empty(t, due[0].TriggerSource)
	until := time.Now().Add(time.Minute).Truncate(time.Microsecond)
	claimed, err := repo.ClaimPelican(t.Context(), due[0], time.Now(), until, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	require.EqualValues(t, 1, due[0].AvailabilityFaultVersion)
	action, err := repo.ApplyAvailabilityOutcomes(t.Context(), due[0], until, availabilityPostgresObservation("pass"))
	require.NoError(t, err)
	require.Equal(t, "observed", action)
	report, err := repo.ReadAvailability(t.Context(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, "pass", report.Models[0].Status)
}

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
