package service

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMonitoringProbeQueueInventory(t *testing.T) {
	var q monitoringProbeQueue
	a := monitoringProbeQueueKey{groupID: 4, scope: "a"}
	b := monitoringProbeQueueKey{groupID: 4, scope: "b"}
	c := monitoringProbeQueueKey{groupID: 5, scope: "a"}
	targets := []monitoringProbeQueueKey{c, b, a}
	due := slices.Clone(targets)
	q.order(targets, due)
	require.Equal(t, []monitoringProbeQueueKey{a, b, c}, due)
	q.turnSequence += 10
	q.turns[a] = monitoringProbeTurn{served: q.turnSequence}
	q.turns[b] = monitoringProbeTurn{served: 1, tried: q.turnSequence}
	newTarget := monitoringProbeQueueKey{groupID: 1, scope: "new"}
	targets = append(targets, newTarget)
	due = slices.Clone(targets)
	q.order(targets, due)
	require.Equal(t, []monitoringProbeQueueKey{c, b, a, newTarget}, due, "a low-ID newcomer must not jump old pending work")
	q.order([]monitoringProbeQueueKey{b}, nil)
	require.Len(t, q.turns, 1, "a complete inventory removes obsolete queue state")
	require.Contains(t, q.turns, b)
	q.order(nil, nil)
	require.Empty(t, q.turns)
}

func TestMonitoringProbeQueueRetainsParallelismAcrossRounds(t *testing.T) {
	var q monitoringProbeQueue
	keys := make([]monitoringProbeQueueKey, 12)
	for i := range keys {
		keys[i] = monitoringProbeQueueKey{groupID: int64(i + 1)}
	}
	q.order(keys, keys)
	q.run(t.Context(), keys, func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun {
		return monitoringProbeQueueRun{served: true}
	})
	q.order(keys, keys)
	ctx, cancel := context.WithCancel(t.Context())
	entered, release, done := make(chan struct{}, monitoringProbeQueueWorkers), make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() {
		cancel()
		once.Do(func() { close(release) })
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("the probe queue did not drain")
		}
	})
	var calls atomic.Int32
	go func() {
		defer close(done)
		q.run(ctx, keys, func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun {
			calls.Add(1)
			entered <- struct{}{}
			<-release
			return monitoringProbeQueueRun{served: true}
		})
	}()
	for range monitoringProbeQueueWorkers {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("a completed round must remain one concurrent age tier")
		}
	}
	cancel()
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not drain in-flight work")
	}
	require.EqualValues(t, monitoringProbeQueueWorkers, calls.Load(), "canceled queues must not dispatch the rest")
}

func TestMonitoringProbeQueueCanceledAndPanickingWorkers(t *testing.T) {
	var q monitoringProbeQueue
	keys := []monitoringProbeQueueKey{{groupID: 1}, {groupID: 2}, {groupID: 3}, {groupID: 4}, {groupID: 5}}
	q.order(keys, keys)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	q.run(ctx, keys, func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun {
		t.Error("a canceled queue started work")
		return monitoringProbeQueueRun{}
	})
	var calls atomic.Int32
	q.run(t.Context(), keys, func(_ context.Context, key monitoringProbeQueueKey) monitoringProbeQueueRun {
		calls.Add(1)
		if key.groupID == 1 {
			panic("fixture-only diagnostic")
		}
		return monitoringProbeQueueRun{served: true}
	})
	require.EqualValues(t, len(keys), calls.Load(), "a worker panic must neither crash nor strand the queue")
	require.Less(t, q.turns[keys[0]].served, q.turns[keys[1]].served, "an unobserved panic is not physical admission")
}
