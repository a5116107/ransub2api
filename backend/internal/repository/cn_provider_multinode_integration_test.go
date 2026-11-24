//go:build integration

package repository

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	redisclient "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestCNProviderIndependentProcessesPreserveQueueAge(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	options := *integrationRedis.Options()
	options.DB = 10
	rdb := redisclient.NewClient(&options)
	t.Cleanup(func() { _ = rdb.Close() })
	size, err := rdb.DBSize(ctx).Result()
	require.NoError(t, err)
	require.Zero(t, size)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, rdb.FlushDB(cleanup).Err())
	})
	var calls [12]atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index, err := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer cn-age-fixture-"))
		if err != nil || index < 0 || index >= len(calls) || r.Method != http.MethodGet || r.URL.Path != "/tenant/user/balance" {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		calls[index].Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10"}]}`)
	}))
	t.Cleanup(server.Close)
	accounts := NewAccountRepository(client, db, nil)
	for i := range calls {
		require.NoError(t, accounts.Create(ctx, &service.Account{Name: fmt.Sprintf("cn-age-%d", i), Platform: service.PlatformDeepseek,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1,
			Credentials: map[string]any{"api_key": fmt.Sprintf("cn-age-fixture-%d", i), "base_url": server.URL + "/tenant", "account_mode": service.AccountModePayG}}))
	}
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 4
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	command := monitoringBudgetNodeCommand{Mode: "cn", DSN: monitoringBudgetNodeDSN(t, db), RedisAddress: options.Addr, RedisDB: options.DB, Requests: 1}
	previousPID := 0
	for _, capacity := range []int64{4, 8, 12} {
		if capacity != 4 {
			policy.RequestsPerDay = capacity
			policy, err = budget.SaveSettings(ctx, policy)
			require.NoError(t, err)
		}
		replies := make(chan monitoringBudgetNodeReply, 1)
		node := startMonitoringBudgetNode(t, command, replies)
		require.NotEqual(t, previousPID, node.pid)
		previousPID = node.pid
		require.Equal(t, http.StatusOK, receiveMonitoringBudgetReply(t, replies).Status)
		node.wait()
		unique := 0
		for i := range calls {
			count := calls[i].Load()
			if count > 0 {
				unique++
			}
			require.LessOrEqual(t, count, int64(1), "a restarted node must preserve account %d's consumed turn", i)
		}
		require.EqualValues(t, capacity, unique, "newly restored capacity must serve older unserved peers, not restart the low-ID prefix")
		summary, err := budget.Summary(ctx)
		require.NoError(t, err)
		require.Equal(t, capacity, summary.DayUsed)
		require.Zero(t, summary.ActiveRequests)
	}
	var downstream int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&downstream))
	require.Zero(t, downstream)
}

