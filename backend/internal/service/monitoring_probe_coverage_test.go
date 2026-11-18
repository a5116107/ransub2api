package service

import (
	"encoding/json"
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
	c := CompileMonitoringProbeCoverage(p, b, items, true, nil, true, nil, true, nil, true, now)
	require.True(t, c.CanSave)
	require.EqualValues(t, 200, c.MinimumRequestsPerSweep)
	require.EqualValues(t, 57600, c.MinimumRequestsPerDay)
	require.EqualValues(t, 5, c.MinimumSweepBatches)
	require.EqualValues(t, 480, c.RegularMinuteCapacity)
	require.EqualValues(t, 80000, c.RegularDayCapacity)
	require.Equal(t, now.UTC(), c.ObservedAt)
	require.Equal(t, []string{"runner_no_headroom"}, c.Warnings)
	c = CompileMonitoringProbeCoverage(p, b, coverageAccounts(101), true, nil, true, nil, true, nil, true, now)
	require.False(t, c.CanSave)
	require.Contains(t, c.BlockingReasons, "runner_capacity")
	require.Error(t, c.ValidateSave())
	p.Enabled = false
	require.True(t, CompileMonitoringProbeCoverage(p, b, coverageAccounts(101), true, nil, true, nil, true, nil, true, now).CanSave, "enforcement can always be disabled")
	b.Enabled = false
	c = CompileMonitoringProbeCoverage(p, b, items, true, nil, true, nil, true, nil, true, now)
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
	c := CompileMonitoringProbeCoverage(DefaultMonitoringProbeBudgetSettings(), UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 30}, items, false, nil, true, nil, true, nil, true, time.Now())
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
	c := CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}, coverageAccounts(3), true, nil, true, nil, true, nil, true, time.Now())
	require.EqualValues(t, 1, c.RegularMinuteCapacity)
	require.EqualValues(t, 1, c.RegularDayCapacity)
	require.ElementsMatch(t, []string{"minute_capacity", "day_capacity"}, c.BlockingReasons)
	for _, interval := range []int{0, -1, 1441} {
		c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: interval}, nil, true, nil, true, nil, true, nil, true, time.Now())
		require.Contains(t, c.BlockingReasons, "invalid_interval")
	}
	// Actual-usage token budgets are deliberately not forecast or reserved.
	p = DefaultMonitoringProbeBudgetSettings()
	p.Enabled, p.TokensPerMinute, p.TokensPerDay = true, 1, 1
	c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 30}, coverageAccounts(1), true, nil, true, nil, true, nil, true, time.Now())
	require.True(t, c.CanSave)
}

func TestMonitoringProbeCoverageBillingSharedScopeLimits(t *testing.T) {
	for _, scope := range []string{"site", "key"} {
		t.Run(scope, func(t *testing.T) {
			policy := DefaultMonitoringProbeBudgetSettings()
			policy.Enabled = true
			if scope == "site" {
				policy.SiteRequestsPerMinute = 1
			} else {
				policy.KeyRequestsPerMinute = 1
			}
			items := coverageAccounts(3)
			if scope == "site" {
				for i := range items {
					items[i].Account.Credentials["api_key"] = fmt.Sprintf("independent-key-%d", i)
				}
			} else {
				for i := range items {
					items[i].Account.Credentials["base_url"] = fmt.Sprintf("https://relay-%d.invalid/v1", i)
				}
			}
			coverage := CompileMonitoringProbeCoverage(policy, UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}, items, true, nil, true, nil, true, nil, true, time.Now())
			require.False(t, coverage.CanSave, "six calls sharing a five-call scope must be rejected even with spare global capacity")
			require.Contains(t, coverage.BlockingReasons, scope+"_capacity")
			require.Error(t, coverage.ValidateSave())
		})
	}
}

