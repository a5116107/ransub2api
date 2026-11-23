//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// stubMonitorSvc 实现 monitorRunnerSvc，用于隔离 runner 与真实 service/repo。
type stubMonitorSvc struct {
	enabled   []*ChannelMonitor
	runCount  atomic.Int64
	runCalled chan int64 // 每次 RunCheck 触发时 push 一次（缓冲足够大避免阻塞）
	runErr    error
	listErr   error
	list      func(context.Context) ([]*ChannelMonitor, error)
	run       func(context.Context, int64) ([]*CheckResult, error)
}

func (s *stubMonitorSvc) ListEnabledMonitors(ctx context.Context) ([]*ChannelMonitor, error) {
	if s.list != nil {
		return s.list(ctx)
	}
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.enabled, nil
}

func (s *stubMonitorSvc) RunCheck(ctx context.Context, id int64) ([]*CheckResult, error) {
	s.runCount.Add(1)
	if s.runCalled != nil {
		select {
		case s.runCalled <- id:
		default:
		}
	}
	if s.run != nil {
		return s.run(ctx, id)
	}
	return nil, s.runErr
}

func newRunnerForTest(svc monitorRunnerSvc) *ChannelMonitorRunner {
	return newChannelMonitorRunner(svc, nil)
}

// 等待 condition 在 timeout 内变 true，否则 t.Fatalf。轮询 5ms 一次。
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("waitFor timed out: %s", msg)
	}
}

func runnerTaskCount(r *ChannelMonitorRunner) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tasks)
}

func runnerTaskPtr(r *ChannelMonitorRunner, id int64) *scheduledMonitor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tasks[id]
}

