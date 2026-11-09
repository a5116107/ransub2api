//go:build unit && integration

package repository

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestAvailabilityManagedProjectionParentRouteWithPostgres(t *testing.T) {
	dsn, err := pq.ParseURL(integrationPostgresDSN)
	require.NoError(t, err)
	t.Setenv("SUB2API_QUALITY_TEST_DSN", dsn)
	repo, plan := availabilityPostgresPlan(t)
	_, err = repo.db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_connections_managed}','true') WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	until := availabilityPostgresClaim(t, repo, plan)
	_, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("pass"))
	require.NoError(t, err)

	parent := availabilityPreparedAccount(plan)
	parent.Extra = map[string]any{"openai_connections_managed": true}
	parentRoute := service.AccountRouteFingerprint(parent)
	projection := *parent
	projection.Type = service.AccountTypeOAuth
	projection.Credentials = map[string]any{"access_token": "synthetic-managed"}
	projection.AuthConnectionID = "synthetic-connection"
	projection.AuthConnectionRevision = 1
	projection.AuthConnectionParentRoute = &parentRoute

	reports, err := repo.ReadAccountAvailability(t.Context(), &projection)
	require.NoError(t, err)
	require.Equal(t, "unknown", reports[0].Models[0].Status, "parent-only evidence must not be attributed to a managed grant")

	projection.AuthConnectionParentRoute = nil
	_, err = repo.ReadAccountAvailability(t.Context(), &projection)
	require.ErrorIs(t, err, service.ErrAvailabilityRouteChanged, "an uncaptured parent cannot be reconstructed")
	projection.AuthConnectionParentRoute = &parentRoute

	_, err = repo.db.Exec(`UPDATE accounts SET credentials='{"api_key":"new-synthetic"}' WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	_, err = repo.ReadAccountAvailability(t.Context(), &projection)
	require.ErrorIs(t, err, service.ErrAvailabilityRouteChanged, "parent route changes must reject a prepared projection")
	_, err = repo.db.Exec(`UPDATE accounts SET credentials='{}' WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	reports, err = repo.ReadAccountAvailability(t.Context(), &projection)
	require.NoError(t, err)
	require.Equal(t, "unknown", reports[0].Models[0].Status, "restoring the parent route must not invent connection evidence")

	_, err = repo.db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_connections_managed}','false') WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	_, err = repo.ReadAccountAvailability(t.Context(), &projection)
	require.ErrorIs(t, err, service.ErrAvailabilityRouteChanged, "a removed managed policy must reject the old projection")
	_, err = repo.ReadAccountAvailability(t.Context(), parent)
	require.ErrorIs(t, err, service.ErrAvailabilityRouteChanged, "an unprojected managed snapshot must not authorize a legacy route")
}