func TestMonitoringProbeCoverageBillingFinalWireScopes(t *testing.T) {
	for _, scenario := range []string{"blocked_auth_override", "additional", "duplicate", "disabled_override", "unknown", "adaptive", "canonical"} {
		t.Run(scenario, func(t *testing.T) {
			items := coverageAccounts(3)
			policy := DefaultMonitoringProbeBudgetSettings()
			policy.Enabled, policy.KeyRequestsPerMinute = true, 1
			groups, maximum, unknown := int64(4), int64(6), int64(0)
			for i := range items {
				a := &items[i].Account
				a.Credentials["api_key"] = fmt.Sprintf("stored-%d", i)
				a.Credentials[credKeyHeaderOverrideEnabled] = true
				headers := map[string]any{"api-key": "final-shared-fixture"}
				switch scenario {
				case "blocked_auth_override":
					headers = map[string]any{"authorization": "Bearer final-shared-fixture", "x-api-key": "final-shared-fixture"}
					groups, maximum = 3, 2
				case "additional":
					headers["x-custom"] = "unmetered-header"
				case "duplicate":
					a.Credentials["api_key"] = "final-shared-fixture"
					groups = 1
				case "disabled_override":
					a.Credentials[credKeyHeaderOverrideEnabled] = false
					groups, maximum = 3, 2
				case "unknown":
					a.Credentials["api_key"] = "ambiguous credential with spaces"
					groups = 1
					unknown = 3
				case "adaptive":
					a.Platform = PlatformKimi
					a.Credentials["api_protocol"] = APIProtocolAdaptive
					a.Credentials["base_url"] = fmt.Sprintf("https://unused-%d.invalid/v1", i)
					a.Credentials["api_base_urls"] = map[string]any{APIProtocolChatCompletions: "https://relay.invalid/v1"}
				case "canonical":
					a.Credentials["base_url"] = []string{"https://RELAY.invalid:443/tenant/v1/", "https://relay.invalid/tenant", "https://relay.invalid:8443/tenant/v1"}[i]
				}
				a.Credentials[credKeyHeaderOverrides] = headers
			}
			c := CompileMonitoringProbeCoverage(policy, UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}, items, true, nil, true, nil, true, nil, true, time.Now())
			s := c.BillingScopes
			require.NotNil(t, s)
			require.Equal(t, groups, s.CredentialGroups)
			require.Equal(t, maximum, s.MaxCredentialRequestsPerSweep)
			require.Equal(t, unknown, s.UnresolvedCredentials)
			require.Equal(t, maximum <= 5, c.CanSave)
			require.EqualValues(t, 6, c.MinimumRequestsPerSweep, "multiple credential buckets must not multiply physical calls")
			if scenario == "canonical" {
				require.EqualValues(t, 2, s.Sites, "tenant path and non-default port remain distinct")
				require.EqualValues(t, 4, s.MaxSiteRequestsPerSweep)
			} else {
				require.EqualValues(t, 1, s.Sites)
			}
			raw, err := json.Marshal(c)
			require.NoError(t, err)
			for _, private := range []string{"final-shared-fixture", "stored-", "relay.invalid", "unused-"} {
				require.NotContains(t, string(raw), private)
			}
		})
	}
}

