//go:build unit && integration

package repository

import (
	"testing"

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
	require.Equal(t, "pass", reports[0].Models[0].Status)

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
	require.Equal(t, "pass", reports[0].Models[0].Status, "restoring the captured parent route restores its evidence")

	_, err = repo.db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_connections_managed}','false') WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	_, err = repo.ReadAccountAvailability(t.Context(), &projection)
	require.ErrorIs(t, err, service.ErrAvailabilityRouteChanged, "a removed managed policy must reject the old projection")
	_, err = repo.ReadAccountAvailability(t.Context(), parent)
	require.ErrorIs(t, err, service.ErrAvailabilityRouteChanged, "an unprojected managed snapshot must not authorize a legacy route")
}
