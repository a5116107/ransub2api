//go:build unit

package repository

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func availabilityPostgresPlan(t *testing.T) (*scheduledTestPlanRepository, *service.ScheduledTestPlan) {
	t.Helper()
	db := quality5xxPostgresDB(t)
	sql, err := migrations.FS.ReadFile("325_account_availability_states.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = db.Exec(string(sql))
		require.NoError(t, err, "migration must support idempotent replay")
	}
	repo, plan := quality5xxPostgresPlan(t, db, service.AccountTypeAPIKey)
	plan.PelicanConfig.Availability = &service.AvailabilityPolicy{ValidForSeconds: 60}
	plan, err = repo.Update(t.Context(), plan)
	require.NoError(t, err)
	return repo, plan
}

func availabilityPostgresClaim(t *testing.T, repo *scheduledTestPlanRepository, plan *service.ScheduledTestPlan) time.Time {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	until := now.Add(time.Minute)
	plan.TriggerSource = "upstream_5xx"
	ok, err := repo.ClaimPelican(t.Context(), plan, now, until, *plan.NextRunAt)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, plan.AvailabilityAccountFingerprint)
	return until
}

func availabilityPostgresObservation(status string) []service.AvailabilityModelOutcome {
	finished := time.Now().UTC().Add(-time.Millisecond)
	return []service.AvailabilityModelOutcome{{ModelID: "target", Probe: &service.AvailabilityProbeResult{Version: 1, Status: status, Reason: "completed_response", StartedAt: finished.Add(-time.Second), FinishedAt: finished, AttemptCount: 1}}}
}

func availabilityPostgresState(t *testing.T, repo *scheduledTestPlanRepository, id int64) service.AvailabilityState {
	t.Helper()
	var raw []byte
	require.NoError(t, repo.db.QueryRow(`SELECT state FROM account_availability_states WHERE plan_id=$1 AND model_id='target'`, id).Scan(&raw))
	var state service.AvailabilityState
	require.NoError(t, json.Unmarshal(raw, &state))
	return state
}

func TestAvailabilityPostgresLatestExpiryAndIndependentRecovery(t *testing.T) {
	repo, plan := availabilityPostgresPlan(t)
	var previousUntil *time.Time
	for _, status := range []string{"fail", "inconclusive", "pass", "inconclusive"} {
		until := availabilityPostgresClaim(t, repo, plan)
		observations := availabilityPostgresObservation(status)
		action, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, observations)
		require.NoError(t, err)
		require.Equal(t, "observed", action)
		state := availabilityPostgresState(t, repo, plan.ID)
		require.Equal(t, status, state.Observation.Status)
		if status == "inconclusive" {
			require.Equal(t, previousUntil, state.ValidUntil, "inconclusive must not extend the old deadline")
		} else {
			require.Equal(t, status, state.StatusAt(time.Now()))
			require.WithinDuration(t, observations[0].Probe.FinishedAt.Add(time.Minute), *state.ValidUntil, time.Microsecond)
		}
		require.Equal(t, "stale", state.StatusAt(*state.ValidUntil))
		previousUntil = state.ValidUntil
		// A second delivery cannot revise an already committed round.
		action, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("fail"))
		require.NoError(t, err)
		require.Equal(t, "duplicate_round", action)
		require.Equal(t, state, availabilityPostgresState(t, repo, plan.ID))
		require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	}
	var concurrency, ownership int
	var extra string
	require.NoError(t, repo.db.QueryRow(`SELECT concurrency,extra::text FROM accounts WHERE id=$1`, plan.AccountID).Scan(&concurrency, &extra))
	require.Equal(t, 17, concurrency)
	require.Contains(t, extra, "kept")
	require.NoError(t, repo.db.QueryRow(`SELECT count(*) FROM account_quality_states`).Scan(&ownership))
	require.Zero(t, ownership, "availability must not create or clear quality ownership")
	require.NoError(t, repo.Delete(t.Context(), plan.ID))
	require.NoError(t, repo.db.QueryRow(`SELECT count(*) FROM account_availability_states`).Scan(&ownership))
	require.Zero(t, ownership, "plan deletion cascades only its availability state")
}

func TestAvailabilityPostgresStaleRunGuards(t *testing.T) {
	for _, change := range []string{"paused", "edited", "lease_replaced", "lease_expired", "deleted", "route", "missing_fingerprint", "event"} {
		t.Run(change, func(t *testing.T) {
			repo, plan := availabilityPostgresPlan(t)
			until := availabilityPostgresClaim(t, repo, plan)
			queries := map[string]string{
				"paused":         `UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`,
				"edited":         `UPDATE scheduled_test_plans SET updated_at=updated_at+interval '1 second' WHERE id=$1`,
				"lease_replaced": `UPDATE scheduled_test_plans SET running_until=running_until+interval '1 second' WHERE id=$1`,
				"lease_expired":  `UPDATE scheduled_test_plans SET running_until=clock_timestamp()-interval '1 second' WHERE id=$1`,
				"deleted":        `UPDATE accounts SET deleted_at=now() WHERE id=(SELECT account_id FROM scheduled_test_plans WHERE id=$1)`,
				"route":          `UPDATE accounts SET credentials='{"api_key":"fixture-new"}' WHERE id=(SELECT account_id FROM scheduled_test_plans WHERE id=$1)`,
				"event":          `INSERT INTO account_quality_states(plan_id,state) VALUES($1,'{"quality_5xx_episode":1}')`,
			}
			if query := queries[change]; query != "" {
				_, err := repo.db.Exec(query, plan.ID)
				require.NoError(t, err)
			} else {
				plan.AvailabilityAccountFingerprint = nil
			}
			action, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
			require.NoError(t, err)
			require.Equal(t, "stale_run", action)
			var count int
			require.NoError(t, repo.db.QueryRow(`SELECT count(*) FROM account_availability_states`).Scan(&count))
			require.Zero(t, count)
		})
	}
}