func TestMonitoringProbeCoverageBillingManagementScopes(t *testing.T) {
	for _, scenario := range []string{"shared_authorization", "separate_authorizations", "extra_shared_credential", "missing_profile", "changed_site", "stale_binding"} {
		t.Run(scenario, func(t *testing.T) {
			items := coverageAccounts(3)
			for i := 0; i < 2; i++ {
				a := &items[i].Account
				a.Credentials[credKeyHeaderOverrideEnabled] = true
				headers := map[string]any{"authorization": "Bearer ignored-management-override", "cookie": "ignored-cookie"}
				if scenario == "extra_shared_credential" {
					headers["api-key"] = "private-coverage-fixture"
				}
				a.Credentials[credKeyHeaderOverrides] = headers
				items[i].NewAPIBindingIdentity = NewAPIAccountFingerprint(a)
				items[i].NewAPIAuthorizationID, items[i].NewAPIAuthorizationUserID = 7, 42
				items[i].NewAPIAuthorizationSite = "https://relay.invalid"
				if scenario == "separate_authorizations" {
					items[i].NewAPIAuthorizationID += int64(i)
				}
				if scenario == "missing_profile" {
					items[i].NewAPIAuthorizationID = 0
				}
				if scenario == "changed_site" {
					items[i].NewAPIAuthorizationSite = "https://old.invalid"
				}
				if scenario == "stale_binding" {
					items[i].NewAPIBindingIdentity = "stale"
				}
			}
			policy := DefaultMonitoringProbeBudgetSettings()
			policy.Enabled, policy.KeyRequestsPerMinute = true, 1
			c := CompileMonitoringProbeCoverage(policy, UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}, items, true, nil, true, nil, true, nil, true, time.Now())
			s := c.BillingScopes
			switch scenario {
			case "shared_authorization":
				require.EqualValues(t, 8, s.MaxCredentialRequestsPerSweep)
				require.EqualValues(t, 2, s.CredentialGroups, "management traffic must not use the inference-key bucket")
				require.EqualValues(t, 1, s.NewAPIAuthorizationGroups)
				require.False(t, c.CanSave)
			case "separate_authorizations":
				require.EqualValues(t, 4, s.MaxCredentialRequestsPerSweep)
				require.EqualValues(t, 3, s.CredentialGroups, "separate opaque authorizations are not proven identical")
				require.True(t, c.CanSave)
			case "extra_shared_credential":
				require.EqualValues(t, 10, s.MaxCredentialRequestsPerSweep, "additional final auth headers still share the relay's credential bucket")
				require.EqualValues(t, 2, s.CredentialGroups)
				require.False(t, c.CanSave)
			case "missing_profile", "changed_site":
				require.EqualValues(t, 2, s.UnresolvedNewAPIAuthorizations)
				require.EqualValues(t, 2, s.UnresolvedCredentials)
				require.EqualValues(t, 2, s.MaxCredentialRequestsPerSweep)
				require.Contains(t, c.Warnings, "billing_scope_unresolved")
				require.True(t, c.CanSave, "unknowns alone do not prove a shortfall")
			case "stale_binding":
				require.Zero(t, c.NewAPIAccounts)
				require.Zero(t, s.NewAPIAuthorizationGroups)
				require.EqualValues(t, 6, s.MaxCredentialRequestsPerSweep, "blocked auth overrides do not change the fallback relay credential")
				require.EqualValues(t, 6, c.MinimumRequestsPerSweep)
				require.False(t, c.CanSave)
			}
			raw, err := json.Marshal(c)
			require.NoError(t, err)
			for _, private := range []string{"authorization:7", "ignored-management", "ignored-cookie", "relay.invalid", "old.invalid", "private-coverage-fixture", items[0].NewAPIBindingIdentity} {
				if private != "stale" {
					require.NotContains(t, string(raw), private)
				}
			}
		})
	}
}

func TestMonitoringProbeCoverageBillingScopeBoundaryAndDisable(t *testing.T) {
	policy := DefaultMonitoringProbeBudgetSettings()
	policy.Enabled, policy.KeyRequestsPerMinute, policy.SiteRequestsPerMinute = true, 1, 1
	policy.RecoveryReservePercent = 80
	items := coverageAccounts(3)
	items[0].Account.Credentials["base_url"] = "https://api.openai.com/v1" // one official call + two relay pairs = five shared-key calls
	billing := UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}
	c := CompileMonitoringProbeCoverage(policy, billing, items, true, nil, true, nil, true, nil, true, time.Now())
	require.True(t, c.CanSave, "scope limits do not subtract global recovery reserve")
	require.EqualValues(t, 5, c.BillingScopes.CredentialSweepCapacity)
	require.EqualValues(t, 5, c.BillingScopes.MaxCredentialRequestsPerSweep)
	items[0].Account.Credentials["base_url"] = "https://relay.invalid/v1"
	c = CompileMonitoringProbeCoverage(policy, billing, items, false, nil, true, nil, true, nil, true, time.Now())
	require.ElementsMatch(t, []string{"site_capacity", "key_capacity"}, c.BlockingReasons)
	require.False(t, c.CanSave, "incomplete inventory cannot hide a proven lower-bound shortfall")
	policy.Enabled = false
	require.True(t, CompileMonitoringProbeCoverage(policy, billing, items, true, nil, true, nil, true, nil, true, time.Now()).CanSave)
	policy.Enabled, billing.Enabled = true, false
	require.True(t, CompileMonitoringProbeCoverage(policy, billing, items, true, nil, true, nil, true, nil, true, time.Now()).CanSave)
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
	c := CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{Enabled: false, IntervalMinutes: 30}, nil, true, plans, true, nil, true, nil, true, now)
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
	c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{Enabled: false, IntervalMinutes: 30}, nil, true, plans, true, nil, true, nil, true, now)
	require.False(t, c.CanSave, "billing being off must not bypass scheduled demand")
	require.Equal(t, []string{"day_capacity"}, c.BlockingReasons)
	for _, kind := range []string{"", "llm", "jev"} {
		c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, []MonitoringProbeCoveragePlan{coveragePlan(now, false, "0 * * * *", 2, 3, kind)}, true, nil, true, nil, true, now)
		require.EqualValues(t, 144, c.MinimumJudgeRequests)
	}
	p.Enabled = false
	require.True(t, CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, plans, true, nil, true, nil, true, now).CanSave)
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
		c := CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, []MonitoringProbeCoveragePlan{coveragePlan(now, false, tc.expression, 1, 1, "none")}, true, nil, true, nil, true, now)
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
			c := CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, []MonitoringProbeCoveragePlan{plan}, false, nil, true, nil, true, now)
			require.EqualValues(t, 1, c.UnresolvedPlans)
			require.Zero(t, c.MinimumScheduledRequests)
			require.Contains(t, c.Warnings, "scheduled_unresolved")
			require.Contains(t, c.Warnings, "scheduled_inventory_truncated")
		})
	}
	future := now.Add(72 * time.Hour)
	base.Plan.NextRunAt = &future
	c := CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, []MonitoringProbeCoveragePlan{base}, true, nil, true, nil, true, now)
	require.Zero(t, c.ScheduledRuns)
	base.Plan.NextRunAt = &now
	base.Plan.Enabled = false
	c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, []MonitoringProbeCoveragePlan{base}, true, nil, true, nil, true, now)
	require.Zero(t, c.AccountPlans)
	base.Plan.Enabled = true
	plans := make([]MonitoringProbeCoveragePlan, 10000)
	for i := range plans {
		plans[i] = base
	}
	c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, plans, true, nil, true, nil, true, now)
	require.EqualValues(t, 14400000, c.ScheduledSamples)
	first := coveragePlan(now, false, "0 * * * *", 1, 1, "none")
	second := coveragePlan(now, false, first.Plan.CronExpression, 1, 1, "none")
	boundary := now.Truncate(24 * time.Hour).Add(24*time.Hour + time.Nanosecond)
	second.Plan.NextRunAt = &boundary
	c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, []MonitoringProbeCoveragePlan{first, second}, true, nil, true, nil, true, now)
	require.EqualValues(t, 47, c.ScheduledSamples, "cron caching must retain subsecond start boundaries")
}