// TestSchedule_AddsTaskAndFiresOnce 验证 Schedule 后立即触发一次首检测，并把任务记入 tasks 表。
func TestSchedule_AddsTaskAndFiresOnce(t *testing.T) {
	svc := &stubMonitorSvc{runCalled: make(chan int64, 4)}
	r := newRunnerForTest(svc)
	r.Start() // svc.enabled 为空，Start 立即完成

	r.Schedule(&ChannelMonitor{ID: 1, Name: "m1", Enabled: true, IntervalSeconds: 60})

	if got := runnerTaskCount(r); got != 1 {
		t.Fatalf("expected 1 scheduled task, got %d", got)
	}

	select {
	case id := <-svc.runCalled:
		if id != 1 {
			t.Fatalf("expected first fire for id=1, got %d", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected immediate first fire within 2s")
	}

	r.Stop()
}

// TestSchedule_ReplaceCancelsOldTask 验证对同一 id 二次 Schedule 会替换旧 task 实例。
// （旧 goroutine 通过 ctx 取消退出；这里以 task 指针不同 + Stop 不超时作为证据。）
func TestSchedule_ReplaceCancelsOldTask(t *testing.T) {
	svc := &stubMonitorSvc{runCalled: make(chan int64, 8)}
	r := newRunnerForTest(svc)
	r.Start()

	m := &ChannelMonitor{ID: 7, Name: "m7", Enabled: true, IntervalSeconds: 60}
	r.Schedule(m)
	first := runnerTaskPtr(r, 7)
	if first == nil {
		t.Fatal("first schedule did not register task")
	}

	r.Schedule(m)
	second := runnerTaskPtr(r, 7)
	if second == nil {
		t.Fatal("second schedule did not register task")
	}
	if first == second {
		t.Fatal("re-Schedule should create a new scheduledMonitor instance")
	}

	stoppedWithin(t, r, 3*time.Second)
}

// TestUnschedule_RemovesTask 验证 Unschedule 删除 task 并使对应 goroutine 退出。
func TestUnschedule_RemovesTask(t *testing.T) {
	svc := &stubMonitorSvc{runCalled: make(chan int64, 4)}
	r := newRunnerForTest(svc)
	r.Start()

	r.Schedule(&ChannelMonitor{ID: 3, Enabled: true, IntervalSeconds: 60})
	waitFor(t, time.Second, "task registered", func() bool { return runnerTaskCount(r) == 1 })

	r.Unschedule(3)
	if got := runnerTaskCount(r); got != 0 {
		t.Fatalf("expected tasks empty after Unschedule, got %d", got)
	}

	stoppedWithin(t, r, 3*time.Second)
}

// TestSchedule_DisabledRedirectsToUnschedule 验证 Enabled=false 等同于 Unschedule。
func TestSchedule_DisabledRedirectsToUnschedule(t *testing.T) {
	svc := &stubMonitorSvc{runCalled: make(chan int64, 4)}
	r := newRunnerForTest(svc)
	r.Start()

	r.Schedule(&ChannelMonitor{ID: 9, Enabled: true, IntervalSeconds: 60})
	waitFor(t, time.Second, "task registered", func() bool { return runnerTaskCount(r) == 1 })

	r.Schedule(&ChannelMonitor{ID: 9, Enabled: false, IntervalSeconds: 60})
	if got := runnerTaskCount(r); got != 0 {
		t.Fatalf("expected tasks empty after disabled re-Schedule, got %d", got)
	}

	stoppedWithin(t, r, 3*time.Second)
}

func TestSchedule_DecryptFailedRedirectsToUnschedule(t *testing.T) {
	svc := &stubMonitorSvc{runCalled: make(chan int64, 4)}
	r := newRunnerForTest(svc)
	r.Start()

	r.Schedule(&ChannelMonitor{ID: 10, Enabled: true, IntervalSeconds: 60})
	waitFor(t, time.Second, "task registered", func() bool { return runnerTaskCount(r) == 1 })

	r.Schedule(&ChannelMonitor{ID: 10, Enabled: true, IntervalSeconds: 60, APIKeyDecryptFailed: true})
	if got := runnerTaskCount(r); got != 0 {
		t.Fatalf("expected tasks empty after decrypt-failed re-Schedule, got %d", got)
	}

	stoppedWithin(t, r, 3*time.Second)
}

func TestSchedule_RepairedAPIKeyCanBeScheduled(t *testing.T) {
	svc := &stubMonitorSvc{runCalled: make(chan int64, 1)}
	r := newRunnerForTest(svc)
	r.Start()

	r.Schedule(&ChannelMonitor{ID: 11, Enabled: true, IntervalSeconds: 60, APIKeyDecryptFailed: true})
	if got := runnerTaskCount(r); got != 0 {
		t.Fatalf("expected no task for decrypt-failed monitor, got %d", got)
	}

	r.Schedule(&ChannelMonitor{ID: 11, Enabled: true, IntervalSeconds: 60, APIKey: "replacement-key"})
	if got := runnerTaskCount(r); got != 1 {
		t.Fatalf("expected repaired monitor to be scheduled, got %d tasks", got)
	}
	select {
	case id := <-svc.runCalled:
		if id != 11 {
			t.Fatalf("expected repaired monitor id=11 to fire, got %d", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected repaired monitor to fire immediately")
	}

	stoppedWithin(t, r, 3*time.Second)
}

func TestRunOne_DecryptFailureUnschedulesTask(t *testing.T) {
	svc := &stubMonitorSvc{
		runCalled: make(chan int64, 1),
		runErr:    ErrChannelMonitorAPIKeyDecryptFailed,
	}
	r := newRunnerForTest(svc)
	r.Start()

	r.Schedule(&ChannelMonitor{ID: 12, Enabled: true, IntervalSeconds: 60})
	select {
	case id := <-svc.runCalled:
		if id != 12 {
			t.Fatalf("expected failing monitor id=12 to fire, got %d", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected failing monitor to fire immediately")
	}
	waitFor(t, time.Second, "decrypt-failed task unscheduled", func() bool { return runnerTaskCount(r) == 0 })

	stoppedWithin(t, r, 3*time.Second)
}

// TestSchedule_InvalidIntervalSkipped 验证 IntervalSeconds<=0 不会注册任务（防御性检查）。
func TestSchedule_InvalidIntervalSkipped(t *testing.T) {
	svc := &stubMonitorSvc{}
	r := newRunnerForTest(svc)
	r.Start()

	r.Schedule(&ChannelMonitor{ID: 1, Enabled: true, IntervalSeconds: 0})
	if got := runnerTaskCount(r); got != 0 {
		t.Fatalf("expected no task for invalid interval, got %d", got)
	}
	r.Stop()
}

// TestSchedule_BeforeStartIsNoOp 验证 Start 之前调用 Schedule 不会注册任务。
func TestSchedule_BeforeStartIsNoOp(t *testing.T) {
	svc := &stubMonitorSvc{}
	r := newRunnerForTest(svc)
	// 故意不调用 Start

	r.Schedule(&ChannelMonitor{ID: 1, Enabled: true, IntervalSeconds: 60})
	if got := runnerTaskCount(r); got != 0 {
		t.Fatalf("expected no task before Start, got %d", got)
	}
	r.Stop()
}

// TestStart_LoadsAllEnabledMonitors 验证 Start 会为 ListEnabledMonitors 返回的每条记录建立任务。
func TestStart_LoadsAllEnabledMonitors(t *testing.T) {
	svc := &stubMonitorSvc{
		enabled: []*ChannelMonitor{
			{ID: 1, Enabled: true, IntervalSeconds: 60},
			{ID: 2, Enabled: true, IntervalSeconds: 60},
			{ID: 3, Enabled: true, IntervalSeconds: 60},
		},
	}
	r := newRunnerForTest(svc)
	r.Start()
	waitFor(t, 2*time.Second, "all 3 tasks scheduled", func() bool { return runnerTaskCount(r) == 3 })

	stoppedWithin(t, r, 3*time.Second)
}

func TestStart_SkipsDecryptFailedMonitor(t *testing.T) {
	svc := &stubMonitorSvc{
		enabled: []*ChannelMonitor{
			{ID: 4, Enabled: true, IntervalSeconds: 60, APIKeyDecryptFailed: true},
		},
	}
	r := newRunnerForTest(svc)
	r.Start()

	if got := runnerTaskCount(r); got != 0 {
		t.Fatalf("expected no task for decrypt-failed startup monitor, got %d", got)
	}
	if got := svc.runCount.Load(); got != 0 {
		t.Fatalf("expected decrypt-failed startup monitor not to run, got %d calls", got)
	}

	stoppedWithin(t, r, 3*time.Second)
}

// TestStop_DrainsAllGoroutines 验证 Stop 会等待所有调度 goroutine 退出（无游离）。
func TestStop_DrainsAllGoroutines(t *testing.T) {
	svc := &stubMonitorSvc{}
	r := newRunnerForTest(svc)
	r.Start()

	for id := int64(1); id <= 5; id++ {
		r.Schedule(&ChannelMonitor{ID: id, Enabled: true, IntervalSeconds: 60})
	}
	waitFor(t, 2*time.Second, "5 tasks scheduled", func() bool { return runnerTaskCount(r) == 5 })

	stoppedWithin(t, r, 3*time.Second)
}

// Every Stop caller waits for the original canceled check to drain.
func TestStop_WaitsForInFlightCheck(t *testing.T) {
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	svc := &stubMonitorSvc{run: func(ctx context.Context, _ int64) ([]*CheckResult, error) {
		close(entered)
		select {
		case <-ctx.Done():
			close(canceled)
		case <-release:
			return nil, nil
		}
		<-release
		return nil, ctx.Err()
	}}
	r := newRunnerForTest(svc)
	t.Cleanup(func() { once.Do(func() { close(release) }); r.Stop() })
	r.Start()
	r.Schedule(&ChannelMonitor{ID: 1, Enabled: true, IntervalSeconds: 60})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the original worker did not start")
	}
	stopped := make(chan struct{}, 2)
	go func() { r.Stop(); stopped <- struct{}{} }()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop must cancel the active check, not just its timer")
	}
	go func() { r.Stop(); stopped <- struct{}{} }()
	select {
	case <-stopped:
		t.Fatal("a Stop caller returned before the same in-flight check drained")
	case <-time.After(30 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	for range 2 {
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Fatal("every Stop must complete after draining")
		}
	}
	require.Empty(t, r.inFlight)
}

func TestV1RunnerStopCancelsAndDrainsStartupInventory(t *testing.T) {
	entered, canceled, release, started := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	svc := &stubMonitorSvc{list: func(ctx context.Context) ([]*ChannelMonitor, error) {
		close(entered)
		select {
		case <-ctx.Done():
			close(canceled)
		case <-release:
		}
		<-release
		return []*ChannelMonitor{{ID: 1, Enabled: true, IntervalSeconds: 60}}, nil
	}}
	r := newRunnerForTest(svc)
	t.Cleanup(func() { once.Do(func() { close(release) }); <-started; r.Stop() })
	go func() { defer close(started); r.Start() }()
	<-entered
	stopped := make(chan struct{})
	go func() { r.Stop(); close(stopped) }()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop must reach the startup inventory read")
	}
	select {
	case <-stopped:
		t.Fatal("Stop must await startup inventory drain")
	case <-time.After(30 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	<-started
	<-stopped
	require.Zero(t, svc.runCount.Load(), "a late inventory must not schedule anything after Stop")
}

func TestV1RunnerUnscheduleCancelsOnlyOriginalTask(t *testing.T) {
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	svc := &stubMonitorSvc{run: func(ctx context.Context, id int64) ([]*CheckResult, error) {
		if id != 1 {
			return nil, nil
		}
		close(entered)
		select {
		case <-ctx.Done():
			close(canceled)
		case <-release:
		}
		<-release
		return nil, ctx.Err()
	}}
	r := newRunnerForTest(svc)
	t.Cleanup(func() { once.Do(func() { close(release) }); r.Stop() })
	r.Start()
	r.Schedule(&ChannelMonitor{ID: 1, Enabled: true, IntervalSeconds: 60})
	<-entered
	r.Schedule(&ChannelMonitor{ID: 2, Enabled: true, IntervalSeconds: 60})
	r.Unschedule(1)
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("Unschedule must cancel the original in-flight check")
	}
	require.NotNil(t, runnerTaskPtr(r, 2), "other scheduled monitors remain active")
	require.Nil(t, runnerTaskPtr(r, 1))
	once.Do(func() { close(release) })
	stoppedWithin(t, r, 3*time.Second)
	require.Empty(t, r.inFlight)
}

func TestV1RunnerCanceledFireStartsNoWork(t *testing.T) {
	svc := &stubMonitorSvc{}
	r := newRunnerForTest(svc)
	r.Start()
	defer r.Stop()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range 100 {
		r.fire(ctx, &scheduledMonitor{id: 1001, name: "canceled fixture"})
	}
	r.pool.StopAndWait()
	require.Zero(t, svc.runCount.Load(), "a canceled/retired task must not enter the checker even when the pool is empty")
	require.Empty(t, r.inFlight)
}

func TestV1RunnerReplacementSurvivesLateDecryptFailure(t *testing.T) {
	entered, release, failed, replacement := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	svc := &stubMonitorSvc{run: func(ctx context.Context, _ int64) ([]*CheckResult, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			close(failed)
			return nil, ErrChannelMonitorAPIKeyDecryptFailed
		}
		close(replacement)
		return nil, nil
	}}
	r := newRunnerForTest(svc)
	t.Cleanup(func() { once.Do(func() { close(release) }); r.Stop() })
	r.Start()
	r.Schedule(&ChannelMonitor{ID: 1, Enabled: true, IntervalSeconds: 60})
	<-entered
	old := runnerTaskPtr(r, 1)
	r.Schedule(&ChannelMonitor{ID: 1, Enabled: true, IntervalSeconds: 60})
	current := runnerTaskPtr(r, 1)
	require.NotSame(t, old, current)
	once.Do(func() { close(release) })
	<-failed
	select {
	case <-replacement:
	case <-time.After(3 * time.Second):
		t.Fatal("new configuration must execute after old work drains, not wait a full interval or be unscheduled by its stale failure")
	}
	require.Same(t, current, runnerTaskPtr(r, 1))
	require.EqualValues(t, 2, calls.Load())
	require.False(t, errors.Is(r.parentCtx.Err(), context.Canceled))
}

func TestV1RunnerStopSkipsEveryQueuedMonitor(t *testing.T) {
	entered := make(chan struct{}, monitorWorkerConcurrency)
	canceled := make(chan struct{}, monitorWorkerConcurrency)
	release := make(chan struct{})
	var once sync.Once
	svc := &stubMonitorSvc{run: func(ctx context.Context, _ int64) ([]*CheckResult, error) {
		entered <- struct{}{}
		select {
		case <-ctx.Done():
			canceled <- struct{}{}
		case <-release:
		}
		<-release
		return nil, ctx.Err()
	}}
	r := newRunnerForTest(svc)
	t.Cleanup(func() { once.Do(func() { close(release) }); r.Stop() })
	r.Start()
	for id := int64(1); id <= monitorWorkerConcurrency; id++ {
		r.Schedule(&ChannelMonitor{ID: id, Enabled: true, IntervalSeconds: 60})
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("the original worker pool did not admit its first cohort")
		}
	}
	for id := int64(monitorWorkerConcurrency + 1); id <= 100; id++ {
		r.Schedule(&ChannelMonitor{ID: id, Enabled: true, IntervalSeconds: 60})
	}
	require.Eventually(t, func() bool {
		r.inFlightMu.Lock()
		defer r.inFlightMu.Unlock()
		return len(r.inFlight) == 100
	}, 3*time.Second, time.Millisecond)
	done := make(chan struct{})
	go func() { r.Stop(); close(done) }()
	for range monitorWorkerConcurrency {
		select {
		case <-canceled:
		case <-time.After(3 * time.Second):
			t.Fatal("Stop did not reach every admitted check")
		}
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled queued workers did not release their in-flight entries")
	}
	require.EqualValues(t, monitorWorkerConcurrency, svc.runCount.Load(), "the 95 queued monitors must never enter RunCheck")
	require.Empty(t, r.inFlight)
}

func TestV1RunnerUnscheduleQueuedMonitorLeavesOthersRunning(t *testing.T) {
	entered := make(chan struct{}, monitorWorkerConcurrency)
	release, independent := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var retired atomic.Int32
	svc := &stubMonitorSvc{run: func(ctx context.Context, id int64) ([]*CheckResult, error) {
		switch id {
		case 1001:
			retired.Add(1)
		case 1002:
			close(independent)
		default:
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil, ctx.Err()
	}}
	r := newRunnerForTest(svc)
	t.Cleanup(func() { once.Do(func() { close(release) }); r.Stop() })
	r.Start()
	for id := int64(1); id <= monitorWorkerConcurrency; id++ {
		r.Schedule(&ChannelMonitor{ID: id, Enabled: true, IntervalSeconds: 60})
		<-entered
	}
	r.Schedule(&ChannelMonitor{ID: 1001, Enabled: true, IntervalSeconds: 60})
	require.Eventually(t, func() bool {
		r.inFlightMu.Lock()
		defer r.inFlightMu.Unlock()
		_, queued := r.inFlight[1001]
		return queued
	}, 3*time.Second, time.Millisecond)
	r.Unschedule(1001)
	r.Schedule(&ChannelMonitor{ID: 1002, Enabled: true, IntervalSeconds: 60})
	once.Do(func() { close(release) })
	select {
	case <-independent:
	case <-time.After(3 * time.Second):
		t.Fatal("retiring a queued monitor must not stall other monitors")
	}
	r.Stop()
	require.Zero(t, retired.Load())
	require.Empty(t, r.inFlight)
}

func TestV1RunnerPanicReleasesSlotWithoutLoggingPayload(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	defer slog.SetDefault(previous)
	var calls atomic.Int32
	finished := make(chan struct{}, 2)
	svc := &stubMonitorSvc{run: func(context.Context, int64) ([]*CheckResult, error) {
		finished <- struct{}{}
		if calls.Add(1) == 1 {
			panic("PRIVATE_PANIC_PAYLOAD")
		}
		return nil, nil
	}}
	r := newRunnerForTest(svc)
	defer r.Stop()
	r.Start()
	for range 2 {
		r.Schedule(&ChannelMonitor{ID: 1, Enabled: true, IntervalSeconds: 60})
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Fatal("a recovered worker panic must not strand the same monitor slot")
		}
		require.Eventually(t, func() bool {
			r.inFlightMu.Lock()
			defer r.inFlightMu.Unlock()
			return len(r.inFlight) == 0
		}, 3*time.Second, time.Millisecond)
	}
	r.Stop()
	require.Contains(t, output.String(), "runner panic")
	require.NotContains(t, output.String(), "PRIVATE_PANIC_PAYLOAD")
}

func TestV1RunnerConcurrentStartScheduleAndStop(t *testing.T) {
	for range 25 {
		r := newRunnerForTest(&stubMonitorSvc{})
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(r.Start)
			wg.Go(func() { r.Schedule(&ChannelMonitor{ID: 1, Enabled: true, IntervalSeconds: 60}) })
			wg.Go(func() { r.Unschedule(1) })
			wg.Go(r.Stop)
		}
		wg.Wait()
		r.Stop()
		require.Empty(t, r.inFlight)
		require.Empty(t, r.tasks)
	}
}

// TestInFlight_PoolFullReleasesSlot 直接驱动 fire 路径，模拟 pool.TrySubmit 失败时 inFlight 必须释放。
// 用一个小型 stub pool 替换 r.pool 不便（pond.Pool 是接口但 mock 麻烦），
// 改为：占满 inFlight 后直接 fire，验证不会在 inFlight 空槽时永久卡住。
func TestInFlight_AcquireReleaseSymmetric(t *testing.T) {
	svc := &stubMonitorSvc{}
	r := newRunnerForTest(svc)

	if !r.tryAcquireInFlight(42) {
		t.Fatal("first acquire should succeed")
	}
	if r.tryAcquireInFlight(42) {
		t.Fatal("second acquire (no release) must fail")
	}
	r.releaseInFlight(42)
	if !r.tryAcquireInFlight(42) {
		t.Fatal("acquire after release should succeed")
	}
	r.releaseInFlight(42)
}

// stoppedWithin 在 timeout 内并行调用 Stop，超时则 Fatal。验证 Stop 不会阻塞。
func stoppedWithin(t *testing.T, r *ChannelMonitorRunner, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	go func() {
		r.Stop()
		once.Do(func() { close(done) })
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("Stop did not return within %s — leaked goroutine?", timeout)
	}
}
