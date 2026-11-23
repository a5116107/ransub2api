//go:build integration

package repository

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	redisclient "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestScheduledTestIndependentProcessesHundredKeysRealCronAndRestart(t *testing.T) {
	const targets, failures = 100, 20
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	options := *integrationRedis.Options()
	options.DB = 11
	rdb := redisclient.NewClient(&options)
	t.Cleanup(func() { _ = rdb.Close() })
	size, err := rdb.DBSize(ctx).Result()
	require.NoError(t, err)
	require.Zero(t, size, "scheduled process fixture requires an unused disposable Redis DB")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, rdb.FlushDB(cleanup).Err())
		size, err := rdb.DBSize(cleanup).Result()
		require.NoError(t, err)
		require.Zero(t, size)
	})
	var calls [targets]atomic.Int64
	var active, maximum, invalid atomic.Int64
	entered, gate := make(chan struct{}, 20), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index, err := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer fixture-scheduled-node-"))
		if err != nil || index < 0 || index >= targets || r.Method != http.MethodPost || r.URL.Path != "/tenant/v1/responses" {
			invalid.Add(1)
			http.Error(w, "invalid scheduled process fixture request", http.StatusBadRequest)
			return
		}
		calls[index].Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		if index < failures {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"fixture unavailable"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":3,\"total_tokens\":7}}}\n\n")
	}))
	t.Cleanup(server.Close)
	t.Cleanup(release)
	accounts := NewAccountRepository(client, db, nil)
	plans := NewScheduledTestPlanRepository(db)
	results := NewScheduledTestResultRepository(db)
	scheduled := service.NewScheduledTestService(plans, results)
	before, planAccounts := make(map[int64]string, targets), make(map[int64]int64, targets)
	for i := range targets {
		account := &service.Account{Name: fmt.Sprintf("scheduled-node-account-%03d", i), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 4,
			Credentials: map[string]any{"api_key": fmt.Sprintf("fixture-scheduled-node-%d", i), "base_url": server.URL + "/tenant/v1"}}
		require.NoError(t, accounts.Create(ctx, account))
		plan, err := scheduled.CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: account.ID, ModelID: "fixture-model", Enabled: true,
			CronExpression: "0 0 1 1 *", MaxResults: 20})
		require.NoError(t, err)
		// Only seed the original fixture due time. All execution below uses the
		// original wall-clock cron, ten-second staggering and SQL claim logic.
		_, err = db.ExecContext(ctx, `UPDATE scheduled_test_plans SET next_run_at=$1 WHERE id=$2`, time.Now().UTC().Add(-time.Minute), plan.ID)
		require.NoError(t, err)
		var row string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, account.ID).Scan(&row))
		before[account.ID], planAccounts[plan.ID] = row, account.ID
	}
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay, policy.RequestsPerMinute = 100000, 10000
	policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency = 20, 20, 4
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	// Both processes must install their real cron before the same next minute;
	// avoid a startup split across the boundary without changing any clock.
	if until := time.Until(time.Now().Truncate(time.Minute).Add(time.Minute)); until < 10*time.Second {
		timer := time.NewTimer(until + time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			require.NoError(t, ctx.Err())
		}
	}
	command := monitoringBudgetNodeCommand{Mode: "scheduled", DSN: monitoringBudgetNodeDSN(t, db), RedisAddress: options.Addr,
		RedisDB: options.DB, Requests: 1, Tick: time.Now().UTC()}
	replies := make(chan monitoringBudgetNodeReply, 2)
	left, right := startMonitoringBudgetNode(t, command, replies), startMonitoringBudgetNode(t, command, replies)
	require.NotEqual(t, left.pid, right.pid)
	require.NotEqual(t, os.Getpid(), left.pid)
	wait := time.NewTimer(75 * time.Second)
	defer wait.Stop()
	for range 20 {
		select {
		case <-entered:
		case <-wait.C:
			t.Fatal("two original scheduled cron nodes did not reach twenty unique requests")
		}
	}
	var claims int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_plans WHERE running_until IS NOT NULL`).Scan(&claims))
	require.EqualValues(t, 20, claims)
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 20, summary.DayUsed)
	require.EqualValues(t, 20, summary.ActiveRequests)
	release()
	for _, node := range []monitoringBudgetNode{left, right} {
		node.wait()
		require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
	}
	assertCompleted := func() {
		t.Helper()
		var count, successes, unfinished, usage int64
		require.NoError(t, db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM scheduled_test_results), (SELECT count(*) FROM scheduled_test_results WHERE status='success'),
 (SELECT count(*) FROM scheduled_test_plans WHERE running_until IS NOT NULL), (SELECT count(*) FROM usage_logs)`).Scan(&count, &successes, &unfinished, &usage))
		require.EqualValues(t, targets, count)
		require.EqualValues(t, targets-failures, successes)
		require.Zero(t, unfinished)
		require.Zero(t, usage)
		for id, accountID := range planAccounts {
			plan, err := plans.GetByID(ctx, id)
			require.NoError(t, err)
			require.Equal(t, accountID, plan.AccountID)
			require.NotNil(t, plan.LastRunAt)
			require.NotNil(t, plan.NextRunAt)
			require.True(t, plan.NextRunAt.After(time.Now()))
			require.Nil(t, plan.RunningUntil)
			history, err := results.ListByPlanID(ctx, id, 20)
			require.NoError(t, err)
			require.Len(t, history, 1)
			require.Equal(t, id, history[0].PlanID)
		}
		for i := range calls {
			require.EqualValues(t, 1, calls[i].Load(), "each account has exactly one real request across both cron nodes")
		}
		for id, original := range before {
			var row string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&row))
			require.Equal(t, original, row)
		}
		summary, err := budget.Summary(ctx)
		require.NoError(t, err)
		require.EqualValues(t, targets, summary.DayUsed)
		require.EqualValues(t, (targets-failures)*7, summary.DayTokens)
		require.EqualValues(t, targets-failures, summary.UsageReported)
		require.EqualValues(t, failures, summary.UsageUnknown)
		require.Zero(t, summary.ActiveRequests)
		require.Zero(t, summary.UsagePending)
		require.Zero(t, active.Load())
		require.Zero(t, invalid.Load())
	}
	assertCompleted()
	// A newly constructed process waits for the next real minute boundary and
	// performs the original empty due scan; no fake tick or direct batch call.
	rebuilt := startMonitoringBudgetNode(t, command, replies)
	rebuilt.wait()
	require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
	assertCompleted()
	require.EqualValues(t, 20, maximum.Load())
	t.Log("independent scheduled nodes: original real-minute cron and ten-second staggering, 100 keys/100 unique SQL claims, 20 supplier failures/80 successes, no duplicate after reconstruction, 100 calls/560 actual tokens/20 unknown usages; no downstream usage")
}