func TestMonitoringProbeCoverageCountsUnresolvedPlansOnce(t *testing.T) {
	now := time.Now()
	plan := coveragePlan(now, false, "invalid cron", 1, 1, "unknown-judge")
	c := CompileMonitoringProbeCoverage(DefaultMonitoringProbeBudgetSettings(), UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, []MonitoringProbeCoveragePlan{plan}, true, nil, true, nil, true, now)
	require.EqualValues(t, 1, c.AccountPlans)
	require.EqualValues(t, 1, c.UnresolvedPlans, "count unresolved plans, not the number of invalid fields")
}

func TestMonitoringProbeCoverageCandyUsesAbsoluteSlots(t *testing.T) {
	for _, day := range []int{18, 19, 20} {
		for _, minutes := range []int{1, 7, 13, 59, 1440} {
			t.Run(fmt.Sprintf("%d/%d", day, minutes), func(t *testing.T) {
				now := time.Date(2026, 11, day, 12, 0, 0, 0, time.FixedZone("fixture", -7*3600))
				probe := ChannelMonitorV2CandyProbe{Enabled: true, Model: "model-a", IntervalMinutes: minutes}
				p := DefaultMonitoringProbeBudgetSettings()
				p.Enabled, p.RequestsPerDay, p.RecoveryReservePercent = true, 1, 0
				c := CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, nil, true, []ChannelMonitorV2CandyProbe{probe}, true, nil, true, now)
				previous := c.ScheduleWindowStart.Add(-time.Minute).Truncate(time.Duration(minutes) * time.Minute)
				var want int64
				for tick := c.ScheduleWindowStart; tick.Before(c.ScheduleWindowEnd); tick = tick.Add(time.Minute) {
					slot := tick.Truncate(time.Duration(minutes) * time.Minute)
					if slot != previous {
						want++
						previous = slot
					}
				}
				require.Equal(t, want, c.MinimumCandyRequests)
				require.Equal(t, want, c.MinimumTotalRequestsPerDay)
				require.EqualValues(t, 1, c.CandyProbes)
				require.Zero(t, c.MinimumScheduledRequests)
				require.Equal(t, want <= 1, c.CanSave)
			})
		}
	}
	probes := []ChannelMonitorV2CandyProbe{
		{Enabled: true, Model: "model-a", IntervalMinutes: 0},
		{Enabled: true, Model: " ", IntervalMinutes: 5},
		{Enabled: true, Model: "model-a", IntervalMinutes: 1441},
		{Enabled: false, Model: "model-a", IntervalMinutes: 1},
	}
	c := CompileMonitoringProbeCoverage(DefaultMonitoringProbeBudgetSettings(), UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, nil, true, probes, false, nil, true, time.Now())
	require.EqualValues(t, 3, c.CandyProbes)
	require.EqualValues(t, 3, c.UnresolvedCandyProbes)
	require.Zero(t, c.MinimumCandyRequests)
	require.ElementsMatch(t, []string{"candy_unresolved", "candy_inventory_truncated"}, c.Warnings)
}

