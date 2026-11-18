package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func coverageAccounts(count int) []MonitoringProbeCoverageAccount {
	items := make([]MonitoringProbeCoverageAccount, count)
	for i := range items {
		items[i].Account = Account{ID: int64(i + 1), Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
			Credentials: map[string]any{"api_key": "private-coverage-fixture", "base_url": "https://relay.invalid/v1"},
			Extra:       map[string]any{UpstreamBillingProbeEnabledExtraKey: true}}
	}
	return items
}

func TestMonitoringProbeCoverageLowerBound(t *testing.T) {
	p := DefaultMonitoringProbeBudgetSettings()
	p.Enabled = true
	b := UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}
	items := coverageAccounts(100)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.FixedZone("fixture", 8*3600))
	c := CompileMonitoringProbeCoverage(p, b, items, true, nil, true, now)
	require.True(t, c.CanSave)
	require.EqualValues(t, 200, c.MinimumRequestsPerSweep)
	require.EqualValues(t, 57600, c.MinimumRequestsPerDay)
	require.EqualValues(t, 5, c.MinimumSweepBatches)
	require.EqualValues(t, 480, c.RegularMinuteCapacity)
	require.EqualValues(t, 80000, c.RegularDayCapacity)
	require.Equal(t, now.UTC(), c.ObservedAt)
	require.Equal(t, []string{"runner_no_headroom"}, c.Warnings)
	c = CompileMonitoringProbeCoverage(p, b, coverageAccounts(101), true, nil, true, now)
	require.False(t, c.CanSave)
	require.Contains(t, c.BlockingReasons, "runner_capacity")
	require.Error(t, c.ValidateSave())
	p.Enabled = false
	require.True(t, CompileMonitoringProbeCoverage(p, b, coverageAccounts(101), true, nil, true, now).CanSave, "enforcement can always be disabled")
	b.Enabled = false
	c = CompileMonitoringProbeCoverage(p, b, items, true, nil, true, now)
	require.Zero(t, c.MinimumRequestsPerDay)
	require.True(t, c.CanSave)
}

func TestMonitoringProbeCoveragePhysicalAndUnknown(t *testing.T) {
	items := coverageAccounts(9)
	items[0].NewAPIBindingIdentity = NewAPIAccountFingerprint(&items[0].Account)
	items[1].NewAPIBindingIdentity = "stale-binding"
	items[2].Account.Credentials["base_url"] = "https://api.openai.com/v1"
	items[3].Account.Credentials["api_key"] = ""
	items[4].Account.Credentials["base_url"] = "not-a-site"
	items[5].Account.Platform = PlatformAnthropic
	items[5].Account.Credentials["base_url"] = "https://api.anthropic.com"
	items[6].Account.Status = "inactive"
	items[7].Account.Type = AccountTypeOAuth
	items[8].Account.Extra[UpstreamBillingProbeEnabledExtraKey] = false
	c := CompileMonitoringProbeCoverage(DefaultMonitoringProbeBudgetSettings(), UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 30}, items, false, nil, true, time.Now())
	require.EqualValues(t, 6, c.Accounts)
	require.EqualValues(t, 1, c.NewAPIAccounts)
	require.EqualValues(t, 3, c.UnresolvedAccounts)
	require.EqualValues(t, 7, c.MinimumRequestsPerSweep)
	require.False(t, c.InventoryComplete)
}

func TestMonitoringProbeCoverageReserveAndLimits(t *testing.T) {
	p := DefaultMonitoringProbeBudgetSettings()
	p.Enabled, p.RequestsPerMinute, p.RequestsPerDay = true, 1, 1
	p.RecoveryReservePercent = 80
	c := CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}, coverageAccounts(3), true, nil, true, time.Now())
	require.EqualValues(t, 1, c.RegularMinuteCapacity)
	require.EqualValues(t, 1, c.RegularDayCapacity)
	require.ElementsMatch(t, []string{"minute_capacity", "day_capacity"}, c.BlockingReasons)
	for _, interval := range []int{0, -1, 1441} {
		c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: interval}, nil, true, nil, true, time.Now())
		require.Contains(t, c.BlockingReasons, "invalid_interval")
	}
	// Actual-usage token budgets are deliberately not forecast or reserved.
	p = DefaultMonitoringProbeBudgetSettings()
	p.Enabled, p.TokensPerMinute, p.TokensPerDay = true, 1, 1
	c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 30}, coverageAccounts(1), true, nil, true, time.Now())
	require.True(t, c.CanSave)
}

func coveragePlan(now time.Time, group bool, cron string, models, parallel int, judge string) MonitoringProbeCoveragePlan {
	cfg := &PelicanTestConfig{QuestionKind: "candy", ParallelCount: parallel}
	for i := 0; i < models; i++ {
		cfg.ModelIDs = append(cfg.ModelIDs, fmt.Sprintf("model-%d", i))
	}
	if judge != "none" {
		cfg.Quality = &QualityPolicy{Action: QualityActionObserveOnly, Judge: &QualityJudgeConfig{Kind: judge}}
	}
	return MonitoringProbeCoveragePlan{Group: group, Plan: ScheduledTestPlan{Enabled: true, ModelID: "model-0", CronExpression: cron, PelicanConfig: cfg, NextRunAt: &now}}
}

