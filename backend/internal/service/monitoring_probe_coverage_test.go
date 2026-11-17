package service

import (
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
	c := CompileMonitoringProbeCoverage(p, b, items, true, now)
	require.True(t, c.CanSave)
	require.EqualValues(t, 200, c.MinimumRequestsPerSweep)
	require.EqualValues(t, 57600, c.MinimumRequestsPerDay)
	require.EqualValues(t, 5, c.MinimumSweepBatches)
	require.EqualValues(t, 480, c.RegularMinuteCapacity)
	require.EqualValues(t, 80000, c.RegularDayCapacity)
	require.Equal(t, now.UTC(), c.ObservedAt)
	require.Equal(t, []string{"runner_no_headroom"}, c.Warnings)
	c = CompileMonitoringProbeCoverage(p, b, coverageAccounts(101), true, now)
	require.False(t, c.CanSave)
	require.Contains(t, c.BlockingReasons, "runner_capacity")
	require.Error(t, c.ValidateSave())
	p.Enabled = false
	require.True(t, CompileMonitoringProbeCoverage(p, b, coverageAccounts(101), true, now).CanSave, "enforcement can always be disabled")
	b.Enabled = false
	c = CompileMonitoringProbeCoverage(p, b, items, true, now)
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
	c := CompileMonitoringProbeCoverage(DefaultMonitoringProbeBudgetSettings(), UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 30}, items, false, time.Now())
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
	c := CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}, coverageAccounts(3), true, time.Now())
	require.EqualValues(t, 1, c.RegularMinuteCapacity)
	require.EqualValues(t, 1, c.RegularDayCapacity)
	require.ElementsMatch(t, []string{"minute_capacity", "day_capacity"}, c.BlockingReasons)
	for _, interval := range []int{0, -1, 1441} {
		c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: interval}, nil, true, time.Now())
		require.Contains(t, c.BlockingReasons, "invalid_interval")
	}
	// Actual-usage token budgets are deliberately not forecast or reserved.
	p = DefaultMonitoringProbeBudgetSettings()
	p.Enabled, p.TokensPerMinute, p.TokensPerDay = true, 1, 1
	c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 30}, coverageAccounts(1), true, time.Now())
	require.True(t, c.CanSave)
}
