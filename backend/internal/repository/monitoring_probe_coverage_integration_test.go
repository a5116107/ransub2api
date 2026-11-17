//go:build integration

package repository

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestMonitoringProbeCoverageRejectsImpossibleBudget(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra)
 SELECT 'coverage-'||n,'openai','apikey','active',
 '{"api_key":"coverage-private-key","base_url":"https://coverage.invalid/v1"}'::jsonb,
 '{"upstream_billing_probe_enabled":true}'::jsonb FROM generate_series(1,100) n`)
	require.NoError(t, err)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 100
	repo := NewMonitoringProbeBudgetRepository(db)
	_, err = repo.SaveSettings(ctx, policy)
	require.Error(t, err, "100 accounts cannot complete 30-minute billing/balance sweeps under 100 calls/day")
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key=$1`, service.MonitoringProbeBudgetSettingKey).Scan(&count))
	require.Zero(t, count, "reject before persisting settings or incrementing revision")
	policy.Enabled = false
	saved, err := repo.SaveSettings(ctx, policy)
	require.NoError(t, err, "administrators must still be able to disable enforcement")
	require.EqualValues(t, 1, saved.Revision)
	var stored service.MonitoringProbeBudgetSettings
	var raw string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, service.MonitoringProbeBudgetSettingKey).Scan(&raw))
	require.NoError(t, json.Unmarshal([]byte(raw), &stored))
	require.False(t, stored.Enabled)
}

func TestMonitoringProbeCoveragePreviewAndRevalidation(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	ctx := t.Context()
	insert := func(name string) int64 {
		t.Helper()
		var id int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra)
 VALUES($1,'openai','apikey','active','{"api_key":"coverage-private-key","base_url":"https://coverage.invalid/v1"}'::jsonb,
 '{"upstream_billing_probe_enabled":true}'::jsonb) RETURNING id`, name).Scan(&id))
		return id
	}
	id := insert("coverage-first")
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 100
	repo := NewMonitoringProbeBudgetRepository(db)
	preview, err := repo.PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.True(t, preview.CanSave)
	require.EqualValues(t, 96, preview.MinimumRequestsPerDay)
	raw, err := json.Marshal(preview)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "coverage-private-key")
	require.NotContains(t, string(raw), "coverage.invalid")
	insert("coverage-added-after-preview")
	_, err = repo.SaveSettings(ctx, policy)
	require.ErrorContains(t, err, "day_capacity", "save must recompile instead of trusting the earlier preview")
	policy.RequestsPerDay = 1000
	policy, err = repo.SaveSettings(ctx, policy)
	require.NoError(t, err)
	stale := policy
	stale.Revision--
	_, err = repo.PreviewCoverage(ctx, stale)
	require.ErrorIs(t, err, service.ErrMonitoringProbeBudgetConflict)
	_, err = db.ExecContext(ctx, `UPDATE accounts SET extra='{}' WHERE id<>$1`, id)
	require.NoError(t, err)
	var authID int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO new_api_site_authorizations(site_url,upstream_user_id,access_token_ciphertext)
 VALUES('https://coverage.invalid',42,'fixture-ciphertext') RETURNING id`).Scan(&authID))
	a := &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "coverage-private-key", "base_url": "https://coverage.invalid/v1"}}
	_, err = db.ExecContext(ctx, `INSERT INTO new_api_account_bindings(account_id,authorization_id,upstream_token_id,account_fingerprint)
 VALUES($1,$2,13,$3)`, id, authID, service.NewAPIAccountFingerprint(a))
	require.NoError(t, err)
	preview, err = NewMonitoringProbeBudgetRepository(db).PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.EqualValues(t, 1, preview.NewAPIAccounts)
	require.EqualValues(t, 4, preview.MinimumRequestsPerSweep)
	_, err = db.ExecContext(ctx, `UPDATE new_api_account_bindings SET account_fingerprint='stale' WHERE account_id=$1`, id)
	require.NoError(t, err)
	preview, err = repo.PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.Zero(t, preview.NewAPIAccounts)
	require.EqualValues(t, 2, preview.MinimumRequestsPerSweep)
	summary, err := repo.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, summary.Coverage.MinimumRequestsPerSweep)
	for _, table := range []string{"monitoring_probe_budget_counters", "monitoring_probe_budget_leases"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count, "preflight must not consume calls/tokens or reserve slots")
	}
}

func TestMonitoringProbeCoverageSerializesBudgetAndInterval(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra)
 SELECT 'coverage-'||n,'openai','apikey','active',
 '{"api_key":"coverage-private-key","base_url":"https://coverage.invalid/v1"}'::jsonb,
 '{"upstream_billing_probe_enabled":true}'::jsonb FROM generate_series(1,100) n`)
	require.NoError(t, err)
	repo := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 1000000
	policy, err = repo.SaveSettings(ctx, policy)
	require.NoError(t, err)
	policy.RequestsPerDay = 10000 // valid at 30 minutes, not at 5 minutes
	start, results := make(chan struct{}), make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := NewMonitoringProbeBudgetRepository(db).SaveSettings(ctx, policy)
		results <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		results <- NewMonitoringProbeBudgetRepository(db).SaveBillingProbeSettings(ctx, service.UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5})
	}()
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			require.ErrorContains(t, err, "day_capacity")
		}
	}
	require.Equal(t, 1, successes, "the incompatible pair must not both be committed")
	current, err := repo.GetSettings(ctx)
	require.NoError(t, err)
	preview, err := repo.PreviewCoverage(ctx, current)
	require.NoError(t, err)
	require.True(t, preview.CanSave)
	_, err = db.ExecContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra)
 VALUES('coverage-over-runner','openai','apikey','active','{}','{"upstream_billing_probe_enabled":true}')`)
	require.NoError(t, err)
	require.ErrorContains(t, repo.SaveBillingProbeSettings(ctx, service.UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}), "runner_capacity")
	require.NoError(t, repo.SaveBillingProbeSettings(ctx, service.UpstreamBillingProbeSettings{Enabled: false, IntervalMinutes: 5}), "stopping the runner remains possible")
}
