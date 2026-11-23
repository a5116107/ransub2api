package service

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const monitoringProbeQueueWorkers = 4

type monitoringProbeQueueKey struct {
	groupID int64
	scope   string
}

type monitoringProbeTurn struct{ served, tried uint64 }

type monitoringProbeQueueRun struct{ served, stop bool }

// Local scheduling age, not evidence freshness or a distributed reservation.
// The owning executor holds its tick lock throughout order and run.
type monitoringProbeQueue struct {
	turnSequence uint64
	turns        map[monitoringProbeQueueKey]monitoringProbeTurn
}

// New inventory joins behind pending work. Only a complete inventory may prune
// old state; callers preserve it when their inventory read fails.
func (q *monitoringProbeQueue) order(targets, due []monitoringProbeQueueKey) {
	if q.turns == nil {
		q.turns = make(map[monitoringProbeQueueKey]monitoringProbeTurn, len(targets))
	}
	q.turnSequence++
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
		}
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
}

// A completed round forms one age tier so later rounds retain concurrency.
// Older tiers drain first: a recent fast job must not spend the last budget
// while an older one is still acquiring its scheduler or physical slot.
func (q *monitoringProbeQueue) run(ctx context.Context, due []monitoringProbeQueueKey, run func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun) {
	type finished struct {
		key monitoringProbeQueueKey
		run monitoringProbeQueueRun
	}
	done := make(chan finished, monitoringProbeQueueWorkers)
	active, next, exhausted := 0, 0, false
	q.turnSequence++
	servedAge := q.turnSequence
	var age uint64
	if len(due) > 0 {
		age = q.turns[due[0]].served
	}
	for {
		for !exhausted && ctx.Err() == nil && active < monitoringProbeQueueWorkers && next < len(due) {
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
		if active == 0 {
			if !exhausted && ctx.Err() == nil && next < len(due) {
				age = q.turns[due[next]].served
				continue
			}
			return
		}
		result := <-done
		active--
		if result.run.served {
			turn := q.turns[result.key]
			turn.served = servedAge
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