func TestAvailabilityManagedScheduledProbeRotatesFairlyWithPostgres(t *testing.T) {
	dsn, err := pq.ParseURL(integrationPostgresDSN)
	require.NoError(t, err)
	t.Setenv("SUB2API_QUALITY_TEST_DSN", dsn)
	repo, plan := availabilityPostgresPlan(t)
	_, err = repo.db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_connections_managed}','true') WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	ids := []string{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", "33333333-3333-3333-3333-333333333333"}
	for _, id := range ids {
		_, err = repo.db.Exec(`INSERT INTO openai_auth_connections(id,account_id,kind,status,capabilities) VALUES($1,$2,'api_key','active','["responses"]')`, id, plan.AccountID)
		require.NoError(t, err)
	}
	for _, tc := range []struct{ id, status, caps, refresh string }{
		{"44444444-4444-4444-4444-444444444444", "disabled", `["responses"]`, "idle"},
		{"55555555-5555-5555-5555-555555555555", "active", `["embeddings"]`, "idle"},
		{"66666666-6666-6666-6666-666666666666", "active", `["responses"]`, "uncertain"},
	} {
		_, err = repo.db.Exec(`INSERT INTO openai_auth_connections(id,account_id,kind,status,capabilities,refresh_state) VALUES($1,$2,'api_key',$3,$4::jsonb,$5)`, tc.id, plan.AccountID, tc.status, tc.caps, tc.refresh)
		require.NoError(t, err)
	}
	unpublished := availabilityPostgresClaim(t, repo, plan)
	firstCandidate := plan.AvailabilityProbeConnectionID
	require.Contains(t, ids, firstCandidate)
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, unpublished, time.Now()))
	var selected []string
	for round := range 4 {
		until := availabilityPostgresClaim(t, repo, plan)
		require.Contains(t, ids, plan.AvailabilityProbeConnectionID, "the claimed plan must select an actual eligible grant")
		selected = append(selected, plan.AvailabilityProbeConnectionID)
		if round == 0 {
			require.Equal(t, firstCandidate, plan.AvailabilityProbeConnectionID, "an abandoned round must not advance the cursor")
		}
		observation := availabilityPostgresObservation("pass")
		if round == 1 {
			observation[0].Probe.Status = "fail" // No attributable response, still advance fairly.
		} else {
			observation[0].Probe.Scope = &service.AvailabilityRequestScope{Version: 1, ConnectionScope: service.AvailabilityConnectionScope(&service.Account{AuthConnectionID: plan.AvailabilityProbeConnectionID, AuthConnectionRevision: 1})}
		}
		action, applyErr := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, observation)
		require.NoError(t, applyErr)
		if round == 1 {
			require.Equal(t, "unattributed_failure", action)
		} else {
			require.Equal(t, "observed", action)
		}
		require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
		plan, err = repo.GetByID(t.Context(), plan.ID)
		require.NoError(t, err)
	}
	require.ElementsMatch(t, ids, selected[:3], "every active grant gets one round before a repeat")
	require.Equal(t, selected[0], selected[3], "the cursor must wrap after every eligible grant")
	var raw []byte
	require.NoError(t, repo.db.QueryRow(`SELECT state FROM account_availability_states WHERE plan_id=$1 AND model_id=$2`, plan.ID, plan.ModelID).Scan(&raw))
	for _, id := range ids {
		require.NotContains(t, string(raw), id, "the cursor must not persist a raw connection ID")
	}
	_, err = repo.db.Exec(`UPDATE openai_auth_connections SET status='disabled' WHERE id=$1`, selected[1])
	require.NoError(t, err)
	until := availabilityPostgresClaim(t, repo, plan)
	require.NotEqual(t, selected[1], plan.AvailabilityProbeConnectionID, "disabled grants are not selected")
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
}

