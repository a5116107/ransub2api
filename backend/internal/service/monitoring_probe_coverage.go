package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Coverage is an optimistic lower bound for configured successful checks, not
// a reservation, token estimate, or proof of an achievable end-to-end SLA.
type MonitoringProbeCoverage struct {
	ObservedAt                 time.Time `json:"observed_at"`
	BudgetRevision             int64     `json:"budget_revision"`
	BillingEnabled             bool      `json:"billing_enabled"`
	IntervalMinutes            int       `json:"interval_minutes"`
	Accounts                   int64     `json:"accounts"`
	NewAPIAccounts             int64     `json:"new_api_accounts"`
	UnresolvedAccounts         int64     `json:"unresolved_accounts"`
	InventoryComplete          bool      `json:"inventory_complete"`
	MinimumRequestsPerSweep    int64     `json:"minimum_requests_per_sweep"`
	MinimumRequestsPerDay      int64     `json:"minimum_requests_per_day"`
	MinimumSweepBatches        int64     `json:"minimum_sweep_batches"`
	RunnerAccountsPerMinute    int64     `json:"runner_accounts_per_minute"`
	RegularMinuteCapacity      int64     `json:"regular_minute_capacity"`
	RegularDayCapacity         int64     `json:"regular_day_capacity"`
	Enforced                   bool      `json:"enforced"`
	CanSave                    bool      `json:"can_save"`
	BlockingReasons            []string  `json:"blocking_reasons"`
	Warnings                   []string  `json:"warnings"`
	ScheduleWindowStart        time.Time `json:"schedule_window_start"`
	ScheduleWindowEnd          time.Time `json:"schedule_window_end"`
	AccountPlans               int64     `json:"account_plans"`
	GroupPlans                 int64     `json:"group_plans"`
	UnresolvedPlans            int64     `json:"unresolved_plans"`
	PlanInventoryComplete      bool      `json:"plan_inventory_complete"`
	ScheduledRuns              int64     `json:"scheduled_runs"`
	ScheduledSamples           int64     `json:"scheduled_samples"`
	MinimumJudgeRequests       int64     `json:"minimum_judge_requests"`
	MinimumScheduledRequests   int64     `json:"minimum_scheduled_requests"`
	CandyProbes                int64     `json:"candy_probes"`
	UnresolvedCandyProbes      int64     `json:"unresolved_candy_probes"`
	CandyInventoryComplete     bool      `json:"candy_inventory_complete"`
	MinimumCandyRequests       int64     `json:"minimum_candy_requests"`
	ActiveMonitors             int64     `json:"active_monitors"`
	ActiveQuotaMonitors        int64     `json:"active_quota_monitors"`
	UnresolvedActiveMonitors   int64     `json:"unresolved_active_monitors"`
	ActiveInventoryComplete    bool      `json:"active_inventory_complete"`
	ActiveMonitorRuns          int64     `json:"active_monitor_runs"`
	MinimumActiveRequests      int64     `json:"minimum_active_requests"`
	MinimumTotalRequestsPerDay int64     `json:"minimum_total_requests_per_day"`
	UnestimatedDemandSources   []string  `json:"unestimated_demand_sources"`

	BillingScopes       *MonitoringProbeCoverageBillingScopes       `json:"billing_scopes,omitempty"`
	PeriodicSites       *MonitoringProbeCoveragePeriodicSites       `json:"periodic_sites,omitempty"`
	PeriodicCredentials *MonitoringProbeCoveragePeriodicCredentials `json:"periodic_credentials,omitempty"`
	CNQuotaCycle        *CNMonitoringRuntime                        `json:"cn_quota_cycle,omitempty"`
	OllamaUsage         *OllamaMonitoringRuntime                    `json:"ollama_usage,omitempty"`
	ResponsesCapability *ResponsesCapabilityMonitoringRuntime       `json:"responses_capability,omitempty"`
}

