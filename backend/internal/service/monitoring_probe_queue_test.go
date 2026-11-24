package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMonitoringProbeQueueNativeReviewTiers(t *testing.T) {
	keys := []monitoringProbeQueueKey{{groupID: 1}, {groupID: 2}, {groupID: 3}, {groupID: 4}}
	store := &groupMonitoringTurnsFake{nativeReviewDue: map[string]time.Time{}}
	var q monitoringProbeQueue
	q.store = store
	orderProbeQueue(t, &q, keys, keys)
	for _, key := range keys {
		store.setAge(key, 10, 0)
	}
	store.setAge(keys[3], 1, 0)
	hash := fmt.Sprintf("%x", sha256.Sum256(nil))
	store.nativeReviewDue[fmt.Sprintf("2/%s", hash)] = time.Now().Add(-time.Minute)
	store.nativeReviewDue[fmt.Sprintf("3/%s", hash)] = time.Now().Add(-2 * time.Minute)
	due := slices.Clone(keys)
	orderProbeQueue(t, &q, keys, due)
	require.Equal(t, []monitoringProbeQueueKey{keys[3], keys[2], keys[1], keys[0]}, due,
		"unserved old rounds precede reviews; reviews then follow their native due clocks")
	view := q.runtimeSnapshot()
	require.Equal(t, 1, view.LastScan.PriorityVersion)
	require.EqualValues(t, 2, view.LastScan.ReviewTargets)
	require.EqualValues(t, 2, view.LastScan.RoutineTargets)
}

func TestMonitoringProbeQueueReviewsDrainBeforeRoutine(t *testing.T) {
	store := &groupMonitoringTurnsFake{nativeReviewDue: map[string]time.Time{}}
	q := monitoringProbeQueue{store: store}
	keys := make([]monitoringProbeQueueKey, 8)
	hash := fmt.Sprintf("%x", sha256.Sum256(nil))
	for i := range keys {
		keys[i].groupID = int64(i + 1)
		if i >= 4 {
			store.nativeReviewDue[fmt.Sprintf("%d/%s", i+1, hash)] = time.Now().Add(-time.Minute)
		}
	}
	orderProbeQueue(t, &q, keys, keys)
	entered, gate, done := make(chan struct{}, 4), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	ctx, cancel := context.WithCancel(t.Context())
	var routines atomic.Int32
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("native review batch did not drain")
		}
	})
	go func() {
		defer close(done)
		q.run(ctx, keys, func(ctx context.Context, key monitoringProbeQueueKey) monitoringProbeQueueRun {
			if key.groupID > 4 {
				entered <- struct{}{}
				select {
				case <-gate:
				case <-ctx.Done():
				}
			} else {
				routines.Add(1)
			}
			return monitoringProbeQueueRun{served: true}
		})
	}()
	for range 4 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("all four review workers must retain their original concurrency")
		}
	}
	require.Zero(t, routines.Load(), "sorting alone must not allow a routine goroutine to race a blocked review")
	release()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("routine phase did not resume")
	}
	require.EqualValues(t, 4, routines.Load())
}

func TestMonitoringProbeQueueInventory(t *testing.T) {
	var q monitoringProbeQueue
	a := monitoringProbeQueueKey{groupID: 4, scope: "a"}
	b := monitoringProbeQueueKey{groupID: 4, scope: "b"}
	c := monitoringProbeQueueKey{groupID: 5, scope: "a"}
	targets := []monitoringProbeQueueKey{c, b, a}
	due := slices.Clone(targets)
	orderProbeQueue(t, &q, targets, due)
	require.Equal(t, []monitoringProbeQueueKey{a, b, c}, due)
	store := q.store.(*groupMonitoringTurnsFake)
	store.setAge(a, 12, 0)
	store.setAge(b, 1, 12)
	newTarget := monitoringProbeQueueKey{groupID: 1, scope: "new"}
	targets = append(targets, newTarget)
	due = slices.Clone(targets)
	orderProbeQueue(t, &q, targets, due)
	require.Equal(t, []monitoringProbeQueueKey{c, b, a, newTarget}, due, "a low-ID newcomer must not jump old pending work")
	orderProbeQueue(t, &q, []monitoringProbeQueueKey{b}, nil)
	require.Len(t, q.turns, 1, "a complete inventory removes obsolete queue state")
	require.Contains(t, q.turns, b)
	orderProbeQueue(t, &q, nil, nil)
	require.Empty(t, q.turns)
}