func TestAvailabilityManagedProbeCursorFailureRollsBackWithPostgres(t *testing.T) {
	dsn, err := pq.ParseURL(integrationPostgresDSN)
	require.NoError(t, err)
	t.Setenv("SUB2API_QUALITY_TEST_DSN", dsn)
	repo, plan := availabilityPostgresPlan(t)
	_, err = repo.db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_connections_managed}','true') WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	for _, id := range []string{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"} {
		_, err = repo.db.Exec(`INSERT INTO openai_auth_connections(id,account_id,kind,status,capabilities) VALUES($1,$2,'api_key','active','["responses"]')`, id, plan.AccountID)
		require.NoError(t, err)
	}
	until := availabilityPostgresClaim(t, repo, plan)
	first := plan.AvailabilityProbeConnectionID
	probe := availabilityPostgresObservation("pass")
	probe[0].Probe.Scope = &service.AvailabilityRequestScope{Version: 1, ConnectionScope: service.AvailabilityConnectionScope(&service.Account{AuthConnectionID: first, AuthConnectionRevision: 1})}
	_, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, probe)
	require.NoError(t, err)
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	until = availabilityPostgresClaim(t, repo, plan)
	second := plan.AvailabilityProbeConnectionID
	require.NotEqual(t, first, second)
	var stateBefore, stateAfter string
	var versionBefore, versionAfter int64
	var nextBefore, nextAfter time.Time
	require.NoError(t, repo.db.QueryRow(`SELECT state::text FROM account_availability_states WHERE plan_id=$1`, plan.ID).Scan(&stateBefore))
	require.NoError(t, repo.db.QueryRow(`SELECT availability_fault_version,next_run_at FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&versionBefore, &nextBefore))
	_, err = repo.db.Exec(`CREATE FUNCTION reject_probe_cursor() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'fixture cursor write failure'; END $$;
 CREATE TRIGGER reject_probe_cursor BEFORE INSERT OR UPDATE ON account_availability_states FOR EACH ROW EXECUTE FUNCTION reject_probe_cursor();`)
	require.NoError(t, err)
	_, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("fail"))
	require.ErrorContains(t, err, "fixture cursor write failure")
	require.NoError(t, repo.db.QueryRow(`SELECT state::text FROM account_availability_states WHERE plan_id=$1`, plan.ID).Scan(&stateAfter))
	require.NoError(t, repo.db.QueryRow(`SELECT availability_fault_version,next_run_at FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&versionAfter, &nextAfter))
	require.Equal(t, stateBefore, stateAfter)
	require.Equal(t, versionBefore, versionAfter, "failure to save the cursor must roll back the fault fence")
	require.True(t, nextBefore.Equal(nextAfter), "failure to save the cursor must roll back the retry schedule")
	_, err = repo.db.Exec(`DROP TRIGGER reject_probe_cursor ON account_availability_states`)
	require.NoError(t, err)
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	until = availabilityPostgresClaim(t, repo, plan)
	require.Equal(t, second, plan.AvailabilityProbeConnectionID, "a rolled-back round must be retried, not skipped")
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
}