func TestMonitoringProbeCoverageScheduledModelsSamplesAndJudges(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	p := DefaultMonitoringProbeBudgetSettings()
	p.Enabled, p.RequestsPerDay, p.RecoveryReservePercent = true, 336, 0
	plans := []MonitoringProbeCoveragePlan{coveragePlan(now, false, "0 * * * *", 2, 3, "jev"), coveragePlan(now, true, "0 * * * *", 1, 2, "none")}
	c := CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{Enabled: false, IntervalMinutes: 30}, nil, true, plans, true, now)
	require.EqualValues(t, 1, c.AccountPlans)
	require.EqualValues(t, 1, c.GroupPlans)
	require.EqualValues(t, 48, c.ScheduledRuns)
	require.EqualValues(t, 192, c.ScheduledSamples)
	require.EqualValues(t, 144, c.MinimumJudgeRequests)
	require.EqualValues(t, 336, c.MinimumScheduledRequests)
	require.EqualValues(t, 336, c.MinimumTotalRequestsPerDay)
	require.Equal(t, now.Truncate(24*time.Hour).Add(24*time.Hour), c.ScheduleWindowStart)
	require.Equal(t, c.ScheduleWindowStart.Add(24*time.Hour), c.ScheduleWindowEnd)
	require.True(t, c.CanSave)
	p.RequestsPerDay--
	c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{Enabled: false, IntervalMinutes: 30}, nil, true, plans, true, now)
	require.False(t, c.CanSave, "billing being off must not bypass scheduled demand")
	require.Equal(t, []string{"day_capacity"}, c.BlockingReasons)
	for _, kind := range []string{"", "llm", "jev"} {
		c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, []MonitoringProbeCoveragePlan{coveragePlan(now, false, "0 * * * *", 2, 3, kind)}, true, now)
		require.EqualValues(t, 144, c.MinimumJudgeRequests)
	}
	p.Enabled = false
	require.True(t, CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, plans, true, now).CanSave)
}

func TestMonitoringProbeCoverageCronWindowsUnknownAndBoundedInventory(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	p := DefaultMonitoringProbeBudgetSettings()
	for _, tc := range []struct {
		expression string
		runs       int64
	}{
		{expression: "0 0 * * *", runs: 1},
		{expression: "*/15 * * * *", runs: 96},
	} {
		c := CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, []MonitoringProbeCoveragePlan{coveragePlan(now, false, tc.expression, 1, 1, "none")}, true, now)
		require.EqualValues(t, tc.runs, c.ScheduledRuns, tc.expression)
	}
	base := coveragePlan(now, false, "* * * * *", 1, 1, "none")
	for _, scenario := range []string{"invalid_cron", "missing_model", "missing_next", "parallel", "admission"} {
		t.Run(scenario, func(t *testing.T) {
			plan := coveragePlan(now, false, base.Plan.CronExpression, 1, 1, "none")
			switch scenario {
			case "invalid_cron":
				plan.Plan.CronExpression = "not a cron"
			case "missing_model":
				plan.Plan.ModelID = ""
			case "missing_next":
				plan.Plan.NextRunAt = nil
			case "parallel":
				plan.Plan.PelicanConfig.ParallelCount = 100
			case "admission":
				plan.Plan.PelicanConfig.Quality = &QualityPolicy{Action: QualityActionOpenAITwoFAAdmission}
			}
			c := CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, []MonitoringProbeCoveragePlan{plan}, false, now)
			require.EqualValues(t, 1, c.UnresolvedPlans)
			require.Zero(t, c.MinimumScheduledRequests)
			require.Contains(t, c.Warnings, "scheduled_unresolved")
			require.Contains(t, c.Warnings, "scheduled_inventory_truncated")
		})
	}
	future := now.Add(72 * time.Hour)
	base.Plan.NextRunAt = &future
	c := CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, []MonitoringProbeCoveragePlan{base}, true, now)
	require.Zero(t, c.ScheduledRuns)
	base.Plan.NextRunAt = &now
	base.Plan.Enabled = false
	c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, []MonitoringProbeCoveragePlan{base}, true, now)
	require.Zero(t, c.AccountPlans)
	base.Plan.Enabled = true
	plans := make([]MonitoringProbeCoveragePlan, 10000)
	for i := range plans {
		plans[i] = base
	}
	c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, plans, true, now)
	require.EqualValues(t, 14400000, c.ScheduledSamples)
	first := coveragePlan(now, false, "0 * * * *", 1, 1, "none")
	second := coveragePlan(now, false, first.Plan.CronExpression, 1, 1, "none")
	boundary := now.Truncate(24 * time.Hour).Add(24*time.Hour + time.Nanosecond)
	second.Plan.NextRunAt = &boundary
	c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, []MonitoringProbeCoveragePlan{first, second}, true, now)
	require.EqualValues(t, 47, c.ScheduledSamples, "cron caching must retain subsecond start boundaries")
}

func TestMonitoringProbeCoverageCountsUnresolvedPlansOnce(t *testing.T) {
	now := time.Now()
	plan := coveragePlan(now, false, "invalid cron", 1, 1, "unknown-judge")
	c := CompileMonitoringProbeCoverage(DefaultMonitoringProbeBudgetSettings(), UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, []MonitoringProbeCoveragePlan{plan}, true, now)
	require.EqualValues(t, 1, c.AccountPlans)
	require.EqualValues(t, 1, c.UnresolvedPlans, "count unresolved plans, not the number of invalid fields")
}
