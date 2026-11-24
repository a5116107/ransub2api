package service

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	monitoringProbeQueueWorkers        = 4
	monitoringProbeQueueStoreTimeout   = 2 * time.Second
	monitoringProbeQueueAttemptTimeout = 2 * time.Minute
)

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

type monitoringProbeQueueRun struct {
	served, stop   bool
	budgetDeferral *MonitoringProbeBudgetDenial
}

type monitoringProbeQueueTarget struct {
	identity, evidenceKey string
	intervalMinutes       int
}

// Shared scheduling age, not evidence freshness or execution authority.
// The map is a single scan's snapshot, never a process-local fallback.
type monitoringProbeQueue struct {
	round     int64
	store     GroupMonitoringTurnRepository
	turns     map[monitoringProbeQueueKey]GroupMonitoringTurn
	runtimeMu sync.Mutex
	runtime   MonitoringProbeQueueRuntime
}

// Call only after a complete inventory read. Configuration fencing and pruning
// happen in the same transaction; a failed scan must not erase shared age.
func (q *monitoringProbeQueue) order(ctx context.Context, source any, cfg GroupMonitoringQueueConfig, targets map[monitoringProbeQueueKey]monitoringProbeQueueTarget, due []monitoringProbeQueueKey) bool {
	store, ok := source.(GroupMonitoringTurnRepository)
	if !ok {
		q.observeState("coordination_unavailable")
		return false
	}
	dueSet := make(map[monitoringProbeQueueKey]bool, len(due))
	for _, key := range due {
		dueSet[key] = true
	}
	input := make([]GroupMonitoringTurn, 0, len(targets))
	byHash := make(map[monitoringProbeQueueKey]monitoringProbeQueueKey, len(targets))
	for key, target := range targets {
		turn := GroupMonitoringTurn{GroupID: key.groupID, ScopeHash: fmt.Sprintf("%x", sha256.Sum256([]byte(key.scope))),
			IdentityHash: fmt.Sprintf("%x", sha256.Sum256([]byte(target.identity))), Due: dueSet[key],
			EvidenceKey: target.evidenceKey, IntervalMinutes: target.intervalMinutes}
		input = append(input, turn)
		byHash[monitoringProbeQueueKey{groupID: key.groupID, scope: turn.ScopeHash}] = key
	}
	loadCtx, cancel := context.WithTimeout(ctx, monitoringProbeQueueStoreTimeout)
	queue, err := store.SyncGroupMonitoringTurns(loadCtx, cfg, input)
	cancel()
	if err != nil || queue == nil || queue.Round <= 0 || len(queue.Turns) != len(targets) {
		q.observeState("coordination_unavailable")
		return false
	}
	turns := make(map[monitoringProbeQueueKey]GroupMonitoringTurn, len(targets))
	for _, turn := range queue.Turns {
		key, exists := byHash[monitoringProbeQueueKey{groupID: turn.GroupID, scope: turn.ScopeHash}]
		if !exists || turn.Served <= 0 || turn.Served >= queue.Round || turn.Tried < 0 ||
			turn.IdentityHash != fmt.Sprintf("%x", sha256.Sum256([]byte(targets[key].identity))) ||
			!turn.ReviewDueAt.IsZero() && (cfg.ObservedAt.IsZero() || turn.ReviewDueAt.After(cfg.ObservedAt)) {
			q.observeState("coordination_unavailable")
			return false
		}
		if _, duplicate := turns[key]; duplicate {
			q.observeState("coordination_unavailable")
			return false
		}
		turns[key] = turn
	}
	q.store, q.round, q.turns = store, queue.Round, turns
	slices.SortFunc(due, func(a, b monitoringProbeQueueKey) int {
		left, right := q.turns[a], q.turns[b]
		if order := cmp.Compare(left.Served, right.Served); order != 0 {
			return order
		}
		if left.ReviewDueAt.IsZero() != right.ReviewDueAt.IsZero() {
			if left.ReviewDueAt.IsZero() {
				return 1
			}
			return -1
		}
		if order := left.ReviewDueAt.Truncate(time.Microsecond).Compare(right.ReviewDueAt.Truncate(time.Microsecond)); order != 0 {
			return order
		}
		if order := cmp.Compare(left.Tried, right.Tried); order != 0 {
			return order
		}
		if order := cmp.Compare(a.groupID, b.groupID); order != 0 {
			return order
		}
		return strings.Compare(a.scope, b.scope)
	})
	reviews := 0
	for _, key := range due {
		if !q.turns[key].ReviewDueAt.IsZero() {
			reviews++
		}
	}
	q.observeInventory(time.Now().UTC(), len(targets), len(due), reviews)
	return true
}

