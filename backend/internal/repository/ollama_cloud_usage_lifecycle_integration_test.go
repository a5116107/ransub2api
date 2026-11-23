//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	redisclient "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestOllamaCloudUsageIndependentProcessesHundredGroupsAndRecovery(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	options := *integrationRedis.Options()
	options.DB = 15
	rdb := redisclient.NewClient(&options)
	t.Cleanup(func() { _ = rdb.Close() })
	size, err := rdb.DBSize(ctx).Result()
	require.NoError(t, err)
	require.Zero(t, size, "Ollama process fixture requires an unused DB in its disposable Redis")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		keys, err := rdb.Keys(cleanup, leaderLockKeyPrefix+"ollama:cloud:usage:leader").Result()
		require.NoError(t, err)
		if len(keys) > 0 {
			require.NoError(t, rdb.Del(cleanup, keys...).Err())
		}
		size, err := rdb.DBSize(cleanup).Result()
		require.NoError(t, err)
		require.Zero(t, size)
	})
	body, err := os.ReadFile("../service/testdata/ollama_settings_usage.html")
	require.NoError(t, err)
	var calls [104]atomic.Int64
	var active, maximum, invalid atomic.Int64
	entered, gate := make(chan struct{}, 4), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("wos-session")
		if err != nil || r.Method != http.MethodGet || r.URL.Path != "/settings" || r.Host != "ollama.com" || r.Header.Get("Authorization") != "" {
			invalid.Add(1)
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		index, err := strconv.Atoi(strings.TrimPrefix(cookie.Value, "multinode-"))
		if err != nil || index < 0 || index >= len(calls) {
			invalid.Add(1)
			http.Error(w, "unexpected fixture session", http.StatusBadRequest)
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
		if index < 20 {
			w.Header().Set("Retry-After", "600")
			http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(release)
	settings := NewSettingRepository(client)
	require.NoError(t, settings.Set(ctx, service.SettingKeyOllamaCloudUsageSettings, `{"enabled":true,"interval_minutes":60,"debounce_minutes":1}`))
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 8
	policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency = 4, 4, 1
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	old, lastUsed := time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-2*time.Minute)
	controls := make(map[int64]string)
	for i := range calls {
		platforms := []string{service.PlatformOpenAI, service.PlatformAnthropic}
		if i >= 100 {
			platforms = platforms[:1]
		}
		for _, platform := range platforms {
			kind, status, enabled, baseURL := service.AccountTypeAPIKey, service.StatusActive, i != 100, "https://ollama.com"
			if i == 101 {
				status = "disabled"
			}
			if i == 102 {
				kind = service.AccountTypeOAuth
			}
			if i == 103 {
				baseURL = server.URL
			}
			extra, err := json.Marshal(map[string]any{service.OllamaCloudUsageSessionExtraKey: fmt.Sprintf("wos-session=multinode-%d", i),
				service.OllamaCloudUsageAutoRefreshExtraKey: enabled,
				service.OllamaCloudUsageSnapshotExtraKey: &service.OllamaCloudUsageSnapshot{Status: service.OllamaCloudUsageStatusOK,
					Data: &service.OllamaCloudUsageData{Plan: "previous"}, FetchedAt: &old, LastAttemptAt: old, NextRefreshAt: old}})
			require.NoError(t, err)
			credentials, err := json.Marshal(map[string]string{"api_key": fmt.Sprintf("fixture-ollama-%d", i), "base_url": baseURL})
			require.NoError(t, err)
			var id int64
			require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra,last_used_at,rate_multiplier)
 VALUES($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,1) RETURNING id`, fmt.Sprintf("ollama-process-%03d-%s", i, platform), platform, kind, status,
				string(credentials), string(extra), lastUsed).Scan(&id))
			if i >= 100 {
				var row string
				require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&row))
				controls[id] = row
			}
		}
	}
	command := monitoringBudgetNodeCommand{Mode: "ollama", DSN: monitoringBudgetNodeDSN(t, db), RedisAddress: options.Addr,
		RedisDB: options.DB, Site: server.URL, Certificate: server.Certificate().Raw, Requests: 1}
	replies := make(chan monitoringBudgetNodeReply, 2)
	leader := startMonitoringBudgetNode(t, command, replies)
	for range 4 {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("independent Ollama leader did not reach its four physical requests")
		}
	}
	peer := startMonitoringBudgetNode(t, command, replies)
	require.NotEqual(t, leader.pid, peer.pid)
	require.NotEqual(t, os.Getpid(), leader.pid)
	peerReply := receiveMonitoringBudgetReply(t, replies)
	require.Equal(t, http.StatusOK, peerReply.Status)
	require.Equal(t, "not_leader", peerReply.Reason)
	peer.wait()
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 4, summary.DayUsed)
	require.EqualValues(t, 4, summary.ActiveRequests)
	release()
	leaderReply := receiveMonitoringBudgetReply(t, replies)
	require.Equal(t, "observed", leaderReply.Reason)
	leader.wait()
	summary, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 8, summary.DayUsed)
	require.Zero(t, summary.ActiveRequests)
	readSnapshots := func() string {
		t.Helper()
		var rows string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_agg(row_to_json(a) ORDER BY id)::text FROM accounts a`).Scan(&rows))
		return rows
	}
	beforeExhausted := readSnapshots()
	runNext := func() {
		t.Helper()
		node := startMonitoringBudgetNode(t, command, replies)
		reply := receiveMonitoringBudgetReply(t, replies)
		require.Equal(t, http.StatusOK, reply.Status)
		require.Equal(t, "observed", reply.Reason)
		node.wait()
	}
	runNext()
	require.Equal(t, beforeExhausted, readSnapshots(), "budget deferral must not renew or fail an uncalled group")
	summary, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 8, summary.DayUsed)
	policy.RequestsPerDay = 100
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	// These are the original bounded automatic batches, not simulated timer
	// cycles: every new process must re-read SQL and the real shared leader lock.
	for range 5 {
		runNext()
	}
	for i := range calls {
		want := int64(1)
		if i >= 100 {
			want = 0
		}
		require.Equal(t, want, calls[i].Load(), "each credential group sends once despite cross-platform sibling accounts")
	}
	beforeIdle := readSnapshots()
	runNext()
	require.Equal(t, beforeIdle, readSnapshots(), "process restart must not repeat fresh or backed-off work")
	for id, before := range controls {
		var after string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&after))
		require.Equal(t, before, after)
	}
	var fresh, failures, usage, rates int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM accounts WHERE (extra#>>'{ollama_cloud_usage_snapshot,last_attempt_at}')::timestamptz>$1),
 (SELECT count(*) FROM accounts WHERE extra#>>'{ollama_cloud_usage_snapshot,status}'='failed'),
 (SELECT count(*) FROM usage_logs), (SELECT count(*) FROM accounts WHERE rate_multiplier<>1)`, old).Scan(&fresh, &failures, &usage, &rates))
	require.EqualValues(t, 200, fresh)
	require.EqualValues(t, 40, failures)
	require.Zero(t, usage)
	require.Zero(t, rates)
	summary, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 100, summary.DayUsed)
	require.Zero(t, summary.ActiveRequests)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.UsageUnknown)
	require.Zero(t, summary.UsageReported)
	require.Zero(t, summary.DayTokens)
	require.EqualValues(t, 4, maximum.Load())
	require.Zero(t, active.Load())
	require.Zero(t, invalid.Load())
	size, err = rdb.DBSize(ctx).Result()
	require.NoError(t, err)
	require.Zero(t, size, "exited nodes must release the real leader lock")
}

func TestOllamaCloudUsageLifecycleHTTPPostgresCancelAndRecovery(t *testing.T) {
	for _, entry := range []string{"background", "external_due"} {
		t.Run(entry, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			ctx := t.Context()
			body, err := os.ReadFile("../service/testdata/ollama_settings_usage.html")
			require.NoError(t, err)
			var calls, canceled, invalid atomic.Int64
			var block atomic.Bool
			block.Store(true)
			entered := make(chan struct{}, 4)
			upstream := monitoringBudgetTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/settings" || r.Host != "ollama.com" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") == "" {
					invalid.Add(1)
					http.Error(w, "unexpected settings request", http.StatusBadRequest)
					return
				}
				calls.Add(1)
				if block.Load() {
					entered <- struct{}{}
					<-r.Context().Done()
					canceled.Add(1)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write(body)
			})
			settingsRepo := NewSettingRepository(client)
			require.NoError(t, settingsRepo.Set(ctx, service.SettingKeyOllamaCloudUsageSettings, `{"enabled":true,"interval_minutes":60,"debounce_minutes":1}`))
			budget := NewMonitoringProbeBudgetRepository(db)
			_, err = budget.SaveSettings(ctx, monitoringBudgetPolicy())
			require.NoError(t, err)
			old, lastUsed := time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-2*time.Minute)
			rows := make(map[int64]string)
			var first int64
			for i := range 20 {
				account := mustCreateAccount(t, client, &service.Account{Name: fmt.Sprintf("ollama-lifecycle-%d", i), Platform: service.PlatformOpenAI,
					Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 2, LastUsedAt: &lastUsed,
					Credentials: map[string]any{"api_key": fmt.Sprintf("fixture-inference-%d", i), "base_url": "https://ollama.com"},
					Extra: map[string]any{service.OllamaCloudUsageSessionExtraKey: fmt.Sprintf("wos-session=lifecycle-%d", i),
						service.OllamaCloudUsageAutoRefreshExtraKey: true,
						service.OllamaCloudUsageSnapshotExtraKey: &service.OllamaCloudUsageSnapshot{Status: service.OllamaCloudUsageStatusOK,
							Data: &service.OllamaCloudUsageData{Plan: "previous"}, FetchedAt: &old, LastAttemptAt: old, NextRefreshAt: old}}})
				var row string
				require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, account.ID).Scan(&row))
				rows[account.ID] = row
				if first == 0 {
					first = account.ID
				}
			}
			newRunner := func() *service.OllamaCloudUsageService {
				accounts := newAccountRepositoryWithSQL(client, db, nil)
				runner := service.NewOllamaCloudUsageService(accounts, upstream, service.NewSettingService(settingsRepo, nil), ollamaBudgetFixtureEncryptor{}, true)
				runner.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)))
				t.Cleanup(runner.Stop)
				return runner
			}
			runner := newRunner()
			done := make(chan error, 1)
			if entry == "background" {
				runner.Start()
			} else {
				go func() { done <- runner.RunDue(ctx) }()
			}
			for range 4 {
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatal("original Ollama runner did not enter four physical requests")
				}
			}
			stopped := make(chan struct{})
			go func() { runner.Stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("Ollama automatic work did not cancel and drain")
			}
			if entry == "external_due" {
				require.ErrorIs(t, <-done, context.Canceled)
			}
			require.Eventually(t, func() bool { return canceled.Load() == 4 }, 5*time.Second, 5*time.Millisecond)
			require.EqualValues(t, 4, calls.Load(), "retired queued work must not send")
			require.ErrorIs(t, runner.RunDue(ctx), context.Canceled)
			for id, before := range rows {
				var after string
				require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&after))
				require.Equal(t, before, after, "cancellation must preserve prior activity, evidence and session")
			}
			summary, err := budget.Summary(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 4, summary.DayUsed, "already sent calls must not be refunded")
			require.Zero(t, summary.ActiveRequests)
			require.Zero(t, summary.UsagePending)
			block.Store(false)
			state, err := runner.Refresh(ctx, first)
			require.NoError(t, err, "manual refresh retains its caller lifetime after automatic shutdown")
			require.Equal(t, service.OllamaCloudUsageStatusOK, state.Snapshot.Status)
			require.NoError(t, newRunner().RunDue(ctx))
			require.EqualValues(t, 24, calls.Load(), "only the remaining nineteen due accounts recover automatically")
			var fresh, failed, usage int64
			require.NoError(t, db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM accounts WHERE (extra#>>'{ollama_cloud_usage_snapshot,last_attempt_at}')::timestamptz>$1),
 (SELECT count(*) FROM accounts WHERE extra#>>'{ollama_cloud_usage_snapshot,status}'<>'ok'),
 (SELECT count(*) FROM usage_logs)`, old).Scan(&fresh, &failed, &usage))
			require.EqualValues(t, 20, fresh)
			require.Zero(t, failed)
			require.Zero(t, usage)
			summary, err = budget.Summary(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 24, summary.DayUsed)
			require.Zero(t, summary.DayTokens)
			require.Zero(t, summary.UsageReported)
			require.Zero(t, summary.UsageUnknown, "management-only cancellation must not create generation usage")
			require.Zero(t, summary.UsagePending)
			require.Zero(t, summary.ActiveRequests)
			require.Zero(t, invalid.Load())
		})
	}
}