// Known periodic demand only. Site minute counters are scaled to a full UTC
// day, not a new daily allowance or a claim about phase/peak feasibility.
type MonitoringProbeCoveragePeriodicSites struct {
	Version                   int    `json:"version"`
	Scope                     string `json:"scope"`
	Sites                     int64  `json:"sites"`
	BillingRequestsPerDay     int64  `json:"billing_requests_per_day"`
	ActiveModelRequestsPerDay int64  `json:"active_model_requests_per_day"`
	OriginPingRequestsPerDay  int64  `json:"origin_ping_requests_per_day"`
	MaxRequestsPerDay         int64  `json:"max_requests_per_day"`
	SiteDayCapacity           int64  `json:"site_day_capacity"`
	OverLimitSites            int64  `json:"over_limit_sites"`
	UnresolvedBillingTargets  int64  `json:"unresolved_billing_targets"`
	UnresolvedActiveTargets   int64  `json:"unresolved_active_targets"`
}

// Attributions are per credential, not extra physical requests or a unique-key
// census. Management authorization IDs remain separate confirmed lower bounds.
type MonitoringProbeCoveragePeriodicCredentials struct {
	Version                       int    `json:"version"`
	Scope                         string `json:"scope"`
	CredentialGroups              int64  `json:"credential_groups"`
	NewAPIAuthorizationGroups     int64  `json:"new_api_authorization_groups"`
	BillingAttributionsPerDay     int64  `json:"billing_attributions_per_day"`
	ActiveModelAttributionsPerDay int64  `json:"active_model_attributions_per_day"`
	OriginPingAttributionsPerDay  int64  `json:"origin_ping_attributions_per_day"`
	MaxAttributionsPerDay         int64  `json:"max_attributions_per_day"`
	KeyDayCapacity                int64  `json:"key_day_capacity"`
	OverLimitCredentialGroups     int64  `json:"over_limit_credential_groups"`
	UnresolvedBillingTargets      int64  `json:"unresolved_billing_targets"`
	UnresolvedActiveTargets       int64  `json:"unresolved_active_targets"`
}

// Each bucket is a confirmed sharing lower bound, not a unique-key census.
// Management authorization IDs may refer to the same secret as another ID or
// inference key. Without decrypting them, that overlap remains unknown.
type MonitoringProbeCoverageBillingScopes struct {
	Version                        int   `json:"version"`
	Sites                          int64 `json:"sites"`
	CredentialGroups               int64 `json:"credential_groups"`
	NewAPIAuthorizationGroups      int64 `json:"new_api_authorization_groups"`
	MaxSiteRequestsPerSweep        int64 `json:"max_site_requests_per_sweep"`
	MaxCredentialRequestsPerSweep  int64 `json:"max_credential_requests_per_sweep"`
	SiteSweepCapacity              int64 `json:"site_sweep_capacity"`
	CredentialSweepCapacity        int64 `json:"credential_sweep_capacity"`
	OverLimitSites                 int64 `json:"over_limit_sites"`
	OverLimitCredentialGroups      int64 `json:"over_limit_credential_groups"`
	UnresolvedSites                int64 `json:"unresolved_sites"`
	UnresolvedCredentials          int64 `json:"unresolved_credentials"`
	UnresolvedNewAPIAuthorizations int64 `json:"unresolved_new_api_authorizations"`
}

// Inventory inputs stay internal. No credentials, site names, authorization
// IDs or binding/credential fingerprints enter the administrator response.
type MonitoringProbeCoverageAccount struct {
	Account                   Account
	NewAPIBindingIdentity     string
	NewAPIAuthorizationID     int64
	NewAPIAuthorizationSite   string
	NewAPIAuthorizationUserID int64
}

type MonitoringProbeCoveragePlan struct {
	Plan  ScheduledTestPlan
	Group bool
}

