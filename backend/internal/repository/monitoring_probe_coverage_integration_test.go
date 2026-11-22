//go:build integration

package repository

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestMonitoringProbePlanDraftSameSnapshotPostgres(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	ctx := t.Context()
	var account int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type) VALUES('Snapshot draft','openai','apikey') RETURNING id`).Scan(&account))
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := service.DefaultMonitoringProbeBudgetSettings()
	policy.Enabled, policy.RequestsPerDay, policy.RecoveryReservePercent = true, 100, 0
	policy, err := budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	savedPolicy, err := loadMonitoringProbeBudgetSettings(ctx, tx, "")
	require.NoError(t, err) // establish the snapshot before the external write
	native := service.NewScheduledTestService(NewScheduledTestPlanRepository(db), nil)
	_, err = native.CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: account, ModelID: "other", CronExpression: "0 * * * *", Enabled: true, MaxResults: 50})
	require.NoError(t, err)
	draft := &service.MonitoringProbePlanDraft{AccountID: account, ModelID: "new", CronExpression: "0 * * * *", Enabled: true, MaxResults: 50}
	snapshot, err := compileMonitoringProbeCoverage(ctx, tx, savedPolicy, nil, draft)
	require.NoError(t, err)
	require.Zero(t, snapshot.PlanChange.CurrentMinimumRequestsPerDay, "a new external plan must not enter half of the old snapshot")
	require.EqualValues(t, 24, snapshot.PlanChange.DeltaMinimumRequestsPerDay)
	require.EqualValues(t, 24, snapshot.MinimumTotalRequestsPerDay)
	require.NoError(t, tx.Commit())
	current, err := budget.PreviewCoverage(ctx, policy, draft)
	require.NoError(t, err)
	require.EqualValues(t, 24, current.PlanChange.CurrentMinimumRequestsPerDay)
	require.EqualValues(t, 24, current.PlanChange.DeltaMinimumRequestsPerDay)
	require.EqualValues(t, 48, current.MinimumTotalRequestsPerDay)
	for _, bad := range [][]*service.MonitoringProbePlanDraft{{nil}, {draft, draft}} {
		_, err := budget.PreviewCoverage(ctx, policy, bad...)
		require.ErrorIs(t, err, service.ErrMonitoringProbePlanDraft)
	}
	_, err = budget.PreviewCoverage(service.WithObserverScope(ctx, []int64{1}), policy, draft)
	require.ErrorIs(t, err, service.ErrObserverScope)
	// Imported 2FA plans keep their original enrollment boundary even if a
	// caller proposes a different kind of draft for the same numeric ID.
	var imported int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,pelican_config,next_run_at)
 VALUES($1,'model-a','0 * * * *',false,'{"quality":{"action":"openai_two_fa_admission"}}',NOW()) RETURNING id`, account).Scan(&imported))
	draft.PlanID = imported
	_, err = budget.PreviewCoverage(ctx, policy, draft)
	require.ErrorIs(t, err, service.ErrMonitoringProbePlanDraft)
}

