//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type scheduledLifecyclePlans struct {
	ScheduledTestPlanRepository
	list   func(context.Context) ([]*ScheduledTestPlan, error)
	claims atomic.Int32
	claim  bool
	finish func(context.Context) error
}

func (r *scheduledLifecyclePlans) ListDue(ctx context.Context, _ time.Time) ([]*ScheduledTestPlan, error) {
	return r.list(ctx)
}

func (r *scheduledLifecyclePlans) ClaimPelican(context.Context, *ScheduledTestPlan, time.Time, time.Time, time.Time) (bool, error) {
	r.claims.Add(1)
	return r.claim, nil
}

func (r *scheduledLifecyclePlans) FinishPelican(ctx context.Context, _ int64, _, _ time.Time) error {
	return r.finish(ctx)
}

type scheduledLifecycleResults struct {
	ScheduledTestResultRepository
	prunes atomic.Int32
	create func(context.Context, *ScheduledTestResult) (*ScheduledTestResult, error)
}

type scheduledMaintenancePlans struct {
	*scheduledLifecyclePlans
	reconcile func(context.Context) error
}

func (r *scheduledMaintenancePlans) ReconcileProcurementSharedMonitoring(ctx context.Context) error {
	return r.reconcile(ctx)
}

func TestScheduledRunnerSharedMaintenanceBeforeInventory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		order := []string{}
		plans := &scheduledMaintenancePlans{scheduledLifecyclePlans: &scheduledLifecyclePlans{list: func(context.Context) ([]*ScheduledTestPlan, error) {
			order = append(order, "inventory")
			return nil, nil
		}}, reconcile: func(context.Context) error { order = append(order, "maintenance"); return nil }}
		runner := NewScheduledTestRunnerService(plans, NewScheduledTestService(plans, &scheduledLifecycleResults{}), &AccountTestService{}, nil, nil)
		done := make(chan struct{})
		go func() { defer close(done); runner.runScheduled(t.Context()) }()
		time.Sleep(11 * time.Second)
		<-done
		require.Equal(t, []string{"maintenance", "inventory"}, order)
	})
}

func TestScheduledRunnerSharedMaintenanceBoundedAndCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var reached context.Context
		plans := &scheduledMaintenancePlans{reconcile: func(ctx context.Context) error {
			reached = ctx
			<-ctx.Done()
			return ctx.Err()
		}}
		runner := &ScheduledTestRunnerService{planRepo: plans}
		done := make(chan struct{})
		go func() { defer close(done); runner.reconcileSharedMonitoring(t.Context()) }()
		time.Sleep(16 * time.Second)
		<-done
		require.ErrorIs(t, reached.Err(), context.DeadlineExceeded)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		runner.reconcileSharedMonitoring(ctx)
		require.ErrorIs(t, reached.Err(), context.Canceled)
	})
}

func (r *scheduledLifecycleResults) Create(ctx context.Context, result *ScheduledTestResult) (*ScheduledTestResult, error) {
	return r.create(ctx, result)
}

func (r *scheduledLifecycleResults) PruneOldResults(context.Context, int64, int) error {
	return nil
}

func (r *scheduledLifecycleResults) PruneExpiredPelican(context.Context, time.Time) error {
	r.prunes.Add(1)
	return nil
}

func TestScheduledRunnerStopDuringDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var lists atomic.Int32
		plans := &scheduledLifecyclePlans{list: func(context.Context) ([]*ScheduledTestPlan, error) {
			lists.Add(1)
			return nil, nil
		}}
		results := &scheduledLifecycleResults{}
		runner := NewScheduledTestRunnerService(plans, NewScheduledTestService(plans, results), &AccountTestService{}, nil, nil)
		runner.Start()
		time.Sleep(time.Minute + time.Second)
		synctest.Wait()
		stopping := time.Now()
		runner.Stop()
		elapsed := time.Since(stopping)
		time.Sleep(20 * time.Second)
		synctest.Wait()
		require.Less(t, elapsed, time.Second, "Stop must cancel the 10s stagger, not return after the old 3s timeout")
		require.Zero(t, lists.Load(), "a canceled tick must not scan or claim later")
		require.Zero(t, results.prunes.Load())
	})
}

