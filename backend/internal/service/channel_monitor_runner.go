package service

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/alitto/pond/v2"
)

// MonitorScheduler 调度器接口，供 ChannelMonitorService 在 CRUD 时回调，
// 用 setter 注入避免 service ↔ runner 的 wire 依赖环。
type MonitorScheduler interface {
	// Schedule 为指定监控创建（或重置）独立定时任务。
	// 当 m.Enabled=false 时等同于 Unschedule(m.ID)。
	Schedule(m *ChannelMonitor)
	// Unschedule 取消指定监控的定时任务（若存在）。
	Unschedule(id int64)
}

// monitorRunnerSvc 抽出 runner 实际依赖的两个 service 方法：
//   - 启动时加载 enabled monitor
//   - 每次 ticker 触发执行检测
//
// 用接口而非 *ChannelMonitorService 是为了让 runner 单元测试可注入轻量 stub，
// 避免依赖完整的 repo + encryptor 链路。生产实现 *ChannelMonitorService 自然满足。
type monitorRunnerSvc interface {
	ListEnabledMonitors(ctx context.Context) ([]*ChannelMonitor, error)
	RunCheck(ctx context.Context, id int64) ([]*CheckResult, error)
}

// ChannelMonitorRunner 渠道监控调度器。
//
// 设计：
//   - 每个 enabled monitor 对应一个独立 goroutine + ticker（按各自 IntervalSeconds）
//   - Start 时一次性加载所有 enabled monitor 并为每个建立任务
//   - Service 在 Create/Update/Delete 后通过 MonitorScheduler 接口回调，
//     即时重建/取消对应任务（无需轮询 DB）
//   - 实际 HTTP 检测交给 pond 池（容量 monitorWorkerConcurrency），
//     防止突发并发拖垮上游
//
// 历史清理与日聚合维护由 OpsCleanupService 的 cron 触发
// ChannelMonitorService.RunDailyMaintenance（复用 leader lock + heartbeat），
// 不在 runner 职责内。
type ChannelMonitorRunner struct {
	svc            monitorRunnerSvc
	settingService *SettingService

	pool         pond.Pool
	parentCtx    context.Context
	parentCancel context.CancelFunc

	mu       sync.Mutex
	tasks    map[int64]*scheduledMonitor
	wg       sync.WaitGroup
	started  bool
	stopped  bool
	stopDone chan struct{}

	// inFlight 跟踪正在执行的 monitor.ID。fire 调度前会检查避免重复提交，
	// 防止单次检测耗时 > interval 时同一 monitor 被并发执行。
	inFlight   map[int64]chan struct{}
	inFlightMu sync.Mutex
}

// scheduledMonitor 单个监控的运行时上下文。
type scheduledMonitor struct {
	id          int64
	name        string
	interval    time.Duration
	jitter      time.Duration // 每轮 ± [0, jitter] 的均匀随机偏移；0 = 固定间隔
	cancel      context.CancelFunc
	previousRun <-chan struct{}
}

// nextDelay 计算下一次触发的等待时长：interval ± [0, jitter] 的均匀随机偏移。
// 校验链路已保证 interval - jitter >= monitorMinIntervalSeconds，
// 这里仍 clamp 一次下限，兜底数据库中违反约束的脏数据。
func (t *scheduledMonitor) nextDelay() time.Duration {
	if t.jitter <= 0 {
		return t.interval
	}
	offset := time.Duration(rand.Int64N(int64(2*t.jitter) + 1)) // [0, 2*jitter]
	d := t.interval - t.jitter + offset
	if floor := monitorMinIntervalSeconds * time.Second; d < floor {
		d = floor
	}
	return d
}

// NewChannelMonitorRunner 构造调度器。Start 在 wire 中调用一次。
// settingService 用于在每次 fire 前读取功能开关；传 nil 时视为总是启用（兼容测试）。
//
// pool 在构造时即建好：避免 Start 在 mu 内赋值、fire/Stop 在 mu 外读取的竞态隐患，
// 且 pond.NewPool 创建本身近似零开销，提前建池不会浪费资源。
func NewChannelMonitorRunner(svc *ChannelMonitorService, settingService *SettingService) *ChannelMonitorRunner {
	return newChannelMonitorRunner(svc, settingService)
}

