package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func groupBudgetDispatchFixture(count int, run monitorV3AccountRun) (*PelicanGroupTestService, *groupTestRepoFake, *groupTestRouterFake, *showcaseRepoStub) {
	repo := newGroupTestRepoFake()
	router := &groupTestRouterFake{}
	for i := range count {
		plan := groupTestPlan(1)
		plan.ID, plan.ModelID = int64(i+1), fmt.Sprintf("probe-%03d", i)
		repo.plans[plan.ID] = plan
		repo.due = append(repo.due, plan)
		for range PelicanGroupTestMaxAttempts {
			router.steps = append(router.steps, routeStep{account: account(int64(i+1), "chosen")})
		}
	}
	svc, showcase := newGroupTestService(repo, router, run)
	return svc, repo, router, showcase
}

func TestPelicanGroupScheduledGlobalBudgetStopsClaims(t *testing.T) {
	var calls atomic.Int32
	denial := &MonitoringProbeBudgetDenial{Reason: "token_limit", Scope: "global_day", RetryAt: groupTestNow.Add(time.Hour)}
	svc, repo, router, showcase := groupBudgetDispatchFixture(100, func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
		calls.Add(1)
		return &ScheduledTestResult{BudgetDeferral: denial}, nil
	})
	svc.RunDue(t.Context(), groupTestNow)
	require.LessOrEqual(t, len(repo.claims), pelicanGroupTestMaxRunning, "a global denial must not advance every unstarted plan's schedule")
	require.EqualValues(t, len(repo.claims), calls.Load())
	require.Len(t, repo.finished, len(repo.claims), "all dispatched leases still finish")
	require.Len(t, router.released, len(repo.claims))
	require.Empty(t, showcase.published)
	for _, result := range repo.results {
		require.Equal(t, "deferred", result.Status)
		require.Empty(t, result.ErrorMessage)
	}
}

func TestPelicanGroupScheduledBudgetScopesAndRecovery(t *testing.T) {
	for _, scope := range []string{"global_minute", "global_day", "global", "system", "unknown", "", "site", "site_minute", "key", "key_minute", "plugin"} {
		t.Run(scope, func(t *testing.T) {
			var paused atomic.Bool
			paused.Store(true)
			svc, repo, router, showcase := groupBudgetDispatchFixture(24, func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
				if paused.Load() {
					return &ScheduledTestResult{BudgetDeferral: &MonitoringProbeBudgetDenial{Reason: "fixture_limit", Scope: scope}}, nil
				}
				return &ScheduledTestResult{Status: "success", ResponseText: "<svg></svg>"}, nil
			})
			svc.RunDue(t.Context(), groupTestNow)
			global := !strings.HasPrefix(scope, "key") && !strings.HasPrefix(scope, "site") && scope != "plugin"
			if global {
				require.LessOrEqual(t, len(repo.claims), pelicanGroupTestMaxRunning)
			} else {
				require.Len(t, repo.claims, 24, "a local deferral must not stop independent plans")
			}
			first := len(repo.claims)
			require.Len(t, router.released, first)
			require.Len(t, repo.finished, first)
			require.Empty(t, showcase.published)
			paused.Store(false)
			svc.RunDue(t.Context(), groupTestNow.Add(30*time.Minute))
			require.Len(t, repo.claims, first+24, "a new tick must not inherit the previous batch's pause")
			require.Len(t, repo.finished, first+24)
			require.Len(t, router.released, first+24)
		})
	}
}

type groupBudgetDispatchRepo struct {
	*groupTestRepoFake
	reads atomic.Int32
	save  func(context.Context, *PelicanGroupTestResult)
	list  func(context.Context, time.Time) ([]*PelicanGroupTestPlan, error)
}

func (r *groupBudgetDispatchRepo) ListDue(ctx context.Context, now time.Time) ([]*PelicanGroupTestPlan, error) {
	r.reads.Add(1)
	if r.list != nil {
		return r.list(ctx, now)
	}
	return r.groupTestRepoFake.ListDue(ctx, now)
}