func TestMonitoringProbeCoverageV1SharedCredentialPostgres(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	require.NoError(t, NewSettingRepository(client).SetMultiple(ctx, map[string]string{
		service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1",
	}))
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("ab", 32)
	cipher, err := NewAESEncryptor(cfg)
	require.NoError(t, err)
	svc := service.NewChannelMonitorService(NewChannelMonitorRepository(client, db), cipher)
	for i := 1; i <= 2; i++ {
		_, err := svc.Create(ctx, service.ChannelMonitorCreateParams{
			Name: fmt.Sprintf("shared-v1-%d", i), Provider: service.MonitorProviderOpenAI,
			Endpoint: fmt.Sprintf("https://203.0.113.%d/v1", i), APIKey: "same-real-v1-fixture-key",
			PrimaryModel: "model-a", Enabled: true, IntervalSeconds: 60,
		})
		require.NoError(t, err)
	}
	var distinctCiphertexts int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(DISTINCT api_key_encrypted) FROM channel_monitors`).Scan(&distinctCiphertexts))
	require.Equal(t, 2, distinctCiphertexts, "random nonces must not obscure the identical final key")
	policy := monitoringBudgetPolicy()
	policy.KeyRequestsPerMinute, policy.RequestsPerDay, policy.RecoveryReservePercent = 2, 10000, 0
	preview, err := NewMonitoringProbeBudgetRepository(db).PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.False(t, preview.CanSave, "5760 model/HEAD calls sharing one key must not fit 2880 key capacity")
	require.Contains(t, preview.BlockingReasons, "periodic_key_capacity")
	require.EqualValues(t, 1, preview.PeriodicCredentials.CredentialGroups)
	require.EqualValues(t, 5760, preview.PeriodicCredentials.MaxAttributionsPerDay)
	require.Zero(t, preview.PeriodicCredentials.UnresolvedActiveTargets)
	_, err = NewMonitoringProbeBudgetRepository(db).SaveSettings(ctx, policy)
	require.ErrorContains(t, err, "periodic_key_capacity")
	policy.KeyRequestsPerMinute = 4
	policy, err = NewMonitoringProbeBudgetRepository(db).SaveSettings(ctx, policy)
	require.NoError(t, err, "exact equality repairs the shared-key capacity without changing any probe")
	loaded, err := NewMonitoringProbeBudgetRepository(db).Summary(ctx)
	require.NoError(t, err)
	require.True(t, loaded.Coverage.CanSave)
	require.EqualValues(t, 5760, loaded.Coverage.PeriodicCredentials.KeyDayCapacity)
	encoded, err := json.Marshal(loaded)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "same-real-v1-fixture-key")
	require.NotContains(t, string(encoded), config.UpstreamKeyFingerprint("same-real-v1-fixture-key"))
}

func TestMonitoringProbeCoverageV1CredentialCRUDPostgres(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	require.NoError(t, NewSettingRepository(client).SetMultiple(ctx, map[string]string{
		service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1",
	}))
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("ab", 32)
	cipher, err := NewAESEncryptor(cfg)
	require.NoError(t, err)
	monitors := NewChannelMonitorRepository(client, db)
	svc := service.NewChannelMonitorService(monitors, cipher)
	policy := monitoringBudgetPolicy()
	policy.KeyRequestsPerMinute = 2
	_, err = NewMonitoringProbeBudgetRepository(db).SaveSettings(ctx, policy)
	require.NoError(t, err)
	params := service.ChannelMonitorCreateParams{Name: "credential-first", Provider: service.MonitorProviderOpenAI,
		Endpoint: "https://203.0.113.1/v1", APIKey: "first-fixture", PrimaryModel: "model-a", Enabled: true, IntervalSeconds: 60}
	first, err := svc.Create(ctx, params)
	require.NoError(t, err)
	params.Name, params.Endpoint = "credential-second", "https://203.0.113.2/v1"
	_, err = svc.Create(ctx, params)
	require.ErrorContains(t, err, "periodic_key_capacity", "cross-site copies do not multiply one key allowance")
	params.APIKey = "independent-fixture"
	second, err := svc.Create(ctx, params)
	require.NoError(t, err, "independent keys retain their individual capacity")
	before, err := monitors.GetByID(ctx, second.ID)
	require.NoError(t, err)
	key := "first-fixture"
	_, err = svc.Update(ctx, second.ID, service.ChannelMonitorUpdateParams{APIKey: &key})
	require.ErrorContains(t, err, "periodic_key_capacity")
	after, err := monitors.GetByID(ctx, second.ID)
	require.NoError(t, err)
	require.Equal(t, before, after, "a rejected credential edit rolls back ciphertext and evidence together")
	copy, err := svc.Duplicate(ctx, first.ID, 0, "fixture-admin", "credential-copy")
	require.NoError(t, err)
	require.False(t, copy.Enabled)
	storedCopy, err := monitors.GetByID(ctx, copy.ID)
	require.NoError(t, err)
	require.Equal(t, []string{config.UpstreamKeyFingerprint("first-fixture")}, service.ValidatedChannelMonitorBudgetCredentials(storedCopy))
	enabled := true
	_, err = svc.Update(ctx, copy.ID, service.ChannelMonitorUpdateParams{Enabled: &enabled})
	require.ErrorContains(t, err, "periodic_key_capacity")
	enabled = false
	_, err = svc.Update(ctx, first.ID, service.ChannelMonitorUpdateParams{Enabled: &enabled})
	require.NoError(t, err, "manual disabling stays available")
	enabled = true
	_, err = svc.Update(ctx, copy.ID, service.ChannelMonitorUpdateParams{Enabled: &enabled})
	require.NoError(t, err, "capacity is reusable after the original copy is disabled")
	for _, table := range []string{"monitoring_probe_budget_counters", "monitoring_probe_budget_leases", "channel_monitor_histories", "usage_logs"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count, "CRUD/preflight never send probes or create usage")
	}
}

func TestMonitoringProbeCoverageV1CredentialEvidencePostgres(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	require.NoError(t, NewSettingRepository(client).SetMultiple(ctx, map[string]string{
		service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1",
	}))
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("ab", 32)
	cipher, err := NewAESEncryptor(cfg)
	require.NoError(t, err)
	repo := NewChannelMonitorRepository(client, db)
	svc := service.NewChannelMonitorService(repo, cipher)
	m, err := svc.Create(ctx, service.ChannelMonitorCreateParams{Name: "legacy-credential", Provider: service.MonitorProviderOpenAI,
		Endpoint: "https://203.0.113.1/v1", APIKey: "legacy-fixture", PrimaryModel: "model-a", Enabled: true, IntervalSeconds: 60})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE channel_monitors SET extra_headers=extra_headers-$1 WHERE id=$2`, service.ChannelMonitorBudgetCredentialScopeMetadataKey, m.ID)
	require.NoError(t, err)
	legacy, err := repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	budget := NewMonitoringProbeBudgetRepository(db)
	preview, err := budget.PreviewCoverage(ctx, monitoringBudgetPolicy())
	require.NoError(t, err)
	require.EqualValues(t, 1, preview.PeriodicCredentials.UnresolvedActiveTargets)
	_, err = svc.Get(ctx, m.ID)
	require.NoError(t, err)
	unchanged, err := repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, legacy, unchanged, "GET and preview must not decrypt to publish or rewrite legacy evidence")
	policy := monitoringBudgetPolicy()
	policy.KeyRequestsPerMinute = 1
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err, "missing identity evidence alone cannot prove a shortfall")
	_, err = svc.ListEnabledMonitors(ctx)
	require.NoError(t, err, "original startup decrypts and backfills without an outgoing probe")
	current, err := repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, []string{config.UpstreamKeyFingerprint("legacy-fixture")}, service.ValidatedChannelMonitorBudgetCredentials(current))
	require.NotContains(t, current.ExtraHeaders, service.ChannelMonitorBudgetCredentialScopeMetadataKey)
	preview, err = budget.PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.False(t, preview.CanSave, "publication must reveal an existing shortfall, not reject the observation")
	require.Contains(t, preview.BlockingReasons, "periodic_key_capacity")
	withoutEvidence := *current
	withoutEvidence.BudgetCredentialScope = nil
	require.Equal(t, legacy, &withoutEvidence, "backfill changes no ciphertext, cadence, timestamps, manual controls or check result")
	stale := *current
	stale.BudgetCredentialScope = nil
	_, err = db.ExecContext(ctx, `UPDATE channel_monitors SET extra_headers=extra_headers || '{"Api-Key":"rotated-final-fixture"}'::jsonb WHERE id=$1`, m.ID)
	require.NoError(t, err)
	updated, err := repo.RecordBudgetCredentialScope(ctx, &stale)
	require.NoError(t, err)
	require.False(t, updated, "late observation cannot clear evidence for another header configuration")
	_, err = budget.PreviewCoverage(ctx, monitoringBudgetPolicy())
	require.ErrorIs(t, err, service.ErrMonitoringProbeBudgetConflict, "a fresh default must not bypass the saved revision")
	preview, err = budget.PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.EqualValues(t, 1, preview.PeriodicCredentials.UnresolvedActiveTargets)
	_, err = svc.ListEnabledMonitors(ctx)
	require.NoError(t, err)
	current, err = repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{config.UpstreamKeyFingerprint("legacy-fixture"), config.UpstreamKeyFingerprint("rotated-final-fixture")}, service.ValidatedChannelMonitorBudgetCredentials(current))
	_, err = db.ExecContext(ctx, `UPDATE channel_monitors SET api_key_encrypted='unreadable-fixture' WHERE id=$1`, m.ID)
	require.NoError(t, err)
	_, err = svc.ListEnabledMonitors(ctx)
	require.NoError(t, err)
	current, err = repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	require.Nil(t, current.BudgetCredentialScope, "failed decryption must not retain or regenerate a known key")
	for _, table := range []string{"monitoring_probe_budget_counters", "monitoring_probe_budget_leases", "channel_monitor_histories", "usage_logs"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count)
	}
}

func TestMonitoringProbeCoverageV1CredentialSerializeWritesPostgres(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	require.NoError(t, NewSettingRepository(client).SetMultiple(ctx, map[string]string{
		service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1",
	}))
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("ab", 32)
	cipher, err := NewAESEncryptor(cfg)
	require.NoError(t, err)
	svc := service.NewChannelMonitorService(NewChannelMonitorRepository(client, db), cipher)
	var second int64
	for i := 0; i < 2; i++ {
		m, err := svc.Create(ctx, service.ChannelMonitorCreateParams{Name: fmt.Sprintf("serialize-key-%d", i),
			Provider: service.MonitorProviderOpenAI, Endpoint: "https://203.0.113.1/v1", APIKey: fmt.Sprintf("fixture-%d", i),
			PrimaryModel: "model-a", Enabled: true, IntervalSeconds: 60})
		require.NoError(t, err)
		second = m.ID
	}
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.KeyRequestsPerMinute = 4
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		policy.KeyRequestsPerMinute = 2
		_, err := budget.SaveSettings(ctx, policy)
		results <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		key := "fixture-0"
		_, err := svc.Update(ctx, second, service.ChannelMonitorUpdateParams{APIKey: &key})
		results <- err
	}()
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			require.ErrorContains(t, err, "periodic_key_capacity")
		}
	}
	require.Equal(t, 1, successes, "individually valid capacity and key changes must not commit an impossible combination")
	current, err := budget.GetSettings(ctx)
	require.NoError(t, err)
	preview, err := budget.PreviewCoverage(ctx, current)
	require.NoError(t, err)
	require.True(t, preview.CanSave)
}

