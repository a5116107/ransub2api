//go:build integration

package repository

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

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
	sources := []string{"account_quota_cycle", "ollama_usage", "responses_capability", "event_diagnostics"}
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