func TestScheduledConnectivityClaimAndFinishFences(t *testing.T) {
	for _, scenario := range []string{"normal", "not_started", "admin_edit", "successor"} {
		t.Run(scenario, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			ctx := t.Context()
			account := mustCreateAccount(t, client, &service.Account{Name: "connectivity-finish", Platform: service.PlatformOpenAI,
				Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 2,
				Credentials: map[string]any{"api_key": "fixture-finish", "base_url": "https://fixture.invalid"}})
			plans := NewScheduledTestPlanRepository(db)
			now := time.Now().UTC().Truncate(time.Microsecond)
			due, next, until := now.Add(-time.Minute), now.Add(time.Hour), now.Add(15*time.Minute)
			plan, err := plans.Create(ctx, &service.ScheduledTestPlan{AccountID: account.ID, ModelID: "fixture-model", Enabled: true,
				CronExpression: "0 * * * *", MaxResults: 20, NextRunAt: &due})
			require.NoError(t, err)
			claimed, err := plans.ClaimPelican(ctx, plan, now, until, next)
			require.NoError(t, err)
			require.True(t, claimed)
			claimed, err = plans.ClaimPelican(ctx, plan, now, until, next)
			require.NoError(t, err)
			require.False(t, claimed, "the same ordinary plan cannot claim twice")
			loaded, err := plans.GetByID(ctx, plan.ID)
			require.NoError(t, err)
			require.Nil(t, loaded.PelicanConfig, "ordinary claims must not create quality config or round markers")
			require.Equal(t, plan.UpdatedAt, loaded.UpdatedAt)
			frozen := *plan
			frozen.NextRunAt = &next
			finished, retry := now.Add(time.Minute), now.Add(2*time.Hour)
			switch scenario {
			case "not_started":
				require.NoError(t, plans.FinishConnectivity(ctx, &frozen, until, nil, due))
				after, err := plans.GetByID(ctx, plan.ID)
				require.NoError(t, err)
				require.Nil(t, after.RunningUntil)
				require.Nil(t, after.LastRunAt)
				require.Nil(t, after.PelicanConfig)
				require.Equal(t, due, *after.NextRunAt)
				require.Equal(t, plan.UpdatedAt, after.UpdatedAt)
			case "admin_edit":
				manualNext := now.Add(24 * time.Hour)
				loaded.ModelID, loaded.CronExpression, loaded.Enabled, loaded.NextRunAt = "manual-model", "0 0 * * *", false, &manualNext
				edited, err := plans.Update(ctx, loaded)
				require.NoError(t, err)
				require.NoError(t, plans.FinishConnectivity(ctx, &frozen, until, &finished, retry))
				after, err := plans.GetByID(ctx, plan.ID)
				require.NoError(t, err)
				require.False(t, after.Enabled)
				require.Equal(t, "manual-model", after.ModelID)
				require.Equal(t, "0 0 * * *", after.CronExpression)
				require.Equal(t, edited.UpdatedAt, after.UpdatedAt)
				require.Equal(t, manualNext, *after.NextRunAt, "late budget retry must not overwrite an administrator's new schedule")
				require.Nil(t, after.RunningUntil)
			case "successor":
				successorNow, successorUntil, successorNext := next.Add(time.Minute), next.Add(16*time.Minute), next.Add(time.Hour)
				claimed, err := plans.ClaimPelican(ctx, loaded, successorNow, successorUntil, successorNext)
				require.NoError(t, err)
				require.True(t, claimed)
				var before, after string
				require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(p)::text FROM scheduled_test_plans p WHERE id=$1`, plan.ID).Scan(&before))
				require.NoError(t, plans.FinishConnectivity(ctx, &frozen, until, &finished, retry))
				require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(p)::text FROM scheduled_test_plans p WHERE id=$1`, plan.ID).Scan(&after))
				require.Equal(t, before, after, "a stale owner must not clear a successor's lease, last_run or schedule")
				loaded.NextRunAt = &successorNext
				require.NoError(t, plans.FinishConnectivity(ctx, loaded, successorUntil, &successorNow, successorNext))
			default:
				require.NoError(t, plans.FinishConnectivity(ctx, &frozen, until, &finished, retry))
				after, err := plans.GetByID(ctx, plan.ID)
				require.NoError(t, err)
				require.Nil(t, after.RunningUntil)
				require.Nil(t, after.PelicanConfig)
				require.Equal(t, finished, *after.LastRunAt)
				require.Equal(t, retry, *after.NextRunAt)
				require.Equal(t, plan.UpdatedAt, after.UpdatedAt)
			}
		})
	}
}