func TestMonitoringProbeCoverageCountsScheduledQualityAndGroups(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	ctx := t.Context()
	var account, group int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status) VALUES('coverage-quality','openai','apikey','active') RETURNING id`).Scan(&account))
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,status) VALUES('coverage-group','openai','active') RETURNING id`).Scan(&group))
	config := `{"question_kind":"candy","parallel_count":3,"model_ids":["model-a","model-b"],"quality":{"action":"observe_only","judge":{"kind":"jev","prompt":"compare"}}}`
	_, err := db.ExecContext(ctx, `INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,pelican_config,next_run_at)
 VALUES($1,'model-a','0 * * * *',true,$2,NOW())`, account, config)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO pelican_group_test_plans(group_id,model_id,cron_expression,enabled,pelican_config,next_run_at)
 VALUES($1,'model-a','0 * * * *',true,'{"parallel_count":2}',NOW())`, group)
	require.NoError(t, err)
	repo := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 300 // quality: 24 * (2 models * 3 samples) * (answer + judge); group: 24 * 2
	_, err = repo.SaveSettings(ctx, policy)
	require.ErrorContains(t, err, "day_capacity", "the 336-call daily target must not fit in a 300-call budget")
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key=$1`, service.MonitoringProbeBudgetSettingKey).Scan(&count))
	require.Zero(t, count, "failed preflight must leave the policy unchanged")
	policy.RequestsPerDay = 336
	policy, err = repo.SaveSettings(ctx, policy)
	require.NoError(t, err)
	preview, err := NewMonitoringProbeBudgetRepository(db).PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.EqualValues(t, 336, preview.MinimumTotalRequestsPerDay)
	require.EqualValues(t, 192, preview.ScheduledSamples)
	require.EqualValues(t, 144, preview.MinimumJudgeRequests)
	_, err = db.ExecContext(ctx, `UPDATE accounts SET status='disabled' WHERE id=$1`, account)
	require.NoError(t, err)
	preview, err = repo.PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.EqualValues(t, 336, preview.MinimumTotalRequestsPerDay, "recovery plans are not excluded for disabled accounts")
	_, err = db.ExecContext(ctx, `UPDATE groups SET deleted_at=NOW() WHERE id=$1`, group)
	require.NoError(t, err)
	preview, err = repo.PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.EqualValues(t, 288, preview.MinimumTotalRequestsPerDay)
	for _, table := range []string{"monitoring_probe_budget_counters", "monitoring_probe_budget_leases", "scheduled_test_results", "pelican_group_test_results"} {
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count, "preview and configuration saves are not probe executions")
	}
}

func TestMonitoringProbeCoverageSerializesV1AndBudget(t *testing.T) {
	for _, operation := range []string{"create", "update", "enable", "mode", "bulk", "delete_mode", "delete_enabled"} {
		t.Run(operation, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			ctx := t.Context()
			settings := NewSettingRepository(client)
			mode, enabled := "v1", "true"
			if operation == "mode" || operation == "bulk" || operation == "delete_mode" {
				mode = "v2"
			}
			if operation == "delete_enabled" {
				enabled = "false"
			}
			require.NoError(t, settings.SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorMode: mode, service.SettingKeyChannelMonitorEnabled: enabled}))
			budget := NewMonitoringProbeBudgetRepository(db)
			policy := monitoringBudgetPolicy()
			policy.RequestsPerDay, policy.RecoveryReservePercent = 5000, 0
			policy, err := budget.SaveSettings(ctx, policy)
			require.NoError(t, err)
			monitor := &service.ChannelMonitor{Name: "serialize-v1", Provider: service.MonitorProviderOpenAI,
				Endpoint: "https://coverage.invalid/v1", APIKey: "private-fixture", PrimaryModel: "model-a", ExtraModels: []string{"model-b"},
				IntervalSeconds: 60, Enabled: operation != "enable"}
			if operation == "update" {
				monitor.IntervalSeconds = 120
			}
			if operation != "create" {
				require.NoError(t, NewChannelMonitorRepository(client, db).Create(ctx, monitor))
			}
			start, results := make(chan struct{}), make(chan error, 2)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				candidate := policy
				candidate.RequestsPerDay = 3000
				_, err := NewMonitoringProbeBudgetRepository(db).SaveSettings(ctx, candidate)
				results <- err
			}()
			go func() {
				defer wg.Done()
				<-start
				var err error
				switch operation {
				case "create":
					err = NewChannelMonitorRepository(client, db).Create(ctx, monitor)
				case "update", "enable":
					monitor.IntervalSeconds, monitor.Enabled = 60, true
					err = NewChannelMonitorRepository(client, db).Update(ctx, monitor)
				case "mode":
					err = NewSettingRepository(client).Set(ctx, service.SettingKeyChannelMonitorMode, "v1")
				case "bulk":
					err = NewSettingRepository(client).SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorMode: "v1", "v1_atomic_marker": "committed"})
				case "delete_mode":
					err = NewSettingRepository(client).Delete(ctx, service.SettingKeyChannelMonitorMode)
				case "delete_enabled":
					err = NewSettingRepository(client).Delete(ctx, service.SettingKeyChannelMonitorEnabled)
				}
				results <- err
			}()
			close(start)
			wg.Wait()
			close(results)
			var successes int
			for err := range results {
				if err == nil {
					successes++
				} else {
					require.ErrorContains(t, err, "day_capacity")
				}
			}
			require.Equal(t, 1, successes, "concurrent individually valid edits must not create an impossible combination")
			stored, err := budget.GetSettings(ctx)
			require.NoError(t, err)
			coverage, err := NewMonitoringProbeBudgetRepository(db).PreviewCoverage(ctx, stored)
			require.NoError(t, err)
			require.True(t, coverage.CanSave)
			require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorEnabled, "false"))
			stored.RequestsPerDay = 1
			_, err = budget.SaveSettings(ctx, stored)
			require.NoError(t, err, "disabling active probes remains a repair path")
		})
	}
}

func TestMonitoringProbeCoverageV1InventoryModesAndUnknown(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewChannelMonitorRepository(client, db)
	for _, mode := range []string{service.MonitorCheckModeProbe, service.MonitorCheckModeQuota, service.MonitorCheckModeQuotaProbe} {
		require.NoError(t, repo.Create(ctx, &service.ChannelMonitor{Name: mode, Provider: service.MonitorProviderOpenAI,
			APIKey: "private-fixture", PrimaryModel: "model-a", ExtraModels: []string{"model-b"}, Enabled: true, CheckMode: mode, IntervalSeconds: 60}))
	}
	budget := NewMonitoringProbeBudgetRepository(db)
	policy, err := budget.GetSettings(ctx)
	require.NoError(t, err)
	coverage, err := budget.PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.EqualValues(t, 3, coverage.ActiveMonitors, "absent runtime settings preserve the original default-on V1")
	require.EqualValues(t, 2, coverage.ActiveQuotaMonitors)
	require.EqualValues(t, 8640, coverage.MinimumActiveRequests)
	require.EqualValues(t, 2880, coverage.ActiveMonitorRuns)
	require.Contains(t, coverage.Warnings, "active_quota_unestimated")
	raw, err := json.Marshal(coverage)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "private-fixture")
	settings := NewSettingRepository(client)
	for _, mode := range []string{"v2", "v3"} {
		require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorMode, mode))
		coverage, err = budget.PreviewCoverage(ctx, policy)
		require.NoError(t, err)
		require.True(t, coverage.ActiveInventoryComplete)
		require.Zero(t, coverage.ActiveMonitors)
		require.Zero(t, coverage.MinimumActiveRequests)
	}
	require.NoError(t, settings.Delete(ctx, service.SettingKeyChannelMonitorMode))
	_, err = db.ExecContext(ctx, `UPDATE channel_monitors SET primary_model=' ' WHERE check_mode='probe'`)
	require.NoError(t, err)
	coverage, err = budget.PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.EqualValues(t, 1, coverage.UnresolvedActiveMonitors)
	require.EqualValues(t, 4320, coverage.MinimumActiveRequests)
	require.Contains(t, coverage.Warnings, "active_unresolved")
	_, err = db.ExecContext(ctx, `INSERT INTO channel_monitors(name,provider,endpoint,api_key_encrypted,primary_model,extra_models,enabled,interval_seconds,created_by)
 SELECT 'bounded-v1','openai','https://coverage.invalid/v1','private-fixture','model-a','[]'::jsonb,true,3600,0 FROM generate_series(1,$1)`, monitoringCoverageInventoryLimit)
	require.NoError(t, err)
	coverage, err = budget.PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.False(t, coverage.ActiveInventoryComplete)
	require.EqualValues(t, monitoringCoverageInventoryLimit, coverage.ActiveMonitors)
	require.Contains(t, coverage.Warnings, "active_inventory_truncated")
}

