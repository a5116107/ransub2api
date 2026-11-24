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
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	redisclient "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestCNMonitoringPriorityIndependentProcessesBudgetRotation(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	options := *integrationRedis.Options()
	options.DB = 11
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index, err := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer cn-priority-fixture-"))
		if err != nil || index < 0 || index >= len(calls) || r.Method != http.MethodGet || r.URL.Path != "/tenant/user/balance" {
			http.Error(w, "invalid local fixture route", 400)
			return
		}
		calls[index].Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10"}]}`)
	}))
	t.Cleanup(upstream.Close)
	accounts := NewAccountRepository(client, db, nil)
	ids := make([]int64, len(calls))
	before := make([]string, len(calls))
	row := func(id int64) string {
		t.Helper()
		var raw string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&raw))
		return raw
	}
	for i := range calls {
		a := &service.Account{Name: fmt.Sprintf("cn-priority-%d", i), Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1,
			Credentials: map[string]any{"api_key": fmt.Sprintf("cn-priority-fixture-%d", i), "base_url": upstream.URL + "/tenant", "account_mode": service.AccountModePayG}}
		if i >= 8 {
			a.Extra = map[string]any{"deepseek_balance_updated_at": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), "deepseek_balance": 10.0}
		}
		require.NoError(t, accounts.Create(ctx, a))
		ids[i] = a.ID
		before[i] = row(a.ID)
	}
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 4
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	command := monitoringBudgetNodeCommand{Mode: "cn", DSN: monitoringBudgetNodeDSN(t, db), RedisAddress: options.Addr, RedisDB: options.DB, Requests: 1}
	lastPID := 0
	for _, capacity := range []int64{4, 8, 12} {
		if capacity != 4 {
			policy.RequestsPerDay = capacity
			policy, err = budget.SaveSettings(ctx, policy)
			require.NoError(t, err)
		}
		replies := make(chan monitoringBudgetNodeReply, 1)
		node := startMonitoringBudgetNode(t, command, replies)
		require.NotEqual(t, lastPID, node.pid)
		lastPID = node.pid
		node.wait()
		require.Equal(t, http.StatusOK, receiveMonitoringBudgetReply(t, replies).Status)
		unique := 0
		for i := range calls {
			n := calls[i].Load()
			require.LessOrEqual(t, n, int64(1), "persistent turns must protect unserved peers, even after review classifications change")
			if n > 0 {
				unique++
			} else {
				require.Equal(t, before[i], row(ids[i]), "budget deferral must not change an unsent account's evidence or state")
			}
			if capacity == 4 {
				if i < 8 {
					require.Zero(t, n, "routine low-ID inventory must not spend the first budget increment")
				} else {
					require.EqualValues(t, 1, n)
				}
			}
		}
		require.EqualValues(t, capacity, unique)
		summary, err := budget.Summary(ctx)
		require.NoError(t, err)
		require.Equal(t, capacity, summary.DayUsed)
		require.Zero(t, summary.ActiveRequests)
		require.Zero(t, summary.DayTokens)
		require.Zero(t, summary.UsageUnknown)
		require.Zero(t, summary.UsagePending)
	}
	var downstream int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&downstream))
	require.Zero(t, downstream)
	t.Log("CN original ticker/SQL/Redis/HTTP: first 4 admissions all required reviews; 3 independent PIDs with capacity 4/8/12 cover 12 keys once; no downstream usage or synthetic tokens")
}