func TestAvailabilityManagedProbeSelectionFailureRollsBackClaimWithPostgres(t *testing.T) {
	dsn, err := pq.ParseURL(integrationPostgresDSN)
	require.NoError(t, err)
	t.Setenv("SUB2API_QUALITY_TEST_DSN", dsn)
	repo, plan := availabilityPostgresPlan(t)
	_, err = repo.db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_connections_managed}','true') WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	_, err = repo.db.Exec(`DROP TABLE openai_auth_connections`)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	plan.TriggerSource = "upstream_5xx"
	claimed, err := repo.ClaimPelican(t.Context(), plan, now, now.Add(time.Minute), *plan.NextRunAt)
	require.Error(t, err)
	require.False(t, claimed)
	var running *time.Time
	require.NoError(t, repo.db.QueryRow(`SELECT running_until FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&running))
	require.Nil(t, running, "a failed target lookup must not leave a lease or silently select a preferred grant")
}

func TestAvailabilityManagedBusinessFaultFencesOnlyConfirmedGrantWithPostgres(t *testing.T) {
	dsn, err := pq.ParseURL(integrationPostgresDSN)
	require.NoError(t, err)
	t.Setenv("SUB2API_QUALITY_TEST_DSN", dsn)
	repo, plan := availabilityPostgresPlan(t)
	_, err = repo.db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_connections_managed}','true') WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	plan.PelicanConfig.Quality = nil
	plan.PelicanConfig.ModelIDs = []string{"target", "other"}
	plan, err = repo.Update(t.Context(), plan)
	require.NoError(t, err)
	parent := availabilityPreparedAccount(plan)
	parent.Extra = map[string]any{"openai_connections_managed": true}
	route := service.AccountRouteFingerprint(parent)
	projection := func(id string) *service.Account {
		a := *parent
		a.AuthConnectionID, a.AuthConnectionRevision, a.AuthConnectionParentRoute = id, 1, &route
		return &a
	}
	a, b := projection("connection-a"), projection("connection-b")
	outcomes := func(account *service.Account) []service.AvailabilityModelOutcome {
		result := availabilityPostgresObservation("pass")
		result[0].Probe.Scope = &service.AvailabilityRequestScope{Version: 1, ConnectionScope: service.AvailabilityConnectionScope(account)}
		copy := *result[0].Probe
		return append(result, service.AvailabilityModelOutcome{ModelID: "other", Probe: &copy})
	}
	publish := func(account *service.Account) {
		until := availabilityPostgresClaim(t, repo, plan)
		action, publishErr := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, outcomes(account))
		require.NoError(t, publishErr)
		require.Equal(t, "observed", action)
		require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	}
	statuses := func(account *service.Account) []string {
		reports, readErr := repo.ReadAccountAvailability(t.Context(), account)
		require.NoError(t, readErr)
		require.Len(t, reports[0].Models, 2)
		return []string{reports[0].Models[0].Status, reports[0].Models[1].Status}
	}
	publish(a)
	publish(b)
	// Nontransactional sequence proves one model update occurred before the
	// second failed; the enclosing transaction must roll back both and its fence.
	_, err = repo.db.Exec(`CREATE SEQUENCE managed_fault_write_counter;
 CREATE FUNCTION managed_fault_write_fail() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.model_id='other' THEN PERFORM nextval('managed_fault_write_counter'); RETURN NEW; END IF;
 RAISE EXCEPTION 'fixture state write failure'; END $$;
 CREATE TRIGGER managed_fault_write_fail BEFORE UPDATE OF state ON account_availability_states FOR EACH ROW EXECUTE FUNCTION managed_fault_write_fail();`)
	require.NoError(t, err)
	accounts := &accountRepository{sql: repo.db}
	require.ErrorContains(t, accounts.ApplyManagedAvailabilityFault(t.Context(), a, false), "fixture state write failure")
	var wrote bool
	require.NoError(t, repo.db.QueryRow(`SELECT is_called FROM managed_fault_write_counter`).Scan(&wrote))
	require.True(t, wrote, "the first model really was updated before the later failure")
	var version int64
	require.NoError(t, repo.db.QueryRow(`SELECT availability_fault_version FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&version))
	require.Zero(t, version, "a failed state update must roll back the plan fence")
	require.Equal(t, []string{"pass", "pass"}, statuses(a))
	require.Equal(t, []string{"pass", "pass"}, statuses(b))
	_, err = repo.db.Exec(`DROP TRIGGER managed_fault_write_fail ON account_availability_states`)
	require.NoError(t, err)
	until := availabilityPostgresClaim(t, repo, plan)
	require.NoError(t, accounts.ApplyManagedAvailabilityFault(t.Context(), a, false))
	require.Equal(t, []string{"unknown", "unknown"}, statuses(a))
	require.Equal(t, []string{"pass", "pass"}, statuses(b), "the healthy grant retains both models")
	action, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, outcomes(a))
	require.NoError(t, err)
	require.Equal(t, "stale_run", action, "a probe claimed before the fault cannot resurrect the affected grant")
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	plan, err = repo.GetByID(t.Context(), plan.ID)
	require.NoError(t, err)
	publish(a)
	require.Equal(t, []string{"pass", "pass"}, statuses(a))
	require.Equal(t, []string{"pass", "pass"}, statuses(b))
	var before, after int64
	require.NoError(t, repo.db.QueryRow(`SELECT availability_fault_version FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&before))
	_, err = repo.db.Exec(`UPDATE accounts SET credentials='{"api_key":"new-route"}' WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	require.NoError(t, accounts.ApplyManagedAvailabilityFault(t.Context(), a, false))
	require.NoError(t, repo.db.QueryRow(`SELECT availability_fault_version FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&after))
	require.Equal(t, before, after, "a late response from a replaced parent route must not invalidate the new route")
	_, err = repo.db.Exec(`UPDATE accounts SET credentials='{}' WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	require.Equal(t, []string{"pass", "pass"}, statuses(a))
	require.Equal(t, []string{"pass", "pass"}, statuses(b))
	c := projection("connection-c")
	publish(c)
	until = availabilityPostgresClaim(t, repo, plan)
	var wg sync.WaitGroup
	concurrentErrors := make(chan error, 2)
	for _, account := range []*service.Account{a, b} {
		wg.Go(func() { concurrentErrors <- accounts.ApplyManagedAvailabilityFault(t.Context(), account, false) })
	}
	wg.Wait()
	require.NoError(t, <-concurrentErrors)
	require.NoError(t, <-concurrentErrors)
	require.Equal(t, []string{"unknown", "unknown"}, statuses(a))
	require.Equal(t, []string{"unknown", "unknown"}, statuses(b))
	require.Equal(t, []string{"pass", "pass"}, statuses(c), "concurrent faults cannot discard an unrelated healthy grant")
	action, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, outcomes(a))
	require.NoError(t, err)
	require.Equal(t, "stale_run", action, "a pre-fault probe cannot revive a grant after concurrent faults")
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	plan, err = repo.GetByID(t.Context(), plan.ID)
	require.NoError(t, err)
	publish(a)
	publish(b)
	require.NoError(t, accounts.ApplyAvailabilityFault(t.Context(), plan.AccountID))
	require.Equal(t, []string{"unknown", "unknown"}, statuses(a))
	require.Equal(t, []string{"unknown", "unknown"}, statuses(b), "an unknown source remains account-wide")
	require.NoError(t, accounts.ApplyManagedAvailabilityFault(t.Context(), a, false))
	require.Equal(t, []string{"unknown", "unknown"}, statuses(b), "a scoped fault must not revive evidence fenced by an earlier global fault")
}

func TestAvailabilityManagedBusinessFaultPreservesQualityOwnershipWithPostgres(t *testing.T) {
	dsn, err := pq.ParseURL(integrationPostgresDSN)
	require.NoError(t, err)
	t.Setenv("SUB2API_QUALITY_TEST_DSN", dsn)
	repo, plan := availabilityPostgresPlan(t)
	_, err = repo.db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_connections_managed}','true') WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	parent := availabilityPreparedAccount(plan)
	parent.Extra = map[string]any{"openai_connections_managed": true}
	route := service.AccountRouteFingerprint(parent)
	projection := func(id string) *service.Account {
		a := *parent
		a.AuthConnectionID, a.AuthConnectionRevision, a.AuthConnectionParentRoute = id, 1, &route
		return &a
	}
	a, b := projection("connection-a"), projection("connection-b")
	for _, account := range []*service.Account{a, b} {
		until := availabilityPostgresClaim(t, repo, plan)
		observation := availabilityPostgresObservation("pass")
		observation[0].Probe.Scope = &service.AvailabilityRequestScope{Version: 1, ConnectionScope: service.AvailabilityConnectionScope(account)}
		_, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, observation)
		require.NoError(t, err)
		require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	}
	accounts := &accountRepository{sql: repo.db}
	require.NoError(t, accounts.ApplyManagedAvailabilityFault(t.Context(), a, true))
	var ownerCount int
	require.NoError(t, repo.db.QueryRow(`SELECT count(*) FROM account_quality_states WHERE plan_id=$1`, plan.ID).Scan(&ownerCount))
	require.Equal(t, 1, ownerCount, "a confirmed 5xx still applies the existing account quality policy")
	reports, err := repo.ReadAccountAvailability(t.Context(), b)
	require.NoError(t, err)
	require.Equal(t, "pass", reports[0].Models[0].Status, "quality protection and connection-scoped availability are independent")
}