func TestMonitoringProbeCoverageIncludesActiveCandy(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	_, group := coverageScheduleOwners(t, db)
	settings := NewSettingRepository(client)
	require.NoError(t, settings.SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v2"}))
	v2 := NewChannelMonitorV2Repository(db)
	cfg, err := v2.GetConfig(ctx)
	require.NoError(t, err)
	cfg.Enabled = true
	cfg.Platforms = []service.ChannelMonitorV2PlatformConfig{{Platform: service.PlatformOpenAI, Enabled: true}}
	cfg.CandyProbes = []service.ChannelMonitorV2CandyProbe{{GroupID: group, Enabled: true, Model: "model-a", IntervalMinutes: 1}}
	_, err = v2.UpdateConfig(ctx, *cfg, cfg.Version)
	require.NoError(t, err)
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 1000
	_, err = budget.SaveSettings(ctx, policy)
	require.ErrorContains(t, err, "day_capacity", "one healthy candy sample per minute requires at least 1440 calls/day")
	policy.RequestsPerDay = 1440
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	preview, err := budget.PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.EqualValues(t, 1440, preview.MinimumTotalRequestsPerDay)
	require.EqualValues(t, 1, preview.CandyProbes)
	require.EqualValues(t, 1440, preview.MinimumCandyRequests)
	require.True(t, preview.CandyInventoryComplete)
	require.Zero(t, preview.MinimumScheduledRequests, "candy must not masquerade as a saved account/group plan")
	for _, table := range []string{"channel_monitor_v2_candy_results", "monitoring_probe_budget_counters", "monitoring_probe_budget_leases"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count, "preflight does not execute or bill a probe")
	}
}

func TestMonitoringProbeCoverageSerializesCandyAndBudget(t *testing.T) {
	for _, operation := range []string{"config", "mode", "bulk", "delete_enabled"} {
		t.Run(operation, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			ctx := t.Context()
			_, group := coverageScheduleOwners(t, db)
			settings := NewSettingRepository(client)
			require.NoError(t, settings.SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1"}))
			budget := NewMonitoringProbeBudgetRepository(db)
			policy := monitoringBudgetPolicy()
			policy.RequestsPerDay = 2000
			policy, err := budget.SaveSettings(ctx, policy)
			require.NoError(t, err)
			v2 := NewChannelMonitorV2Repository(db)
			cfg, err := v2.GetConfig(ctx)
			require.NoError(t, err)
			cfg.Enabled = operation != "config"
			cfg.Platforms = []service.ChannelMonitorV2PlatformConfig{{Platform: service.PlatformOpenAI, Enabled: true}}
			cfg.CandyProbes = []service.ChannelMonitorV2CandyProbe{{GroupID: group, Enabled: true, Model: "model-a", IntervalMinutes: 1}}
			cfg, err = v2.UpdateConfig(ctx, *cfg, cfg.Version)
			require.NoError(t, err)
			if operation == "config" {
				require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorMode, "v2"))
			}
			if operation == "delete_enabled" {
				require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorEnabled, "false"))
				require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorMode, "v2"))
			}
			start, results := make(chan struct{}), make(chan error, 2)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				policy.RequestsPerDay = 1000
				_, err := budget.SaveSettings(ctx, policy)
				results <- err
			}()
			go func() {
				defer wg.Done()
				<-start
				var err error
				switch operation {
				case "config":
					cfg.Enabled = true
					_, err = v2.UpdateConfig(ctx, *cfg, cfg.Version)
				case "mode":
					err = settings.Set(ctx, service.SettingKeyChannelMonitorMode, "v2")
				case "bulk":
					err = settings.SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorMode: "v2", "coverage_atomic_marker": "committed"})
				case "delete_enabled":
					err = settings.Delete(ctx, service.SettingKeyChannelMonitorEnabled)
				}
				results <- err
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
			require.Equal(t, 1, successes, "individually valid edits cannot commit an impossible combination")
			policy, err = budget.GetSettings(ctx)
			require.NoError(t, err)
			preview, err := budget.PreviewCoverage(ctx, policy)
			require.NoError(t, err)
			require.True(t, preview.CanSave)
			// Disabling remains available without changing the usage counters.
			require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorEnabled, "false"))
			cfg, err = v2.GetConfig(ctx)
			require.NoError(t, err)
			cfg.Enabled = false
			_, err = v2.UpdateConfig(ctx, *cfg, cfg.Version)
			require.NoError(t, err)
			policy.RequestsPerDay = 1
			_, err = budget.SaveSettings(ctx, policy)
			require.NoError(t, err)
		})
	}
}