// newChannelMonitorRunner 内部构造，接受最小化接口，便于单元测试注入 stub。
func newChannelMonitorRunner(svc monitorRunnerSvc, settingService *SettingService) *ChannelMonitorRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &ChannelMonitorRunner{
		svc:            svc,
		settingService: settingService,
		pool:           pond.NewPool(monitorWorkerConcurrency),
		parentCtx:      ctx,
		parentCancel:   cancel,
		tasks:          make(map[int64]*scheduledMonitor),
		inFlight:       make(map[int64]chan struct{}),
	}
}

// Start 加载所有 enabled monitor 并为每个建立独立定时任务。
// 调用方需保证只调一次（wire ProvideChannelMonitorRunner 内只调一次）。
func (r *ChannelMonitorRunner) Start() {
	if r == nil || r.svc == nil {
		return
	}
	r.mu.Lock()
	if r.started || r.stopped {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.wg.Add(1)
	r.mu.Unlock()
	defer r.wg.Done()

	ctx, cancel := context.WithTimeout(r.parentCtx, monitorStartupLoadTimeout)
	defer cancel()
	enabled, err := r.svc.ListEnabledMonitors(ctx)
	if err != nil {
		slog.Error("channel_monitor: load enabled monitors failed at startup", "error", err)
		return
	}
	for _, m := range enabled {
		r.Schedule(m)
	}
	slog.Info("channel_monitor: runner started", "scheduled_tasks", len(enabled))
}

// Schedule 为指定监控创建（或重置）独立定时任务。
//   - m.Enabled=false 或 APIKeyDecryptFailed=true → 等同于 Unschedule(m.ID)
//   - 已存在的任务会先被取消再重建（适用于 IntervalSeconds 变更场景）
//   - 新任务立即触发首次检测，之后按 IntervalSeconds 周期触发
func (r *ChannelMonitorRunner) Schedule(m *ChannelMonitor) {
	if r == nil || m == nil {
		return
	}
	if !m.Enabled || m.APIKeyDecryptFailed {
		r.Unschedule(m.ID)
		return
	}
	interval := time.Duration(m.IntervalSeconds) * time.Second
	if interval <= 0 {
		// Create/Update 已通过 validateInterval 校验区间，正常路径不可能到这里。
		// 真触发说明数据库中存在违反约束的数据或校验链路有 bug，记 Error 暴露问题。
		slog.Error("channel_monitor: skip schedule for invalid interval",
			"monitor_id", m.ID, "interval_seconds", m.IntervalSeconds)
		return
	}
	jitter := time.Duration(m.JitterSeconds) * time.Second
	if jitter < 0 {
		jitter = 0
	}

	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	if !r.started {
		// Start 之前调用 Schedule 通常意味着 wire 顺序错乱：
		// 当前 wire 顺序是 SetScheduler → Start，CRUD 钩子最早也只能在请求到达时触发，
		// 此时 Start 早已完成。出现此分支时把 monitor 信息打出来便于排查，
		// 不入队、不缓存——交给运维通过重启或修复 wire 解决。
		r.mu.Unlock()
		slog.Warn("channel_monitor: schedule before runner started, skip",
			"monitor_id", m.ID, "name", m.Name)
		return
	}
	if existing, ok := r.tasks[m.ID]; ok {
		existing.cancel()
	}
	ctx, cancel := context.WithCancel(r.parentCtx)
	r.inFlightMu.Lock()
	previousRun := r.inFlight[m.ID]
	r.inFlightMu.Unlock()
	task := &scheduledMonitor{
		id:          m.ID,
		name:        m.Name,
		interval:    interval,
		jitter:      jitter,
		cancel:      cancel,
		previousRun: previousRun,
	}
	r.tasks[m.ID] = task
	r.wg.Add(1)
	r.mu.Unlock()

	go r.runScheduled(ctx, task)
}

// Unschedule 取消指定监控的定时任务（若存在）。
// 已经在执行中的检测会通过 ctx 取消信号传递。
func (r *ChannelMonitorRunner) Unschedule(id int64) {
	r.unschedule(id, nil)
}

// A stale worker can retire only its own scheduled task, never its replacement.
func (r *ChannelMonitorRunner) unschedule(id int64, expected *scheduledMonitor) {
	if r == nil {
		return
	}
	r.mu.Lock()
	task, ok := r.tasks[id]
	if ok && (expected == nil || expected == task) {
		delete(r.tasks, id)
		task.cancel()
	}
	r.mu.Unlock()
}

// Stop 优雅停止：取消所有任务、关闭池。
func (r *ChannelMonitorRunner) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.stopped {
		done := r.stopDone
		r.mu.Unlock()
		<-done
		return
	}
	r.stopped = true
	r.stopDone = make(chan struct{})
	done := r.stopDone
	r.parentCancel()
	r.tasks = nil
	r.mu.Unlock()
	defer close(done)

	r.wg.Wait()
	r.pool.StopAndWait()
}

