//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

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