// A completed round forms one age tier so later rounds retain concurrency.
// Older tiers drain first, then native reviews before routine work within that
// tier. A recent fast job must not spend the last budget while an older/review
// job is still acquiring its scheduler or physical slot.
func (q *monitoringProbeQueue) run(ctx context.Context, due []monitoringProbeQueueKey, run func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun) {
	ctx = withMonitoringProbeDispatchStop(ctx)
	dispatch := ctx.Value(monitoringProbeDispatchStopKey{}).(*monitoringProbeDispatchStop)
	stopSignal := dispatch.stopped
	q.observeState("running")
	type finished struct {
		key  monitoringProbeQueueKey
		turn GroupMonitoringTurn
		run  monitoringProbeQueueRun
	}
	done := make(chan finished, monitoringProbeQueueWorkers)
	active, next, exhausted := 0, 0, false
	finishedCount, deferred := 0, 0
	var lastDenial *MonitoringProbeBudgetDenial
	coordinationFailed := false
	claimChanged := false
	var age int64
	var review bool
	if len(due) > 0 {
		age = q.turns[due[0]].Served
		review = !q.turns[due[0]].ReviewDueAt.IsZero()
	}
	for {
		if denial := dispatch.denial.Load(); denial != nil {
			exhausted, lastDenial, stopSignal = true, denial, nil
		}
		for !coordinationFailed && !exhausted && !monitoringProbeDispatchStopped(ctx) && ctx.Err() == nil && active < monitoringProbeQueueWorkers && next < len(due) {
			key := due[next]
			if q.turns[key].Served != age || !q.turns[key].ReviewDueAt.IsZero() != review {
				break
			}
			claimCtx, cancel := context.WithTimeout(ctx, monitoringProbeQueueStoreTimeout)
			turn, claimed, err := q.store.ClaimGroupMonitoringTurn(claimCtx, q.turns[key])
			cancel()
			if err != nil {
				coordinationFailed = true
				q.observeState("coordination_unavailable")
				break
			}
			next++
			if !claimed {
				// Another scan/identity owns it; this is not a health result.
				// Drain independent targets in this tier. A peer must not use
				// the same age's routine budget ahead of outstanding reviews.
				claimChanged = true
				finishedCount++
				continue
			}
			q.turns[key] = turn
			active++
			go func() {
				result := finished{key: key, turn: turn}
				defer func() {
					if recover() != nil {
						logger.LegacyPrintf("service.monitoring_probe_queue", "probe worker panicked")
					}
					done <- result
				}()
				attemptCtx, cancel := context.WithTimeout(ctx, monitoringProbeQueueAttemptTimeout)
				defer cancel()
				result.run = run(attemptCtx, key)
			}()
		}
		q.observeProgress(len(due)-next, active, finishedCount, deferred, lastDenial)
		if active == 0 {
			reviewGate := claimChanged && review && next < len(due) && q.turns[due[next]].Served == age && q.turns[due[next]].ReviewDueAt.IsZero()
			if !coordinationFailed && !exhausted && !reviewGate && ctx.Err() == nil && next < len(due) {
				age = q.turns[due[next]].Served
				review = !q.turns[due[next]].ReviewDueAt.IsZero()
				claimChanged = false
				continue
			}
			state := "idle"
			switch {
			case coordinationFailed:
				state = "coordination_unavailable"
			case ctx.Err() != nil:
				state = "canceled"
			case exhausted:
				state = "budget_paused"
			case reviewGate:
				state = "claim_changed"
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
		// Admitted work still consumes its turn after parent cancellation.
		// This bounded save does not renew evidence or refund procurement.
		saveCtx, cancel := context.WithTimeout(context.Background(), monitoringProbeQueueStoreTimeout)
		round := int64(0)
		if result.run.served {
			round = q.round
		}
		updated, err := q.store.FinishGroupMonitoringTurn(saveCtx, result.turn, round)
		cancel()
		if err != nil {
			coordinationFailed = true
			q.observeState("coordination_unavailable")
		} else if updated {
			turn := result.turn
			turn.ClaimUntil = time.Time{}
			if result.run.served {
				turn.Served, turn.WaitingSince = q.round, time.Time{}
			}
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
