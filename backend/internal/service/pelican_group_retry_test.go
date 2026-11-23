package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPelicanGroupMonitoringBudgetDeferralStopsFailover(t *testing.T) {
	for _, mode := range []string{"answer", "scheduled", "typed_denial", "coordinator", "lease_lost", "after_supplier_failure", "after_partial_answer"} {
		t.Run(mode, func(t *testing.T) {
			denial := &MonitoringProbeBudgetDenial{Reason: "token_limit", Scope: "global_day", RetryAt: groupTestNow.Add(time.Hour)}
			router := &groupTestRouterFake{steps: []routeStep{{account: account(11, "first")}, {account: account(12, "second")}, {account: account(13, "unused")}}}
			calls := 0
			repo := newGroupTestRepoFake()
			svc, showcase := newGroupTestService(repo, router, func(_ context.Context, id int64, _ string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
				calls++
				if mode == "after_supplier_failure" && id == 11 {
					return &ScheduledTestResult{monitoringBudgetAdmitted: true, Status: "failed", ErrorMessage: "API returned 503"}, nil
				}
				switch mode {
				case "typed_denial":
					return nil, errors.Join(errors.New("wrapped fixture"), denial)
				case "coordinator":
					return nil, ErrMonitoringProbeBudgetUnavailable
				case "lease_lost":
					return nil, ErrMonitoringProbeBudgetLeaseLost
				case "scheduled":
					return &ScheduledTestResult{Status: "deferred", BudgetDeferral: denial}, nil
				case "after_partial_answer":
					return &ScheduledTestResult{monitoringBudgetAdmitted: true, BudgetDeferral: denial}, nil
				default:
					return &ScheduledTestResult{Status: "failed", ErrorMessage: "answer_budget_deferred", ResponseText: "partial output", QualityJudgment: &QualityJudgment{Verdict: "unknown", BudgetDeferral: denial}}, nil
				}
			})
			svc.execute(t.Context(), groupTestPlan(1), groupTestNow)
			require.Len(t, repo.results, 1)
			result := repo.results[0]
			require.Empty(t, showcase.published, "deferred samples never enter the public showcase")
			require.Equal(t, "deferred", result.Status)
			wantCalls := 1
			if mode == "after_supplier_failure" {
				wantCalls = 2
			}
			require.Equal(t, wantCalls, calls, "local budget deferral must not consume another account")
			require.Len(t, router.released, wantCalls)
			require.Len(t, result.Attempts, wantCalls-1, "keep only real earlier supplier failures")
			require.Empty(t, result.ErrorMessage, "do not publish local budget failure as supplier error")
			require.Equal(t, mode == "after_supplier_failure" || mode == "after_partial_answer", result.monitoringBudgetAdmitted)
			raw, err := json.Marshal(result)
			require.NoError(t, err)
			require.Contains(t, string(raw), `"budget_deferral":`)
			require.NotContains(t, string(raw), "monitoringBudgetAdmitted")
			require.NotContains(t, string(raw), "monitoring_budget_admitted")
		})
	}
}

