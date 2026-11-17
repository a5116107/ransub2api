//go:build integration

package repository_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestScheduledConnectivityMonitoringBudgetPostgresRestartAndHistory(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/tenant/v1/responses" || r.Header.Get("Authorization") != "Bearer scheduled-budget-fixture" {
			http.Error(w, "invalid scheduled fixture", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"connected\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":3,\"total_tokens\":7}}}\n\n"))
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	accounts := repository.NewAccountRepository(client, db, nil)
	create := func(name string) *service.Account {
		a := &service.Account{Name: name, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 2,
			Credentials: map[string]any{"api_key": "scheduled-budget-fixture", "base_url": server.URL + "/tenant/v1"}}
		require.NoError(t, accounts.Create(ctx, a))
		return a
	}
	first, alias := create("scheduled-budget"), create("scheduled-budget-same-key")
	snapshot := func(id int64) string {
		var row string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&row))
		return row
	}
	before := map[int64]string{first.ID: snapshot(first.ID), alias.ID: snapshot(alias.ID)}
	budget := repository.NewMonitoringProbeBudgetRepository(db)
	policy := service.DefaultMonitoringProbeBudgetSettings()
	policy.Enabled, policy.RecoveryReservePercent = true, 0
	policy.TokensPerDay = 5
	policy, err := budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	newTester := func() *service.AccountTestService {
		tests := service.NewAccountTestService(accounts, nil, nil, nil, nil, repository.NewHTTPUpstream(cfg), cfg, nil)
		tests.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(repository.NewMonitoringProbeBudgetRepository(db)))
		return tests
	}
	firstResult, err := newTester().RunTestBackground(ctx, first.ID, "fixture-model")
	require.NoError(t, err)
	require.Equal(t, "success", firstResult.Status)
	require.Nil(t, firstResult.BudgetDeferral)
	secondResult, err := newTester().RunTestBackground(ctx, alias.ID, "fixture-model")
	require.NoError(t, err)
	require.Equal(t, "deferred", secondResult.Status)
	require.Equal(t, "token_limit", secondResult.BudgetDeferral.Reason)
	require.Equal(t, "global_day", secondResult.BudgetDeferral.Scope)
	require.EqualValues(t, 1, calls.Load(), "same-key aliases and service reconstruction cannot bypass shared tokens")
	usage, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 7, usage.DayTokens, "in-flight usage may exceed five; never truncate the actual seven")
	require.EqualValues(t, 1, usage.DayUsed)
	require.EqualValues(t, 1, usage.UsageReported)
	require.Zero(t, usage.UsageUnknown)
	require.Zero(t, usage.ActiveRequests)
	require.Zero(t, usage.UsagePending)

	plans, results := repository.NewScheduledTestPlanRepository(db), repository.NewScheduledTestResultRepository(db)
	scheduled := service.NewScheduledTestService(plans, results)
	plan, err := scheduled.CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: alias.ID, ModelID: "fixture-model", Enabled: true, AutoRecover: true, CronExpression: "* * * * *", MaxResults: 20})
	require.NoError(t, err)
	require.NoError(t, scheduled.SaveResult(ctx, plan.ID, plan.MaxResults, firstResult))
	require.NoError(t, scheduled.SaveResult(ctx, plan.ID, plan.MaxResults, secondResult))
	// Recreate both repositories, then read through the real admin handlers.
	reloaded := service.NewScheduledTestService(repository.NewScheduledTestPlanRepository(db), repository.NewScheduledTestResultRepository(db))
	h := admin.NewScheduledTestHandler(reloaded)
	router := gin.New()
	router.GET("/plans/:id/results", h.ListResults)
	router.GET("/plans/:id/results/:resultID", h.GetResult)
	listed := httptest.NewRecorder()
	router.ServeHTTP(listed, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/plans/%d/results?include_content=false", plan.ID), nil))
	require.Equal(t, http.StatusOK, listed.Code, listed.Body.String())
	require.NotContains(t, listed.Body.String(), "scheduled-budget-fixture")
	var history []*service.ScheduledTestResult
	require.NoError(t, json.Unmarshal(listed.Body.Bytes(), &history))
	require.Len(t, history, 2)
	require.Equal(t, "deferred", history[0].Status)
	require.Equal(t, "monitoring_budget_deferred", history[0].ErrorMessage)
	require.Equal(t, secondResult.BudgetDeferral.Reason, history[0].BudgetDeferral.Reason)
	require.WithinDuration(t, secondResult.BudgetDeferral.RetryAt, history[0].BudgetDeferral.RetryAt, time.Microsecond)
	require.Nil(t, history[0].QualityJudgment)
	require.Nil(t, history[0].PelicanConfig)
	require.Empty(t, history[1].ResponseText, "summary reads retain the original lazy-body contract")
	require.Nil(t, history[1].BudgetDeferral, "older/nil metadata remains absent, not fabricated")
	detail := httptest.NewRecorder()
	router.ServeHTTP(detail, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/plans/%d/results/%d", plan.ID, history[0].ID), nil))
	require.Equal(t, http.StatusOK, detail.Code, detail.Body.String())
	var stored service.ScheduledTestResult
	require.NoError(t, json.Unmarshal(detail.Body.Bytes(), &stored))
	require.Equal(t, history[0].BudgetDeferral, stored.BudgetDeferral)
	require.EqualValues(t, 1, calls.Load(), "reading history must not run another probe")
	// An ordinary manual check remains outside this background-only budget.
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(ctx)
	require.NoError(t, newTester().TestAccountConnection(c, alias.ID, "fixture-model", "", ""))
	usage, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 7, usage.DayTokens)
	require.EqualValues(t, 1, usage.DayUsed)
	require.EqualValues(t, 2, calls.Load())
	policy.TokensPerDay = 50
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	recovered, err := newTester().RunTestBackground(ctx, alias.ID, "fixture-model")
	require.NoError(t, err)
	require.Equal(t, "success", recovered.Status)
	require.Nil(t, recovered.BudgetDeferral)
	usage, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 14, usage.DayTokens)
	require.EqualValues(t, 2, usage.UsageReported)
	require.Zero(t, usage.ActiveRequests)
	require.Zero(t, usage.UsagePending)
	policy.RequestsPerDay = 2
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	requestDenied, err := newTester().RunTestBackground(ctx, alias.ID, "fixture-model")
	require.NoError(t, err)
	require.Equal(t, "deferred", requestDenied.Status)
	require.Equal(t, "request_limit", requestDenied.BudgetDeferral.Reason)
	require.EqualValues(t, 3, calls.Load(), "request limits still apply when tokens have capacity")
	for _, a := range []*service.Account{first, alias} {
		require.Equal(t, before[a.ID], snapshot(a.ID), "budget decisions must not mutate the account")
	}
	// Old applications select their own columns and ignore the nullable JSON.
	require.NoError(t, repository.ApplyMigrations(ctx, db), "reapplying the migration must preserve history")
	var oldStatus string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status FROM scheduled_test_results WHERE id=$1`, stored.ID).Scan(&oldStatus))
	require.Equal(t, "deferred", oldStatus)
}
