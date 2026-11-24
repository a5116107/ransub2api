package service

import (
	"time"

	"github.com/google/uuid"
)

// These are local executor observations, not physical requests, model-quality
// verdicts, a fleet inventory, or a future-capacity forecast.
type MonitoringProbeGroupQueues struct {
	V2 *MonitoringProbeQueueRuntime `json:"v2"`
	V3 *MonitoringProbeQueueRuntime `json:"v3"`
}

type MonitoringProbeQueueRuntime struct {
	Version    int                            `json:"version"`
	Scope      string                         `json:"scope"`
	InstanceID string                         `json:"instance_id"`
	State      string                         `json:"state"`
	ObservedAt time.Time                      `json:"observed_at"`
	LastScan   *MonitoringProbeQueueInventory `json:"last_scan"`
	Cycle      *MonitoringProbeQueueCycle     `json:"cycle"`
}

type MonitoringProbeQueueInventory struct {
	ObservedAt      time.Time `json:"observed_at"`
	Targets         int64     `json:"targets"`
	Due             int64     `json:"due"`
	PriorityVersion int       `json:"priority_version"`
	ReviewTargets   int64     `json:"review_targets"`
	RoutineTargets  int64     `json:"routine_targets"`
}

type MonitoringProbeQueueCycle struct {
	StartedAt       time.Time                    `json:"started_at"`
	FinishedAt      *time.Time                   `json:"finished_at"`
	Queued          int64                        `json:"queued"`
	Active          int64                        `json:"active"`
	Finished        int64                        `json:"finished"`
	Deferred        int64                        `json:"deferred"`
	OldestPendingAt *time.Time                   `json:"oldest_pending_at"`
	BudgetDeferral  *MonitoringProbeBudgetDenial `json:"budget_deferral"`
}

func (q *monitoringProbeQueue) runtimeSnapshot() *MonitoringProbeQueueRuntime {
	view := &MonitoringProbeQueueRuntime{Version: 1, Scope: "responding_process", State: "unavailable", ObservedAt: time.Now().UTC()}
	if q == nil {
		return view
	}
	q.runtimeMu.Lock()
	defer q.runtimeMu.Unlock()
	view.ObservedAt = time.Now().UTC()
	if q.runtime.InstanceID == "" {
		q.runtime.InstanceID = uuid.NewString()
	}
	view.InstanceID, view.State = q.runtime.InstanceID, q.runtime.State
	if view.State == "" {
		view.State = "unobserved"
	}
	if q.runtime.LastScan != nil {
		scan := *q.runtime.LastScan
		view.LastScan = &scan
	}
	if q.runtime.Cycle != nil {
		cycle := *q.runtime.Cycle
		if cycle.FinishedAt != nil {
			at := *cycle.FinishedAt
			cycle.FinishedAt = &at
		}
		if cycle.OldestPendingAt != nil {
			at := *cycle.OldestPendingAt
			cycle.OldestPendingAt = &at
		}
		if cycle.BudgetDeferral != nil {
			denial := *cycle.BudgetDeferral
			cycle.BudgetDeferral = &denial
		}
		view.Cycle = &cycle
	}
	return view
}

func (q *monitoringProbeQueue) observeState(state string) {
	q.runtimeMu.Lock()
	q.runtime.State = state
	q.runtimeMu.Unlock()
}

// Called only while the owning executor holds its tick lock.
func (q *monitoringProbeQueue) oldestPending() *time.Time {
	var oldest time.Time
	for _, turn := range q.turns {
		if !turn.WaitingSince.IsZero() && (oldest.IsZero() || turn.WaitingSince.Before(oldest)) {
			oldest = turn.WaitingSince
		}
	}
	if oldest.IsZero() {
		return nil
	}
	return &oldest
}

func (q *monitoringProbeQueue) observeInventory(now time.Time, targets, due, reviews int) {
	oldest := q.oldestPending()
	q.runtimeMu.Lock()
	q.runtime.State = "queued"
	q.runtime.LastScan = &MonitoringProbeQueueInventory{ObservedAt: now, Targets: int64(targets), Due: int64(due),
		PriorityVersion: 1, ReviewTargets: int64(reviews), RoutineTargets: int64(due - reviews)}
	q.runtime.Cycle = &MonitoringProbeQueueCycle{StartedAt: now, Queued: int64(due), OldestPendingAt: oldest}
	q.runtimeMu.Unlock()
}

func (q *monitoringProbeQueue) observeProgress(queued, active, finished, deferred int, denial *MonitoringProbeBudgetDenial) {
	oldest := q.oldestPending()
	q.runtimeMu.Lock()
	defer q.runtimeMu.Unlock()
	if cycle := q.runtime.Cycle; cycle != nil {
		cycle.Queued, cycle.Active, cycle.Finished, cycle.Deferred = int64(queued), int64(active), int64(finished), int64(deferred)
		cycle.OldestPendingAt = oldest
		if denial != nil {
			copy := *denial
			cycle.BudgetDeferral = &copy
		}
	}
}

func (q *monitoringProbeQueue) finishRuntime(state string) {
	q.runtimeMu.Lock()
	defer q.runtimeMu.Unlock()
	q.runtime.State = state
	if q.runtime.Cycle != nil {
		now := time.Now().UTC()
		q.runtime.Cycle.FinishedAt = &now
	}
}

func (s *ChannelMonitorV2CandyService) MonitoringRuntime() *MonitoringProbeQueueRuntime {
	if s == nil || s.repo == nil || s.config == nil || s.groups == nil {
		return (*monitoringProbeQueue)(nil).runtimeSnapshot()
	}
	return s.queue.runtimeSnapshot()
}

func (p *ChannelMonitorV3Prober) MonitoringRuntime() *MonitoringProbeQueueRuntime {
	if p == nil || p.repo == nil || p.groups == nil {
		return (*monitoringProbeQueue)(nil).runtimeSnapshot()
	}
	return p.queue.runtimeSnapshot()
}
