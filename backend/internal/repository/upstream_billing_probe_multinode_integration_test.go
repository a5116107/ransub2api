//go:build integration

package repository

import (
	"context"
	"encoding/json"
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

func TestUpstreamBillingProbeIndependentProcessesHundredAccountsRealCadence(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	options := *integrationRedis.Options()
	options.DB = 13
	rdb := redisclient.NewClient(&options)
	t.Cleanup(func() { _ = rdb.Close() })
	size, err := rdb.DBSize(ctx).Result()
	require.NoError(t, err)
	require.Zero(t, size, "this fixture needs an unused database in the disposable Redis container")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		keys, err := rdb.Keys(cleanup, leaderLockKeyPrefix+"upstream:billing:probe:leader*").Result()
		require.NoError(t, err)
		if len(keys) > 0 {
			require.NoError(t, rdb.Del(cleanup, keys...).Err())
		}
		size, err := rdb.DBSize(cleanup).Result()
		require.NoError(t, err)
		require.Zero(t, size)
	})
	var rateCalls, balanceCalls [103]atomic.Int64
	var active, maximum, unexpected atomic.Int64
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
		}
		index, err := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer fixture-cadence-"))
		if err != nil || index < 0 || index >= len(rateCalls) || r.Method != http.MethodGet {
			unexpected.Add(1)
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/v1/sub2api/billing":
			rateCalls[index].Add(1)
		case "/v1/usage":
			balanceCalls[index].Add(1)
		default:
			unexpected.Add(1)
			http.NotFound(w, r)
			return
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
		w.Header().Set("Content-Type", "application/json")
		if index < 20 {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"fixture unavailable"}`)
			return
		}
		if r.URL.Path == "/v1/usage" {
			_, _ = io.WriteString(w, `{"mode":"unrestricted","isValid":true,"remaining":12.34,"unit":"USD","balance":12.34}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":0.03,"resolved_rate_multiplier":0.03,"peak_rate_enabled":false,"effective_rate_multiplier":0.03,"observed_at":%q}`, time.Now().UTC().Format(time.RFC3339Nano))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(release)
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	settings := service.NewSettingService(NewSettingRepository(client), cfg)
	require.NoError(t, settings.SetUpstreamBillingProbeSettings(ctx, &service.UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}))
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerMinute, policy.RequestsPerDay, policy.SiteRequestsPerMinute = 240, 100000, 240
	policy.KeyRequestsPerMinute = 3
	policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency = 4, 4, 1
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	for i := range rateCalls {
		credentials, err := json.Marshal(map[string]string{"api_key": fmt.Sprintf("fixture-cadence-%d", i), "base_url": server.URL})
		require.NoError(t, err)
		kind, status, enabled := service.AccountTypeAPIKey, service.StatusActive, i != 100
		if i == 101 {
			status = "disabled"
		}
		if i == 102 {
			kind = service.AccountTypeOAuth
		}
		extra := fmt.Sprintf(`{"upstream_billing_probe_enabled":%t,"upstream_billing_rate_sync_enabled":false}`, enabled)
		_, err = db.ExecContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra,rate_multiplier)
 VALUES($1,'openai',$2,$3,$4::jsonb,$5::jsonb,1)`, fmt.Sprintf("cadence-%03d", i), kind, status, string(credentials), extra)
		require.NoError(t, err)
	}
	waitUntil := func(at time.Time) {
		t.Helper()
		timer := time.NewTimer(time.Until(at))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			require.NoError(t, ctx.Err())
		}
	}
	// Leave enough of the first real UTC minute for the blocked leader,
	// overlapping peer and later process restart without changing any clock.
	boundary := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	if time.Until(boundary) < 15*time.Second {
		waitUntil(boundary.Add(100 * time.Millisecond))
	}
	firstSlot := time.Now().UTC().Truncate(time.Minute)
	command := monitoringBudgetNodeCommand{Mode: "billing", DSN: monitoringBudgetNodeDSN(t, db),
		RedisAddress: options.Addr, RedisDB: options.DB, Requests: 1}
	replies := make(chan monitoringBudgetNodeReply, 2)
	first := startMonitoringBudgetNode(t, command, replies)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("native billing leader did not reach its physical request")
	}
	peer := startMonitoringBudgetNode(t, command, replies)
	require.NotEqual(t, first.pid, peer.pid)
	require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
	peer.wait()
	require.LessOrEqual(t, active.Load(), int64(4), "a second process must not create another worker batch")
	release()
	require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
	first.wait()
	for cycle := 0; cycle < 5; cycle++ {
		if cycle > 0 {
			waitUntil(firstSlot.Add(time.Duration(cycle)*time.Minute + 100*time.Millisecond))
			left := startMonitoringBudgetNode(t, command, replies)
			right := startMonitoringBudgetNode(t, command, replies)
			require.NotEqual(t, left.pid, right.pid)
			for range 2 {
				require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
			}
			left.wait()
			right.wait()
		}
		for i := range rateCalls {
			wantRate, wantBalance := int64(0), int64(0)
			if i < (cycle+1)*20 {
				wantRate = 1
				if i >= 20 {
					wantBalance = 1
				}
			}
			require.Equal(t, wantRate, rateCalls[i].Load(), "cycle %d account %d must get only one turn", cycle+1, i)
			require.Equal(t, wantBalance, balanceCalls[i].Load(), "failure backoff must not leak extra balance calls")
		}
		// A fresh process after the preceding leader exited must still see
		// the Redis cadence lease, not use a new process-local allowance.
		staggered := startMonitoringBudgetNode(t, command, replies)
		require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
		staggered.wait()
		for i := range rateCalls {
			want := int64(0)
			if i < (cycle+1)*20 {
				want = 1
			}
			require.Equal(t, want, rateCalls[i].Load(), "a process restart must not repeat this minute's batch")
		}
	}
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	due, err := accounts.ListDueUpstreamBillingProbeAccounts(ctx, time.Now(), 104)
	require.NoError(t, err)
	require.Empty(t, due)
	var used, rows, failed, healthy, unchanged int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT
 (SELECT COALESCE(sum(used),0) FROM monitoring_probe_budget_counters WHERE dimension='global_day'),
 (SELECT count(*) FROM usage_logs),
 (SELECT count(*) FROM accounts WHERE extra#>>'{upstream_billing_probe,status}'='failed'),
 (SELECT count(*) FROM accounts WHERE extra#>>'{upstream_billing_probe,status}'='ok'),
 (SELECT count(*) FROM accounts WHERE rate_multiplier=1)`).Scan(&used, &rows, &failed, &healthy, &unchanged))
	require.EqualValues(t, 180, used)
	require.Zero(t, rows)
	require.EqualValues(t, 20, failed)
	require.EqualValues(t, 80, healthy)
	require.EqualValues(t, 103, unchanged)
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.Zero(t, summary.ActiveRequests)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.UsageUnknown)
	require.Zero(t, summary.DayTokens)
	require.Zero(t, unexpected.Load())
	require.LessOrEqual(t, maximum.Load(), int64(4))
	require.Zero(t, active.Load())
	t.Log("100 accounts covered once over five actual UTC minute slots; 20 supplier failures, 80 passes, 180 physical calls, no duplicate process batches")
}
