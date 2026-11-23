//go:build integration

package repository_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type budgetDispatchPlanProbe struct {
	service.ScheduledTestPlanRepository
	service.QualitySharedBatchRepository
	finalizations atomic.Int32
	done          chan struct{}
}

func (r *budgetDispatchPlanProbe) FinalizeQualityBatches(ctx context.Context) error {
	err := r.QualitySharedBatchRepository.FinalizeQualityBatches(ctx)
	if r.finalizations.Add(1) == 2 {
		close(r.done)
	}
	return err
}

func TestScheduledBudgetDispatchHTTPPostgresPauseAndRestart(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	var calls, invalid atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tenant/v1/responses" || r.Header.Get("Authorization") != "Bearer scheduled-dispatch-fixture" {
			invalid.Add(1)
			http.Error(w, "invalid scheduled dispatch fixture", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"<svg></svg>\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":3,\"total_tokens\":7}}}\n\n")
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{Timezone: "UTC", Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	accounts := repository.NewAccountRepository(client, db, nil)
	plans, results := repository.NewScheduledTestPlanRepository(db), repository.NewScheduledTestResultRepository(db)
	scheduled := service.NewScheduledTestService(plans, results)
	budget := repository.NewMonitoringProbeBudgetRepository(db)
	policy := service.DefaultMonitoringProbeBudgetSettings()
	policy.Enabled, policy.RecoveryReservePercent, policy.TokensPerDay = true, 0, 5
	policy.RequestsPerDay, policy.RequestsPerMinute = 10000, 1000
	policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency = 20, 20, 20
	policy, err := budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	newTester := func() *service.AccountTestService {
		tester := service.NewAccountTestService(accounts, nil, nil, nil, nil, repository.NewHTTPUpstream(cfg), cfg, nil)
		tester.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(repository.NewMonitoringProbeBudgetRepository(db)))
		return tester
	}
	before := make(map[int64]string)
	original := make(map[int64]time.Time)
	for i := range 40 {
		account := &service.Account{Name: fmt.Sprintf("scheduled-dispatch-%02d", i), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 2,
			Credentials: map[string]any{"api_key": "scheduled-dispatch-fixture", "base_url": server.URL + "/tenant/v1"}}
		require.NoError(t, accounts.Create(ctx, account))
		if i == 0 {
			seed, seedErr := newTester().RunTestBackground(ctx, account.ID, "fixture-model")
			require.NoError(t, seedErr)
			require.Equal(t, "success", seed.Status)
		}
		var row string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, account.ID).Scan(&row))
		before[account.ID] = row
		plan := &service.ScheduledTestPlan{AccountID: account.ID, ModelID: "fixture-model", Enabled: true, AutoRecover: true,
			CronExpression: "0 0 1 1 *", MaxResults: 20}
		if i%2 == 1 {
			plan.PelicanConfig = &service.PelicanTestConfig{Prompt: "draw a pelican", ReasoningEffort: "high", ParallelCount: 1}
		}
		created, createErr := scheduled.CreatePlan(ctx, plan)
		require.NoError(t, createErr)
		due := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
		_, err = db.ExecContext(ctx, `UPDATE scheduled_test_plans SET next_run_at=$1 WHERE id=$2`, due, created.ID)
		require.NoError(t, err)
		original[created.ID] = due
	}
	newRunner := func() (*service.ScheduledTestRunnerService, <-chan struct{}) {
		original := repository.NewScheduledTestPlanRepository(db)
		batches, ok := original.(service.QualitySharedBatchRepository)
		require.True(t, ok)
		probe := &budgetDispatchPlanProbe{ScheduledTestPlanRepository: original, QualitySharedBatchRepository: batches, done: make(chan struct{})}
		runner := service.NewScheduledTestRunnerService(probe,
			service.NewScheduledTestService(repository.NewScheduledTestPlanRepository(db), repository.NewScheduledTestResultRepository(db)), newTester(), nil, cfg)
		t.Cleanup(runner.Stop)
		return runner, probe.done
	}
	waitBatch := func(done <-chan struct{}) {
		t.Helper()
		wait, cancel := context.WithTimeout(ctx, 75*time.Second)
		defer cancel()
		select {
		case <-done:
		case <-wait.Done():
			t.Fatal("the real cron must drain dispatched work and reach its original end-of-batch finalizer")
		}
	}
	first, firstDone := newRunner()
	first.Start()
	waitBatch(firstDone)
	first.Stop()
	require.EqualValues(t, 1, calls.Load(), "a fully exhausted SQL allowance causes no new physical sends")
	var deferred, unclaimed int
	reader := repository.NewScheduledTestPlanRepository(db)
	for id, due := range original {
		plan, loadErr := reader.GetByID(ctx, id)
		require.NoError(t, loadErr)
		require.Nil(t, plan.RunningUntil)
		history, loadErr := repository.NewScheduledTestResultRepository(db).ListByPlanID(ctx, id, 20)
		require.NoError(t, loadErr)
		if len(history) == 0 {
			unclaimed++
			require.Nil(t, plan.LastRunAt)
			require.Equal(t, due, *plan.NextRunAt, "the untouched queue must retain its nominal due time")
		} else {
			deferred++
			require.Len(t, history, 1)
			denial := history[0].BudgetDeferral
			if plan.PelicanConfig == nil {
				require.Equal(t, "deferred", history[0].Status)
			} else {
				// Pelican retains its existing answer/quality separation. A local
				// budget failure is inconclusive judgment, not a provider failure.
				require.Equal(t, "failed", history[0].Status)
				require.Equal(t, "answer_budget_deferred", history[0].ErrorMessage)
				require.NotNil(t, history[0].QualityJudgment)
				require.Equal(t, "unknown", history[0].QualityJudgment.Verdict)
				denial = history[0].QualityJudgment.BudgetDeferral
			}
			require.NotNil(t, denial)
			require.Equal(t, "global_day", denial.Scope)
		}
	}
	require.Positive(t, deferred)
	require.LessOrEqual(t, deferred, 10, "legacy connectivity and Pelican plans share the existing ten-worker dispatch boundary")
	require.Equal(t, 40-deferred, unclaimed)
	policy.TokensPerDay = 10000
	_, err = repository.NewMonitoringProbeBudgetRepository(db).SaveSettings(ctx, policy)
	require.NoError(t, err)
	restarted, recoveredDone := newRunner()
	restarted.Start()
	waitBatch(recoveredDone)
	restarted.Stop()
	var succeeded int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_results WHERE status='success'`).Scan(&succeeded))
	require.Equal(t, unclaimed, succeeded, "a reconstructed cron must run every original untouched plan after allowance repair")
	require.EqualValues(t, 1+unclaimed, calls.Load())
	usage, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, calls.Load(), usage.DayUsed)
	require.EqualValues(t, calls.Load()*7, usage.DayTokens)
	require.EqualValues(t, calls.Load(), usage.UsageReported)
	require.Zero(t, usage.UsageUnknown)
	require.Zero(t, usage.UsagePending)
	require.Zero(t, usage.ActiveRequests)
	require.Zero(t, invalid.Load())
	for accountID, row := range before {
		var current string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, accountID).Scan(&current))
		require.Equal(t, row, current, "budget stops do not recover or penalize accounts")
	}
	var activeLeases, downstream int
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT count(*) FROM scheduled_test_plans WHERE running_until IS NOT NULL`).Scan(&activeLeases))
	require.Zero(t, activeLeases)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&downstream))
	require.Zero(t, downstream)
	t.Logf("native cron/HTTP/SQL: %d neutral deferrals, %d original due plans recovered; %d physical calls; no active leases or downstream usage", deferred, unclaimed, calls.Load())
}