func CompileMonitoringProbeCoverage(policy MonitoringProbeBudgetSettings, billing UpstreamBillingProbeSettings,
	accounts []MonitoringProbeCoverageAccount, complete bool, plans []MonitoringProbeCoveragePlan, plansComplete bool,
	candy []ChannelMonitorV2CandyProbe, candyComplete bool, active []ChannelMonitor, activeComplete bool, now time.Time) *MonitoringProbeCoverage {
	result := &MonitoringProbeCoverage{
		ObservedAt: now.UTC(), BudgetRevision: policy.Revision, BillingEnabled: billing.Enabled,
		IntervalMinutes: billing.IntervalMinutes, InventoryComplete: complete,
		RunnerAccountsPerMinute: upstreamBillingProbeMaxPerCycle,
		RegularMinuteCapacity:   policy.RequestsPerMinute * int64(100-policy.RecoveryReservePercent) / 100,
		RegularDayCapacity:      policy.RequestsPerDay * int64(100-policy.RecoveryReservePercent) / 100,
		Enforced:                policy.Enabled, CanSave: true, BlockingReasons: []string{}, Warnings: []string{},
		PlanInventoryComplete: plansComplete, CandyInventoryComplete: candyComplete,
		ActiveInventoryComplete: activeComplete,
		// These are source-level omissions, not observed enabled inventory.
		// Node configuration and future events cannot be inferred from SQL.
		UnestimatedDemandSources: []string{"account_quota_cycle", "ollama_usage", "responses_capability", "event_diagnostics"},
		ScheduleWindowStart:      now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour),
	}
	result.ScheduleWindowEnd = result.ScheduleWindowStart.Add(24 * time.Hour)
	if result.RegularMinuteCapacity < 1 {
		result.RegularMinuteCapacity = 1
	}
	if result.RegularDayCapacity < 1 {
		result.RegularDayCapacity = 1
	}
	scopes := monitoringProbeCoverageBillingDemand{
		view:  &MonitoringProbeCoverageBillingScopes{Version: 1},
		sites: make(map[string]int64), credentials: make(map[string]int64),
		management: make(map[int64]struct{}),
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
			scopes.addNewAPI(target)
			continue
		}
		base := upstreamBillingProbeBaseURL(account)
		if account.Platform == PlatformOpenAI && base == "" {
			base = "https://api.openai.com"
		}
		if account.GetCredential("api_key") == "" || (account.Platform != PlatformOpenAI && upstreamBillingProbeTargetIsOfficialAPI(base)) {
			result.UnresolvedAccounts++
			if account.GetCredential("api_key") == "" {
				scopes.view.UnresolvedCredentials++
				if _, err := CanonicalNewAPISite(base); err != nil {
					scopes.view.UnresolvedSites++
				}
			}
			continue
		}
		site, err := CanonicalNewAPISite(base)
		if err != nil {
			result.UnresolvedAccounts++
			scopes.view.UnresolvedSites++
			continue
		}
		requests := int64(1)
		if !upstreamBillingProbeTargetIsOfficialAPI(base) {
			// A completed relay declaration read is followed by the balance read.
			requests++
		}
		result.MinimumRequestsPerSweep += requests
		scopes.sites[site] += requests
		scopes.bucketedTargets++
		headers := make(http.Header)
		applyUpstreamBillingProbeHeaders(headers, account, account.GetCredential("api_key"))
		if scopes.addCredentials(headers, requests, false) {
			scopes.credentialTargets++
		}
	}
	scopes.finish(result, policy, billing)
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
		}
	}
	periodicSites := make(map[string]int64)
	periodicKeys := make(map[string]int64)
	result.PeriodicSites = &MonitoringProbeCoveragePeriodicSites{
		Version: 1, Scope: "billing_and_v1", SiteDayCapacity: policy.SiteRequestsPerMinute * 1440,
	}
	result.PeriodicCredentials = &MonitoringProbeCoveragePeriodicCredentials{
		Version: 1, Scope: "billing_and_v1", KeyDayCapacity: policy.KeyRequestsPerMinute * 1440,
	}
	if billing.Enabled {
		result.PeriodicSites.UnresolvedBillingTargets = result.Accounts
		result.PeriodicCredentials.UnresolvedBillingTargets = result.Accounts
		if billing.IntervalMinutes >= upstreamBillingProbeMinIntervalMinutes && billing.IntervalMinutes <= upstreamBillingProbeMaxIntervalMinutes {
			sweeps := int64(1440 / billing.IntervalMinutes)
			for site, requests := range scopes.sites {
				periodicSites[site] = requests * sweeps
				result.PeriodicSites.BillingRequestsPerDay += requests * sweeps
			}
			result.PeriodicSites.UnresolvedBillingTargets -= scopes.bucketedTargets
			result.PeriodicCredentials.UnresolvedBillingTargets -= scopes.credentialTargets
			result.PeriodicCredentials.NewAPIAuthorizationGroups = int64(len(scopes.management))
			for key, requests := range scopes.credentials {
				periodicKeys[key] = requests * sweeps
				result.PeriodicCredentials.BillingAttributionsPerDay += requests * sweeps
			}
		}
	}
	result.compileSchedules(plans)
	result.compileCandy(candy)
	result.compileActive(active, periodicSites, periodicKeys)
	result.PeriodicSites.Sites = int64(len(periodicSites))
	for _, requests := range periodicSites {
		if requests > result.PeriodicSites.MaxRequestsPerDay {
			result.PeriodicSites.MaxRequestsPerDay = requests
		}
		if requests > result.PeriodicSites.SiteDayCapacity {
			result.PeriodicSites.OverLimitSites++
		}
	}
	if policy.Enabled && result.PeriodicSites.OverLimitSites > 0 {
		result.BlockingReasons = append(result.BlockingReasons, "periodic_site_capacity")
	}
	if result.PeriodicSites.UnresolvedBillingTargets+result.PeriodicSites.UnresolvedActiveTargets > 0 {
		result.Warnings = append(result.Warnings, "periodic_site_unresolved")
	}
	result.PeriodicCredentials.CredentialGroups = int64(len(periodicKeys))
	for _, requests := range periodicKeys {
		if requests > result.PeriodicCredentials.MaxAttributionsPerDay {
			result.PeriodicCredentials.MaxAttributionsPerDay = requests
		}
		if requests > result.PeriodicCredentials.KeyDayCapacity {
			result.PeriodicCredentials.OverLimitCredentialGroups++
		}
	}
	if policy.Enabled && result.PeriodicCredentials.OverLimitCredentialGroups > 0 {
		result.BlockingReasons = append(result.BlockingReasons, "periodic_key_capacity")
	}
	if result.PeriodicCredentials.UnresolvedBillingTargets+result.PeriodicCredentials.UnresolvedActiveTargets > 0 {
		result.Warnings = append(result.Warnings, "periodic_key_unresolved")
	}
	result.MinimumScheduledRequests = result.ScheduledSamples + result.MinimumJudgeRequests
	result.MinimumTotalRequestsPerDay = result.MinimumRequestsPerDay + result.MinimumScheduledRequests + result.MinimumCandyRequests + result.MinimumActiveRequests
	if policy.Enabled && result.MinimumTotalRequestsPerDay > result.RegularDayCapacity {
		result.BlockingReasons = append(result.BlockingReasons, "day_capacity")
	}
	result.CanSave = !policy.Enabled || len(result.BlockingReasons) == 0
	return result
}

