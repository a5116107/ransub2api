package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/robfig/cron/v3"
)

const scheduledTestDefaultMaxWorkers = 10

// ScheduledTestRunnerService periodically scans due test plans and executes them.
type ScheduledTestRunnerService struct {
	planRepo       ScheduledTestPlanRepository
	scheduledSvc   *ScheduledTestService
	accountTestSvc *AccountTestService
	rateLimitSvc   *RateLimitService
	cfg            *config.Config
	judgeQuality   func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment
	runPelican     func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error)
	// groupTests runs the Pelican group tests on the same tick; nil disables them.
	groupTests   *PelicanGroupTestService
	candyMonitor *ChannelMonitorV2CandyService

	qualityTrigger *quality5xxTrigger
	triggerCancel  context.CancelFunc
	triggerWG      sync.WaitGroup
	planSlots      chan struct{}
	cron           *cron.Cron
	lifecycleMu    sync.Mutex
	started        bool
	stopped        bool
	cancel         context.CancelFunc
	stopDone       chan struct{}
	tickMu         sync.Mutex
}

// NewScheduledTestRunnerService creates a new runner.
func NewScheduledTestRunnerService(
	planRepo ScheduledTestPlanRepository,
	scheduledSvc *ScheduledTestService,
	accountTestSvc *AccountTestService,
	rateLimitSvc *RateLimitService,
	cfg *config.Config,
) *ScheduledTestRunnerService {
	return &ScheduledTestRunnerService{
		planRepo:       planRepo,
		planSlots:      make(chan struct{}, scheduledTestDefaultMaxWorkers),
		scheduledSvc:   scheduledSvc,
		accountTestSvc: accountTestSvc,
		rateLimitSvc:   rateLimitSvc,
		cfg:            cfg,
		runPelican:     accountTestSvc.RunPelicanBackground,
	}
}

// Start begins the cron ticker (every minute).
func (s *ScheduledTestRunnerService) Start() {
	if s == nil {
		return
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.started || s.stopped {
		return
	}
	loc := time.Local
	if s.cfg != nil {
		if parsed, err := time.LoadLocation(s.cfg.Timezone); err == nil && parsed != nil {
			loc = parsed
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := cron.New(cron.WithParser(scheduledTestCronParser), cron.WithLocation(loc))
	_, err := c.AddFunc("* * * * *", func() { s.runScheduled(ctx) })
	if err != nil {
		cancel()
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] not started (invalid schedule): %v", err)
		return
	}
	s.cancel, s.cron, s.started = cancel, c, true
	s.startQualityTriggers()
	c.Start()
	logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] started (tick=every minute)")
}

// Stop gracefully shuts down the cron scheduler.
func (s *ScheduledTestRunnerService) Stop() {
	if s == nil {
		return
	}
	s.lifecycleMu.Lock()
	if s.stopped {
		done := s.stopDone
		s.lifecycleMu.Unlock()
		<-done
		return
	}
	s.stopped = true
	s.stopDone = make(chan struct{})
	done := s.stopDone
	if s.cancel != nil {
		s.cancel()
	}
	if s.triggerCancel != nil {
		s.triggerCancel()
	}
	var cronDone <-chan struct{}
	if s.cron != nil {
		cronDone = s.cron.Stop().Done()
	}
	s.lifecycleMu.Unlock()
	defer close(done)
	s.triggerWG.Wait()
	if cronDone != nil {
		<-cronDone
	}
	if s.qualityTrigger != nil && s.qualityTrigger.queue != nil {
		_ = s.qualityTrigger.queue.Close()
	}
}

func (s *ScheduledTestRunnerService) runScheduled(parent context.Context) {
	if s == nil || parent.Err() != nil || !s.tickMu.TryLock() {
		return
	}
	defer s.tickMu.Unlock()
	// Delay 10s so execution lands at ~:10 of each minute instead of :00.
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-parent.Done():
		return
	case <-timer.C:
	}
	if parent.Err() != nil {
		return
	}

	ctx, cancel := context.WithTimeout(parent, 12*time.Minute)
	defer cancel()
	ctx = withMonitoringProbeDispatchStop(ctx)

	s.reconcileSharedMonitoring(ctx)
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	s.finalizeQualityBatches(ctx)
	if ctx.Err() != nil {
		return
	}
	if err := s.scheduledSvc.resultRepo.PruneExpiredPelican(ctx, now.Add(-7*24*time.Hour)); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "pelican history cleanup failed: %v", err)
	}
	s.groupTests.Cleanup(ctx, now)
	if ctx.Err() != nil {
		return
	}
	// Group tests run beside the account plans rather than after them.
	var groupRuns sync.WaitGroup
	groupRuns.Add(2)
	go func(ctx context.Context) { defer groupRuns.Done(); s.candyMonitor.RunDue(ctx, now) }(ctx)
	go func(ctx context.Context) {
		defer groupRuns.Done()
		s.groupTests.RunDue(ctx, now)
	}(ctx)
	defer groupRuns.Wait()
	// Group rules cover accounts that started matching since the last tick.
	if created, err := s.scheduledSvc.SyncQualityTemplates(ctx); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] quality template sync error: %v", err)
	} else if created > 0 {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] quality templates added %d rules", created)
	}
	if ctx.Err() != nil {
		return
	}
	plans, err := s.planRepo.ListDue(ctx, now)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] ListDue error: %v", err)
		return
	}
	if ctx.Err() != nil || len(plans) == 0 {
		return
	}

	logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] found %d due plans", len(plans))
	for _, plan := range plans {
		if plan.PelicanConfig != nil && plan.PelicanConfig.QuestionKind == QualityRandomQuestionKind {
			batch, batchErr := newQualitySharedBatch(now)
			if batchErr == nil {
				ctx = context.WithValue(ctx, qualitySharedBatchContextKey{}, batch)
			}
			break
		}
	}

	sem := make(chan struct{}, scheduledTestDefaultMaxWorkers)
	var wg sync.WaitGroup
	defer wg.Wait()

	for _, plan := range plans {
		if ctx.Err() != nil {
			return
		}
		if monitoringProbeDispatchStopped(ctx) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		if ctx.Err() != nil || monitoringProbeDispatchStopped(ctx) {
			<-sem
			break
		}
		wg.Add(1)
		go func(p *ScheduledTestPlan) {
			defer wg.Done()
			defer func() { <-sem }()
			s.runOnePlan(ctx, p)
		}(plan)
	}

	wg.Wait()
	if ctx.Err() == nil {
		s.finalizeQualityBatches(ctx)
	}
}

