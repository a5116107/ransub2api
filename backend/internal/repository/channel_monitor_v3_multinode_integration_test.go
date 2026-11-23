//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
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

func TestChannelMonitorV3IndependentProcessesClaimsAndRecovery(t *testing.T) {
	for _, scenario := range []struct {
		name            string
		failures        int
		independentKeys bool
	}{{"shared_key", 0, false}, {"independent_keys", 20, true}} {
		t.Run(scenario.name, func(t *testing.T) {
			testChannelMonitorV3IndependentProcesses(t, scenario.failures, scenario.independentKeys)
		})
	}
}

func testChannelMonitorV3IndependentProcesses(t *testing.T, failures int, independentKeys bool) {
	t.Helper()
	const targets = 100
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	options := *integrationRedis.Options()
	options.DB = 14
	rdb := redisclient.NewClient(&options)
	t.Cleanup(func() { _ = rdb.Close() })
	size, err := rdb.DBSize(ctx).Result()
	require.NoError(t, err)
	require.Zero(t, size, "V3 process fixture requires an unused DB in its disposable Redis")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, rdb.FlushDB(cleanup).Err())
		size, err := rdb.DBSize(cleanup).Result()
		require.NoError(t, err)
		require.Zero(t, size)
	})
	var mu sync.Mutex
	scopes := make(map[string]int)
	var sends, active, maximum, invalid atomic.Int64
	var failing atomic.Bool
	failing.Store(true)
	entered := make(chan struct{}, 8)
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	challenge := regexp.MustCompile(`(\d+) ([+-]) (\d+) = \?`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tenant/v1/responses" || r.Method != http.MethodPost {
			invalid.Add(1)
			http.Error(w, "invalid independent V3 fixture request", http.StatusBadRequest)
			return
		}
		owner := 0
		if independentKeys {
			suffix := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer fixture-v3-nodes-")
			var err error
			owner, err = strconv.Atoi(suffix)
			if err != nil || owner < 0 || owner >= targets {
				invalid.Add(1)
				http.Error(w, "unknown fixture credential", http.StatusBadRequest)
				return
			}
		} else if r.Header.Get("Authorization") != "Bearer fixture-v3-nodes" {
			invalid.Add(1)
			http.Error(w, "unexpected fixture credential", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			invalid.Add(1)
			return
		}
		var payload struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &payload) != nil || payload.Model == "" {
			invalid.Add(1)
			http.Error(w, "missing fixture model", http.StatusBadRequest)
			return
		}
		mu.Lock()
		scopes[fmt.Sprintf("%d/%s", owner, payload.Model)]++
		mu.Unlock()
		sends.Add(1)
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
		index, err := strconv.Atoi(strings.TrimPrefix(payload.Model, "model-"))
		if err != nil || index < 0 || index >= targets {
			invalid.Add(1)
			http.Error(w, "unknown fixture model", http.StatusBadRequest)
			return
		}
		if independentKeys {
			index = owner
		}
		if failing.Load() && index < failures {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"fixture model temporarily unavailable"}}`)
			return
		}
		questions := challenge.FindAllSubmatch(body, -1)
		if len(questions) == 0 {
			invalid.Add(1)
			http.Error(w, "missing current arithmetic challenge", http.StatusBadRequest)
			return
		}
		// The first two expressions are few-shot examples, not this probe.
		match := questions[len(questions)-1]
		a, _ := strconv.Atoi(string(match[1]))
		b, _ := strconv.Atoi(string(match[3]))
		if string(match[2]) == "-" {
			b = -b
		}
		answer := strconv.Itoa(a + b)
		text, _ := json.Marshal(answer)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%s}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":3,\"total_tokens\":7}}}\n\n", text)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(release)
	cfg := &config.Config{Timezone: "UTC", Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	cfg.Default.RateMultiplier = 1
	settings := NewSettingRepository(client)
	require.NoError(t, settings.SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v3"}))
	accounts := NewAccountRepository(client, db, nil)
	groupRepo := NewGroupRepository(client, db)
	accountCount := 1
	if independentKeys {
		accountCount = targets
	}
	before := make(map[int64]string, accountCount)
	groupIDs := make([]int64, 0, accountCount)
	for i := range accountCount {
		key := "fixture-v3-nodes"
		if independentKeys {
			key = fmt.Sprintf("fixture-v3-nodes-%d", i)
		}
		account := &service.Account{Name: fmt.Sprintf("v3-node-account-%d", i), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 20,
			Credentials: map[string]any{"api_key": key, "base_url": server.URL + "/tenant/v1"}}
		require.NoError(t, accounts.Create(ctx, account))
		group := &service.Group{Name: fmt.Sprintf("v3-node-group-%d", i), Platform: service.PlatformOpenAI, Status: service.StatusActive,
			SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 1}
		require.NoError(t, groupRepo.Create(ctx, group))
		_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account.ID, group.ID)
		require.NoError(t, err)
		var row string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, account.ID).Scan(&row))
		before[account.ID] = row
		groupIDs = append(groupIDs, group.ID)
	}
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay, policy.RequestsPerMinute = 100000, 10000
	policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency = 20, 20, 20
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	v3 := NewChannelMonitorV3Repository(db)
	current, err := v3.GetConfig(ctx)
	require.NoError(t, err)
	current.IntervalMinutes, current.MinRequests = 5, 1
	_, err = v3.UpdateConfig(ctx, *current, current.Version, 0)
	require.NoError(t, err)
	idle := service.ChannelMonitorV3IdleProbe{Enabled: true}
	for _, id := range groupIDs {
		idle.Groups = append(idle.Groups, service.ChannelMonitorV3IdleProbeGroup{GroupID: id})
	}
	require.NoError(t, v3.UpdateIdleProbe(ctx, idle))
	for i := range targets {
		groupID, model := groupIDs[0], fmt.Sprintf("model-%03d", i)
		if independentKeys {
			groupID, model = groupIDs[i], "model-000"
		}
		_, err = v3.CreateComponent(ctx, service.ChannelMonitorV3ComponentInput{Name: fmt.Sprintf("scope-%03d", i), GroupID: groupID,
			Model: model, Enabled: true, Visibility: "public"})
		require.NoError(t, err)
	}
	command := monitoringBudgetNodeCommand{Mode: "v3", DSN: monitoringBudgetNodeDSN(t, db),
		RedisAddress: options.Addr, RedisDB: options.DB, Requests: 1,
		Tick: time.Now().UTC().Truncate(5 * time.Minute).Add(-30 * time.Second)}
	replies := make(chan monitoringBudgetNodeReply, 2)
	left := startMonitoringBudgetNode(t, command, replies)
	right := startMonitoringBudgetNode(t, command, replies)
	require.NotEqual(t, left.pid, right.pid)
	for range 8 {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("independent V3 nodes did not share eight unique in-flight claims")
		}
	}
	require.EqualValues(t, 8, sends.Load())
	var claims, distinct int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*),count(DISTINCT (group_id,model)) FROM channel_monitor_v3_probe_results`).Scan(&claims, &distinct))
	require.EqualValues(t, 8, claims)
	require.EqualValues(t, 8, distinct)
	release()
	for range 2 {
		require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
	}
	left.wait()
	right.wait()
	assertRound := func(rounds int64) {
		t.Helper()
		var total, successes, rejected, unfinished, usage, remaining int64
		require.NoError(t, db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM channel_monitor_v3_probe_results),
 (SELECT count(*) FROM channel_monitor_v3_probe_results WHERE outcome='success'),
 (SELECT count(*) FROM channel_monitor_v3_probe_results WHERE outcome='failure'),
 (SELECT count(*) FROM channel_monitor_v3_probe_results WHERE outcome='running'),
 (SELECT count(*) FROM usage_logs),
 (SELECT count(*) FROM channel_monitor_v3_probe_results WHERE budget_deferral IS NOT NULL)`).Scan(&total, &successes, &rejected, &unfinished, &usage, &remaining))
		require.EqualValues(t, targets*rounds, total)
		require.EqualValues(t, targets*rounds-int64(failures), successes)
		require.EqualValues(t, failures, rejected)
		require.Zero(t, unfinished)
		require.Zero(t, usage)
		require.Zero(t, remaining)
		require.EqualValues(t, targets*rounds, sends.Load())
		mu.Lock()
		observed := make(map[string]int, len(scopes))
		for scope, count := range scopes {
			observed[scope] = count
		}
		mu.Unlock()
		require.Len(t, observed, targets)
		for scope, count := range observed {
			require.EqualValues(t, rounds, count, "scope %s must have one physical request per slot", scope)
		}
		summary, err := budget.Summary(ctx)
		require.NoError(t, err)
		require.EqualValues(t, targets*rounds, summary.DayUsed)
		require.EqualValues(t, (targets*rounds-int64(failures))*7, summary.DayTokens)
		require.EqualValues(t, targets*rounds-int64(failures), summary.UsageReported)
		require.EqualValues(t, failures, summary.UsageUnknown, "supplier errors without usage must not be counted as free")
		require.Zero(t, summary.UsagePending)
		require.Zero(t, summary.ActiveRequests)
		for id, original := range before {
			var after string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&after))
			require.Equal(t, original, after, "synthetic detection must not rewrite supply account %d", id)
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
	command.Tick = command.Tick.Add(5 * time.Minute)
	left = startMonitoringBudgetNode(t, command, replies)
	right = startMonitoringBudgetNode(t, command, replies)
	for range 2 {
		require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
	}
	left.wait()
	right.wait()
	assertRound(2)
	require.LessOrEqual(t, maximum.Load(), int64(8))
	t.Logf("two independent V3 nodes: %d fixture keys, 100 scopes per slot, %d supplier failures did not starve peers, rebuilt process sends zero duplicates, next slot recovers; 200 calls/%d reported tokens/%d unknown usages", accountCount, failures, (200-failures)*7, failures)
}