// runScheduled 单个监控的循环：立即触发首次（满足"新建/启用即跑"），
// 之后按 interval ± jitter 周期触发；ctx 取消即退出。
// 用 timer 而非 ticker：jitter > 0 时每轮等待时长都需要重新随机化。
func (r *ChannelMonitorRunner) runScheduled(ctx context.Context, task *scheduledMonitor) {
	defer r.wg.Done()

	if task.previousRun != nil {
		select {
		case <-ctx.Done():
			return
		case <-task.previousRun:
		}
	}
	r.fire(ctx, task)

	timer := time.NewTimer(task.nextDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			r.fire(ctx, task)
			timer.Reset(task.nextDelay())
		}
	}
}

// fire 提交一次检测到 worker 池。功能开关关闭时跳过本次（不取消任务，
// 重新启用时立即恢复）；池满或重复在飞时也跳过。
func (r *ChannelMonitorRunner) fire(ctx context.Context, task *scheduledMonitor) {
	if ctx.Err() != nil {
		return
	}
	if r.settingService != nil {
		rt := r.settingService.GetChannelMonitorRuntime(ctx)
		if !rt.ActiveProbesAllowed() {
			return
		}
	}
	// Submission and retirement share the lock. A canceled timer, queued old
	// task or late settings read must not submit new work after replacement/Stop.
	r.mu.Lock()
	if r.stopped || ctx.Err() != nil || r.tasks[task.id] != task {
		r.mu.Unlock()
		return
	}
	if !r.tryAcquireInFlight(task.id) {
		r.mu.Unlock()
		slog.Debug("channel_monitor: skip already in-flight",
			"monitor_id", task.id, "name", task.name)
		return
	}
	_, submitted := r.pool.TrySubmit(func() { r.runOne(ctx, task) })
	r.mu.Unlock()
	if !submitted {
		// 池满：丢弃本次检测，但必须释放已占用的 inFlight 槽，否则该 monitor 会被永久卡住。
		r.releaseInFlight(task.id)
		slog.Warn("channel_monitor: worker pool full, skip submission",
			"monitor_id", task.id, "name", task.name)
	}
}

// tryAcquireInFlight 原子地占用 monitor 的 in-flight 槽。
// 已被占用返回 false（调用方应跳过本次提交）。
func (r *ChannelMonitorRunner) tryAcquireInFlight(id int64) bool {
	r.inFlightMu.Lock()
	defer r.inFlightMu.Unlock()
	if _, exists := r.inFlight[id]; exists {
		return false
	}
	r.inFlight[id] = make(chan struct{})
	return true
}

// releaseInFlight 释放 in-flight 槽。runOne 完成（含 panic recover）后必须调用。
func (r *ChannelMonitorRunner) releaseInFlight(id int64) {
	r.inFlightMu.Lock()
	if done, exists := r.inFlight[id]; exists {
		delete(r.inFlight, id)
		close(done)
	}
	r.inFlightMu.Unlock()
}

// runOne 执行单个监控的检测。普通错误只记日志；API key 解密失败会撤销任务。
// 任务结束时（含 panic recover）必须释放 in-flight 槽。
func (r *ChannelMonitorRunner) runOne(parent context.Context, task *scheduledMonitor) {
	ctx, cancel := context.WithTimeout(parent, monitorRequestTimeout+monitorPingTimeout+monitorRunOneBuffer)
	defer cancel()

	defer r.releaseInFlight(task.id)

	defer func() {
		if recover() != nil {
			slog.Error("channel_monitor: runner panic",
				"monitor_id", task.id, "name", task.name)
		}
	}()

	r.mu.Lock()
	retired := r.stopped || r.tasks[task.id] != task || ctx.Err() != nil
	r.mu.Unlock()
	if retired {
		return
	}
	if _, err := r.svc.RunCheck(ctx, task.id); err != nil {
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrChannelMonitorAPIKeyDecryptFailed) {
			r.unschedule(task.id, task)
		}
		slog.Warn("channel_monitor: run check failed",
			"monitor_id", task.id, "name", task.name, "error", err)
	}
}