func TestScheduledRunnerStopCancelsInventory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var active context.Context
		release := make(chan struct{})
		plans := &scheduledLifecyclePlans{list: func(ctx context.Context) ([]*ScheduledTestPlan, error) {
			active = ctx
			select {
			case <-ctx.Done():
			case <-release:
			}
			return nil, nil
		}}
		runner := NewScheduledTestRunnerService(plans, NewScheduledTestService(plans, &scheduledLifecycleResults{}), &AccountTestService{}, nil, nil)
		runner.Start()
		time.Sleep(time.Minute + 11*time.Second)
		synctest.Wait()
		require.NotNil(t, active)
		runner.Stop()
		// Release a regressed detached scan before asserting cancellation.
		canceled := active.Err()
		close(release)
		synctest.Wait()
		require.ErrorIs(t, canceled, context.Canceled, "Stop must reach an in-flight scan")
	})
}

func TestScheduledRunnerCanceledPlanDoesNotClaim(t *testing.T) {
	plans := &scheduledLifecyclePlans{}
	runner := NewScheduledTestRunnerService(plans, NewScheduledTestService(plans, &scheduledLifecycleResults{}), &AccountTestService{}, nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range 100 {
		runner.runOnePlan(ctx, pelicanPlan())
	}
	require.Zero(t, plans.claims.Load(), "an available worker slot must not randomly win over cancellation")
}

func TestScheduledRunnerStopBeforeStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var lists atomic.Int32
		plans := &scheduledLifecyclePlans{list: func(context.Context) ([]*ScheduledTestPlan, error) {
			lists.Add(1)
			return nil, nil
		}}
		runner := NewScheduledTestRunnerService(plans, NewScheduledTestService(plans, &scheduledLifecycleResults{}), &AccountTestService{}, nil, nil)
		runner.Stop()
		runner.Start()
		time.Sleep(time.Minute + 11*time.Second)
		runner.Stop()
		// A regressed restart must still leave no live cron in the test.
		if runner.cron != nil {
			<-runner.cron.Stop().Done()
		}
		require.Zero(t, lists.Load(), "a stopped service must not restart its cron")
	})
}

func TestScheduledRunnerConcurrentStartStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		plans := &scheduledLifecyclePlans{list: func(context.Context) ([]*ScheduledTestPlan, error) {
			return nil, nil
		}}
		runner := NewScheduledTestRunnerService(plans, NewScheduledTestService(plans, &scheduledLifecycleResults{}), &AccountTestService{}, nil, nil)
		var wg sync.WaitGroup
		for range 32 {
			wg.Add(2)
			go func() { defer wg.Done(); runner.Start() }()
			go func() { defer wg.Done(); runner.Stop() }()
		}
		wg.Wait()
		runner.Start()
		require.True(t, runner.stopped)
		select {
		case <-runner.stopDone:
		default:
			t.Fatal("every Stop must observe completed draining")
		}
	})
}

func TestScheduledRunnerStopDrainsOnlyDispatchedPlans(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var due []*ScheduledTestPlan
		for i := range 100 {
			plan := pelicanPlan()
			plan.ID = int64(i + 1)
			plan.PelicanConfig.ParallelCount = 1
			due = append(due, plan)
		}
		var started, canceled, saved, finished atomic.Int32
		var invalidSaveContext atomic.Bool
		cleanup := make(chan struct{})
		plans := &scheduledLifecyclePlans{claim: true, list: func(context.Context) ([]*ScheduledTestPlan, error) { return due, nil },
			finish: func(ctx context.Context) error {
				if ctx.Err() != nil {
					invalidSaveContext.Store(true)
				}
				finished.Add(1)
				return ctx.Err()
			}}
		results := &scheduledLifecycleResults{create: func(ctx context.Context, result *ScheduledTestResult) (*ScheduledTestResult, error) {
			if ctx.Err() != nil {
				invalidSaveContext.Store(true)
			}
			<-cleanup
			saved.Add(1)
			return result, ctx.Err()
		}}
		runner := NewScheduledTestRunnerService(plans, NewScheduledTestService(plans, results), &AccountTestService{}, nil, nil)
		runner.runPelican = func(ctx context.Context, _ int64, _ string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
			started.Add(1)
			<-ctx.Done()
			canceled.Add(1)
			return nil, ctx.Err()
		}
		runner.Start()
		time.Sleep(time.Minute + 11*time.Second)
		synctest.Wait()
		require.EqualValues(t, scheduledTestDefaultMaxWorkers, started.Load())
		var stopped atomic.Int32
		for range 2 {
			go func() { runner.Stop(); stopped.Add(1) }()
		}
		synctest.Wait()
		require.EqualValues(t, scheduledTestDefaultMaxWorkers, canceled.Load())
		require.Zero(t, stopped.Load(), "all Stop callers must wait for the original result/lease cleanup")
		close(cleanup)
		synctest.Wait()
		require.EqualValues(t, 2, stopped.Load())
		require.EqualValues(t, scheduledTestDefaultMaxWorkers, plans.claims.Load(), "the other 90 plans must stay unclaimed")
		require.EqualValues(t, scheduledTestDefaultMaxWorkers, saved.Load())
		require.EqualValues(t, scheduledTestDefaultMaxWorkers, finished.Load())
		require.False(t, invalidSaveContext.Load())
		require.Empty(t, runner.planSlots)
	})
}

func TestScheduledRunnerStopReachesGroupRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		plan := groupTestPlan(1)
		repo := newGroupTestRepoFake(plan)
		repo.due = []*PelicanGroupTestPlan{plan}
		router := &groupTestRouterFake{steps: []routeStep{{account: account(11, "fixture")}}}
		var started, canceled atomic.Int32
		groups, showcase := newGroupTestService(repo, router, func(ctx context.Context, _ int64, _ string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
			started.Add(1)
			<-ctx.Done()
			canceled.Add(1)
			return nil, ctx.Err()
		})
		plans := &scheduledLifecyclePlans{list: func(context.Context) ([]*ScheduledTestPlan, error) { return nil, nil }}
		runner := NewScheduledTestRunnerService(plans, NewScheduledTestService(plans, &scheduledLifecycleResults{}), &AccountTestService{}, nil, nil)
		runner.groupTests = groups
		runner.Start()
		time.Sleep(time.Minute + 11*time.Second)
		synctest.Wait()
		require.EqualValues(t, 1, started.Load())
		runner.Stop()
		require.EqualValues(t, 1, canceled.Load())
		require.Len(t, repo.finished, 1)
		require.Len(t, router.released, 1)
		require.Empty(t, showcase.published)
	})
}

func TestScheduledRunnerCanceledQualityDoesNotApplyLateVerdict(t *testing.T) {
	for _, tc := range []struct{ stage, verdict string }{
		{"answer", "correct"}, {"answer", "incorrect"}, {"judge", "correct"}, {"judge", "incorrect"},
	} {
		t.Run(tc.stage+"/"+tc.verdict, func(t *testing.T) {
			plans, results := &qualityPlanRepo{}, &pelicanResults{}
			runner := NewScheduledTestRunnerService(plans, NewScheduledTestService(plans, results), &AccountTestService{}, nil, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			runner.runPelican = func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
				if tc.stage == "answer" {
					cancel()
				}
				return &ScheduledTestResult{Status: "success", ResponseText: "21", QualityJudgment: &QualityJudgment{Verdict: tc.verdict}}, nil
			}
			var judged bool
			runner.judgeQuality = func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment {
				judged = true
				cancel()
				return &QualityJudgment{Verdict: tc.verdict}
			}
			plan := pelicanPlan()
			plan.PelicanConfig.ParallelCount = 1
			plan.PelicanConfig.QuestionKind = "candy"
			plan.PelicanConfig.Quality = &QualityPolicy{Action: "disable_scheduling", ExpectedAnswer: "21"}
			require.True(t, runner.runOnePlan(ctx, plan))
			require.Empty(t, plans.outcomes, "cancellation is neither a quality failure nor recovery evidence")
			require.Equal(t, tc.stage == "judge", judged, "stop must not start a judge after the answer exits")
			require.True(t, plans.finished)
			require.Len(t, results.results, 1)
			require.Nil(t, results.results[0].QualityJudgment)
			require.NotEqual(t, "success", results.results[0].Status)
		})
	}
}

