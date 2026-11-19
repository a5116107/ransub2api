//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestProcurementAvailabilitySQLLatestStateAndRecovery(t *testing.T) {
	plans, plan := availabilityPostgresPlan(t)
	reader := &groupRepository{sql: plans.db}
	account := availabilityPreparedAccount(plan)
	scope := &service.AvailabilityRequestScope{Version: 2, Protocol: "responses_http", Model: "target", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}
	policy := &service.ProcurementPolicy{Version: 5, Revision: 1, Sources: &domain.ProcurementSources{}, Buckets: &domain.ProcurementBuckets{}, Quality: "not_required", Availability: "current_state"}
	assertState := func(want string) {
		t.Helper()
		reports, err := reader.ReadAccountAvailability(t.Context(), account)
		require.NoError(t, err)
		require.Len(t, reports, 1)
		evidence := service.AssessAvailabilityScopeEvidence(account, scope, reports, reports[0].EvaluatedAt)
		view := service.ProcurementAvailability(policy, evidence, evidence.EvaluatedAt)
		require.True(t, view.Required)
		require.Equal(t, want, view.State)
	}
	publish := func(status string) {
		t.Helper()
		current, err := plans.GetByID(t.Context(), plan.ID)
		require.NoError(t, err)
		until := availabilityPostgresClaim(t, plans, current)
		observed := availabilityPostgresObservation(t, plans, status)
		observed[0].Probe.Scope = scope
		_, err = plans.ApplyAvailabilityOutcomes(t.Context(), current, until, observed)
		require.NoError(t, err)
		require.NoError(t, plans.FinishPelican(t.Context(), current.ID, until, time.Now()))
	}
	assertState("unknown")
	publish("pass")
	assertState("pass")
	publish("fail")
	assertState("fail")
	publish("pass")
	assertState("pass")
	for _, query := range []string{
		`UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`,
		`UPDATE scheduled_test_plans SET updated_at=updated_at+interval '1 microsecond' WHERE id=$1`,
		`UPDATE scheduled_test_plans SET availability_fault_version=availability_fault_version+1 WHERE id=$1`,
	} {
		_, err := plans.db.ExecContext(t.Context(), query, plan.ID)
		require.NoError(t, err)
		assertState("unknown")
		_, err = plans.db.ExecContext(t.Context(), `UPDATE scheduled_test_plans SET enabled=true WHERE id=$1`, plan.ID)
		require.NoError(t, err)
		publish("pass")
		assertState("pass")
	}
	// Preserve the real JSON timestamp contract and the policy's exact TTL.
	// PostgreSQL's textual interval representation is not an evidence timestamp.
	var raw []byte
	require.NoError(t, plans.db.QueryRowContext(t.Context(), `SELECT state FROM account_availability_states WHERE plan_id=$1`, plan.ID).Scan(&raw))
	var state availabilityStoredState
	require.NoError(t, json.Unmarshal(raw, &state))
	for _, probe := range []*service.AvailabilityProbeResult{state.Observation, state.Decision} {
		require.NotNil(t, probe)
		probe.StartedAt = probe.StartedAt.Add(-time.Hour)
		probe.FinishedAt = probe.FinishedAt.Add(-time.Hour)
	}
	expired := state.Decision.FinishedAt.Add(time.Minute)
	state.ValidUntil = &expired
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = plans.db.ExecContext(t.Context(), `UPDATE account_availability_states SET state=$2 WHERE plan_id=$1`, plan.ID, raw)
	require.NoError(t, err)
	assertState("unknown")
	publish("pass")
	assertState("pass")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reports, err := reader.ReadAccountAvailability(ctx, account)
	require.Error(t, err)
	require.Nil(t, reports)
	assertState("pass")
	_, err = plans.db.ExecContext(t.Context(), `UPDATE accounts SET credentials='{"api_key":"changed-fixture"}'::jsonb WHERE id=$1`, account.ID)
	require.NoError(t, err)
	reports, err = reader.ReadAccountAvailability(t.Context(), account)
	require.ErrorIs(t, err, service.ErrAvailabilityRouteChanged)
	require.Nil(t, reports)
}