func (s *ScheduledTestRunnerService) runOnePlan(ctx context.Context, plan *ScheduledTestPlan) (completed bool) {
	if ctx.Err() != nil || monitoringProbeDispatchStopped(ctx) {
		return false
	}
	if s.planSlots != nil {
		select {
		case s.planSlots <- struct{}{}:
			defer func() { <-s.planSlots }()
		case <-ctx.Done():
			return false
		}
	}
	// A free slot and cancellation can both be ready in the select above.
	if ctx.Err() != nil || monitoringProbeDispatchStopped(ctx) {
		return false
	}
	ctx = context.WithValue(withAutomaticMonitoringProbe(ctx), qualityProbeContextKey{}, true)
	// Background plans do not run under the HTTP recovery middleware.
	defer func() {
		if recover() != nil {
			logger.LegacyPrintf("service.scheduled_test_runner", "scheduled plan=%d account=%d panicked", plan.ID, plan.AccountID)
		}
	}()
	if plan.PelicanConfig != nil {
		completed = s.runPelicanPlan(ctx, plan)
		if completed && plan.TriggerSource == "" {
			s.runAvailabilityCoverage(ctx, plan)
		}
		return completed
	}
	now := time.Now()
	nextRun, err := computeNextRun(plan.CronExpression, now)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d computeNextRun error: %v", plan.ID, err)
		return false
	}
	until := now.Add(15 * time.Minute).Truncate(time.Microsecond)
	if ctx.Err() != nil || monitoringProbeDispatchStopped(ctx) {
		return false
	}
	claimed, err := s.planRepo.ClaimPelican(ctx, plan, now, until, nextRun)
	if err != nil || !claimed {
		if err != nil {
			logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d claim error: %v", plan.ID, err)
		}
		return false
	}
	// Connectivity shares the original persisted claim but has no quality marker.
	// Freeze the claimed nominal next time: edits and successors must win over
	// this worker's delayed finish, including after cancellation or a panic.
	claimedPlan, claimedNext := *plan, nextRun
	claimedPlan.NextRunAt = &claimedNext
	var retryAt time.Time
	started := false
	defer func() {
		finished := time.Now()
		next, err := computeNextRun(plan.CronExpression, finished)
		if err != nil {
			next = claimedNext
		}
		if retryAt.After(next) {
			next = retryAt
		}
		var finishedAt *time.Time
		if started {
			finishedAt = &finished
		} else if plan.NextRunAt != nil {
			// A local stop may race the claim. No adapter ran, so return the
			// nominal due round instead of consuming it without a result.
			next = *plan.NextRunAt
		}
		finishCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.planRepo.FinishConnectivity(finishCtx, &claimedPlan, until, finishedAt, next); err != nil {
			logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d finish error: %v", plan.ID, err)
		}
	}()
	if ctx.Err() != nil || monitoringProbeDispatchStopped(ctx) {
		return false
	}
	started = true
	result, err := s.accountTestSvc.RunTestBackground(ctx, plan.AccountID, plan.ModelID)
	if err != nil {
		stopMonitoringProbeDispatch(ctx, monitoringProbeDeferral(err, time.Now()))
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d RunTestBackground error: %v", plan.ID, err)
		return false
	}
	stopMonitoringProbeDispatch(ctx, monitoringProbeResultDeferral(result))
	if result.BudgetDeferral != nil {
		retryAt = result.BudgetDeferral.RetryAt
	}

	if ctx.Err() != nil && result.Status == "success" {
		result.Status, result.ErrorMessage = "failed", ctx.Err().Error()
	}
	saveCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	if err := s.scheduledSvc.SaveResult(saveCtx, plan.ID, plan.MaxResults, result); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d SaveResult error: %v", plan.ID, err)
	}

	// Auto-recover account if test succeeded and auto_recover is enabled.
	if ctx.Err() == nil && result.Status == "success" && result.BudgetDeferral == nil && plan.AutoRecover {
		s.tryRecoverAccount(ctx, plan.AccountID, plan.ID)
	}

	return true
}

