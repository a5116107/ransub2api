//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func availabilityPreparedAccount(plan *service.ScheduledTestPlan) *service.Account {
	return &service.Account{ID: plan.AccountID, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
}

func TestAvailabilityReadAccountPostgresAllRulesAndOptOut(t *testing.T) {
	repo, plan := availabilityPostgresPlan(t)
	until := availabilityPostgresClaim(t, repo, plan)
	_, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
	require.NoError(t, err)
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	peer := *plan
	peer.ModelID, peer.Enabled = "other-model", false
	peer.PelicanConfig = &service.PelicanTestConfig{Availability: &service.AvailabilityPolicy{ValidForSeconds: 60}}
	peerPlan, err := repo.Create(t.Context(), &peer)
	require.NoError(t, err)
	peer.PelicanConfig = nil
	legacyPlan, err := repo.Create(t.Context(), &peer)
	require.NoError(t, err)
	reports, err := repo.ReadAccountAvailability(t.Context(), availabilityPreparedAccount(plan))
	require.NoError(t, err)
	require.Len(t, reports, 3)
	require.Equal(t, plan.ID, reports[0].PlanID)
	require.Equal(t, "pass", reports[0].Models[0].Status)
	require.Equal(t, peerPlan.ID, reports[1].PlanID)
	require.True(t, reports[1].Configured)
	require.False(t, reports[1].Enabled)
	require.Equal(t, "rule_paused", reports[1].Models[0].Reason)
	require.Equal(t, legacyPlan.ID, reports[2].PlanID)
	require.False(t, reports[2].Configured)
	for _, report := range reports {
		require.Equal(t, plan.AccountID, report.AccountID)
		require.Equal(t, reports[0].EvaluatedAt, report.EvaluatedAt, "all rules must be evaluated after the entire snapshot was read")
	}
	_, err = repo.db.Exec(`DELETE FROM scheduled_test_plans WHERE account_id=$1`, plan.AccountID)
	require.NoError(t, err)
	reports, err = repo.ReadAccountAvailability(t.Context(), availabilityPreparedAccount(plan))
	require.NoError(t, err)
	require.Empty(t, reports, "absence of rules preserves legacy admission")
}

func TestAvailabilityReadAccountPostgresInvalidationAndRecovery(t *testing.T) {
	for _, change := range []string{"fail", "rule", "route", "fault", "paused", "expired", "missing", "invalid", "removed_policy"} {
		t.Run(change, func(t *testing.T) {
			repo, plan := availabilityPostgresPlan(t)
			until := availabilityPostgresClaim(t, repo, plan)
			observations := availabilityPostgresObservation("pass")
			if change == "expired" {
				observations[0].Probe.StartedAt = time.Now().Add(-2 * time.Minute)
				observations[0].Probe.FinishedAt = time.Now().Add(-time.Minute - time.Second)
			}
			if change == "fail" {
				observations[0].Probe.Status = "fail"
			}
			_, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, observations)
			require.NoError(t, err)
			require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
			queries := map[string]string{
				"rule":           `UPDATE scheduled_test_plans SET updated_at=updated_at+interval '1 second' WHERE id=$1`,
				"route":          `UPDATE accounts SET credentials='{"api_key":"replacement-fixture"}' WHERE id=(SELECT account_id FROM scheduled_test_plans WHERE id=$1)`,
				"fault":          `UPDATE scheduled_test_plans SET availability_fault_version=availability_fault_version+1 WHERE id=$1`,
				"paused":         `UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`,
				"missing":        `DELETE FROM account_availability_states WHERE plan_id=$1`,
				"invalid":        `UPDATE account_availability_states SET state=jsonb_set(state,'{decision,version}','99') WHERE plan_id=$1`,
				"removed_policy": `UPDATE scheduled_test_plans SET pelican_config=pelican_config-'availability' WHERE id=$1`,
			}
			if query := queries[change]; query != "" {
				_, err = repo.db.Exec(query, plan.ID)
				require.NoError(t, err)
			}
			account := availabilityPreparedAccount(plan)
			if change == "route" {
				_, err = repo.ReadAccountAvailability(t.Context(), account)
				require.ErrorIs(t, err, service.ErrAvailabilityRouteChanged, "a prepared old key must not use evidence for the new route")
				account.Credentials = map[string]any{"api_key": "replacement-fixture"}
			}
			reports, err := repo.ReadAccountAvailability(t.Context(), account)
			require.NoError(t, err)
			require.Len(t, reports, 1)
			if change == "removed_policy" {
				require.False(t, reports[0].Configured)
				return
			}
			require.NotEqual(t, "pass", reports[0].Models[0].Status)
			if change == "paused" {
				_, err = repo.db.Exec(`UPDATE scheduled_test_plans SET enabled=true WHERE id=$1`, plan.ID)
				require.NoError(t, err)
			}
			plan, err = repo.GetByID(t.Context(), plan.ID)
			require.NoError(t, err)
			until = availabilityPostgresClaim(t, repo, plan)
			_, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
			require.NoError(t, err)
			reports, err = repo.ReadAccountAvailability(t.Context(), account)
			require.NoError(t, err)
			require.Equal(t, "pass", reports[0].Models[0].Status, "only a newly completed valid probe restores the gate")
		})
	}
}

