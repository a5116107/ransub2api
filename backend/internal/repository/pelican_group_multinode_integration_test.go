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

func TestPelicanGroupIndependentProcessesHundredPlansAndRecovery(t *testing.T) {
	const targets, failures = 100, 20
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	options := *integrationRedis.Options()
	options.DB = 12
	rdb := redisclient.NewClient(&options)
	t.Cleanup(func() { _ = rdb.Close() })
	size, err := rdb.DBSize(ctx).Result()
	require.NoError(t, err)
	require.Zero(t, size, "group process fixture requires an unused DB in its disposable Redis")
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
	var failing atomic.Bool
	failing.Store(true)
	entered, gate := make(chan struct{}, 8), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index, err := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer fixture-group-node-"))
		if err != nil || index < 0 || index >= targets || r.Method != http.MethodPost || r.URL.Path != "/tenant/v1/responses" {
			invalid.Add(1)
			http.Error(w, "invalid independent group fixture request", http.StatusBadRequest)
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
		if failing.Load() && index < failures {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"fixture unavailable"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"<svg></svg>\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":3,\"total_tokens\":7}}}\n\n")
	}))
	t.Cleanup(server.Close)
	t.Cleanup(release)
	accounts := NewAccountRepository(client, db, nil)
	groups := NewGroupRepository(client, db)
	plans := NewPelicanGroupTestRepository(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	before := make(map[int64]string, targets)
	planIndexes := make(map[int64]int, targets)
	for i := range targets {
		account := &service.Account{Name: fmt.Sprintf("group-node-account-%03d", i), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 4,
			Credentials: map[string]any{"api_key": fmt.Sprintf("fixture-group-node-%d", i), "base_url": server.URL + "/tenant/v1"}}
		require.NoError(t, accounts.Create(ctx, account))
		group := &service.Group{Name: fmt.Sprintf("group-node-%03d", i), Platform: service.PlatformOpenAI, Status: service.StatusActive,
			SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 1}
		require.NoError(t, groups.Create(ctx, group))
		_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account.ID, group.ID)
		require.NoError(t, err)
		due := now.Add(-time.Minute).Add(time.Duration(i) * time.Microsecond)
		plan, err := plans.CreatePlan(ctx, &service.PelicanGroupTestPlan{GroupID: group.ID, ModelID: "model-fixture", Enabled: true,
			CronExpression: "*/30 * * * *", NextRunAt: &due,
			PelicanConfig: &service.PelicanTestConfig{QuestionKind: "pelican", Prompt: "draw a pelican", ParallelCount: 1, ModelID: "model-fixture"}})
		require.NoError(t, err)
		planIndexes[plan.ID] = i
		var row string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, account.ID).Scan(&row))
		before[account.ID] = row
	}
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay, policy.RequestsPerMinute = 100000, 10000
	policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency = 20, 20, 4
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	command := monitoringBudgetNodeCommand{Mode: "group", DSN: monitoringBudgetNodeDSN(t, db), RedisAddress: options.Addr,
		RedisDB: options.DB, Requests: 1, Tick: now}
	replies := make(chan monitoringBudgetNodeReply, 2)
	left, right := startMonitoringBudgetNode(t, command, replies), startMonitoringBudgetNode(t, command, replies)
	require.NotEqual(t, left.pid, right.pid)
	require.NotEqual(t, os.Getpid(), left.pid)
	for range 8 {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("independent group nodes did not reach eight unique physical requests")
		}
	}
	var claimed, distinct int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*),count(DISTINCT id) FROM pelican_group_test_plans WHERE running_until IS NOT NULL`).Scan(&claimed, &distinct))
	require.EqualValues(t, 8, claimed)
	require.EqualValues(t, 8, distinct)
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 8, summary.DayUsed)
	require.EqualValues(t, 8, summary.ActiveRequests)
	release()
	for range 2 {
		require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
	}
	left.wait()
	right.wait()
	assertRound := func(rounds int64) {
		t.Helper()
		var results, successes, leases, usage int64
		require.NoError(t, db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM pelican_group_test_results),
 (SELECT count(*) FROM pelican_group_test_results WHERE status='success'),
 (SELECT count(*) FROM pelican_group_test_plans WHERE running_until IS NOT NULL),
 (SELECT count(*) FROM usage_logs)`).Scan(&results, &successes, &leases, &usage))
		require.EqualValues(t, targets*rounds, results)
		require.EqualValues(t, targets*rounds-failures, successes)
		require.Zero(t, leases)
		require.Zero(t, usage)
		for i := range calls {
			require.Equal(t, rounds, calls[i].Load(), "credential %d must send once per claimed round", i)
		}
		summary, err := budget.Summary(ctx)
		require.NoError(t, err)
		require.EqualValues(t, targets*rounds, summary.DayUsed)
		require.EqualValues(t, (targets*rounds-failures)*7, summary.DayTokens)
		require.EqualValues(t, targets*rounds-failures, summary.UsageReported)
		require.EqualValues(t, failures, summary.UsageUnknown, "supplier errors without usage are not free generation")
		require.Zero(t, summary.ActiveRequests)
		require.Zero(t, summary.UsagePending)
		for id, original := range before {
			var row string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&row))
			require.Equal(t, original, row)
			active, err := NewConcurrencyCache(rdb, 1, 60).GetAccountConcurrency(ctx, id)
			require.NoError(t, err)
			require.Zero(t, active)
		}
		require.Zero(t, invalid.Load())
		require.Zero(t, active.Load())
	}
	assertRound(1)
	rebuilt := startMonitoringBudgetNode(t, command, replies)
	require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
	rebuilt.wait()
	assertRound(1)
	failing.Store(false)
	command.Tick = now.Add(31 * time.Minute)
	left, right = startMonitoringBudgetNode(t, command, replies), startMonitoringBudgetNode(t, command, replies)
	for range 2 {
		require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
	}
	left.wait()
	right.wait()
	assertRound(2)
	require.EqualValues(t, 8, maximum.Load())
	// Keep the next nominal due round, including every plan's original age,
	// while the shared SQL token balance is already exhausted.
	command.Tick = now.Add(61 * time.Minute)
	planRow := func(id int64) string {
		t.Helper()
		var row string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(p)::text FROM pelican_group_test_plans p WHERE id=$1`, id).Scan(&row))
		return row
	}
	priorPlans := make(map[int64]string, targets)
	for id := range planIndexes {
		priorPlans[id] = planRow(id)
	}
	policy.TokensPerDay = (targets*2 - failures) * 7
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	left, right = startMonitoringBudgetNode(t, command, replies), startMonitoringBudgetNode(t, command, replies)
	for range 2 {
		require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
	}
	left.wait()
	right.wait()
	deferred := make(map[int64]bool)
	for id := range planIndexes {
		history, total, err := plans.ListResults(ctx, id, 0, 20)
		require.NoError(t, err)
		switch total {
		case 2:
			require.Equal(t, priorPlans[id], planRow(id), "unclaimed work retains its original due time and history through exhaustion")
		case 3:
			require.Equal(t, "deferred", history[0].Status)
			require.NotNil(t, history[0].BudgetDeferral)
			require.Equal(t, "token_limit", history[0].BudgetDeferral.Reason)
			require.Equal(t, "global_day", history[0].BudgetDeferral.Scope)
			require.Empty(t, history[0].ErrorMessage)
			deferred[id] = true
		default:
			t.Fatalf("plan %d has %d results; want two prior results or one neutral deferred result", id, total)
		}
	}
	require.NotEmpty(t, deferred)
	require.LessOrEqual(t, len(deferred), 8, "two nodes only claim their already-dispatched workers before stopping")
	summary, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 200, summary.DayUsed, "exhaustion causes no physical send")
	require.EqualValues(t, 1260, summary.DayTokens)
	require.Zero(t, summary.ActiveRequests)
	require.Zero(t, summary.UsagePending)
	remaining, err := plans.ListDue(ctx, command.Tick)
	require.NoError(t, err)
	require.Len(t, remaining, targets-len(deferred))
	policy.TokensPerDay = 10000
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	left, right = startMonitoringBudgetNode(t, command, replies), startMonitoringBudgetNode(t, command, replies)
	for range 2 {
		require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
	}
	left.wait()
	right.wait()
	remaining, err = plans.ListDue(ctx, command.Tick)
	require.NoError(t, err)
	require.Empty(t, remaining, "new independent nodes resume all unclaimed work after budget repair")
	for id, index := range planIndexes {
		want := int64(3)
		if deferred[id] {
			want = 2 // A consumed nominal round is not silently replayed.
		}
		require.Equal(t, want, calls[index].Load())
		history, total, err := plans.ListResults(ctx, id, 0, 20)
		require.NoError(t, err)
		require.EqualValues(t, 3, total)
		if !deferred[id] {
			require.Equal(t, "success", history[0].Status)
		}
		plan, err := plans.GetPlan(ctx, id)
		require.NoError(t, err)
		require.Nil(t, plan.RunningUntil)
	}
	summary, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 300-len(deferred), summary.DayUsed)
	require.EqualValues(t, (280-len(deferred))*7, summary.DayTokens)
	require.EqualValues(t, 280-len(deferred), summary.UsageReported)
	require.EqualValues(t, failures, summary.UsageUnknown)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.ActiveRequests)
	for id, original := range before {
		var row string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&row))
		require.Equal(t, original, row)
		active, err := NewConcurrencyCache(rdb, 1, 60).GetAccountConcurrency(ctx, id)
		require.NoError(t, err)
		require.Zero(t, active)
	}
	var usage, resultCount int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM usage_logs),(SELECT count(*) FROM pelican_group_test_results)`).Scan(&usage, &resultCount))
	require.Zero(t, usage)
	require.EqualValues(t, 300, resultCount)
	require.Zero(t, invalid.Load())
	require.Zero(t, active.Load())
	t.Logf("independent group nodes: 100 original plans, 20 supplier failures did not starve peers; restart sent no duplicate, next nominal round recovered, exhausted budget sent zero and left %d neutral deferrals; repaired nodes resumed %d untouched plans without replay or downstream usage", len(deferred), targets-len(deferred))
}