func TestMonitoringProbeCoverageCandyReadsExecutableScope(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	_, group := coverageScheduleOwners(t, db)
	var composite, inactive, deleted, wrongPlatform int64
	for _, item := range []struct {
		id       *int64
		platform string
		status   string
	}{{&composite, service.PlatformComposite, "active"}, {&inactive, service.PlatformOpenAI, "inactive"}, {&deleted, service.PlatformOpenAI, "active"}, {&wrongPlatform, service.PlatformAnthropic, "active"}} {
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,status) VALUES('candy-scope-'||$1||'-'||$2,$1,$2) RETURNING id`, item.platform, item.status).Scan(item.id))
	}
	_, err := db.ExecContext(ctx, `UPDATE groups SET deleted_at=NOW() WHERE id=$1`, deleted)
	require.NoError(t, err)
	settings := NewSettingRepository(client)
	require.NoError(t, settings.SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v2"}))
	v2 := NewChannelMonitorV2Repository(db)
	cfg, err := v2.GetConfig(ctx)
	require.NoError(t, err)
	cfg.Enabled = true
	cfg.Platforms = []service.ChannelMonitorV2PlatformConfig{{Platform: service.PlatformOpenAI, Enabled: true, Models: []string{"display-only"}}}
	for _, id := range []int64{group, composite, inactive, deleted, wrongPlatform, -1} {
		cfg.CandyProbes = append(cfg.CandyProbes, service.ChannelMonitorV2CandyProbe{GroupID: id, Enabled: true, Model: "probe-model", IntervalMinutes: 1})
	}
	cfg.CandyProbes = append(cfg.CandyProbes, service.ChannelMonitorV2CandyProbe{GroupID: group, Enabled: false, Model: "disabled", IntervalMinutes: 1})
	cfg, err = v2.UpdateConfig(ctx, *cfg, cfg.Version)
	require.NoError(t, err)
	budget := NewMonitoringProbeBudgetRepository(db)
	policy, err := budget.GetSettings(ctx)
	require.NoError(t, err)
	check := func(want int64) {
		t.Helper()
		preview, err := NewMonitoringProbeBudgetRepository(db).PreviewCoverage(ctx, policy)
		require.NoError(t, err)
		require.Equal(t, want, preview.CandyProbes)
		require.Equal(t, want*1440, preview.MinimumCandyRequests)
		require.True(t, preview.CandyInventoryComplete)
	}
	check(2)
	cfg.GroupIDs = []int64{group}
	cfg, err = v2.UpdateConfig(ctx, *cfg, cfg.Version)
	require.NoError(t, err)
	check(1)
	require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorMode, "v3"))
	check(0)
	require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorMode, " V2 "))
	check(1)
	require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorEnabled, " off "))
	check(0)
	require.NoError(t, settings.Delete(ctx, service.SettingKeyChannelMonitorEnabled))
	check(1)
	cfg.CandyProbes[0].IntervalMinutes = 0 // Historical invalid data stays unresolved, not free.
	cfg, err = v2.UpdateConfig(ctx, *cfg, cfg.Version)
	require.NoError(t, err)
	preview, err := budget.PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.EqualValues(t, 1, preview.UnresolvedCandyProbes)
	require.Contains(t, preview.Warnings, "candy_unresolved")
	require.Zero(t, preview.MinimumCandyRequests)
}

func TestMonitoringProbeCoverageCandyConfigAndModeRollback(t *testing.T) {
	for _, mode := range []string{"config", "mode", "bulk", "delete_enabled"} {
		t.Run(mode, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			ctx := t.Context()
			_, group := coverageScheduleOwners(t, db)
			settings := NewSettingRepository(client)
			require.NoError(t, settings.SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1"}))
			budget := NewMonitoringProbeBudgetRepository(db)
			policy := monitoringBudgetPolicy()
			policy.RequestsPerDay = 1000
			_, err := budget.SaveSettings(ctx, policy)
			require.NoError(t, err)
			v2 := NewChannelMonitorV2Repository(db)
			cfg, err := v2.GetConfig(ctx)
			require.NoError(t, err)
			cfg.Enabled = mode != "config"
			cfg.Platforms = []service.ChannelMonitorV2PlatformConfig{{Platform: service.PlatformOpenAI, Enabled: true}}
			cfg.CandyProbes = []service.ChannelMonitorV2CandyProbe{{GroupID: group, Enabled: true, Model: "model-a", IntervalMinutes: 1}}
			cfg, err = v2.UpdateConfig(ctx, *cfg, cfg.Version)
			require.NoError(t, err, "inactive V2 configuration remains editable")
			switch mode {
			case "config":
				require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorMode, "v2"))
				cfg.Enabled = true
				_, err = v2.UpdateConfig(ctx, *cfg, cfg.Version)
			case "mode":
				err = settings.Set(ctx, service.SettingKeyChannelMonitorMode, "v2")
			case "bulk":
				err = settings.SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorMode: "v2", "coverage_atomic_marker": "must-rollback"})
			case "delete_enabled":
				require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorEnabled, "false"))
				require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorMode, "v2"))
				err = settings.Delete(ctx, service.SettingKeyChannelMonitorEnabled)
			}
			require.ErrorContains(t, err, "day_capacity")
			read, err := v2.GetConfig(ctx)
			require.NoError(t, err)
			require.Equal(t, cfg.Version, read.Version, "a rejected save cannot advance the optimistic version")
			if mode == "config" {
				require.False(t, read.Enabled)
			}
			values, err := settings.GetMultiple(ctx, []string{service.SettingKeyChannelMonitorMode, service.SettingKeyChannelMonitorEnabled, "coverage_atomic_marker"})
			require.NoError(t, err)
			require.Empty(t, values["coverage_atomic_marker"])
			if mode == "mode" || mode == "bulk" {
				require.Equal(t, "v1", values[service.SettingKeyChannelMonitorMode])
			}
			if mode == "delete_enabled" {
				require.Equal(t, "false", values[service.SettingKeyChannelMonitorEnabled])
			}
		})
	}
}

func coverageScheduleOwners(t *testing.T, db *sql.DB) (int64, int64) {
	t.Helper()
	var account, group int64
	require.NoError(t, db.QueryRowContext(t.Context(), `INSERT INTO accounts(name,platform,type,status) VALUES('coverage-schedule','openai','apikey','active') RETURNING id`).Scan(&account))
	require.NoError(t, db.QueryRowContext(t.Context(), `INSERT INTO groups(name,platform,status) VALUES('coverage-schedule','openai','active') RETURNING id`).Scan(&group))
	return account, group
}

func coverageAccountPlan(account int64) *service.ScheduledTestPlan {
	now := time.Now().UTC()
	return &service.ScheduledTestPlan{AccountID: account, ModelID: "model-a", CronExpression: "0 * * * *", Enabled: true, MaxResults: 100, NextRunAt: &now,
		PelicanConfig: &service.PelicanTestConfig{QuestionKind: "pelican", Prompt: "draw", ParallelCount: 2, ReasoningEffort: "high"}}
}

func TestMonitoringProbeCoverageSerializesAccountAndGroupSchedules(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	ctx := t.Context()
	account, group := coverageScheduleOwners(t, db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 70
	_, err := NewMonitoringProbeBudgetRepository(db).SaveSettings(ctx, policy)
	require.NoError(t, err)
	start, results := make(chan struct{}), make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := NewScheduledTestPlanRepository(db).Create(ctx, coverageAccountPlan(account))
		results <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		plan := newPelicanGroupTestPlan(group, true, time.Now().UTC())
		plan.CronExpression = "0 * * * *"
		_, err := NewPelicanGroupTestRepository(db).CreatePlan(ctx, plan)
		results <- err
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
	require.Equal(t, 1, successes, "two individually valid 48-call plans must not commit a 96-call combination")
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM scheduled_test_plans)+(SELECT count(*) FROM pelican_group_test_plans)`).Scan(&count))
	require.Equal(t, 1, count, "the rejected write rolls back")
}

func TestMonitoringProbeCoverageScheduleUpdatesRollbackAndDisable(t *testing.T) {
	for _, groupPlan := range []bool{false, true} {
		t.Run(fmt.Sprint(groupPlan), func(t *testing.T) {
			db, _ := newBillingAuditDatabase(t)
			ctx := t.Context()
			account, group := coverageScheduleOwners(t, db)
			budget := NewMonitoringProbeBudgetRepository(db)
			policy := monitoringBudgetPolicy()
			policy.RequestsPerDay = 70
			policy, err := budget.SaveSettings(ctx, policy)
			require.NoError(t, err)
			if groupPlan {
				repo := NewPelicanGroupTestRepository(db)
				input := newPelicanGroupTestPlan(group, true, time.Now().UTC())
				input.CronExpression = "0 * * * *"
				plan, err := repo.CreatePlan(ctx, input)
				require.NoError(t, err)
				plan.PelicanConfig.ParallelCount = 3
				_, err = repo.UpdatePlan(ctx, plan)
				require.ErrorContains(t, err, "day_capacity")
				read, err := repo.GetPlan(ctx, plan.ID)
				require.NoError(t, err)
				require.Equal(t, 2, read.PelicanConfig.ParallelCount)
				plan.Enabled = false
				_, err = repo.UpdatePlan(ctx, plan)
				require.NoError(t, err)
			} else {
				repo := NewScheduledTestPlanRepository(db)
				plan, err := repo.Create(ctx, coverageAccountPlan(account))
				require.NoError(t, err)
				plan.PelicanConfig.ParallelCount = 3
				_, err = repo.Update(ctx, plan)
				require.ErrorContains(t, err, "day_capacity")
				read, err := repo.GetByID(ctx, plan.ID)
				require.NoError(t, err)
				require.Equal(t, 2, read.PelicanConfig.ParallelCount)
				plan.Enabled = false
				_, err = repo.Update(ctx, plan)
				require.NoError(t, err)
			}
			preview, err := budget.PreviewCoverage(ctx, policy)
			require.NoError(t, err)
			require.Zero(t, preview.MinimumScheduledRequests)
		})
	}
}