type monitoringProbeCoverageBillingDemand struct {
	view               *MonitoringProbeCoverageBillingScopes
	sites, credentials map[string]int64
	management         map[int64]struct{}
	bucketedTargets    int64
	credentialTargets  int64
}

func (d *monitoringProbeCoverageBillingDemand) addNewAPI(target MonitoringProbeCoverageAccount) {
	site, err := NewAPIAccountSite(&target.Account)
	if err != nil || site != target.NewAPIAuthorizationSite || target.NewAPIAuthorizationID <= 0 || target.NewAPIAuthorizationUserID <= 0 {
		d.view.UnresolvedNewAPIAuthorizations++
		d.view.UnresolvedCredentials++
		if err != nil || site != target.NewAPIAuthorizationSite {
			d.view.UnresolvedSites++
		}
		return
	}
	d.sites[site] += 4
	d.bucketedTargets++
	d.credentials[fmt.Sprintf("new_api_authorization:%d", target.NewAPIAuthorizationID)] += 4
	d.management[target.NewAPIAuthorizationID] = struct{}{}
	// The management secret is deliberately never loaded/decrypted. Reuse the
	// real builder to retain additional final credentials, excluding the known
	// management Authorization header which is represented by the ID group.
	headers := make(http.Header)
	applyNewAPIManagementHeaders(headers, &target.Account, target.NewAPIAuthorizationUserID, "")
	headers.Del("Authorization")
	if d.addCredentials(headers, 4, true) {
		d.credentialTargets++
	}
}