func TestMonitoringProbeCoverageCandySharesRuntimeEligibility(t *testing.T) {
	probe := ChannelMonitorV2CandyProbe{Enabled: true, GroupID: 1, Model: "not-in-display-list"}
	cfg := ChannelMonitorV2Config{Enabled: true, Platforms: []ChannelMonitorV2PlatformConfig{{Platform: PlatformOpenAI, Enabled: true, Models: []string{"display-only"}}}}
	group := &Group{ID: 1, Platform: PlatformOpenAI, Status: StatusActive}
	require.True(t, cfg.CandyProbeAllowed(probe, group))
	cfg.GroupIDs = []int64{2}
	require.False(t, cfg.CandyProbeAllowed(probe, group))
	cfg.GroupIDs = nil
	group.Platform = PlatformAnthropic
	require.False(t, cfg.CandyProbeAllowed(probe, group))
	group.Platform = PlatformComposite
	require.True(t, cfg.CandyProbeAllowed(probe, group))
	cfg.Platforms[0].Enabled = false
	require.False(t, cfg.CandyProbeAllowed(probe, group))
	cfg.Platforms[0].Enabled = true
	group.Status = "inactive"
	require.False(t, cfg.CandyProbeAllowed(probe, group))
	require.False(t, cfg.CandyProbeAllowed(probe, nil))
	group.Status = StatusActive
	cfg.Enabled = false
	require.False(t, cfg.CandyProbeAllowed(probe, group))
	for _, value := range []string{"false", "0", " off ", "DISABLED"} {
		require.False(t, ParseChannelMonitorRuntime(map[string]string{SettingKeyChannelMonitorMode: " V2 ", SettingKeyChannelMonitorEnabled: value}).V2Active())
	}
	require.True(t, ParseChannelMonitorRuntime(map[string]string{SettingKeyChannelMonitorMode: " V2 "}).V2Active(), "deleting the switch restores its original enabled default")
	for _, value := range []string{"", "v1", "v3", "unknown"} {
		require.False(t, ParseChannelMonitorRuntime(map[string]string{SettingKeyChannelMonitorMode: value}).V2Active())
	}
}

func TestMonitoringProbeCoverageActiveModelsQuotaAndJitter(t *testing.T) {
	p := DefaultMonitoringProbeBudgetSettings()
	p.Enabled, p.RecoveryReservePercent, p.RequestsPerDay = true, 0, 8640
	monitors := []ChannelMonitor{
		{Enabled: true, Provider: MonitorProviderOpenAI, PrimaryModel: "model-a", ExtraModels: []string{"model-b"}, IntervalSeconds: 60},
		{Enabled: true, Provider: MonitorProviderAnthropic, CheckMode: MonitorCheckModeQuotaProbe, PrimaryModel: "model-a", IntervalSeconds: 30, JitterSeconds: 10},
		{Enabled: true, Provider: MonitorProviderAntigravity, CheckMode: MonitorCheckModeQuota, IntervalSeconds: 15},
		{Enabled: false, Provider: MonitorProviderOpenAI, PrimaryModel: "model-a", IntervalSeconds: 15},
	}
	compile := func() *MonitoringProbeCoverage {
		return CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, nil, true, nil, true, monitors, true, time.Now())
	}
	c := compile()
	require.EqualValues(t, 3, c.ActiveMonitors)
	require.EqualValues(t, 2, c.ActiveQuotaMonitors)
	require.Zero(t, c.UnresolvedActiveMonitors)
	require.True(t, c.ActiveInventoryComplete)
	require.EqualValues(t, 3600, c.ActiveMonitorRuns)
	require.EqualValues(t, 8640, c.MinimumActiveRequests, "1440 * (2 models + HEAD) + 2160 * (1 model + HEAD)")
	require.EqualValues(t, 8640, c.MinimumTotalRequestsPerDay)
	require.Equal(t, []string{"active_quota_unestimated"}, c.Warnings)
	require.True(t, c.CanSave)
	p.RequestsPerDay--
	c = compile()
	require.False(t, c.CanSave)
	require.Equal(t, []string{"day_capacity"}, c.BlockingReasons)
	require.ErrorContains(t, c.ValidateSave(), "8640 V1 model/HEAD")
	p.TokensPerDay = 1
	p.RequestsPerDay++
	require.True(t, compile().CanSave, "actual-usage tokens are never forecast")
	p.Enabled, p.RequestsPerDay = false, 1
	require.True(t, compile().CanSave, "disabling enforcement remains a repair")
}

