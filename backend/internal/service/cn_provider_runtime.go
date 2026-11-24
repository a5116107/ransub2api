package service

import "time"

// This is a responding-process observation, not a fleet registry or a future
// physical-call estimate. A missing scan is unknown; only an observed complete
// empty queue proves zero targets at that scan's timestamp.
type CNMonitoringRuntime struct {
	Version         int                    `json:"version"`
	Scope           string                 `json:"scope"`
	InstanceID      string                 `json:"instance_id"`
	State           string                 `json:"state"`
	IntervalSeconds *float64               `json:"interval_seconds"`
	ObservedAt      time.Time              `json:"observed_at"`
	LastScan        *CNMonitoringInventory `json:"last_scan"`
}

type CNMonitoringInventory struct {
	ObservedAt     time.Time `json:"observed_at"`
	Complete       bool      `json:"complete"`
	Targets        int64     `json:"targets"`
	BalanceTargets int64     `json:"balance_targets"`
	QuotaTargets   int64     `json:"quota_targets"`
}

func (s *CNProviderBalanceCheckService) MonitoringRuntime() *CNMonitoringRuntime {
	view := &CNMonitoringRuntime{Version: 1, Scope: "responding_process", State: "unavailable", ObservedAt: time.Now().UTC()}
	if s == nil {
		return view
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	view.InstanceID = s.instanceID
	seconds := s.interval.Seconds()
	view.IntervalSeconds = &seconds
	if s.lastScan != nil {
		scan := *s.lastScan
		view.LastScan = &scan
	}
	switch {
	case s.stopped && s.drained:
		view.State = "stopped"
	case s.stopped:
		view.State = "stopping"
	case s.cfg == nil:
	case !s.cfg.Gateway.CNProviders.BalanceCheckEnabled || s.interval <= 0:
		view.State = "disabled"
	case s.accountRepo == nil || s.balanceService == nil:
	case s.turnCoordinationFailed:
		view.State = "coordination_unavailable"
	case s.started:
		view.State = "running"
	case !s.cfg.RunsBackgroundJobs():
		view.State = "role_disabled"
	default:
		view.State = "not_started"
	}
	return view
}

func (s *CNProviderBalanceCheckService) observeMonitoringInventory(targets []accountProbeTarget, complete bool) {
	scan := &CNMonitoringInventory{ObservedAt: time.Now().UTC(), Complete: complete, Targets: int64(len(targets))}
	for _, target := range targets {
		if target.balance {
			scan.BalanceTargets++
		} else {
			scan.QuotaTargets++
		}
	}
	s.lifecycleMu.Lock()
	s.lastScan = scan
	s.lifecycleMu.Unlock()
}