func TestMonitoringProbeCoverageTemplateExpansionIsAtomic(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	account, _ := coverageScheduleOwners(t, db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 20
	budget := NewMonitoringProbeBudgetRepository(db)
	policy, err := budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	var template service.QualityRuleTemplate
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO quality_rule_templates(account_filter,model_id,cron_expression,enabled,max_results,pelican_config)
 VALUES('{}','model-a','0 * * * *',true,100,'{}') RETURNING id,updated_at`).Scan(&template.ID, &template.UpdatedAt))
	plan := coverageAccountPlan(account)
	repo := NewQualityRuleTemplateRepository(client, db)
	_, err = repo.CreateLinkedPlan(ctx, &template, plan)
	require.ErrorContains(t, err, "day_capacity")
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM scheduled_test_plans)+(SELECT count(*) FROM quality_rule_template_accounts)`).Scan(&count))
	require.Zero(t, count, "insufficient capacity must not orphan a template link or plan")
	policy.RequestsPerDay = 100
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	_, err = repo.CreateLinkedPlan(ctx, &template, plan)
	require.NoError(t, err)
	_, err = repo.CreateLinkedPlan(ctx, &template, plan)
	require.ErrorIs(t, err, service.ErrQualityTemplateSkipped)
}

func TestMonitoringProbeCoverageSerializesBudgetAndScheduleUpdate(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	ctx := t.Context()
	account, _ := coverageScheduleOwners(t, db)
	repo := NewScheduledTestPlanRepository(db)
	input := coverageAccountPlan(account)
	input.PelicanConfig.ParallelCount = 1
	plan, err := repo.Create(ctx, input)
	require.NoError(t, err)
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 64
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	policy.RequestsPerDay = 32
	plan.PelicanConfig.ParallelCount = 2
	start, results := make(chan struct{}), make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _, err := budget.SaveSettings(ctx, policy); results <- err }()
	go func() { defer wg.Done(); <-start; _, err := repo.Update(ctx, plan); results <- err }()
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
	require.Equal(t, 1, successes, "a 48-call plan and 32-call budget must not both commit")
	current, err := budget.GetSettings(ctx)
	require.NoError(t, err)
	preview, err := budget.PreviewCoverage(ctx, current)
	require.NoError(t, err)
	require.True(t, preview.CanSave)
}

func TestMonitoringProbeCoverageInitialAccountQualityIsAtomic(t *testing.T) {
	for _, mode := range []string{"create", "groups", "caller_transaction"} {
		t.Run(mode, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			ctx := t.Context()
			policy := monitoringBudgetPolicy()
			policy.RequestsPerDay = 20
			budget := NewMonitoringProbeBudgetRepository(db)
			policy, err := budget.SaveSettings(ctx, policy)
			require.NoError(t, err)
			_, group := coverageScheduleOwners(t, db)
			account := &service.Account{Name: "initial-coverage", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Status: service.StatusActive, Concurrency: 1, Schedulable: true, InitialQualityPlan: coverageAccountPlan(0)}
			create := func() error {
				repo := newAccountRepositoryWithSQL(client, db, nil)
				if mode == "caller_transaction" {
					tx, err := client.Tx(ctx)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					if err := repo.Create(dbent.NewTxContext(ctx, tx), account); err != nil {
						return err
					}
					return tx.Commit()
				}
				if mode == "groups" {
					return repo.CreateWithAccountGroups(ctx, account, []service.AccountGroup{{GroupID: group, Priority: 7}})
				}
				return repo.Create(ctx, account)
			}
			assertCount := func(query string, expected int) {
				t.Helper()
				var count int
				require.NoError(t, db.QueryRowContext(ctx, query).Scan(&count))
				require.Equal(t, expected, count)
			}
			assertNewAccount := func(expected int) {
				t.Helper()
				assertCount(`SELECT count(*) FROM accounts WHERE name='initial-coverage'`, expected)
				assertCount(`SELECT count(*) FROM scheduled_test_plans`, expected)
			}
			err = create()
			require.ErrorContains(t, err, "day_capacity", "the initial quality rule must not bypass the ordinary plan capacity gate")
			assertNewAccount(0)
			assertCount(`SELECT count(*) FROM account_groups`, 0)
			assertCount(`SELECT count(*) FROM scheduler_outbox`, 0)
			policy.RequestsPerDay = 70
			_, err = budget.SaveSettings(ctx, policy)
			require.NoError(t, err)
			require.NoError(t, create())
			assertNewAccount(1)
			if mode == "groups" {
				assertCount(`SELECT count(*) FROM account_groups`, 1)
			}
		})
	}
}

func TestMonitoringProbeCoverageInitialQualityAndBudgetSerialize(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 70
	policy, err := budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	policy.RequestsPerDay = 20
	start, results := make(chan struct{}), make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		results <- newAccountRepositoryWithSQL(client, db, nil).Create(ctx, &service.Account{
			Name: "initial-coverage", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Concurrency: 1, Schedulable: true, InitialQualityPlan: coverageAccountPlan(0)})
	}()
	go func() { defer wg.Done(); <-start; _, err := budget.SaveSettings(ctx, policy); results <- err }()
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
	require.Equal(t, 1, successes, "the initial 48-call rule and a 20-call budget must not both commit")
	current, err := budget.GetSettings(ctx)
	require.NoError(t, err)
	preview, err := budget.PreviewCoverage(ctx, current)
	require.NoError(t, err)
	require.True(t, preview.CanSave)
}

func TestMonitoringProbeCoverageInitialQualityDisableAndPlainCreate(t *testing.T) {
	for _, mode := range []string{"disabled_plan", "disabled_enforcement", "plain_account"} {
		t.Run(mode, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			ctx := t.Context()
			policy := monitoringBudgetPolicy()
			policy.Enabled = mode != "disabled_enforcement"
			policy.RequestsPerDay = 1
			budget := NewMonitoringProbeBudgetRepository(db)
			policy, err := budget.SaveSettings(ctx, policy)
			require.NoError(t, err)
			account := &service.Account{Name: "initial-coverage", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Status: service.StatusActive, Concurrency: 1, Schedulable: true, InitialQualityPlan: coverageAccountPlan(0)}
			if mode == "plain_account" {
				account.InitialQualityPlan = nil
			} else if mode == "disabled_plan" {
				account.InitialQualityPlan.Enabled = false
			}
			require.NoError(t, newAccountRepositoryWithSQL(client, db, nil).Create(ctx, account))
			preview, err := budget.PreviewCoverage(ctx, policy)
			require.NoError(t, err)
			if mode == "disabled_enforcement" {
				require.EqualValues(t, 48, preview.MinimumScheduledRequests)
			} else {
				require.Zero(t, preview.MinimumScheduledRequests)
			}
			require.True(t, preview.CanSave)
		})
	}
}

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