func TestAvailabilityPostgresRouteChangeDropsOldDecision(t *testing.T) {
	repo, plan := availabilityPostgresPlan(t)
	until := availabilityPostgresClaim(t, repo, plan)
	_, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
	require.NoError(t, err)
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	_, err = repo.db.Exec(`UPDATE accounts SET credentials='{"api_key":"fixture-new"}' WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	until = availabilityPostgresClaim(t, repo, plan)
	_, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("inconclusive"))
	require.NoError(t, err)
	state := availabilityPostgresState(t, repo, plan.ID)
	require.Nil(t, state.Decision)
	require.Nil(t, state.ValidUntil)
	require.Equal(t, "unknown", state.StatusAt(time.Now()))
}

func TestAvailabilityPostgresConcurrentDeliveryAndAtomicValidation(t *testing.T) {
	repo, plan := availabilityPostgresPlan(t)
	until := availabilityPostgresClaim(t, repo, plan)
	var wg sync.WaitGroup
	actions, failures := make(chan string, 2), make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			action, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
			actions <- action
			failures <- err
		}()
	}
	wg.Wait()
	require.NoError(t, <-failures)
	require.NoError(t, <-failures)
	require.ElementsMatch(t, []string{"observed", "duplicate_round"}, []string{<-actions, <-actions})
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	plan.PelicanConfig.ModelIDs = []string{"target", "other"}
	var err error
	plan, err = repo.Update(t.Context(), plan)
	require.NoError(t, err)
	until = availabilityPostgresClaim(t, repo, plan)
	before := availabilityPostgresState(t, repo, plan.ID)
	observations := availabilityPostgresObservation("fail")
	observations = append(observations, service.AvailabilityModelOutcome{ModelID: "forged", Probe: observations[0].Probe})
	_, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, observations)
	require.ErrorContains(t, err, "invalid availability model observation")
	require.Equal(t, before, availabilityPostgresState(t, repo, plan.ID), "invalid second model must roll back the first write")
}

func TestAvailabilityPostgresLeaseExpiresBeforeCommit(t *testing.T) {
	repo, plan := availabilityPostgresPlan(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	until := now.Add(5 * time.Second)
	plan.TriggerSource = "upstream_5xx"
	ok, err := repo.ClaimPelican(t.Context(), plan, now, until, *plan.NextRunAt)
	require.NoError(t, err)
	require.True(t, ok)
	// Wait inside the actual state write until the database lease expires.
	// The nontransactional sequence proves the trigger ran even after rollback.
	_, err = repo.db.Exec(`CREATE SEQUENCE availability_commit_wait;
 CREATE FUNCTION availability_expire_lease() RETURNS trigger LANGUAGE plpgsql AS $$
 DECLARE remaining double precision;
 BEGIN
 PERFORM nextval('availability_commit_wait');
 SELECT extract(epoch FROM (running_until-clock_timestamp())) INTO remaining FROM scheduled_test_plans WHERE id=NEW.plan_id;
 PERFORM pg_sleep(GREATEST(remaining+0.02,0));
 RETURN NEW;
 END $$;
 CREATE TRIGGER availability_commit_wait BEFORE INSERT ON account_availability_states FOR EACH ROW EXECUTE FUNCTION availability_expire_lease();`)
	require.NoError(t, err)
	action, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
	require.NoError(t, err)
	require.Equal(t, "stale_run", action)
	var waited bool
	require.NoError(t, repo.db.QueryRow(`SELECT is_called FROM availability_commit_wait`).Scan(&waited))
	require.True(t, waited, "prove an actual state write crossed the deadline")
	var count int
	require.NoError(t, repo.db.QueryRow(`SELECT count(*) FROM account_availability_states`).Scan(&count))
	require.Zero(t, count, "the commit-time lease check must roll back earlier writes")
}
func TestAvailabilityPostgresMigrationRollbackAndReplay(t *testing.T) {
	repo, plan := availabilityPostgresPlan(t)
	until := availabilityPostgresClaim(t, repo, plan)
	_, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
	require.NoError(t, err)
	// Manual rollback discards only this feature's derived state. Never put
	// executable down statements into the forward-only migration directory.
	_, err = repo.db.Exec(`DROP TABLE account_availability_states`)
	require.NoError(t, err)
	var count int
	require.NoError(t, repo.db.QueryRow(`SELECT count(*) FROM accounts WHERE id=$1`, plan.AccountID).Scan(&count))
	require.Equal(t, 1, count)
	migration, err := migrations.FS.ReadFile("325_account_availability_states.sql")
	require.NoError(t, err)
	_, err = repo.db.Exec(string(migration))
	require.NoError(t, err)
	require.NoError(t, repo.db.QueryRow(`SELECT count(*) FROM account_availability_states`).Scan(&count))
	require.Zero(t, count, "replay starts unknown; rollback must not manufacture healthy evidence")
}