func TestMonitoringProbeQueueRetainsParallelismAcrossRounds(t *testing.T) {
	var q monitoringProbeQueue
	keys := make([]monitoringProbeQueueKey, 12)
	for i := range keys {
		keys[i] = monitoringProbeQueueKey{groupID: int64(i + 1)}
	}
	orderProbeQueue(t, &q, keys, keys)
	q.run(t.Context(), keys, func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun {
		return monitoringProbeQueueRun{served: true}
	})
	orderProbeQueue(t, &q, keys, keys)
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
	orderProbeQueue(t, &q, keys, keys)
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
	require.Less(t, q.turns[keys[0]].Served, q.turns[keys[1]].Served, "an unobserved panic is not physical admission")
}

type candySaveGateRepo struct {
	*candyRepoFake
	finish func(context.Context, ChannelMonitorV2CandyResult) error
}

func (r *candySaveGateRepo) FinishCandyProbe(ctx context.Context, result ChannelMonitorV2CandyResult) error {
	return r.finish(ctx, result)
}

func TestMonitoringProbeQueueGlobalPauseBeforeSaveDrain(t *testing.T) {
	for _, version := range []string{"v2", "v3"} {
		for _, failSave := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/saveFailure=%v", version, failSave), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					const targets = 64
					deny, others, held := make(chan struct{}), make(chan struct{}), make(chan struct{})
					var releaseDeny, releaseOthers, releaseHeld sync.Once
					var calls, saved atomic.Int32
					var recovered atomic.Bool
					denial := &MonitoringProbeBudgetDenial{Reason: "token_limit", Scope: "global_day", RetryAt: groupTestNow.Add(time.Hour)}
					router := &groupTestRouterFake{}
					for range targets * 2 {
						router.steps = append(router.steps, routeStep{account: account(77, "queue fixture")})
					}
					runAccount := func(ctx context.Context, _ int64, _ string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
						call := calls.Add(1)
						if !recovered.Load() {
							if call == 1 {
								<-deny
								return &ScheduledTestResult{BudgetDeferral: denial}, nil
							}
							<-others
						}
						require.NoError(t, ctx.Err(), "budget pause must not cancel already-admitted work")
						answer := "21"
						if version == "v3" {
							answer = monitorV3Answer(cfg.Prompt)
						}
						return &ScheduledTestResult{Status: "success", ResponseText: answer}, nil
					}
					save := func(ctx context.Context, cause *MonitoringProbeBudgetDenial) error {
						if cause != nil {
							<-held
						}
						require.NoError(t, ctx.Err(), "independent result saves must drain under their original lifetime")
						saved.Add(1)
						if failSave && cause != nil {
							return context.DeadlineExceeded
						}
						return nil
					}
					var run func(context.Context, time.Time)
					var runtime func() *MonitoringProbeQueueRuntime
					if version == "v2" {
						config := candyConfigFixture()
						config.CandyProbes = nil
						for i := range targets {
							probe := candyProbeFixture()
							probe.Model = fmt.Sprintf("probe-%03d", i)
							config.CandyProbes = append(config.CandyProbes, probe)
						}
						repo := &candyRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: config}}
						groups, _ := newGroupTestService(newGroupTestRepoFake(), router, runAccount)
						svc := candyServiceFixture(repo, groups)
						svc.repo = &candySaveGateRepo{candyRepoFake: repo, finish: func(ctx context.Context, result ChannelMonitorV2CandyResult) error {
							if err := save(ctx, result.BudgetDeferral); err != nil {
								return err
							}
							return repo.FinishCandyProbe(ctx, result)
						}}
						run, runtime = svc.RunDue, svc.MonitoringRuntime
					} else {
						prober, repo, _ := monitorV3ProberFixture(router, runAccount)
						defer prober.Stop()
						repo.components = nil
						for i := range targets {
							repo.components = append(repo.components, ChannelMonitorV3Component{ID: int64(i + 1), GroupID: 4, Model: fmt.Sprintf("probe-%03d", i), Enabled: true})
						}
						prober.repo = &monitorV3LifecycleRepo{monitorV3RepoFake: repo, finish: func(ctx context.Context, result ChannelMonitorV3ProbeResult) error {
							if err := save(ctx, result.BudgetDeferral); err != nil {
								return err
							}
							return repo.FinishProbe(ctx, result)
						}}
						run, runtime = prober.RunDue, prober.MonitoringRuntime
					}
					done := make(chan struct{})
					go func() { defer close(done); run(t.Context(), monitorV3ProbeTick) }()
					defer func() {
						releaseDeny.Do(func() { close(deny) })
						releaseOthers.Do(func() { close(others) })
						releaseHeld.Do(func() { close(held) })
						<-done
					}()
					synctest.Wait()
					require.EqualValues(t, monitoringProbeQueueWorkers, calls.Load())
					releaseDeny.Do(func() { close(deny) })
					synctest.Wait()
					waiting := runtime().Cycle
					require.NotNil(t, waiting)
					require.EqualValues(t, monitoringProbeQueueWorkers, waiting.Active)
					require.Zero(t, waiting.Finished)
					require.Zero(t, waiting.Deferred)
					require.Equal(t, denial, waiting.BudgetDeferral, "the stop signal must wake observation without waiting for any worker to finish")
					releaseOthers.Do(func() { close(others) })
					synctest.Wait()
					require.EqualValues(t, monitoringProbeQueueWorkers, calls.Load(), "a global denial must stop new claims before its slow independent save finishes")
					view := runtime()
					require.EqualValues(t, 1, view.Cycle.Active)
					require.EqualValues(t, targets-monitoringProbeQueueWorkers, view.Cycle.Queued)
					require.EqualValues(t, monitoringProbeQueueWorkers-1, view.Cycle.Finished)
					require.Zero(t, view.Cycle.Deferred, "a pending save is not an exited deferred worker")
					require.Equal(t, denial, view.Cycle.BudgetDeferral, "the read-only view exposes why the queue is paused even while saving")
					releaseHeld.Do(func() { close(held) })
					<-done
					require.EqualValues(t, monitoringProbeQueueWorkers, saved.Load())
					require.Equal(t, "budget_paused", runtime().State)
					recovered.Store(true)
					run(t.Context(), monitorV3ProbeTick.Add(5*time.Minute))
					require.EqualValues(t, targets+monitoringProbeQueueWorkers, calls.Load(), "new rounds must not inherit an old batch's pause")
					require.EqualValues(t, targets+monitoringProbeQueueWorkers, saved.Load())
					require.Equal(t, "idle", runtime().State)
					require.Nil(t, runtime().Cycle.BudgetDeferral)
				})
			})
		}
	}
}