func (d *monitoringProbeCoverageBillingDemand) addCredentials(headers http.Header, requests int64, primaryKnown bool) bool {
	keys := config.UpstreamCredentialFingerprints(&http.Request{Header: headers})
	unresolved := len(keys) == 0 && !primaryKnown
	for _, key := range keys {
		if key == config.UnverifiedUpstreamCredential {
			unresolved = true
			continue
		}
		d.credentials[key] += requests
	}
	if unresolved {
		d.view.UnresolvedCredentials++
	}
	return !unresolved
}

func (d *monitoringProbeCoverageBillingDemand) finish(c *MonitoringProbeCoverage, policy MonitoringProbeBudgetSettings, billing UpstreamBillingProbeSettings) {
	v := d.view
	v.Sites, v.CredentialGroups, v.NewAPIAuthorizationGroups = int64(len(d.sites)), int64(len(d.credentials)), int64(len(d.management))
	validInterval := billing.IntervalMinutes >= upstreamBillingProbeMinIntervalMinutes && billing.IntervalMinutes <= upstreamBillingProbeMaxIntervalMinutes
	if validInterval {
		// Unlike the global regular allowance, per-site and per-key counters do
		// not subtract recovery reserve at physical admission.
		v.SiteSweepCapacity = policy.SiteRequestsPerMinute * int64(billing.IntervalMinutes)
		v.CredentialSweepCapacity = policy.KeyRequestsPerMinute * int64(billing.IntervalMinutes)
	}
	for _, calls := range d.sites {
		if calls > v.MaxSiteRequestsPerSweep {
			v.MaxSiteRequestsPerSweep = calls
		}
		if validInterval && calls > v.SiteSweepCapacity {
			v.OverLimitSites++
		}
	}
	for _, calls := range d.credentials {
		if calls > v.MaxCredentialRequestsPerSweep {
			v.MaxCredentialRequestsPerSweep = calls
		}
		if validInterval && calls > v.CredentialSweepCapacity {
			v.OverLimitCredentialGroups++
		}
	}
	if policy.Enabled && billing.Enabled {
		if v.OverLimitSites > 0 {
			c.BlockingReasons = append(c.BlockingReasons, "site_capacity")
		}
		if v.OverLimitCredentialGroups > 0 {
			c.BlockingReasons = append(c.BlockingReasons, "key_capacity")
		}
	}
	if v.UnresolvedSites+v.UnresolvedCredentials+v.UnresolvedNewAPIAuthorizations > 0 {
		c.Warnings = append(c.Warnings, "billing_scope_unresolved")
	}
	c.BillingScopes = v
}

