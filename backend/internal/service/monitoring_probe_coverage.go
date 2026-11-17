package service

import (
	"context"
	"fmt"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Coverage is an optimistic lower bound for the existing billing runner, not
// a reservation, token estimate, or proof of an achievable end-to-end SLA.
type MonitoringProbeCoverage struct {
	ObservedAt              time.Time `json:"observed_at"`
	BudgetRevision          int64     `json:"budget_revision"`
	BillingEnabled          bool      `json:"billing_enabled"`
	IntervalMinutes         int       `json:"interval_minutes"`
	Accounts                int64     `json:"accounts"`
	NewAPIAccounts          int64     `json:"new_api_accounts"`
	UnresolvedAccounts      int64     `json:"unresolved_accounts"`
	InventoryComplete       bool      `json:"inventory_complete"`
	MinimumRequestsPerSweep int64     `json:"minimum_requests_per_sweep"`
	MinimumRequestsPerDay   int64     `json:"minimum_requests_per_day"`
	MinimumSweepBatches     int64     `json:"minimum_sweep_batches"`
	RunnerAccountsPerMinute int64     `json:"runner_accounts_per_minute"`
	RegularMinuteCapacity   int64     `json:"regular_minute_capacity"`
	RegularDayCapacity      int64     `json:"regular_day_capacity"`
	Enforced                bool      `json:"enforced"`
	CanSave                 bool      `json:"can_save"`
	BlockingReasons         []string  `json:"blocking_reasons"`
	Warnings                []string  `json:"warnings"`
}

// Only account identity required by the original runner is read. Neither raw
// credentials, site names nor binding fingerprints enter the admin response.
type MonitoringProbeCoverageAccount struct {
	Account               Account
	NewAPIBindingIdentity string
}

func CompileMonitoringProbeCoverage(policy MonitoringProbeBudgetSettings, billing UpstreamBillingProbeSettings,
	accounts []MonitoringProbeCoverageAccount, complete bool, now time.Time) *MonitoringProbeCoverage {
	result := &MonitoringProbeCoverage{
		ObservedAt: now.UTC(), BudgetRevision: policy.Revision, BillingEnabled: billing.Enabled,
		IntervalMinutes: billing.IntervalMinutes, InventoryComplete: complete,
		RunnerAccountsPerMinute: upstreamBillingProbeMaxPerCycle,
		RegularMinuteCapacity:   policy.RequestsPerMinute * int64(100-policy.RecoveryReservePercent) / 100,
		RegularDayCapacity:      policy.RequestsPerDay * int64(100-policy.RecoveryReservePercent) / 100,
		Enforced:                policy.Enabled, CanSave: true, BlockingReasons: []string{}, Warnings: []string{},
	}
	if result.RegularMinuteCapacity < 1 {
		result.RegularMinuteCapacity = 1
	}
	if result.RegularDayCapacity < 1 {
		result.RegularDayCapacity = 1
	}
	for _, target := range accounts {
		account := &target.Account
		if !isUpstreamBillingProbeAccount(account) || !account.IsActive() || !upstreamBillingProbeEnabled(account) {
			continue
		}
		result.Accounts++
		if target.NewAPIBindingIdentity != "" && target.NewAPIBindingIdentity == NewAPIAccountFingerprint(account) {
			// status, wallet, pricing and at least one inventory page; raw-key
			// verification and any additional pages can only increase this.
			result.NewAPIAccounts++
			result.MinimumRequestsPerSweep += 4
			continue
		}
		base := upstreamBillingProbeBaseURL(account)
		if account.Platform == PlatformOpenAI && base == "" {
			base = "https://api.openai.com"
		}
		if account.GetCredential("api_key") == "" || (account.Platform != PlatformOpenAI && upstreamBillingProbeTargetIsOfficialAPI(base)) {
			result.UnresolvedAccounts++
			continue
		}
		if _, err := CanonicalNewAPISite(base); err != nil {
			result.UnresolvedAccounts++
			continue
		}
		result.MinimumRequestsPerSweep++
		if !upstreamBillingProbeTargetIsOfficialAPI(base) {
			// A completed relay declaration read is followed by the balance read.
			result.MinimumRequestsPerSweep++
		}
	}
	result.MinimumSweepBatches = (result.Accounts + upstreamBillingProbeMaxPerCycle - 1) / upstreamBillingProbeMaxPerCycle
	if billing.IntervalMinutes < upstreamBillingProbeMinIntervalMinutes || billing.IntervalMinutes > upstreamBillingProbeMaxIntervalMinutes {
		result.BlockingReasons = append(result.BlockingReasons, "invalid_interval")
	} else if billing.Enabled {
		interval := int64(billing.IntervalMinutes)
		result.MinimumRequestsPerDay = result.MinimumRequestsPerSweep * (1440 / interval)
		if result.Accounts > interval*upstreamBillingProbeMaxPerCycle {
			result.BlockingReasons = append(result.BlockingReasons, "runner_capacity")
		} else if result.Accounts > 0 && result.Accounts == interval*upstreamBillingProbeMaxPerCycle {
			result.Warnings = append(result.Warnings, "runner_no_headroom")
		}
		// These are steady-state full-sweep targets. Requests already delayed
		// by faults/backoff are not treated as completed fresh coverage.
		if policy.Enabled {
			if result.MinimumRequestsPerSweep > result.RegularMinuteCapacity*interval {
				result.BlockingReasons = append(result.BlockingReasons, "minute_capacity")
			}
			if result.MinimumRequestsPerDay > result.RegularDayCapacity {
				result.BlockingReasons = append(result.BlockingReasons, "day_capacity")
			}
		}
	}
	result.CanSave = !policy.Enabled || !billing.Enabled || len(result.BlockingReasons) == 0
	return result
}

func (c *MonitoringProbeCoverage) ValidateSave() error {
	if c.CanSave {
		return nil
	}
	return infraerrors.BadRequest("MONITORING_PROBE_COVERAGE_IMPOSSIBLE",
		fmt.Sprintf("configured billing coverage exceeds capacity (%v): %d accounts, %d minutes, at least %d calls/sweep and %d calls/day; increase capacity or relax the interval", c.BlockingReasons, c.Accounts, c.IntervalMinutes, c.MinimumRequestsPerSweep, c.MinimumRequestsPerDay))
}

func (s *MonitoringProbeBudgetService) PreviewCoverage(ctx context.Context, settings MonitoringProbeBudgetSettings) (*MonitoringProbeCoverage, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if s == nil || s.repo == nil {
		return nil, ErrMonitoringProbeBudgetUnavailable
	}
	return s.repo.PreviewCoverage(ctx, settings)
}