func TestMonitoringProbeCoverageBillingScopesPostgres(t *testing.T) {
	for _, scope := range []string{"site", "key", "additional_header"} {
		t.Run(scope, func(t *testing.T) {
			db, _ := newBillingAuditDatabase(t)
			ctx := t.Context()
			for i := 0; i < 3; i++ {
				credentials := map[string]any{"api_key": "shared-coverage-key", "base_url": "https://coverage.invalid/tenant/v1"}
				if scope == "site" {
					credentials["api_key"] = fmt.Sprintf("independent-%d", i)
				} else {
					credentials["base_url"] = fmt.Sprintf("https://coverage-%d.invalid/tenant/v1", i)
				}
				if scope == "additional_header" {
					credentials["api_key"] = fmt.Sprintf("unused-stored-%d", i)
					credentials["header_override_enabled"] = true
					credentials["header_overrides"] = map[string]string{"api-key": "shared-coverage-key"}
				}
				raw, err := json.Marshal(credentials)
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra)
 VALUES($1,'openai','apikey','active',$2,'{"upstream_billing_probe_enabled":true}')`, fmt.Sprintf("scope-%d", i), string(raw))
				require.NoError(t, err)
			}
			repo := NewMonitoringProbeBudgetRepository(db)
			require.NoError(t, repo.SaveBillingProbeSettings(ctx, service.UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}))
			policy := monitoringBudgetPolicy()
			reason := "key_capacity"
			if scope == "site" {
				policy.SiteRequestsPerMinute, reason = 1, "site_capacity"
			} else {
				policy.KeyRequestsPerMinute = 1
			}
			preview, err := repo.PreviewCoverage(ctx, policy)
			require.NoError(t, err)
			require.False(t, preview.CanSave)
			reasons := []string{reason}
			if scope == "site" {
				reasons = append(reasons, "periodic_site_capacity")
			} else {
				reasons = append(reasons, "periodic_key_capacity")
			}
			require.Equal(t, reasons, preview.BlockingReasons)
			_, err = repo.SaveSettings(ctx, policy)
			require.ErrorContains(t, err, reason)
			stored, err := repo.GetSettings(ctx)
			require.NoError(t, err)
			require.Zero(t, stored.Revision, "rejected preflight must not write or increment revision")
			policy.SiteRequestsPerMinute, policy.KeyRequestsPerMinute = 2, 2
			policy, err = repo.SaveSettings(ctx, policy)
			require.NoError(t, err)
			summary, err := NewMonitoringProbeBudgetRepository(db).Summary(ctx)
			require.NoError(t, err)
			require.True(t, summary.Coverage.CanSave)
			require.EqualValues(t, 6, summary.Coverage.MinimumRequestsPerSweep)
			require.EqualValues(t, 10, summary.Coverage.BillingScopes.CredentialSweepCapacity)
			raw, err := json.Marshal(summary)
			require.NoError(t, err)
			for _, private := range []string{"shared-coverage-key", "coverage.invalid", "unused-stored-", "coverage-0.invalid"} {
				require.NotContains(t, string(raw), private)
			}
			for _, table := range []string{"monitoring_probe_budget_counters", "monitoring_probe_budget_leases", "usage_logs"} {
				var count int
				require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
				require.Zero(t, count, "capacity preview/save must not probe, reserve, bill or consume tokens")
			}
		})
	}
}

func TestMonitoringProbeCoveragePeriodicSitesPostgres(t *testing.T) {
	for _, tc := range []struct {
		name, billingSite, monitorSite string
		sites, maximum, excess         int64
	}{
		{"same_tenant", "https://coverage.invalid/tenant/v1", "https://COVERAGE.invalid:443/tenant/v1/", 2, 1536, 1},
		{"head_origin", "https://coverage.invalid/v1", "https://coverage.invalid/tenant/v1", 2, 1536, 1},
		{"separate_tenant", "https://coverage.invalid/another/v1", "https://coverage.invalid/tenant/v1", 3, 1440, 0},
		{"separate_port", "https://coverage.invalid:8443/tenant/v1", "https://coverage.invalid/tenant/v1", 3, 1440, 0},
		{"v1_only", "", "https://coverage.invalid/v1", 1, 2880, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			ctx := t.Context()
			if tc.billingSite != "" {
				credentials, err := json.Marshal(map[string]any{"api_key": "private-periodic-key", "base_url": tc.billingSite})
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra)
 VALUES('periodic-billing','openai','apikey','active',$1,'{"upstream_billing_probe_enabled":true}')`, string(credentials))
				require.NoError(t, err)
			}
			require.NoError(t, NewSettingRepository(client).SetMultiple(ctx, map[string]string{
				service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1",
			}))
			repo := NewMonitoringProbeBudgetRepository(db)
			require.NoError(t, repo.SaveBillingProbeSettings(ctx, service.UpstreamBillingProbeSettings{Enabled: tc.billingSite != "", IntervalMinutes: 30}))
			monitor := &service.ChannelMonitor{Name: "periodic-v1", Provider: service.MonitorProviderOpenAI, Endpoint: tc.monitorSite,
				APIKey: "private-v1-key", PrimaryModel: "model-a", IntervalSeconds: 60, Enabled: true}
			require.NoError(t, NewChannelMonitorRepository(client, db).Create(ctx, monitor))
			var before string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(m)::text FROM channel_monitors m WHERE id=$1`, monitor.ID).Scan(&before))
			policy := monitoringBudgetPolicy()
			policy.SiteRequestsPerMinute, policy.RequestsPerDay, policy.RecoveryReservePercent = 1, 10000, 0
			preview, err := repo.PreviewCoverage(ctx, policy)
			require.NoError(t, err)
			require.EqualValues(t, tc.sites, preview.PeriodicSites.Sites)
			require.EqualValues(t, tc.maximum, preview.PeriodicSites.MaxRequestsPerDay)
			require.EqualValues(t, tc.excess, preview.PeriodicSites.OverLimitSites)
			require.Zero(t, preview.PeriodicSites.UnresolvedActiveTargets, "configured endpoints must reach the compiler without credentials")
			require.Zero(t, preview.PeriodicSites.UnresolvedBillingTargets)
			_, err = repo.SaveSettings(ctx, policy)
			if tc.excess > 0 {
				require.ErrorContains(t, err, "periodic_site_capacity")
				var count int
				require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key=$1`, service.MonitoringProbeBudgetSettingKey).Scan(&count))
				require.Zero(t, count, "rejected saves do not create a policy or revision")
			} else {
				require.NoError(t, err)
			}
			policy, err = repo.GetSettings(ctx)
			require.NoError(t, err)
			policy.SiteRequestsPerMinute, policy.RequestsPerDay, policy.RecoveryReservePercent, policy.Enabled = 2, 10000, 0, true
			policy, err = repo.SaveSettings(ctx, policy)
			require.NoError(t, err, "increasing site capacity repairs both families without changing cadence")
			summary, err := NewMonitoringProbeBudgetRepository(db).Summary(ctx)
			require.NoError(t, err)
			require.True(t, summary.Coverage.CanSave)
			require.EqualValues(t, tc.maximum, summary.Coverage.PeriodicSites.MaxRequestsPerDay)
			raw, err := json.Marshal(summary)
			require.NoError(t, err)
			for _, private := range []string{"coverage.invalid", "private-periodic-key", "private-v1-key"} {
				require.NotContains(t, string(raw), private)
			}
			policy.Enabled, policy.SiteRequestsPerMinute = false, 1
			_, err = repo.SaveSettings(ctx, policy)
			require.NoError(t, err, "disabling enforcement remains available")
			var after string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(m)::text FROM channel_monitors m WHERE id=$1`, monitor.ID).Scan(&after))
			require.Equal(t, before, after, "preflight does not rewrite monitor credentials, conclusions or cadence")
			for _, table := range []string{"monitoring_probe_budget_counters", "monitoring_probe_budget_leases", "usage_logs", "channel_monitor_histories"} {
				var count int
				require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
				require.Zero(t, count, "preview/save does not execute probes or consume business usage")
			}
		})
	}
}

func TestMonitoringProbeCoveragePeriodicSitesSerializeWrites(t *testing.T) {
	for _, operation := range []string{"monitor_interval", "enable_billing", "v1_mode"} {
		t.Run(operation, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			ctx := t.Context()
			_, err := db.ExecContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra)
 VALUES('periodic-race','openai','apikey','active','{"api_key":"periodic-race-key","base_url":"https://periodic-race.invalid/v1"}',
 '{"upstream_billing_probe_enabled":true}')`)
			require.NoError(t, err)
			mode := "v1"
			if operation == "v1_mode" {
				mode = "v2"
			}
			settings := NewSettingRepository(client)
			require.NoError(t, settings.SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: mode}))
			repo := NewMonitoringProbeBudgetRepository(db)
			require.NoError(t, repo.SaveBillingProbeSettings(ctx, service.UpstreamBillingProbeSettings{Enabled: operation != "enable_billing", IntervalMinutes: 30}))
			monitor := &service.ChannelMonitor{Name: "periodic-race-v1", Provider: service.MonitorProviderOpenAI,
				Endpoint: "https://periodic-race.invalid/v1", APIKey: "private-v1-key", PrimaryModel: "model-a", IntervalSeconds: 60, Enabled: true}
			if operation == "monitor_interval" {
				monitor.IntervalSeconds = 120
			}
			require.NoError(t, NewChannelMonitorRepository(client, db).Create(ctx, monitor))
			policy := monitoringBudgetPolicy()
			policy.SiteRequestsPerMinute, policy.RequestsPerDay, policy.RecoveryReservePercent = 3, 10000, 0
			policy, err = repo.SaveSettings(ctx, policy)
			require.NoError(t, err)
			start, results := make(chan struct{}), make(chan error, 2)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				policy.SiteRequestsPerMinute = 2
				_, err := NewMonitoringProbeBudgetRepository(db).SaveSettings(ctx, policy)
				results <- err
			}()
			go func() {
				defer wg.Done()
				<-start
				var err error
				switch operation {
				case "monitor_interval":
					monitor.IntervalSeconds = 60
					err = NewChannelMonitorRepository(client, db).Update(ctx, monitor)
				case "enable_billing":
					err = NewMonitoringProbeBudgetRepository(db).SaveBillingProbeSettings(ctx, service.UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 30})
				case "v1_mode":
					err = settings.Set(ctx, service.SettingKeyChannelMonitorMode, "v1")
				}
				results <- err
			}()
			close(start)
			wg.Wait()
			close(results)
			successes := 0
			for err := range results {
				if err == nil {
					successes++
				} else {
					require.ErrorContains(t, err, "periodic_site_capacity")
				}
			}
			require.Equal(t, 1, successes, "two valid changes must not commit an impossible combined demand")
			current, err := repo.GetSettings(ctx)
			require.NoError(t, err)
			preview, err := repo.PreviewCoverage(ctx, current)
			require.NoError(t, err)
			require.True(t, preview.CanSave)
		})
	}
}

