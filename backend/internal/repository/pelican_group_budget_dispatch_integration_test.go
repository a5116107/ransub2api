//go:build integration

package repository_test

import (
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

func TestPelicanGroupScheduledBudgetHTTPPostgresStopAndRestart(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	var sends, invalid atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tenant/v1/responses" || r.Header.Get("Authorization") != "Bearer scheduled-group-fixture" {
			invalid.Add(1)
			http.Error(w, "invalid group probe fixture", http.StatusBadRequest)
			return
		}
		sends.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"<svg></svg>\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":3,\"total_tokens\":7}}}\n\n")
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	cfg.Default.RateMultiplier = 1
	accounts := repository.NewAccountRepository(client, db, nil)
	account := &service.Account{Name: "scheduled-group-fixture", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 20,
		Credentials: map[string]any{"api_key": "scheduled-group-fixture", "base_url": server.URL + "/tenant/v1"}}
	require.NoError(t, accounts.Create(ctx, account))
	groups := repository.NewGroupRepository(client, db)
	group := &service.Group{Name: "scheduled-group-budget", Platform: service.PlatformOpenAI, Status: service.StatusActive,
		SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 1}
	require.NoError(t, groups.Create(ctx, group))
	_, err := db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account.ID, group.ID)
	require.NoError(t, err)
	var accountBefore string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, account.ID).Scan(&accountBefore))

	budget := repository.NewMonitoringProbeBudgetRepository(db)
	policy := service.DefaultMonitoringProbeBudgetSettings()
	policy.Enabled, policy.RecoveryReservePercent = true, 0
	policy.RequestsPerDay, policy.RequestsPerMinute = 10000, 1000
	policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency = 20, 20, 20
	policy.TokensPerDay = 1
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	plans := repository.NewPelicanGroupTestRepository(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	original := make(map[int64]time.Time)
	for i := range 24 {
		due := now.Add(-time.Minute).Add(time.Duration(i/2) * time.Microsecond)
		plan, createErr := plans.CreatePlan(ctx, &service.PelicanGroupTestPlan{GroupID: group.ID, ModelID: "gpt-5.4",
			CronExpression: "*/30 * * * *", Enabled: true, NextRunAt: &due,
			PelicanConfig: &service.PelicanTestConfig{QuestionKind: "pelican", Prompt: "draw a pelican", ParallelCount: 1, ModelID: "gpt-5.4"}})
		require.NoError(t, createErr)
		original[plan.ID] = due
	}
	slots := repository.NewConcurrencyCache(repository.Issue594TestRedis(), 1, 60)
	concurrency := service.NewConcurrencyService(slots)
	newRunner := func() *service.PelicanGroupTestService {
		upstream := repository.NewHTTPUpstream(cfg)
		gateway := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil,
			repository.NewGatewayCache(repository.Issue594TestRedis()), cfg, nil, concurrency,
			service.NewBillingService(cfg, nil), nil, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
		t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
		t.Cleanup(gateway.CloseOpenAIWSPool)
		tester := service.NewAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil)
		tester.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(repository.NewMonitoringProbeBudgetRepository(db)))
		return service.NewPelicanGroupTestService(repository.NewPelicanGroupTestRepository(db), groups, nil, gateway, concurrency, tester, nil, nil)
	}
	readDue := func() []*service.PelicanGroupTestPlan {
		t.Helper()
		due, loadErr := repository.NewPelicanGroupTestRepository(db).ListDue(ctx, now)
		require.NoError(t, loadErr)
		for i, plan := range due {
			require.Equal(t, original[plan.ID], *plan.NextRunAt, "unclaimed plans keep their original due age across repository reconstruction")
			require.Nil(t, plan.RunningUntil)
			require.Nil(t, plan.LastRunAt)
			if i > 0 {
				previous := due[i-1]
				require.True(t, previous.NextRunAt.Before(*plan.NextRunAt) ||
					(previous.NextRunAt.Equal(*plan.NextRunAt) && previous.ID < plan.ID), "SQL due order remains next_run_at, then id")
			}
		}
		return due
	}
	first := newRunner()
	first.RunDue(ctx, now)
	require.Positive(t, sends.Load(), "exercise the actual scheduler, native HTTP adapter and persisted budget")
	require.LessOrEqual(t, sends.Load(), int32(4), "already-admitted calls may exceed the token limit, but later sends stop")
	remaining := readDue()
	require.GreaterOrEqual(t, len(remaining), 24-int(sends.Load())-4)
	require.Less(t, len(remaining), 24)
	beforeDenial := sends.Load()
	beforeRemaining := len(remaining)
	newRunner().RunDue(ctx, now)
	remaining = readDue()
	require.Equal(t, beforeDenial, sends.Load(), "a rebuilt service sends nothing while the SQL token allowance is exhausted")
	require.GreaterOrEqual(t, len(remaining), beforeRemaining-4)
	require.Less(t, len(remaining), beforeRemaining)

	policy.TokensPerDay = 10000
	_, err = repository.NewMonitoringProbeBudgetRepository(db).SaveSettings(ctx, policy)
	require.NoError(t, err)
	newRunner().RunDue(ctx, now)
	require.Empty(t, readDue(), "all original unclaimed plans resume after the allowance is repaired")
	require.EqualValues(t, int(beforeDenial)+len(remaining), sends.Load())
	reader := repository.NewPelicanGroupTestRepository(db)
	var successes, deferred int
	for id := range original {
		plan, loadErr := reader.GetPlan(ctx, id)
		require.NoError(t, loadErr)
		require.Nil(t, plan.RunningUntil, "every claimed plan finishes its original SQL lease")
		require.NotNil(t, plan.LastRunAt)
		history, total, loadErr := reader.ListResults(ctx, id, 0, 20)
		require.NoError(t, loadErr)
		require.EqualValues(t, 1, total)
		require.Len(t, history, 1)
		if history[0].BudgetDeferral != nil {
			deferred++
			require.Equal(t, "deferred", history[0].Status)
			require.Equal(t, "global_day", history[0].BudgetDeferral.Scope)
			require.Empty(t, history[0].ErrorMessage)
		} else {
			successes++
			require.Equal(t, "success", history[0].Status, history[0].ErrorMessage)
		}
	}
	require.Positive(t, deferred)
	require.EqualValues(t, sends.Load(), successes)
	usage, err := repository.NewMonitoringProbeBudgetRepository(db).Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, sends.Load(), usage.DayUsed)
	require.EqualValues(t, sends.Load()*7, usage.DayTokens, "only the actual returned usage is counted")
	require.EqualValues(t, sends.Load(), usage.UsageReported)
	require.Zero(t, usage.UsageUnknown)
	require.Zero(t, usage.UsagePending)
	require.Zero(t, usage.ActiveRequests)
	active, err := slots.GetAccountConcurrency(ctx, account.ID)
	require.NoError(t, err)
	require.Zero(t, active)
	require.Zero(t, invalid.Load())
	var accountAfter string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, account.ID).Scan(&accountAfter))
	require.Equal(t, accountBefore, accountAfter, "budget pauses do not recover or penalize accounts")
	var downstream int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&downstream))
	require.Zero(t, downstream, "monitoring does not become downstream billable usage")
	t.Logf("native scheduler/HTTP, SQL stop and reconstructed recovery: %d sent, %d deferred; no leaked SQL/Redis leases or downstream usage", successes, deferred)
}