func TestCNProviderIndependentProcessesDoNotDuplicateCycle(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	options := *integrationRedis.Options()
	options.DB = 9
	rdb := redisclient.NewClient(&options)
	t.Cleanup(func() { _ = rdb.Close() })
	size, err := rdb.DBSize(ctx).Result()
	require.NoError(t, err)
	require.Zero(t, size)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, rdb.FlushDB(cleanup).Err())
	})
	var mu sync.Mutex
	calls := map[string]int{}
	manual := false
	manualCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index, err := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer fixture-cn-node-"))
		if err != nil || index < 0 || index >= 4 || r.Method != http.MethodGet || r.URL.Path != "/tenant/user/balance" {
			http.Error(w, "invalid CN fixture request", http.StatusBadRequest)
			return
		}
		key := fmt.Sprintf("%d/%d", time.Now().UTC().Truncate(time.Second).Unix(), index)
		mu.Lock()
		if manual {
			manualCalls++
		} else {
			calls[key]++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10"}]}`)
	}))
	t.Cleanup(server.Close)
	accounts := NewAccountRepository(client, db, nil)
	var firstID int64
	for i := range 4 {
		account := &service.Account{Name: fmt.Sprintf("cn-node-%d", i), Platform: service.PlatformDeepseek,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 2,
			Credentials: map[string]any{"api_key": fmt.Sprintf("fixture-cn-node-%d", i), "base_url": server.URL + "/tenant", "account_mode": service.AccountModePayG}}
		require.NoError(t, accounts.Create(ctx, account))
		if i == 0 {
			firstID = account.ID
		}
	}
	budget := NewMonitoringProbeBudgetRepository(db)
	policy, err := budget.SaveSettings(ctx, monitoringBudgetPolicy())
	require.NoError(t, err)
	command := monitoringBudgetNodeCommand{Mode: "cn", DSN: monitoringBudgetNodeDSN(t, db), RedisAddress: options.Addr, RedisDB: options.DB, Requests: 1}
	runPair := func(held bool) {
		t.Helper()
		// Keep the short fixture's outbound work away from the UTC edge;
		// duplicate assertions use actual HTTP arrival time, not a fake tick.
		start := time.Now().UTC().Truncate(time.Second).Add(200 * time.Millisecond)
		if !time.Now().Before(start) {
			start = start.Add(time.Second)
		}
		time.Sleep(time.Until(start))
		replies := make(chan monitoringBudgetNodeReply, 2)
		left, right := startMonitoringBudgetNode(t, command, replies), startMonitoringBudgetNode(t, command, replies)
		require.NotEqual(t, left.pid, right.pid)
		for _, node := range []monitoringBudgetNode{left, right} {
			node.wait()
			require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
		}
		remaining, err := rdb.Exists(ctx, "leader:lock:cn_provider_balance_check").Result()
		require.NoError(t, err)
		if held {
			require.EqualValues(t, 1, remaining, "stopping contenders must preserve the live holder")
		} else {
			require.Zero(t, remaining, "stopped processes must release their running lease")
		}
	}
	physicalCount := func() int64 {
		mu.Lock()
		defer mu.Unlock()
		var total int64
		for _, n := range calls {
			total += int64(n)
		}
		return total
	}
	accountRows := func() string {
		t.Helper()
		var rows string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT json_agg(a ORDER BY id)::text FROM accounts a`).Scan(&rows))
		return rows
	}
	runPair(false)
	firstPhysical := physicalCount()
	require.GreaterOrEqual(t, firstPhysical, int64(8))
	firstRows := accountRows()
	// A live holder gates both independent runners, not ordinary management.
	locks := NewLeaderLockCache(rdb)
	acquired, err := locks.TryAcquireLeaderLock(ctx, "cn_provider_balance_check", "fixture-live-holder", 30*time.Second)
	require.NoError(t, err)
	require.True(t, acquired)
	runPair(true)
	require.Equal(t, firstPhysical, physicalCount())
	require.Equal(t, firstRows, accountRows(), "a peer-owned cycle must not rewrite snapshots or freshness")
	mu.Lock()
	manual = true
	mu.Unlock()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	upstream := NewHTTPUpstream(cfg)
	native := pr612NativeTransport(t, upstream)
	defer func() {
		native.mu.Lock()
		defer native.mu.Unlock()
		for _, entry := range native.clients {
			entry.client.CloseIdleConnections()
		}
	}()
	result, err := service.NewCNProviderBalanceService(accounts, nil, upstream, cfg).QueryBalance(ctx, firstID)
	require.NoError(t, err)
	require.True(t, result.Success)
	mu.Lock()
	manual = false
	manualCount := manualCalls
	mu.Unlock()
	require.Equal(t, 1, manualCount)
	require.NoError(t, locks.ReleaseLeaderLock(ctx, "cn_provider_balance_check", "fixture-live-holder"))
	// New processes must still honor the actual consumed budget, without
	// refreshing the old rows or writing a false successful observation.
	policy.RequestsPerDay = firstPhysical
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	pausedRows := accountRows()
	runPair(false)
	require.Equal(t, firstPhysical, physicalCount())
	require.Equal(t, pausedRows, accountRows())
	policy.RequestsPerDay = 10000
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	runPair(false)
	physical := physicalCount()
	require.GreaterOrEqual(t, physical-firstPhysical, int64(8), "fresh independent processes must resume after the budget is restored")
	mu.Lock()
	for key, n := range calls {
		require.Equal(t, 1, n, "CN credential/cycle %s was sent more than once across independent full nodes", key)
	}
	mu.Unlock()
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.Equal(t, physical, summary.DayUsed)
	require.Zero(t, summary.DayTokens)
	require.Zero(t, summary.ActiveRequests)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.UsageUnknown)
	var usage int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&usage))
	require.Zero(t, usage)
	t.Logf("four real-ticker stages / eight independent processes: periodic=%d, manual=%d, tokens=0, active=0, pending=0, unknown=0, downstream_usage=0", physical, manualCalls)
}