func (c *MonitoringProbeCoverage) compileActive(monitors []ChannelMonitor, sites, credentials map[string]int64) {
	for _, monitor := range monitors {
		if !monitor.Enabled {
			continue
		}
		c.ActiveMonitors++
		mode := defaultCheckMode(monitor.CheckMode)
		if monitorCheckModeUsesQuota(mode) {
			// Cached/coalesced/passive usage queries have no fixed physical
			// call count. Never forecast them as free or predict their tokens.
			c.ActiveQuotaMonitors++
		}
		if validateProvider(monitor.Provider) != nil || validateCheckMode(monitor.Provider, mode) != nil ||
			validateInterval(monitor.IntervalSeconds) != nil || validateJitter(monitor.JitterSeconds, monitor.IntervalSeconds) != nil {
			c.UnresolvedActiveMonitors++
			c.PeriodicSites.UnresolvedActiveTargets++
			c.PeriodicCredentials.UnresolvedActiveTargets++
			continue
		}
		if mode == MonitorCheckModeQuota {
			continue
		}
		valid := strings.TrimSpace(monitor.PrimaryModel) != ""
		for _, model := range monitor.ExtraModels {
			valid = valid && strings.TrimSpace(model) != ""
		}
		if !valid {
			c.UnresolvedActiveMonitors++
			c.PeriodicSites.UnresolvedActiveTargets++
			c.PeriodicCredentials.UnresolvedActiveTargets++
			continue
		}
		// The runner has unknown phase and ± jitter. A successful-check
		// target uses the longest timer interval as an optimistic lower
		// bound, without startup, retries, overlap skips or worker headroom.
		runs := int64(86400 / (monitor.IntervalSeconds + monitor.JitterSeconds))
		c.ActiveMonitorRuns += runs
		c.MinimumActiveRequests += runs * int64(2+len(monitor.ExtraModels)) // primary + extras + one HEAD
		// Match physical admission: model calls use the configured tenant;
		// the unauthenticated HEAD uses extractOrigin, not that tenant path.
		unresolved := false
		var modelRequests, headRequests int64
		if site, err := CanonicalNewAPISite(monitor.Endpoint); err == nil {
			modelRequests = runs * int64(1+len(monitor.ExtraModels))
			sites[site] += modelRequests
			c.PeriodicSites.ActiveModelRequestsPerDay += modelRequests
		} else {
			unresolved = true
		}
		origin, err := extractOrigin(monitor.Endpoint)
		if site, siteErr := CanonicalNewAPISite(origin); err == nil && siteErr == nil {
			sites[site] += runs
			headRequests = runs
			c.PeriodicSites.OriginPingRequestsPerDay += runs
		} else {
			unresolved = true
		}
		if unresolved {
			c.PeriodicSites.UnresolvedActiveTargets++
		}
		keys := ValidatedChannelMonitorBudgetCredentials(&monitor)
		if len(keys) == 0 || unresolved {
			c.PeriodicCredentials.UnresolvedActiveTargets++
		}
		for _, key := range keys {
			credentials[key] += modelRequests + headRequests
			c.PeriodicCredentials.ActiveModelAttributionsPerDay += modelRequests
			c.PeriodicCredentials.OriginPingAttributionsPerDay += headRequests
		}
	}
	if c.ActiveQuotaMonitors > 0 {
		c.Warnings = append(c.Warnings, "active_quota_unestimated")
	}
	if c.UnresolvedActiveMonitors > 0 {
		c.Warnings = append(c.Warnings, "active_unresolved")
	}
	if !c.ActiveInventoryComplete {
		c.Warnings = append(c.Warnings, "active_inventory_truncated")
	}
}

func (c *MonitoringProbeCoverage) compileCandy(probes []ChannelMonitorV2CandyProbe) {
	for _, probe := range probes {
		if !probe.Enabled {
			continue
		}
		c.CandyProbes++
		if strings.TrimSpace(probe.Model) == "" || probe.IntervalMinutes < 1 || probe.IntervalMinutes > 1440 {
			c.UnresolvedCandyProbes++
			continue
		}
		interval := time.Duration(probe.IntervalMinutes) * time.Minute
		first := c.ScheduleWindowStart.Truncate(interval)
		if first.Before(c.ScheduleWindowStart) {
			first = first.Add(interval)
		}
		// Match runOne's absolute UTC slots, not a midnight-reset cadence. The
		// partial slot carried in from the previous day may already be claimed.
		if first.Before(c.ScheduleWindowEnd) {
			c.MinimumCandyRequests += 1 + int64(c.ScheduleWindowEnd.Add(-time.Nanosecond).Sub(first)/interval)
		}
	}
	if c.UnresolvedCandyProbes > 0 {
		c.Warnings = append(c.Warnings, "candy_unresolved")
	}
	if !c.CandyInventoryComplete {
		c.Warnings = append(c.Warnings, "candy_inventory_truncated")
	}
}