func TestOllamaCloudUsageIndependentProcessesLeaderLossAndRecovery(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	options := *integrationRedis.Options()
	options.DB = 14
	rdb := redisclient.NewClient(&options)
	t.Cleanup(func() { _ = rdb.Close() })
	size, err := rdb.DBSize(ctx).Result()
	require.NoError(t, err)
	require.Zero(t, size, "lease fixture must own an empty DB in the isolated Redis")
	const key = leaderLockKeyPrefix + "ollama:cloud:usage:leader"
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, rdb.Del(cleanup, key).Err())
		size, err := rdb.DBSize(cleanup).Result()
		require.NoError(t, err)
		require.Zero(t, size)
	})
	body, err := os.ReadFile("../service/testdata/ollama_settings_usage.html")
	require.NoError(t, err)
	var calls, canceled, invalid atomic.Int64
	var block atomic.Bool
	block.Store(true)
	entered := make(chan struct{}, 4)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("wos-session")
		if err != nil || !strings.HasPrefix(cookie.Value, "lease-fixture-") || r.Host != "ollama.com" || r.URL.Path != "/settings" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
			invalid.Add(1)
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		if block.Load() {
			entered <- struct{}{}
			<-r.Context().Done()
			canceled.Add(1)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	require.NoError(t, NewSettingRepository(client).Set(ctx, service.SettingKeyOllamaCloudUsageSettings, `{"enabled":true,"interval_minutes":60,"debounce_minutes":1}`))
	budget := NewMonitoringProbeBudgetRepository(db)
	_, err = budget.SaveSettings(ctx, monitoringBudgetPolicy())
	require.NoError(t, err)
	old, lastUsed := time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-2*time.Minute)
	for i := range 20 {
		for _, platform := range []string{service.PlatformOpenAI, service.PlatformAnthropic} {
			mustCreateAccount(t, client, &service.Account{Name: fmt.Sprintf("lease-%d-%s", i, platform), Platform: platform,
				Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 2, LastUsedAt: &lastUsed,
				Credentials: map[string]any{"api_key": fmt.Sprintf("fixture-lease-%d", i), "base_url": "https://ollama.com"},
				Extra: map[string]any{service.OllamaCloudUsageSessionExtraKey: fmt.Sprintf("wos-session=lease-fixture-%d", i),
					service.OllamaCloudUsageAutoRefreshExtraKey: true,
					service.OllamaCloudUsageSnapshotExtraKey: &service.OllamaCloudUsageSnapshot{Status: service.OllamaCloudUsageStatusOK,
						Data: &service.OllamaCloudUsageData{Plan: "previous"}, FetchedAt: &old, LastAttemptAt: old, NextRefreshAt: old}}})
		}
	}
	readRows := func() string {
		t.Helper()
		var rows string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_agg(row_to_json(a) ORDER BY id)::text FROM accounts a`).Scan(&rows))
		return rows
	}
	before := sha256.Sum256([]byte(readRows()))
	// A live Postgres connection is not a second coordination authority when
	// this node's configured Redis client fails but a peer still owns Redis.
	require.NoError(t, rdb.Set(ctx, key, "fixture-existing-authority", time.Minute).Err())
	closedRedis := redisclient.NewClient(&options)
	require.NoError(t, closedRedis.Close())
	cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}}
	cfg.Totp.EncryptionKeyConfigured = true
	failedNode := service.ProvideOllamaCloudUsageService(newAccountRepositoryWithSQL(client, db, nil),
		monitoringBudgetPinnedUpstream(t, server.Listener.Addr().String(), server.Certificate().Raw),
		service.NewSettingService(NewSettingRepository(client), cfg), ollamaBudgetFixtureEncryptor{}, cfg,
		NewLeaderLockCache(closedRedis), db, budget)
	t.Cleanup(failedNode.Stop)
	require.Error(t, failedNode.RunDue(ctx))
	require.Equal(t, "coordination_unavailable", failedNode.MonitoringRuntime().LastScan.State)
	require.Nil(t, failedNode.MonitoringRuntime().LastScan.Candidates)
	require.Zero(t, calls.Load(), "Redis failure must not acquire an independent Postgres leader lock")
	require.Equal(t, before, sha256.Sum256([]byte(readRows())))
	require.NoError(t, NewLeaderLockCache(rdb).ReleaseLeaderLock(ctx, "ollama:cloud:usage:leader", "fixture-existing-authority"))
	failedNode.Stop()
	command := monitoringBudgetNodeCommand{Mode: "ollama_lease", DSN: monitoringBudgetNodeDSN(t, db), RedisAddress: options.Addr,
		RedisDB: options.DB, Site: server.URL, Certificate: server.Certificate().Raw, Requests: 1}
	replies := make(chan monitoringBudgetNodeReply, 3)
	leader := startMonitoringBudgetNode(t, command, replies)
	for range 4 {
		select {
		case <-entered:
		case <-time.After(45 * time.Second):
			t.Fatal("native Ollama wave did not reach the actual HTTP transport before first renewal")
		}
	}
	firstOwner, err := rdb.Get(ctx, key).Result()
	require.NoError(t, err)
	require.NotEmpty(t, firstOwner)
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 4, summary.DayUsed)
	require.EqualValues(t, 4, summary.ActiveRequests)
	// Simulate an acknowledged successor, not a timer shortcut or a second
	// budget. The original runner must notice it on its real forty-second tick.
	require.NoError(t, rdb.Set(ctx, key, "fixture-successor", 2*time.Minute).Err())
	leader.wait()
	require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusServiceUnavailable, Reason: "coordination_unavailable"}, receiveMonitoringBudgetReply(t, replies))
	require.Eventually(t, func() bool { return canceled.Load() == 4 }, 5*time.Second, 10*time.Millisecond)
	require.EqualValues(t, 4, calls.Load(), "queued credential groups must not dispatch after the lease is lost")
	require.Equal(t, before, sha256.Sum256([]byte(readRows())), "loss must not refresh, fail, disable or erase the old snapshots")
	owner, err := rdb.Get(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, "fixture-successor", owner, "the stale process must not release the successor")
	command.Mode = "ollama"
	peer := startMonitoringBudgetNode(t, command, replies)
	require.NotEqual(t, leader.pid, peer.pid)
	peer.wait()
	require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK, Reason: "not_leader"}, receiveMonitoringBudgetReply(t, replies))
	require.EqualValues(t, 4, calls.Load())
	owner, err = rdb.Get(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, "fixture-successor", owner)
	require.NoError(t, NewLeaderLockCache(rdb).ReleaseLeaderLock(ctx, "ollama:cloud:usage:leader", "fixture-successor"))
	block.Store(false)
	recovery := startMonitoringBudgetNode(t, command, replies)
	require.NotEqual(t, leader.pid, recovery.pid)
	recovery.wait()
	require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK, Reason: "observed"}, receiveMonitoringBudgetReply(t, replies))
	require.EqualValues(t, 24, calls.Load(), "only the twenty original credential groups recover; siblings share each request")
	var fresh, failures, usage int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM accounts WHERE (extra#>>'{ollama_cloud_usage_snapshot,last_attempt_at}')::timestamptz>$1),
 (SELECT count(*) FROM accounts WHERE extra#>>'{ollama_cloud_usage_snapshot,status}'<>'ok'),
 (SELECT count(*) FROM usage_logs)`, old).Scan(&fresh, &failures, &usage))
	require.EqualValues(t, 40, fresh)
	require.Zero(t, failures)
	require.Zero(t, usage)
	summary, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 24, summary.DayUsed)
	require.Zero(t, summary.DayTokens)
	require.Zero(t, summary.UsageReported)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.UsageUnknown)
	require.Zero(t, summary.ActiveRequests)
	require.Zero(t, invalid.Load())
	exists, err := rdb.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
	t.Log("three independent processes / real 40-second renewal: canceled=4, recovered_groups=20, updated_accounts=40, physical_calls=24; active/pending/unknown/tokens/downstream_usage=0")
}