func TestMonitoringProbeDispatchSignalScopesAndIsolation(t *testing.T) {
	for _, scope := range []string{"global", "global_day", "global_minute", "system", "unknown", "", "key", "key_minute", "site", "site_minute", "plugin"} {
		t.Run(scope, func(t *testing.T) {
			parent := withMonitoringProbeDispatchStop(t.Context())
			ctx := withMonitoringProbeDispatchStop(parent)
			stop := ctx.Value(monitoringProbeDispatchStopKey{}).(*monitoringProbeDispatchStop)
			denial := &MonitoringProbeBudgetDenial{Reason: "fixture_limit", Scope: scope, RetryAt: time.Now().Add(time.Hour)}
			stopMonitoringProbeDispatch(ctx, nil)
			stopMonitoringProbeDispatch(ctx, denial)
			global := !slices.Contains([]string{"key", "key_minute", "site", "site_minute", "plugin"}, scope)
			require.Equal(t, global, monitoringProbeDispatchStopped(ctx))
			require.False(t, monitoringProbeDispatchStopped(parent), "each native lane owns its own batch signal")
			select {
			case <-stop.stopped:
				require.True(t, global)
			default:
				require.False(t, global)
			}
			if global {
				denial.Reason = "mutated caller value"
				require.Equal(t, "fixture_limit", stop.denial.Load().Reason, "the signal owns a copy, not a mutable returned result")
				var wg sync.WaitGroup
				for range 32 {
					wg.Go(func() {
						stopMonitoringProbeDispatch(ctx, &MonitoringProbeBudgetDenial{Reason: "later denial", Scope: "system"})
					})
				}
				wg.Wait()
				require.Equal(t, "fixture_limit", stop.denial.Load().Reason, "repeat signals must neither close twice nor overwrite the first stop cause")
			}
		})
	}
}