func scheduledBudgetReply() *http.Response {
	response := qualityBudgetReply("connected")
	response.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"connected\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":3,\"total_tokens\":7}}}\n\n"))
	return response
}

func scheduledBudgetDeferral(t *testing.T, result *ScheduledTestResult) *MonitoringProbeBudgetDenial {
	t.Helper()
	data, err := json.Marshal(result)
	require.NoError(t, err)
	var wire struct {
		Deferral *MonitoringProbeBudgetDenial `json:"budget_deferral"`
	}
	require.NoError(t, json.Unmarshal(data, &wire))
	return wire.Deferral
}

func TestScheduledConnectivityMonitoringBudgetDenial(t *testing.T) {
	denial := &MonitoringProbeBudgetDenial{Reason: "token_limit", Scope: "global_day", RetryAt: time.Now().UTC().Add(time.Hour)}
	for _, cause := range []error{denial, ErrMonitoringProbeBudgetUnavailable, ErrMonitoringProbeBudgetLeaseLost} {
		t.Run(cause.Error(), func(t *testing.T) {
			repo := &monitoringBudgetRepoStub{denyAt: 1, err: cause}
			tests, upstream := qualityBudgetTestService(repo, scheduledBudgetReply())
			result, err := tests.RunTestBackground(t.Context(), 2, "test-model")
			require.NoError(t, err)
			require.Equal(t, "deferred", result.Status)
			require.Equal(t, "monitoring_budget_deferred", result.ErrorMessage)
			require.NotNil(t, scheduledBudgetDeferral(t, result))
			require.Nil(t, result.QualityJudgment, "connectivity is not a model quality verdict")
			require.Empty(t, upstream.requests, "denied scheduled tests must not send")
			require.Len(t, repo.requests, 1)
			require.False(t, repo.requests[0].Recovery, "auto_recover must not borrow the reserved recovery budget")
		})
	}
}

func TestScheduledConnectivityMonitoringBudgetAccountingAndManualIsolation(t *testing.T) {
	repo := &monitoringBudgetRepoStub{}
	tests, upstream := qualityBudgetTestService(repo, scheduledBudgetReply(), scheduledBudgetReply(), scheduledBudgetReply())
	result, err := tests.RunTestBackground(t.Context(), 2, "test-model")
	require.NoError(t, err)
	require.Equal(t, "success", result.Status)
	require.Equal(t, "connected", result.ResponseText)
	require.Nil(t, scheduledBudgetDeferral(t, result))
	require.Equal(t, []MonitoringProbeTokenUsage{{Tokens: 7, Reported: true}}, repo.usages)
	require.Len(t, repo.requests, 1)
	require.Equal(t, monitoringProbeHash("https://quality.example/tenant"), repo.requests[0].SiteHash)
	require.True(t, repo.requests[0].MeterTokens)
	// Every background invocation has its own local-error observer. A failed
	// prior round must not poison later tests using the same parent context.
	repo.denyAt, repo.err = 2, ErrMonitoringProbeBudgetUnavailable
	result, err = tests.RunTestBackground(t.Context(), 2, "test-model")
	require.NoError(t, err)
	require.Equal(t, "deferred", result.Status)
	repo.denyAt = 0
	result, err = tests.RunTestBackground(t.Context(), 2, "test-model")
	require.NoError(t, err)
	require.Equal(t, "success", result.Status)
	require.Nil(t, scheduledBudgetDeferral(t, result))
	// Ordinary manual connectivity checks retain the original unmetered path.
	repo.denyAt = 1
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(t.Context())
	require.NoError(t, tests.TestAccountConnection(c, 2, "test-model", "", ""))
	require.Len(t, repo.requests, 3)
	require.Len(t, upstream.requests, 3)
}

type scheduledBudgetPlanRepo struct {
	ScheduledTestPlanRepository
	next time.Time
	runs int
}

func (r *scheduledBudgetPlanRepo) UpdateAfterRun(_ context.Context, _ int64, _, next time.Time) error {
	r.next, r.runs = next, r.runs+1
	return nil
}