// Additional coverage stays inside the same worker slot and uses a fresh
// database claim per connection. Configuration edits, competing claims,
// cancellation and unattributed failures stop the remaining batch.
func (s *ScheduledTestRunnerService) runAvailabilityCoverage(ctx context.Context, plan *ScheduledTestPlan) {
	if plan.AvailabilityProbeConnectionID == "" || plan.AvailabilityCoverageStop || plan.NextRunAt == nil {
		return
	}
	rounds := availabilityCoverageRounds(plan, plan.AvailabilityEligibleConnections, time.Now())
	seen := []string{plan.AvailabilityProbeConnectionID}
	expectedFault := plan.AvailabilityFaultVersion
	for i := 1; i < rounds && ctx.Err() == nil && !monitoringProbeDispatchStopped(ctx); i++ {
		latest, err := s.planRepo.GetByID(ctx, plan.ID)
		if err != nil || latest == nil || !latest.Enabled || !latest.UpdatedAt.Equal(plan.UpdatedAt) || latest.PelicanConfig == nil || latest.PelicanConfig.Availability == nil || latest.NextRunAt == nil || !latest.NextRunAt.Equal(*plan.NextRunAt) || !latest.NextRunAt.After(time.Now()) {
			return
		}
		latest.TriggerSource = AvailabilityCoverageSource
		latest.AvailabilityExcludedConnections = append([]string(nil), seen...)
		latest.AvailabilityExpectedFault = expectedFault
		if !s.runPelicanPlan(ctx, latest) || latest.AvailabilityCoverageStop || latest.AvailabilityProbeConnectionID == "" {
			return
		}
		seen = append(seen, latest.AvailabilityProbeConnectionID)
	}
}

// tryRecoverAccount attempts to recover an account from recoverable runtime state.
func (s *ScheduledTestRunnerService) tryRecoverAccount(ctx context.Context, accountID int64, planID int64) {
	if s.rateLimitSvc == nil {
		return
	}

	recovery, err := s.rateLimitSvc.RecoverAccountAfterSuccessfulTest(ctx, accountID)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover failed: %v", planID, err)
		return
	}
	if recovery == nil {
		return
	}

	if recovery.ClearedError {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover: account=%d recovered from error status", planID, accountID)
	}
	if recovery.ClearedRateLimit {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover: account=%d cleared rate-limit/runtime state", planID, accountID)
	}
}

// Maintenance precedes native plan discovery and shares this runner's lifecycle.
// A failed maintenance pass leaves original plans and ordinary detection intact.
func (s *ScheduledTestRunnerService) reconcileSharedMonitoring(parent context.Context) {
	if maintainer, ok := s.planRepo.(ProcurementSharedMonitoringMaintainer); ok {
		ctx, cancel := context.WithTimeout(parent, 15*time.Second)
		defer cancel()
		if err := maintainer.ReconcileProcurementSharedMonitoring(ctx); err != nil && parent.Err() == nil {
			logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] shared observation maintenance incomplete; saved plans preserved")
		}
	}
}
