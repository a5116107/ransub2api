package service

import (
	"cmp"
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const monitoringProbeQueueWorkers = 4

// Activity-driven usage observations already persist their due clocks. Keep
// those required refreshes ahead of exploratory inventory, without inventing a
// health/quality expiry or resetting any provider backoff. The native runners
// drain the first phase before dispatching the second with their shared slots.
func monitoringUsageProbePhases(accounts []Account, dueTimes map[int64]time.Time) [][]Account {
	sort.SliceStable(accounts, func(i, j int) bool {
		left, right := dueTimes[accounts[i].ID], dueTimes[accounts[j].ID]
		if left.IsZero() != right.IsZero() {
			return !left.IsZero()
		}
		if left.IsZero() {
			left, right = accounts[i].CreatedAt, accounts[j].CreatedAt
		}
		left, right = left.Truncate(time.Microsecond), right.Truncate(time.Microsecond)
		if !left.Equal(right) {
			return left.Before(right)
		}
		return accounts[i].ID < accounts[j].ID
	})
	split := sort.Search(len(accounts), func(i int) bool { return dueTimes[accounts[i].ID].IsZero() })
	return [][]Account{accounts[:split], accounts[split:]}
}

type monitoringProbeQueueKey struct {
	groupID int64
	scope   string
}

type monitoringProbeTurn struct {
	served, tried uint64
	waitingSince  time.Time
}

type monitoringProbeQueueRun struct {
	served, stop   bool
	budgetDeferral *MonitoringProbeBudgetDenial
}

// Local scheduling age, not evidence freshness or a distributed reservation.
// The owning executor holds its tick lock throughout order and run.
type monitoringProbeQueue struct {
	turnSequence uint64
	turns        map[monitoringProbeQueueKey]monitoringProbeTurn
	runtimeMu    sync.Mutex
	runtime      MonitoringProbeQueueRuntime
}

// New inventory joins behind pending work. Only a complete inventory may prune
// old state; callers preserve it when their inventory read fails.
func (q *monitoringProbeQueue) order(targets, due []monitoringProbeQueueKey) {
	now := time.Now().UTC()
	if q.turns == nil {
		q.turns = make(map[monitoringProbeQueueKey]monitoringProbeTurn, len(targets))
	}
	q.turnSequence++
	dueSet := make(map[monitoringProbeQueueKey]bool, len(due))
	for _, key := range due {
		dueSet[key] = true
	}
	present := make(map[monitoringProbeQueueKey]bool, len(targets))
	for _, key := range targets {
		present[key] = true
		if _, exists := q.turns[key]; !exists {
			q.turns[key] = monitoringProbeTurn{served: q.turnSequence}
		}
	}
	for key := range q.turns {
		if !present[key] {
			delete(q.turns, key)
			continue
		}
		turn := q.turns[key]
		if !dueSet[key] {
			turn.waitingSince = time.Time{}
		} else if turn.waitingSince.IsZero() {
			turn.waitingSince = now
		}
		q.turns[key] = turn
	}
	slices.SortFunc(due, func(a, b monitoringProbeQueueKey) int {
		left, right := q.turns[a], q.turns[b]
		if order := cmp.Compare(left.served, right.served); order != 0 {
			return order
		}
		if order := cmp.Compare(left.tried, right.tried); order != 0 {
			return order
		}
		if order := cmp.Compare(a.groupID, b.groupID); order != 0 {
			return order
		}
		return strings.Compare(a.scope, b.scope)
	})
	q.observeInventory(now, len(targets), len(due))
}

// A completed round forms one age tier so later rounds retain concurrency.
// Older tiers drain first: a recent fast job must not spend the last budget
// while an older one is still acquiring its scheduler or physical slot.
func (q *monitoringProbeQueue) run(ctx context.Context, due []monitoringProbeQueueKey, run func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun) {
	ctx = withMonitoringProbeDispatchStop(ctx)
	dispatch := ctx.Value(monitoringProbeDispatchStopKey{}).(*monitoringProbeDispatchStop)
	stopSignal := dispatch.stopped
	q.observeState("running")
	type finished struct {
		key monitoringProbeQueueKey
		run monitoringProbeQueueRun
	}
	done := make(chan finished, monitoringProbeQueueWorkers)
	active, next, exhausted := 0, 0, false
	finishedCount, deferred := 0, 0
	var lastDenial *MonitoringProbeBudgetDenial
	q.turnSequence++
	servedAge := q.turnSequence
	var age uint64
	if len(due) > 0 {
		age = q.turns[due[0]].served
	}
	for {
		if denial := dispatch.denial.Load(); denial != nil {
			exhausted, lastDenial, stopSignal = true, denial, nil
		}
		for !exhausted && !monitoringProbeDispatchStopped(ctx) && ctx.Err() == nil && active < monitoringProbeQueueWorkers && next < len(due) {
			key := due[next]
			if q.turns[key].served != age {
				break
			}
			next++
			q.turnSequence++
			turn := q.turns[key]
			turn.tried = q.turnSequence
			q.turns[key] = turn
			active++
			go func() {
				result := finished{key: key}
				defer func() {
					if recover() != nil {
						logger.LegacyPrintf("service.monitoring_probe_queue", "probe worker panicked")
					}
					done <- result
				}()
				result.run = run(ctx, key)
			}()
		}
		q.observeProgress(len(due)-next, active, finishedCount, deferred, lastDenial)
		if active == 0 {
			if !exhausted && ctx.Err() == nil && next < len(due) {
				age = q.turns[due[next]].served
				continue
			}
			state := "idle"
			switch {
			case ctx.Err() != nil:
				state = "canceled"
			case exhausted:
				state = "budget_paused"
			case deferred > 0:
				state = "deferred"
			}
			q.finishRuntime(state)
			return
		}
		var result finished
		select {
		case <-stopSignal:
			// A sample can stop dispatch before its bounded save returns.
			// Keep draining the original workers and count only their exits.
			continue
		case result = <-done:
		}
		active--
		finishedCount++
		if result.run.budgetDeferral != nil {
			deferred++
			// A later local/plugin deferral must not hide the global reason
			// that stopped this round while its other workers were draining.
			if !exhausted || result.run.stop {
				lastDenial = result.run.budgetDeferral
			}
		}
		if result.run.served {
			turn := q.turns[result.key]
			turn.served = servedAge
			turn.waitingSince = time.Time{}
			q.turns[result.key] = turn
		}
		exhausted = exhausted || result.run.stop
	}
}

func monitoringProbeBudgetStopsQueue(denial *MonitoringProbeBudgetDenial) bool {
	if denial == nil {
		return false
	}
	switch denial.Scope {
	case "key", "key_minute", "site", "site_minute", "plugin":
		return false // Independent credentials, sites and native routes may run.
	default:
		return true
	}
}

// Each native scheduled lane owns one stop signal per batch. This value is
// internal, not configuration or request metadata; it never cancels admitted
// work, grading, independent result storage or lease cleanup.
type monitoringProbeDispatchStopKey struct{}

type monitoringProbeDispatchStop struct {
	denial  atomic.Pointer[MonitoringProbeBudgetDenial]
	stopped chan struct{}
}

func withMonitoringProbeDispatchStop(ctx context.Context) context.Context {
	return context.WithValue(ctx, monitoringProbeDispatchStopKey{}, &monitoringProbeDispatchStop{stopped: make(chan struct{})})
}

func monitoringProbeDispatchStopped(ctx context.Context) bool {
	stop, _ := ctx.Value(monitoringProbeDispatchStopKey{}).(*monitoringProbeDispatchStop)
	return stop != nil && stop.denial.Load() != nil
}

func stopMonitoringProbeDispatch(ctx context.Context, denial *MonitoringProbeBudgetDenial) {
	if stop, ok := ctx.Value(monitoringProbeDispatchStopKey{}).(*monitoringProbeDispatchStop); ok && monitoringProbeBudgetStopsQueue(denial) {
		copy := *denial
		if stop.denial.CompareAndSwap(nil, &copy) {
			close(stop.stopped)
		}
	}
}

func monitoringProbeResultDeferral(result *ScheduledTestResult) *MonitoringProbeBudgetDenial {
	if result == nil {
		return nil
	}
	if result.BudgetDeferral != nil {
		return result.BudgetDeferral
	}
	if result.QualityJudgment != nil {
		return result.QualityJudgment.BudgetDeferral
	}
	return nil
}