func TestScheduledConnectivityMonitoringBudgetRunnerNoFalseRecovery(t *testing.T) {
	retryAt := time.Now().UTC().Add(2 * time.Hour)
	budget := &monitoringBudgetRepoStub{denyAt: 1, err: &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day", RetryAt: retryAt}}
	tests, upstream := qualityBudgetTestService(budget, scheduledBudgetReply())
	plans, results := &scheduledBudgetPlanRepo{}, &pelicanResults{}
	account := &rateLimitClearRepoStub{getByIDAccount: &Account{ID: 2, Status: StatusError}}
	runner := NewScheduledTestRunnerService(plans, NewScheduledTestService(plans, results), tests,
		NewRateLimitService(account, nil, &config.Config{}, nil, nil), &config.Config{})
	plan := &ScheduledTestPlan{ID: 11, AccountID: 2, ModelID: "test-model", CronExpression: "* * * * *", MaxResults: 50, AutoRecover: true}
	require.True(t, runner.runOnePlan(t.Context(), plan))
	require.Len(t, results.results, 1)
	require.Equal(t, "deferred", results.results[0].Status)
	require.EqualValues(t, 11, results.results[0].PlanID)
	require.NotNil(t, scheduledBudgetDeferral(t, results.results[0]))
	require.Zero(t, account.getByIDCalls, "local budget deferral must not enter recovery")
	require.Empty(t, upstream.requests)
	require.Equal(t, 1, plans.runs)
	require.False(t, plans.next.Before(retryAt), "persist a schedule no earlier than the budget retry time")
	// An actual completed upstream success still uses the existing recovery.
	budget.denyAt = 0
	require.True(t, runner.runOnePlan(t.Context(), plan))
	require.Equal(t, "success", results.results[1].Status)
	require.Equal(t, 1, account.clearErrorCalls)
	require.True(t, plans.next.Before(retryAt))
}

func TestScheduledConnectivityMonitoringBudgetSettlementFailure(t *testing.T) {
	repo := &monitoringBudgetRepoStub{releaseErr: ErrMonitoringProbeBudgetUnavailable}
	tests, upstream := qualityBudgetTestService(repo, scheduledBudgetReply())
	result, err := tests.RunTestBackground(t.Context(), 2, "test-model")
	require.NoError(t, err)
	require.Equal(t, "deferred", result.Status, "upstream completion is not local settlement success")
	require.Equal(t, "budget_unavailable", scheduledBudgetDeferral(t, result).Reason)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, []MonitoringProbeTokenUsage{{Tokens: 7, Reported: true}}, repo.usages)
}

func TestScheduledConnectivityMonitoringBudgetLeaseLoss(t *testing.T) {
	for _, phase := range []string{"read", "close"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repo := &monitoringBudgetRepoStub{renew: func(context.Context) (bool, error) { return false, nil }}
				response := scheduledBudgetReply()
				lost := func() { time.Sleep(41 * time.Second); synctest.Wait() }
				body := qualityBudgetCloseBody{ReadCloser: response.Body, close: func() {}}
				if phase == "read" {
					body.read = lost
				} else {
					body.close = lost
				}
				response.Body = body
				tests, upstream := qualityBudgetTestService(repo, response)
				result, err := tests.RunTestBackground(t.Context(), 2, "test-model")
				require.NoError(t, err)
				require.Equal(t, "deferred", result.Status)
				require.Equal(t, "budget_unavailable", scheduledBudgetDeferral(t, result).Reason)
				require.Len(t, upstream.requests, 1, "a lost lease must not replay the completed generation")
				require.Equal(t, 1, repo.released)
				require.NoError(t, t.Context().Err(), "the local probe must not cancel its parent")
			})
		})
	}
}

func TestScheduledConnectivityMonitoringBudgetProviderErrorIsNotDeferral(t *testing.T) {
	repo := &monitoringBudgetRepoStub{}
	response := newJSONResponse(http.StatusBadGateway, `{"error":{"message":"monitoring_budget_deferred token_limit global_day"}}`)
	tests, upstream := qualityBudgetTestService(repo, response)
	result, err := tests.RunTestBackground(t.Context(), 2, "test-model")
	require.NoError(t, err)
	require.Equal(t, "failed", result.Status)
	require.Contains(t, result.ErrorMessage, "502")
	require.Nil(t, result.BudgetDeferral, "provider text is not a local typed budget failure")
	require.Len(t, upstream.requests, 1)
	require.Equal(t, 1, repo.released)
}