func TestAvailabilityManagedConnectionsIndependentEvidenceWithPostgres(t *testing.T) {
	dsn, err := pq.ParseURL(integrationPostgresDSN)
	require.NoError(t, err)
	t.Setenv("SUB2API_QUALITY_TEST_DSN", dsn)
	repo, plan := availabilityPostgresPlan(t)
	_, err = repo.db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_connections_managed}','true') WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	parent := availabilityPreparedAccount(plan)
	parent.Extra = map[string]any{"openai_connections_managed": true}
	route := service.AccountRouteFingerprint(parent)
	projection := func(id string, revision int64) *service.Account {
		a := *parent
		a.AuthConnectionID, a.AuthConnectionRevision, a.AuthConnectionParentRoute = id, revision, &route
		return &a
	}
	a, b := projection("connection-a", 1), projection("connection-b", 1)
	status := func(account *service.Account) string {
		reports, readErr := repo.ReadAccountAvailability(t.Context(), account)
		require.NoError(t, readErr)
		return reports[0].Models[0].Status
	}
	apply := func(account *service.Account, result string) string {
		until := availabilityPostgresClaim(t, repo, plan)
		observation := availabilityPostgresObservation(result)
		if account != nil {
			observation[0].Probe.Scope = &service.AvailabilityRequestScope{Version: 1, ConnectionScope: service.AvailabilityConnectionScope(account)}
		}
		action, applyErr := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, observation)
		require.NoError(t, applyErr)
		require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
		return action
	}
	// Seed exactly the pre-upgrade single-connection JSON representation.
	until := availabilityPostgresClaim(t, repo, plan)
	legacyProbe := availabilityPostgresObservation("pass")[0].Probe
	legacyProbe.Scope = &service.AvailabilityRequestScope{Version: 1, ConnectionScope: service.AvailabilityConnectionScope(a)}
	validUntil := legacyProbe.FinishedAt.Add(time.Minute)
	legacy := availabilityStoredState{AvailabilityState: service.AvailabilityState{Observation: legacyProbe, Decision: legacyProbe, ValidUntil: &validUntil}, FaultVersion: &plan.AvailabilityFaultVersion}
	legacy.captureConnectionScopes()
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	_, err = repo.db.Exec(`INSERT INTO account_availability_states(plan_id,model_id,plan_version,route_fingerprint,round_lease,state) VALUES($1,$2,$3,$4,$5,$6)`, plan.ID, plan.ModelID, plan.UpdatedAt, plan.AvailabilityAccountFingerprint[:], until, string(raw))
	require.NoError(t, err)
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	require.Equal(t, "pass", status(a), "the old evidence remains readable before any new write")
	require.Equal(t, "observed", apply(b, "pass"))
	require.Equal(t, "pass", status(a), "writing another connection preserves migrated old evidence")
	require.Equal(t, "pass", status(b))
	require.Equal(t, "observed", apply(a, "fail"))
	require.Equal(t, "fail", status(a))
	require.Equal(t, "pass", status(b), "an attributed failure belongs only to the observed grant")
	require.Equal(t, "observed", apply(a, "inconclusive"))
	require.Equal(t, "fail", status(a), "inconclusive observations cannot restore a failed grant")
	require.Equal(t, "pass", status(b))
	require.Equal(t, "unknown", status(projection("connection-a", 2)), "rotated grants start unknown")
	require.Equal(t, "observed", apply(a, "pass"))
	require.Equal(t, "unattributed_failure", apply(nil, "fail"))
	require.Equal(t, "unknown", status(a), "an unattributed real failure fences every possibly affected grant")
	require.Equal(t, "unknown", status(b))
	var version int64
	var next time.Time
	require.NoError(t, repo.db.QueryRow(`SELECT availability_fault_version,next_run_at FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&version, &next))
	require.EqualValues(t, 1, version)
	require.InDelta(t, 30, time.Until(next).Seconds(), 5, "an unattributed failure schedules a bounded retry rather than a tight loop")
	// Capture the new fence as the ordinary worker does when reclaiming a plan.
	plan, err = repo.GetByID(t.Context(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, "observed", apply(b, "pass"))
	require.Equal(t, "unknown", status(a))
	require.Equal(t, "pass", status(b))
	require.Equal(t, "observed", apply(nil, "inconclusive"))
	require.Equal(t, "pass", status(b), "an unattributed inconclusive result must not overwrite a peer")
	until = availabilityPostgresClaim(t, repo, plan)
	unattributed := availabilityPostgresObservation("inconclusive")
	action, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, unattributed)
	require.NoError(t, err)
	require.Equal(t, "observed", action)
	revised := availabilityPostgresObservation("fail")
	revised[0].Probe.Scope = &service.AvailabilityRequestScope{Version: 1, ConnectionScope: service.AvailabilityConnectionScope(b)}
	action, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, revised)
	require.NoError(t, err)
	require.Equal(t, "duplicate_round", action, "a duplicate cannot replace an unattributed round")
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	require.Equal(t, "pass", status(b))
	until = availabilityPostgresClaim(t, repo, plan)
	scoped := availabilityPostgresObservation("pass")
	scoped[0].Probe.Scope = &service.AvailabilityRequestScope{Version: 1, ConnectionScope: service.AvailabilityConnectionScope(b)}
	action, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, scoped)
	require.NoError(t, err)
	require.Equal(t, "observed", action)
	action, err = repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("fail"))
	require.NoError(t, err)
	require.Equal(t, "duplicate_round", action, "an unattributed duplicate cannot fence an already committed round")
	require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, time.Now()))
	require.Equal(t, "pass", status(b))
	accounts := &accountRepository{sql: repo.db}
	require.NoError(t, accounts.ApplyAvailabilityFault(t.Context(), plan.AccountID))
	require.Equal(t, "unknown", status(a))
	require.Equal(t, "unknown", status(b), "an account-level business fault still fences every connection")
}

func TestAvailabilityManagedConnectionStateBoundAndLegacy(t *testing.T) {
	now := time.Now()
	until := now.Add(time.Minute)
	account := &service.Account{AuthConnectionID: "legacy", AuthConnectionRevision: 1}
	scope := service.AvailabilityConnectionScope(account)
	state := availabilityStoredState{AvailabilityState: service.AvailabilityState{Decision: &service.AvailabilityProbeResult{
		Status: "pass", Scope: &service.AvailabilityRequestScope{ConnectionScope: scope}}, ValidUntil: &until}}
	state.captureConnectionScopes()
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	var restored availabilityStoredState
	require.NoError(t, json.Unmarshal(raw, &restored))
	restored.promoteLegacyConnection()
	require.Equal(t, "pass", restored.Connections[scope].StatusAt(now), "the old single-connection record remains usable")
	raw, err = json.Marshal(restored)
	require.NoError(t, err)
	var olderReader service.AvailabilityState
	require.NoError(t, json.Unmarshal(raw, &olderReader))
	require.Equal(t, "unknown", olderReader.StatusAt(now), "an older reader must not interpret the new multi-connection record as a parent pass")
	for i := 0; i < maxAvailabilityConnectionStates+8; i++ {
		other := &service.Account{AuthConnectionID: fmt.Sprintf("connection-%d", i), AuthConnectionRevision: 1}
		key := service.AvailabilityConnectionScope(other)
		expires := until.Add(time.Duration(i) * time.Second)
		restored.Connections[key] = service.AvailabilityState{ValidUntil: &expires}
	}
	restored.pruneConnections(scope)
	require.Len(t, restored.Connections, maxAvailabilityConnectionStates)
	require.Equal(t, "pass", restored.Connections[scope].StatusAt(now), "the just-updated grant is not evicted")
}

func TestAvailabilityManagedAttributedRoundConcurrentDuplicateWithPostgres(t *testing.T) {
	dsn, err := pq.ParseURL(integrationPostgresDSN)
	require.NoError(t, err)
	t.Setenv("SUB2API_QUALITY_TEST_DSN", dsn)
	repo, plan := availabilityPostgresPlan(t)
	_, err = repo.db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_connections_managed}','true') WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	until := availabilityPostgresClaim(t, repo, plan)
	var wg sync.WaitGroup
	actions, failures := make(chan string, 2), make(chan error, 2)
	for _, id := range []string{"connection-a", "connection-b"} {
		wg.Go(func() {
			observation := availabilityPostgresObservation("pass")
			observation[0].Probe.Scope = &service.AvailabilityRequestScope{Version: 1, ConnectionScope: service.AvailabilityConnectionScope(&service.Account{AuthConnectionID: id, AuthConnectionRevision: 1})}
			action, applyErr := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, observation)
			actions <- action
			failures <- applyErr
		})
	}
	wg.Wait()
	require.NoError(t, <-failures)
	require.NoError(t, <-failures)
	require.ElementsMatch(t, []string{"observed", "duplicate_round"}, []string{<-actions, <-actions})
	var raw []byte
	require.NoError(t, repo.db.QueryRow(`SELECT state FROM account_availability_states WHERE plan_id=$1 AND model_id=$2`, plan.ID, plan.ModelID).Scan(&raw))
	var stored availabilityStoredState
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Len(t, stored.Connections, 1, "a duplicate delivery cannot add another connection to the committed round")
}

func TestAvailabilityManagedUnknownFailureRetryAndLeaseWithPostgres(t *testing.T) {
	dsn, err := pq.ParseURL(integrationPostgresDSN)
	require.NoError(t, err)
	t.Setenv("SUB2API_QUALITY_TEST_DSN", dsn)
	t.Run("preserve earlier cron", func(t *testing.T) {
		repo, plan := availabilityPostgresPlan(t)
		_, err := repo.db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_connections_managed}','true') WHERE id=$1`, plan.AccountID)
		require.NoError(t, err)
		until := availabilityPostgresClaim(t, repo, plan)
		var earlier time.Time
		require.NoError(t, repo.db.QueryRow(`UPDATE scheduled_test_plans SET next_run_at=clock_timestamp()+interval '5 seconds' WHERE id=$1 RETURNING next_run_at`, plan.ID).Scan(&earlier))
		action, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("fail"))
		require.NoError(t, err)
		require.Equal(t, "unattributed_failure", action)
		var next time.Time
		require.NoError(t, repo.db.QueryRow(`SELECT next_run_at FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&next))
		require.True(t, next.Equal(earlier), "the failure retry must not postpone an earlier scheduled probe")
	})
	t.Run("lease expires before commit", func(t *testing.T) {
		repo, plan := availabilityPostgresPlan(t)
		_, err := repo.db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_connections_managed}','true') WHERE id=$1`, plan.AccountID)
		require.NoError(t, err)
		now := time.Now().UTC().Truncate(time.Microsecond)
		until := now.Add(2 * time.Second)
		plan.TriggerSource = "upstream_5xx"
		claimed, err := repo.ClaimPelican(t.Context(), plan, now, until, *plan.NextRunAt)
		require.NoError(t, err)
		require.True(t, claimed)
		_, err = repo.db.Exec(`CREATE FUNCTION availability_fault_wait() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN PERFORM pg_sleep(2.1); RETURN NEW; END $$;
 CREATE TRIGGER availability_fault_wait BEFORE UPDATE OF availability_fault_version ON scheduled_test_plans FOR EACH ROW EXECUTE FUNCTION availability_fault_wait();`)
		require.NoError(t, err)
		action, err := repo.ApplyAvailabilityOutcomes(t.Context(), plan, until, availabilityPostgresObservation("fail"))
		require.NoError(t, err)
		require.Equal(t, "stale_run", action)
		var version int64
		require.NoError(t, repo.db.QueryRow(`SELECT availability_fault_version FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&version))
		require.Zero(t, version, "an expired round cannot publish a fault or retry schedule")
	})
}