func TestAvailabilityReadAccountPostgresBindingAndReadFailures(t *testing.T) {
	for _, change := range []string{"key", "model_mapping", "proxy", "missing_account", "corrupt_config", "invalid_validity", "read_error", "nil_account"} {
		t.Run(change, func(t *testing.T) {
			repo, plan := availabilityPostgresPlan(t)
			until := availabilityPostgresClaim(t, repo, plan)
			_, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
			require.NoError(t, err)
			account := availabilityPreparedAccount(plan)
			switch change {
			case "key":
				account.Credentials = map[string]any{"api_key": "old-fixture"}
			case "model_mapping":
				account.Credentials = map[string]any{"model_mapping": map[string]any{"alias": "target"}}
			case "proxy":
				id := int64(33)
				account.ProxyID = &id
				account.Proxy = &service.Proxy{ID: id, Protocol: "http", Host: "127.0.0.1", Port: 1}
			case "missing_account":
				_, err = repo.db.Exec(`UPDATE accounts SET deleted_at=now() WHERE id=$1`, plan.AccountID)
			case "corrupt_config":
				_, err = repo.db.Exec(`UPDATE scheduled_test_plans SET pelican_config='[]' WHERE id=$1`, plan.ID)
			case "invalid_validity":
				_, err = repo.db.Exec(`UPDATE scheduled_test_plans SET pelican_config=jsonb_set(pelican_config,'{availability,valid_for_seconds}','0') WHERE id=$1`, plan.ID)
			case "read_error":
				_, err = repo.db.Exec(`DROP TABLE account_availability_states`)
			case "nil_account":
				account = nil
			}
			require.NoError(t, err)
			reports, err := repo.ReadAccountAvailability(t.Context(), account)
			switch change {
			case "missing_account", "invalid_validity":
				require.NoError(t, err)
				require.Equal(t, "unknown", reports[0].Models[0].Status)
			default:
				require.Error(t, err)
				require.Nil(t, reports)
			}
		})
	}
}

func TestAvailabilityReadAccountPostgresSnapshotAndClock(t *testing.T) {
	for _, scenario := range []string{"concurrent_fault", "expires_while_reading"} {
		t.Run(scenario, func(t *testing.T) {
			repo, first := availabilityPostgresPlan(t)
			peer := *first
			second, err := repo.Create(t.Context(), &peer)
			require.NoError(t, err)
			var expires time.Time
			for _, plan := range []*service.ScheduledTestPlan{first, second} {
				until := availabilityPostgresClaim(t, repo, plan)
				outcomes := availabilityPostgresObservation("pass")
				if scenario == "expires_while_reading" {
					outcomes[0].Probe.FinishedAt = time.Now().Add(-59 * time.Second)
					outcomes[0].Probe.StartedAt = outcomes[0].Probe.FinishedAt.Add(-time.Second)
					expires = outcomes[0].Probe.FinishedAt.Add(time.Minute)
				}
				_, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, outcomes)
				require.NoError(t, err)
				require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
			}
			lock, err := repo.db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = lock.Rollback() })
			_, err = lock.Exec(`LOCK TABLE account_availability_states IN ACCESS EXCLUSIVE MODE`)
			require.NoError(t, err)
			type result struct {
				reports []*service.AvailabilityReport
				err     error
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan result, 1)
			go func() {
				reports, err := repo.ReadAccountAvailability(ctx, availabilityPreparedAccount(first))
				done <- result{reports, err}
			}()
			require.Eventually(t, func() bool {
				var blocked bool
				err := repo.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE relation='account_availability_states'::regclass AND NOT granted)`).Scan(&blocked)
				return err == nil && blocked
			}, 5*time.Second, 10*time.Millisecond, "reader must have captured the plans before blocking on state")
			if scenario == "concurrent_fault" {
				_, err = repo.db.Exec(`UPDATE scheduled_test_plans SET availability_fault_version=availability_fault_version+1 WHERE id=$1`, second.ID)
				require.NoError(t, err)
			} else {
				timer := time.NewTimer(time.Until(expires) + 20*time.Millisecond)
				defer timer.Stop()
				<-timer.C
			}
			require.NoError(t, lock.Commit())
			read := <-done
			require.NoError(t, read.err)
			require.Len(t, read.reports, 2)
			for _, report := range read.reports {
				want := "pass"
				if scenario == "expires_while_reading" {
					want = "stale"
				}
				require.Equal(t, want, report.Models[0].Status)
				require.Equal(t, read.reports[0].EvaluatedAt, report.EvaluatedAt)
			}
			if scenario == "concurrent_fault" {
				reports, err := repo.ReadAccountAvailability(t.Context(), availabilityPreparedAccount(first))
				require.NoError(t, err)
				require.Equal(t, "pass", reports[0].Models[0].Status)
				require.Equal(t, "new_fault_event", reports[1].Models[0].Reason, "the next send must observe the newly committed fault")
			}
		})
	}
}
