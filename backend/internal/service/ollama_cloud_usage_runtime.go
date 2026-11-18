package service

import (
	"context"
	"time"
)

// Observed bounded work in this runner, not a complete fleet/key inventory or
// a forecast of physical calls. Event work remains independent of auto refresh.
type OllamaMonitoringRuntime struct {
	Version                int                   `json:"version"`
	Scope                  string                `json:"scope"`
	InstanceID             string                `json:"instance_id"`
	State                  string                `json:"state"`
	PeriodicState          string                `json:"periodic_state"`
	ObservedAt             time.Time             `json:"observed_at"`
	ScanIntervalSeconds    float64               `json:"scan_interval_seconds"`
	ScanLimit              int                   `json:"scan_limit"`
	PendingRateLimitProbes int                   `json:"pending_rate_limit_probes"`
	ActiveRateLimitProbes  int                   `json:"active_rate_limit_probes"`
	ActiveRefreshes        int                   `json:"active_refreshes"`
	LastScan               *OllamaMonitoringScan `json:"last_scan"`
}

type OllamaMonitoringScan struct {
	ObservedAt      time.Time `json:"observed_at"`
	State           string    `json:"state"`
	Enabled         *bool     `json:"enabled"`
	IntervalMinutes *int      `json:"interval_minutes"`
	DebounceMinutes *int      `json:"debounce_minutes"`
	Candidates      *int      `json:"candidates"`
	DueGroups       *int      `json:"due_groups"`
	LimitReached    *bool     `json:"limit_reached"`
}

func (s *OllamaCloudUsageService) MonitoringRuntime() *OllamaMonitoringRuntime {
	view := &OllamaMonitoringRuntime{Version: 1, Scope: "responding_process", State: "unavailable", PeriodicState: "unknown", ObservedAt: time.Now().UTC(), ScanIntervalSeconds: ollamaCloudUsageCycleInterval.Seconds(), ScanLimit: ollamaCloudUsageMaxPerCycle}
	if s == nil {
		return view
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	view.InstanceID = s.instanceID
	if s.lastScan != nil {
		scan := *s.lastScan
		if scan.Enabled != nil {
			v := *scan.Enabled
			scan.Enabled = &v
		}
		if scan.IntervalMinutes != nil {
			v := *scan.IntervalMinutes
			scan.IntervalMinutes = &v
		}
		if scan.DebounceMinutes != nil {
			v := *scan.DebounceMinutes
			scan.DebounceMinutes = &v
		}
		if scan.Candidates != nil {
			v := *scan.Candidates
			scan.Candidates = &v
		}
		if scan.DueGroups != nil {
			v := *scan.DueGroups
			scan.DueGroups = &v
		}
		if scan.LimitReached != nil {
			v := *scan.LimitReached
			scan.LimitReached = &v
		}
		view.LastScan = &scan
	}
	switch {
	case s.stopped && s.drained:
		view.State = "stopped"
	case s.stopped:
		view.State = "stopping"
	case s.accountRepo == nil || s.httpUpstream == nil || s.settingService == nil:
	case s.started:
		view.State = "running"
	default:
		view.State = "not_started"
	}
	switch {
	case s.stopped:
		view.PeriodicState = view.State
	case !s.started:
		view.PeriodicState = "not_started"
	case !s.background:
		view.PeriodicState = "role_disabled"
	case view.LastScan == nil || view.LastScan.Enabled == nil:
	case *view.LastScan.Enabled:
		view.PeriodicState = "enabled"
	default:
		view.PeriodicState = "disabled"
	}
	s.probeMu.Lock()
	view.PendingRateLimitProbes = len(s.probeQueue) + s.probeBatchPending
	if s.probeRunning {
		view.ActiveRateLimitProbes = 1
	}
	s.probeMu.Unlock()
	view.ActiveRefreshes = len(s.refreshSlots)
	return view
}

func (s *OllamaCloudUsageService) observeMonitoringScan(ctx context.Context, state string, settings *OllamaCloudUsageSettings, candidates, groups int) {
	if ctx.Err() != nil {
		return
	}
	scan := &OllamaMonitoringScan{ObservedAt: time.Now().UTC(), State: state}
	if settings != nil {
		enabled, interval, debounce := settings.Enabled, settings.IntervalMinutes, settings.DebounceMinutes
		scan.Enabled, scan.IntervalMinutes, scan.DebounceMinutes = &enabled, &interval, &debounce
	}
	if state == "observed" {
		limited := candidates >= ollamaCloudUsageMaxPerCycle
		scan.Candidates, scan.DueGroups, scan.LimitReached = &candidates, &groups, &limited
	}
	s.mu.Lock()
	s.lastScan = scan
	s.mu.Unlock()
}