func TestPelicanGroupMonitoringBudgetPhysicalWS(t *testing.T) {
	for _, mode := range []string{"handshake", "generation", "settlement", "success"} {
		t.Run(mode, func(t *testing.T) {
			server, handshakes, frames := qualityBudgetWSServer(t)
			tests, _, chosen := availabilityWSTestServices(t, server)
			denial := &MonitoringProbeBudgetDenial{Reason: "token_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}
			budget := &monitoringBudgetRepoStub{err: denial}
			switch mode {
			case "handshake":
				budget.denyAt = 1
			case "generation":
				budget.denyAt = 2
			case "settlement":
				budget.releaseErr = errors.New("fixture SQL settlement unavailable")
			}
			tests.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
			router := &groupTestRouterFake{steps: []routeStep{{account: chosen}, {account: account(88, "unused")}}}
			groups, _ := newGroupTestService(newGroupTestRepoFake(), router, tests.RunPelicanBackground)
			plan := groupTestPlan(1)
			plan.ModelID, plan.PelicanConfig.QuestionKind = "target", "candy"
			plan.PelicanConfig.Prompt = CandyPrompt
			result := groups.runSample(t.Context(), plan, &Group{ID: 4})
			require.Len(t, router.excluded, 1, "the real native WS path must not escape to another account after local deferral")
			require.Equal(t, []int64{chosen.ID}, router.released)
			if mode == "success" {
				require.Equal(t, "success", result.Status, result.ErrorMessage)
				require.Nil(t, result.BudgetDeferral)
				require.EqualValues(t, 1, frames.Load())
				require.Equal(t, []MonitoringProbeTokenUsage{{}, {Tokens: 2, Reported: true}}, budget.usages)
			} else {
				require.Equal(t, "deferred", result.Status, result.ErrorMessage)
				require.NotNil(t, result.BudgetDeferral)
				require.Empty(t, result.ErrorMessage)
				require.Zero(t, frames.Load(), "rejected/failed handshake settlement must not start generation")
			}
			if mode == "handshake" {
				require.Zero(t, handshakes.Load())
			} else {
				require.EqualValues(t, 1, handshakes.Load())
			}
			require.NoError(t, t.Context().Err())
		})
	}
}

func TestPelicanGroupMonitoringBudgetTextIsSupplierFailure(t *testing.T) {
	router := &groupTestRouterFake{steps: []routeStep{{account: account(11, "first")}, {account: account(12, "second")}}}
	svc, _ := newGroupTestService(newGroupTestRepoFake(), router, answers(map[int64]ScheduledTestResult{
		11: {Status: "failed", ErrorMessage: "answer_budget_deferred"},
		12: {Status: "success", ResponseText: "<svg>real replacement</svg>"},
	}))
	result := svc.runSample(t.Context(), groupTestPlan(1), &Group{ID: 4})
	require.Equal(t, "success", result.Status)
	require.EqualValues(t, 12, result.AccountID)
	require.Len(t, result.Attempts, 1)
}

func TestPelicanGroupRetriesPartialTransientFailures(t *testing.T) {
	for _, message := range []string{
		"The service is temporarily unavailable. Please retry later.",
		"Service temporarily unavailable",
		"Stream read error: unexpected EOF",
		"Stream ended before response.completed",
		"Generation stream ended before completion",
		"Chat Completions stream from /v1/chat/completions ended before [DONE]",
		"Chat Completions stream read error from /v1/chat/completions: connection reset by peer",
	} {
		t.Run(message, func(t *testing.T) {
			repo := newGroupTestRepoFake()
			router := &groupTestRouterFake{steps: []routeStep{{account: account(11, "a")}, {account: account(12, "b")}}}
			svc, showcase := newGroupTestService(repo, router, answers(map[int64]ScheduledTestResult{
				11: {Status: "failed", ResponseText: "<html>incomplete", ErrorMessage: message},
				12: {Status: "success", ResponseText: "<svg>complete</svg>"},
			}))
			svc.execute(t.Context(), groupTestPlan(1), groupTestNow)
			require.Len(t, repo.results, 1, "one result is persisted for the scheduled sample")
			result := repo.results[0]
			require.Equal(t, "success", result.Status)
			require.EqualValues(t, 12, result.AccountID)
			require.Equal(t, "<svg>complete</svg>", result.ResponseText)
			require.Equal(t, []PelicanGroupTestAttempt{{AccountID: 11, AccountName: "a", Error: message}}, result.Attempts)
			require.Len(t, showcase.published, 1, "the successful replacement reaches the showcase")
			require.ElementsMatch(t, []int64{11, 12}, router.released)
			require.ElementsMatch(t, []int64{11}, router.excluded[1])
		})
	}
}

func TestPelicanGroupPartialRetryExhaustionKeepsFailure(t *testing.T) {
	for _, available := range []int{1, 3, 4} {
		t.Run(strconv.Itoa(available)+" accounts available", func(t *testing.T) {
			repo := newGroupTestRepoFake()
			router := &groupTestRouterFake{}
			for id := 1; id <= available; id++ {
				router.steps = append(router.steps, routeStep{account: account(int64(id), "pool")})
			}
			clock := groupTestNow
			calls := 0
			svc, showcase := newGroupTestService(repo, router, func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
				calls++
				clock = clock.Add(20 * time.Second)
				return &ScheduledTestResult{Status: "failed", ResponseText: "<html>partial", ErrorMessage: "Stream ended before response.completed", LatencyMs: 20000}, nil
			})
			svc.now = func() time.Time { return clock }
			svc.execute(t.Context(), groupTestPlan(1), groupTestNow)
			require.Equal(t, min(available, 3), calls)
			require.Len(t, repo.results, 1, "exhaustion still records a failure for this interval")
			result := repo.results[0]
			require.Equal(t, "failed", result.Status)
			require.EqualValues(t, calls, result.AccountID)
			require.Equal(t, "<html>partial", result.ResponseText)
			require.Equal(t, "Stream ended before response.completed", result.ErrorMessage)
			require.Len(t, result.Attempts, calls-1)
			require.EqualValues(t, calls*20000, result.LatencyMs, "elapsed time includes all attempts")
			require.Len(t, router.released, calls)
			require.Empty(t, showcase.published)
		})
	}
}

func TestPelicanGroupKeepsModelFailuresAfterPartialOutput(t *testing.T) {
	for _, message := range []string{
		pelicanErrEmptyOutput, pelicanErrCaptureLimit, pelicanErrHistoryLimit,
		pelicanErrMaxTokens, pelicanErrRefused + ": policy",
		"Model did not return HTML or SVG", "Unknown error",
	} {
		t.Run(message, func(t *testing.T) {
			router := &groupTestRouterFake{steps: []routeStep{{account: account(11, "a")}, {account: account(12, "b")}}}
			svc, _ := newGroupTestService(newGroupTestRepoFake(), router, answers(map[int64]ScheduledTestResult{
				11: {Status: "failed", ResponseText: "partial answer", ErrorMessage: message},
			}))
			result := svc.runSample(context.Background(), groupTestPlan(1), &Group{ID: 4})
			require.EqualValues(t, 11, result.AccountID)
			require.Equal(t, message, result.ErrorMessage)
			require.Equal(t, "partial answer", result.ResponseText)
			require.Empty(t, result.Attempts)
			require.Len(t, router.excluded, 1)
		})
	}
}

func TestPelicanGroupCancellationStopsPartialRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router := &groupTestRouterFake{steps: []routeStep{{account: account(11, "a")}, {account: account(12, "b")}}}
	svc, _ := newGroupTestService(newGroupTestRepoFake(), router, func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
		cancel()
		return &ScheduledTestResult{Status: "failed", ResponseText: "partial", ErrorMessage: "Stream read error: context canceled"}, nil
	})
	result := svc.runSample(ctx, groupTestPlan(1), &Group{ID: 4})
	require.Equal(t, "failed", result.Status)
	require.Equal(t, "partial", result.ResponseText)
	require.Empty(t, result.Attempts)
	require.Len(t, router.excluded, 1)
	require.Equal(t, []int64{11}, router.released)
}