func TestMonitoringProbeCoverageActiveUnknownAndBoundaries(t *testing.T) {
	base := ChannelMonitor{Enabled: true, Provider: MonitorProviderOpenAI, PrimaryModel: "model-a", IntervalSeconds: 60}
	for _, tc := range []struct {
		name   string
		mutate func(*ChannelMonitor)
	}{
		{"interval_zero", func(m *ChannelMonitor) { m.IntervalSeconds = 0 }},
		{"interval_large", func(m *ChannelMonitor) { m.IntervalSeconds = 3601 }},
		{"negative_jitter", func(m *ChannelMonitor) { m.JitterSeconds = -1 }},
		{"excessive_jitter", func(m *ChannelMonitor) { m.JitterSeconds = 46 }},
		{"mode", func(m *ChannelMonitor) { m.CheckMode = "future" }},
		{"provider", func(m *ChannelMonitor) { m.Provider = "future" }},
		{"quota_only_provider", func(m *ChannelMonitor) { m.Provider = MonitorProviderAntigravity }},
		{"primary", func(m *ChannelMonitor) { m.PrimaryModel = " " }},
		{"extra", func(m *ChannelMonitor) { m.ExtraModels = []string{"model-b", " "} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := base
			tc.mutate(&m)
			c := CompileMonitoringProbeCoverage(DefaultMonitoringProbeBudgetSettings(), UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, nil, true, nil, true, []ChannelMonitor{m}, false, time.Now())
			require.EqualValues(t, 1, c.ActiveMonitors)
			require.EqualValues(t, 1, c.UnresolvedActiveMonitors)
			require.Zero(t, c.MinimumActiveRequests)
			require.ElementsMatch(t, []string{"active_unresolved", "active_inventory_truncated"}, c.Warnings)
		})
	}
	for _, tc := range []struct{ interval, jitter, runs int }{{15, 0, 5760}, {60, 45, 822}, {3600, 3585, 12}, {59, 0, 1464}} {
		m := base
		m.IntervalSeconds, m.JitterSeconds = tc.interval, tc.jitter
		c := CompileMonitoringProbeCoverage(DefaultMonitoringProbeBudgetSettings(), UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, nil, true, nil, true, []ChannelMonitor{m}, true, time.Now())
		require.EqualValues(t, tc.runs, c.ActiveMonitorRuns)
		require.EqualValues(t, 2*tc.runs, c.MinimumActiveRequests)
	}
}

// A zero compiled minimum is not an inventory of every running monitor. CN
// cadence comes from node config; activity/account/diagnostic triggers are not
// future schedules. Keep these unknown demands separate from known calls.
func TestMonitoringProbeCoverageUnestimatedSources(t *testing.T) {
	for _, enforce := range []bool{false, true} {
		t.Run(fmt.Sprint(enforce), func(t *testing.T) {
			policy := DefaultMonitoringProbeBudgetSettings()
			policy.Enabled, policy.RequestsPerDay = enforce, 1
			coverage := CompileMonitoringProbeCoverage(policy, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, nil, true, nil, true, nil, true, time.Now())
			encoded, err := json.Marshal(coverage)
			require.NoError(t, err)
			var view struct {
				Sources []string `json:"unestimated_demand_sources"`
			}
			require.NoError(t, json.Unmarshal(encoded, &view))
			require.ElementsMatch(t, []string{"account_quota_cycle", "ollama_usage", "responses_capability", "event_diagnostics"}, view.Sources)
			require.Zero(t, coverage.MinimumTotalRequestsPerDay, "do not invent future physical calls or token demand")
			require.Empty(t, coverage.BlockingReasons, "unestimated demand is not a proved capacity shortfall")
			require.True(t, coverage.CanSave, "repair and enforcement-disable paths must remain available")
		})
	}
}