func (r *groupBudgetDispatchRepo) CreateResult(ctx context.Context, result *PelicanGroupTestResult) (*PelicanGroupTestResult, error) {
	if r.save != nil {
		r.save(ctx, result)
	}
	return r.groupTestRepoFake.CreateResult(ctx, result)
}

func TestPelicanGroupScheduledPauseBeforeSiblingAndSaveDrain(t *testing.T) {
	for _, hold := range []string{"sample", "save"} {
		t.Run(hold, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				deny, releaseOthers, releaseHeld := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var firstSamples atomic.Int32
				var started atomic.Int32
				svc, repo, router, _ := groupBudgetDispatchFixture(100, func(ctx context.Context, _ int64, model string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
					started.Add(1)
					if model == "probe-000" {
						if firstSamples.Add(1) == 1 {
							<-deny
							return &ScheduledTestResult{BudgetDeferral: &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day"}}, nil
						}
						<-releaseHeld
					} else {
						<-releaseOthers
					}
					require.NoError(t, ctx.Err(), "a budget pause must not cancel admitted siblings or independent manual work")
					return &ScheduledTestResult{Status: "success", ResponseText: "<svg></svg>"}, nil
				})
				if hold == "sample" {
					repo.due[0].PelicanConfig.ParallelCount = 2
				}
				store := &groupBudgetDispatchRepo{groupTestRepoFake: repo}
				if hold == "save" {
					store.save = func(ctx context.Context, result *PelicanGroupTestResult) {
						if result.BudgetDeferral != nil {
							<-releaseHeld
							require.NoError(t, ctx.Err(), "the independent result-save lifetime remains valid")
						}
					}
				}
				svc.repo = store
				done := make(chan struct{})
				go func() { defer close(done); svc.RunDue(t.Context(), groupTestNow) }()
				synctest.Wait()
				initial := pelicanGroupTestMaxRunning
				if hold == "sample" {
					initial++
				}
				require.EqualValues(t, initial, started.Load())
				close(deny)
				synctest.Wait()
				close(releaseOthers)
				synctest.Wait()
				require.Len(t, repo.claims, pelicanGroupTestMaxRunning, "a slow sibling or result save must not delay stop-dispatch")
				require.Len(t, repo.results, pelicanGroupTestMaxRunning-1)
				svc.RunDue(t.Context(), groupTestNow.Add(time.Minute))
				require.EqualValues(t, 1, store.reads.Load(), "an overlapping tick must not start another due inventory")
				manual := groupTestPlan(1)
				manual.ID, manual.ModelID = 1001, "manual"
				repo.plans[manual.ID] = manual
				require.NoError(t, svc.RunNow(t.Context(), manual.ID))
				svc.runs.Wait()
				require.Len(t, repo.results, pelicanGroupTestMaxRunning, "manual diagnostics are independent of a paused scheduled batch")
				close(releaseHeld)
				<-done
				require.Len(t, repo.claims, pelicanGroupTestMaxRunning+1)
				require.Len(t, repo.finished, pelicanGroupTestMaxRunning+1)
				require.Len(t, router.released, initial+1)
			})
		})
	}
}

func TestPelicanGroupScheduledInventoryFailureAndCancellationRecover(t *testing.T) {
	svc, repo, _, _ := groupBudgetDispatchFixture(1, func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
		return &ScheduledTestResult{Status: "success", ResponseText: "<svg></svg>"}, nil
	})
	store := &groupBudgetDispatchRepo{groupTestRepoFake: repo, list: func(context.Context, time.Time) ([]*PelicanGroupTestPlan, error) {
		return nil, errors.New("fixture inventory unavailable")
	}}
	svc.repo = store
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	svc.RunDue(ctx, groupTestNow)
	require.Zero(t, store.reads.Load())
	svc.RunDue(t.Context(), groupTestNow)
	require.EqualValues(t, 1, store.reads.Load())
	require.Empty(t, repo.claims)
	store.list = nil
	svc.RunDue(t.Context(), groupTestNow)
	require.Len(t, repo.claims, 1, "a failed inventory read must not retain the batch mutex")
}
