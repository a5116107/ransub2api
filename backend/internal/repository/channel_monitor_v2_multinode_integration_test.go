//go:build integration

package repository

import (
	"context"
	"fmt"
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

func TestChannelMonitorV2IndependentProcessesRealCronBudgetAndRecovery(t *testing.T) {
	const targets, wrongAnswers = 64, 16 // The original product cap is 64 probes.
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	options := *integrationRedis.Options()
	options.DB = 10
	rdb := redisclient.NewClient(&options)
	t.Cleanup(func() { _ = rdb.Close() })
	size, err := rdb.DBSize(ctx).Result()
	require.NoError(t, err)
	require.Zero(t, size, "V2 process fixture needs an unused DB in the disposable Redis")
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
	var incorrect atomic.Bool
	incorrect.Store(true)
	entered, gate := make(chan struct{}, 8), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index, err := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer fixture-v2-node-"))
		if err != nil || index < 0 || index >= targets || r.Method != http.MethodPost || r.URL.Path != "/tenant/v1/responses" {
			invalid.Add(1)
			http.Error(w, "invalid V2 fixture request", http.StatusBadRequest)
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
		answer := "21"
		if incorrect.Load() && index < wrongAnswers {
			answer = "20"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":3,\"total_tokens\":7}}}\n\n", answer)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(release)
	accounts := NewAccountRepository(client, db, nil)
	groups := NewGroupRepository(client, db)
	settings := NewSettingRepository(client)
	require.NoError(t, settings.SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v2"}))
	v2 := NewChannelMonitorV2Repository(db)
	config, err := v2.GetConfig(ctx)
	require.NoError(t, err)
	config.Enabled = true
	config.GroupIDs, config.CandyProbes = nil, nil
	for i := range config.Platforms {
		config.Platforms[i].Enabled = config.Platforms[i].Platform == service.PlatformOpenAI
	}
	before := make(map[int64]string, targets)
	for i := range targets {
		account := &service.Account{Name: fmt.Sprintf("v2-node-account-%03d", i), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 4,
			Credentials: map[string]any{"api_key": fmt.Sprintf("fixture-v2-node-%d", i), "base_url": server.URL + "/tenant/v1"}}
		require.NoError(t, accounts.Create(ctx, account))
		group := &service.Group{Name: fmt.Sprintf("v2-node-group-%03d", i), Platform: service.PlatformOpenAI, Status: service.StatusActive,
			SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 1}
		require.NoError(t, groups.Create(ctx, group))
		_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account.ID, group.ID)
		require.NoError(t, err)
		config.GroupIDs = append(config.GroupIDs, group.ID)
		config.CandyProbes = append(config.CandyProbes, service.ChannelMonitorV2CandyProbe{GroupID: group.ID, Enabled: true,
			Model: "model-fixture", ReasoningEffort: "medium", IntervalMinutes: 1})
		var row string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, account.ID).Scan(&row))
		before[account.ID] = row
	}
	config, err = v2.UpdateConfig(ctx, *config, config.Version)
	require.NoError(t, err)
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay, policy.RequestsPerMinute = 1000000, 10000
	policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency = 20, 20, 4
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	command := monitoringBudgetNodeCommand{Mode: "v2", DSN: monitoringBudgetNodeDSN(t, db), RedisAddress: options.Addr,
		RedisDB: options.DB, Requests: 1, Tick: time.Now().UTC()}
	replies := make(chan monitoringBudgetNodeReply, 2)
	startPair := func() (monitoringBudgetNode, monitoringBudgetNode) {
		t.Helper()
		if until := time.Until(time.Now().Truncate(time.Minute).Add(time.Minute)); until < 10*time.Second {
			timer := time.NewTimer(until + time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				require.NoError(t, ctx.Err())
			}
		}
		left, right := startMonitoringBudgetNode(t, command, replies), startMonitoringBudgetNode(t, command, replies)
		require.NotEqual(t, left.pid, right.pid)
		require.NotEqual(t, os.Getpid(), left.pid)
		return left, right
	}
	waitNodes := func(nodes ...monitoringBudgetNode) {
		t.Helper()
		for _, node := range nodes {
			node.wait()
			require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
		}
	}
	left, right := startPair()
	timer := time.NewTimer(75 * time.Second)
	defer timer.Stop()
	for range 8 {
		select {
		case <-entered:
		case <-timer.C:
			t.Fatal("two V2 cron nodes did not enter eight unique in-flight probes")
		}
	}
	var claims, scopes int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*),count(DISTINCT(group_id,config_key,slot)) FROM channel_monitor_v2_candy_results WHERE verdict='running'`).Scan(&claims, &scopes))
	require.EqualValues(t, 8, claims)
	require.EqualValues(t, 8, scopes)
	release()
	waitNodes(left, right)
	assertPhysical := func(rounds int64) {
		t.Helper()
		for i := range calls {
			require.Equal(t, rounds, calls[i].Load(), "each Key sends once per real V2 slot; wrong answers are not retried")
		}
		for id, original := range before {
			var row string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&row))
			require.Equal(t, original, row)
			n, err := NewConcurrencyCache(rdb, 1, 60).GetAccountConcurrency(ctx, id)
			require.NoError(t, err)
			require.Zero(t, n)
		}
		summary, err := budget.Summary(ctx)
		require.NoError(t, err)
		require.EqualValues(t, targets*rounds, summary.DayUsed)
		require.EqualValues(t, targets*rounds*7, summary.DayTokens)
		require.EqualValues(t, targets*rounds, summary.UsageReported)
		require.Zero(t, summary.UsageUnknown)
		require.Zero(t, summary.UsagePending)
		require.Zero(t, summary.ActiveRequests)
		require.Zero(t, active.Load())
		require.Zero(t, invalid.Load())
	}
	assertHistory := func(physicalRounds, deferredRows int64) {
		t.Helper()
		var total, correct, wrong, running, deferred, duplicate, usage int64
		require.NoError(t, db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM channel_monitor_v2_candy_results),
 (SELECT count(*) FROM channel_monitor_v2_candy_results WHERE verdict='correct'),
 (SELECT count(*) FROM channel_monitor_v2_candy_results WHERE verdict='incorrect'),
 (SELECT count(*) FROM channel_monitor_v2_candy_results WHERE verdict='running'),
 (SELECT count(*) FROM channel_monitor_v2_candy_results WHERE verdict='deferred' AND budget_deferral IS NOT NULL),
 (SELECT count(*) FROM (SELECT group_id,config_key,slot FROM channel_monitor_v2_candy_results GROUP BY group_id,config_key,slot HAVING count(*)>1) d),
 (SELECT count(*) FROM usage_logs)`).Scan(&total, &correct, &wrong, &running, &deferred, &duplicate, &usage))
		require.EqualValues(t, targets*physicalRounds+deferredRows, total)
		require.EqualValues(t, targets*physicalRounds-wrongAnswers, correct)
		require.EqualValues(t, wrongAnswers, wrong, "later correct slots do not rewrite incorrect history")
		require.Equal(t, deferredRows, deferred)
		require.Zero(t, running)
		require.Zero(t, duplicate)
		require.Zero(t, usage)
	}
	assertPhysical(1)
	assertHistory(1, 0)
	incorrect.Store(false)
	// A fresh process waits for the next real cron tick and original stagger.
	waitNodes(startMonitoringBudgetNode(t, command, replies))
	assertPhysical(2)
	assertHistory(2, 0)
	policy.TokensPerDay = targets * 2 * 7
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	left, right = startPair()
	waitNodes(left, right)
	assertPhysical(2)
	var deferred int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM channel_monitor_v2_candy_results WHERE verdict='deferred'`).Scan(&deferred))
	require.Positive(t, deferred)
	require.LessOrEqual(t, deferred, int64(8))
	assertHistory(2, deferred)
	policy.TokensPerDay = 10000
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	left, right = startPair()
	waitNodes(left, right)
	assertPhysical(3)
	assertHistory(3, deferred)
	require.EqualValues(t, 8, maximum.Load())
	t.Logf("V2 original real cron: 64 Keys, four real minute slots, two independent nodes, 16 wrong answers retained without retry, exhausted slot zero sends/%d neutral deferred claims; recovery 192 calls/1344 actual tokens/0 downstream usage", deferred)
}