func (c *MonitoringProbeCoverage) compileSchedules(plans []MonitoringProbeCoveragePlan) {
	// Reuse each cron expansion across accounts/models. Even 10,000 identical
	// plans expand only once, with at most 1,440 minute slots in this UTC day.
	type cadence struct {
		expression string
		notBefore  time.Time
	}
	runs := make(map[cadence]int64)
	for _, target := range plans {
		plan, cfg := &target.Plan, target.Plan.PelicanConfig
		if !plan.Enabled {
			continue
		}
		if target.Group {
			c.GroupPlans++
		} else {
			c.AccountPlans++
		}
		if strings.TrimSpace(plan.ModelID) == "" || plan.NextRunAt == nil || plan.NextRunAt.IsZero() ||
			(cfg != nil && cfg.Quality != nil && cfg.Quality.Action == QualityActionOpenAITwoFAAdmission) {
			c.UnresolvedPlans++
			continue
		}
		samples, judge := int64(1), int64(0)
		unresolved := false
		if cfg != nil {
			parallel := cfg.ParallelCount
			if target.Group && parallel < 1 {
				parallel = 1
			}
			models := 1
			if !target.Group && len(cfg.ModelIDs) > 0 {
				models = len(cfg.ModelIDs)
				for _, model := range cfg.ModelIDs {
					if strings.TrimSpace(model) == "" {
						models = 0
						break
					}
				}
			}
			if parallel < 1 || parallel > 8 || models < 1 || models > 50 || models*parallel > 100 {
				unresolved = true
			} else {
				samples = int64(models * parallel)
				if !target.Group && cfg.Quality != nil && cfg.Quality.Judge != nil && !isOpenAICodexStateProbePlan(cfg) {
					switch cfg.Quality.Judge.Kind {
					case "", "llm", "jev":
						judge = samples
					default:
						unresolved = true
					}
				}
			}
		} else if target.Group {
			unresolved = true
		}
		if unresolved {
			c.UnresolvedPlans++
			continue
		}
		notBefore := c.ScheduleWindowStart
		if plan.NextRunAt.After(notBefore) {
			notBefore = *plan.NextRunAt
		}
		key := cadence{plan.CronExpression, notBefore.UTC()}
		count, found := runs[key]
		if !found {
			schedule, err := scheduledTestCronParser.Parse(plan.CronExpression)
			if err != nil {
				c.UnresolvedPlans++
				continue
			}
			for next := schedule.Next(notBefore.Add(-time.Nanosecond).In(time.Local)); !next.IsZero() && next.Before(c.ScheduleWindowEnd); next = schedule.Next(next) {
				count++
			}
			runs[key] = count
		}
		c.ScheduledRuns += count
		c.ScheduledSamples += count * samples
		c.MinimumJudgeRequests += count * judge
	}
	if c.UnresolvedPlans > 0 {
		c.Warnings = append(c.Warnings, "scheduled_unresolved")
	}
	if !c.PlanInventoryComplete {
		c.Warnings = append(c.Warnings, "scheduled_inventory_truncated")
	}
}

func (c *MonitoringProbeCoverage) ValidateSave() error {
	if c.CanSave {
		return nil
	}
	return infraerrors.BadRequest("MONITORING_PROBE_COVERAGE_IMPOSSIBLE",
		fmt.Sprintf("configured monitoring coverage exceeds capacity (%v): %d billing accounts, %d minutes, at least %d calls/sweep and %d calls/day including %d scheduled, %d candy and %d V1 model/HEAD calls; quota demand is unestimated; regular day capacity is %d; increase capacity or relax the cadence", c.BlockingReasons, c.Accounts, c.IntervalMinutes, c.MinimumRequestsPerSweep, c.MinimumTotalRequestsPerDay, c.MinimumScheduledRequests, c.MinimumCandyRequests, c.MinimumActiveRequests, c.RegularDayCapacity))
}

func (s *MonitoringProbeBudgetService) PreviewCoverage(ctx context.Context, settings MonitoringProbeBudgetSettings) (*MonitoringProbeCoverage, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if s == nil || s.repo == nil {
		return nil, ErrMonitoringProbeBudgetUnavailable
	}
	coverage, err := s.repo.PreviewCoverage(ctx, settings)
	if err != nil || coverage == nil {
		return coverage, err
	}
	return s.runtimeCoverage(coverage), nil
}