func TestMonitoringProbeCoverageBillingManagementProfilesPostgres(t *testing.T) {
	for _, shared := range []bool{true, false} {
		t.Run(fmt.Sprintf("shared=%t", shared), func(t *testing.T) {
			db, _ := newBillingAuditDatabase(t)
			ctx := t.Context()
			var firstID int64
			for i := 0; i < 2; i++ {
				a := &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
					Credentials: map[string]any{"api_key": "inference-fixture", "base_url": "https://coverage.invalid/v1"}}
				raw, err := json.Marshal(a.Credentials)
				require.NoError(t, err)
				require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra)
 VALUES($1,'openai','apikey','active',$2,'{"upstream_billing_probe_enabled":true}') RETURNING id`, fmt.Sprintf("management-%d", i), string(raw)).Scan(&a.ID))
				authID := firstID
				if i == 0 || !shared {
					require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO new_api_site_authorizations(site_url,upstream_user_id,access_token_ciphertext)
 VALUES('https://coverage.invalid',$1,'intentionally-identical-opaque-ciphertext') RETURNING id`, 40+i).Scan(&authID))
					firstID = authID
				}
				_, err = db.ExecContext(ctx, `INSERT INTO new_api_account_bindings(account_id,authorization_id,upstream_token_id,account_fingerprint)
 VALUES($1,$2,$3,$4)`, a.ID, authID, 20+i, service.NewAPIAccountFingerprint(a))
				require.NoError(t, err)
			}
			repo := NewMonitoringProbeBudgetRepository(db)
			require.NoError(t, repo.SaveBillingProbeSettings(ctx, service.UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}))
			policy := monitoringBudgetPolicy()
			policy.KeyRequestsPerMinute = 1
			preview, err := repo.PreviewCoverage(ctx, policy)
			require.NoError(t, err)
			require.EqualValues(t, 8, preview.MinimumRequestsPerSweep)
			require.Zero(t, preview.BillingScopes.UnresolvedNewAPIAuthorizations)
			if shared {
				require.EqualValues(t, 1, preview.BillingScopes.NewAPIAuthorizationGroups)
				require.EqualValues(t, 8, preview.BillingScopes.MaxCredentialRequestsPerSweep)
				require.False(t, preview.CanSave)
				_, err = repo.SaveSettings(ctx, policy)
				require.ErrorContains(t, err, "key_capacity")
				policy.Enabled = false
			} else {
				require.EqualValues(t, 2, preview.BillingScopes.NewAPIAuthorizationGroups)
				require.EqualValues(t, 4, preview.BillingScopes.MaxCredentialRequestsPerSweep)
				require.True(t, preview.CanSave, "opaque ciphertext equality is not credential identity")
			}
			_, err = repo.SaveSettings(ctx, policy)
			require.NoError(t, err)
		})
	}
}

func TestMonitoringProbeCoverageBillingScopesSerializePolicyCadence(t *testing.T) {
	for _, scope := range []string{"site", "key"} {
		t.Run(scope, func(t *testing.T) {
			db, _ := newBillingAuditDatabase(t)
			ctx := t.Context()
			_, err := db.ExecContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra)
 SELECT 'scope-race-'||n,'openai','apikey','active',
 '{"api_key":"scope-race-key","base_url":"https://scope-race.invalid/v1"}'::jsonb,
 '{"upstream_billing_probe_enabled":true}'::jsonb FROM generate_series(1,3) n`)
			require.NoError(t, err)
			repo := NewMonitoringProbeBudgetRepository(db)
			require.NoError(t, repo.SaveBillingProbeSettings(ctx, service.UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 10}))
			policy := monitoringBudgetPolicy()
			policy.SiteRequestsPerMinute, policy.KeyRequestsPerMinute = 2, 2
			policy, err = repo.SaveSettings(ctx, policy)
			require.NoError(t, err)
			if scope == "site" {
				policy.SiteRequestsPerMinute = 1
			} else {
				policy.KeyRequestsPerMinute = 1
			}
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
					require.ErrorContains(t, err, scope+"_capacity")
				}
			}
			require.Equal(t, 1, successes, "individually valid changes must not commit an incompatible pair")
			current, err := repo.GetSettings(ctx)
			require.NoError(t, err)
			preview, err := repo.PreviewCoverage(ctx, current)
			require.NoError(t, err)
			require.True(t, preview.CanSave)
		})
	}
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
	sources := []string{"account_quota_cycle", "ollama_usage", "responses_capability", "event_diagnostics", "v3_idle_probes"}
	require.ElementsMatch(t, sources, preview.UnestimatedDemandSources)
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
	summary, err := NewMonitoringProbeBudgetRepository(db).Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, summary.Coverage.MinimumRequestsPerSweep)
	require.ElementsMatch(t, sources, summary.Coverage.UnestimatedDemandSources, "a fresh reader must preserve the uncompiled-scope contract")
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
