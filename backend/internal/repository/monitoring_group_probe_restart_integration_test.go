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
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	redisclient "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestGroupMonitoringTurnsIndependentProcessesBudgetRotation(t *testing.T) {
	for i, lane := range []string{"v2", "v3"} {
		t.Run(lane, func(t *testing.T) {
			t.Parallel()
			const targets = 12
			db, client := newBillingAuditDatabase(t)
			ctx := t.Context()
			options := *integrationRedis.Options()
			options.DB = 12 + i
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
			seen := map[string]int{}
			challenge := regexp.MustCompile(`(\d+) ([+-]) (\d+) = \?`)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/tenant/v1/responses" || r.Header.Get("Authorization") != "Bearer shared-turn-fixture" {
					http.Error(w, "invalid fixture route", 400)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return
				}
				var input struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(body, &input) != nil || input.Model == "" {
					http.Error(w, "missing fixture model", 400)
					return
				}
				mu.Lock()
				seen[input.Model]++
				mu.Unlock()
				answer := "21"
				if lane == "v3" {
					matches := challenge.FindAllSubmatch(body, -1)
					if len(matches) == 0 {
						http.Error(w, "missing arithmetic", 400)
						return
					}
					m := matches[len(matches)-1]
					a, _ := strconv.Atoi(string(m[1]))
					b, _ := strconv.Atoi(string(m[3]))
					if string(m[2]) == "-" {
						b = -b
					}
					answer = strconv.Itoa(a + b)
				}
				text, _ := json.Marshal(answer)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%s}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":3,\"total_tokens\":7}}}\n\n", text)
			}))
			t.Cleanup(server.Close)
			accounts := NewAccountRepository(client, db, nil)
			a := &service.Account{Name: "shared-age-account", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 8,
				Credentials: map[string]any{"api_key": "shared-turn-fixture", "base_url": server.URL + "/tenant/v1"}}
			require.NoError(t, accounts.Create(ctx, a))
			g := &service.Group{Name: "shared-age-group", Platform: service.PlatformOpenAI, Status: service.StatusActive, RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard}
			require.NoError(t, NewGroupRepository(client, db).Create(ctx, g))
			_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, a.ID, g.ID)
			require.NoError(t, err)
			var accountBefore string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, a.ID).Scan(&accountBefore))
			require.NoError(t, NewSettingRepository(client).SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: lane}))
			if lane == "v2" {
				repo := NewChannelMonitorV2Repository(db)
				cfg, err := repo.GetConfig(ctx)
				require.NoError(t, err)
				cfg.Enabled = true
				cfg.GroupIDs = []int64{g.ID}
				cfg.CandyProbes = nil
				for i := range cfg.Platforms {
					cfg.Platforms[i].Enabled = cfg.Platforms[i].Platform == service.PlatformOpenAI
				}
				for n := range targets {
					cfg.CandyProbes = append(cfg.CandyProbes, service.ChannelMonitorV2CandyProbe{GroupID: g.ID, Model: fmt.Sprintf("rotation-%02d", n), Enabled: true, IntervalMinutes: 1})
				}
				_, err = repo.UpdateConfig(ctx, *cfg, cfg.Version)
				require.NoError(t, err)
			} else {
				repo := NewChannelMonitorV3Repository(db)
				cfg, err := repo.GetConfig(ctx)
				require.NoError(t, err)
				cfg.IntervalMinutes = 5
				cfg.MinRequests = 1
				_, err = repo.UpdateConfig(ctx, *cfg, cfg.Version, 0)
				require.NoError(t, err)
				require.NoError(t, repo.UpdateIdleProbe(ctx, service.ChannelMonitorV3IdleProbe{Enabled: true, Groups: []service.ChannelMonitorV3IdleProbeGroup{{GroupID: g.ID}}}))
				for n := range targets {
					_, err = repo.CreateComponent(ctx, service.ChannelMonitorV3ComponentInput{Name: fmt.Sprintf("rotation-%02d", n), Model: fmt.Sprintf("rotation-%02d", n), GroupID: g.ID, Enabled: true, Visibility: "public"})
					require.NoError(t, err)
				}
			}
			budget := NewMonitoringProbeBudgetRepository(db)
			policy := monitoringBudgetPolicy()
			policy.RecoveryReservePercent = 0
			policy.Concurrency = 8
			policy.SiteConcurrency = 8
			policy.KeyConcurrency = 8
			command := monitoringBudgetNodeCommand{Mode: lane, DSN: monitoringBudgetNodeDSN(t, db), RedisAddress: options.Addr, RedisDB: options.DB, Requests: 1, ObserveQueue: true,
				Tick: time.Now().UTC().Truncate(5 * time.Minute).Add(-30 * time.Second)}
			run := func() monitoringBudgetNodeReply {
				t.Helper()
				replies := make(chan monitoringBudgetNodeReply, 1)
				node := startMonitoringBudgetNode(t, command, replies)
				node.wait()
				return receiveMonitoringBudgetReply(t, replies)
			}
			// Model a busy existing day without relaxing the coverage/save validator.
			// Only the private fixture counter is seeded; HTTP observations start at zero.
			const previouslySpent = 20000
			_, err = db.ExecContext(ctx, `INSERT INTO monitoring_probe_budget_counters(dimension,scope_hash,bucket_start,used,regular_used) VALUES('global_day','',date_trunc('day',clock_timestamp() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',$1,$1)`, previouslySpent)
			require.NoError(t, err)
			for round := range 3 {
				policy.RequestsPerDay = previouslySpent + int64((round+1)*4)
				policy.RequestsPerMinute = 100
				policy, err = budget.SaveSettings(ctx, policy)
				require.NoError(t, err)
				if lane == "v3" && round == 1 {
					_, err = db.ExecContext(ctx, `ALTER TABLE monitoring_group_probe_turns RENAME TO hidden_group_turns`)
					require.NoError(t, err)
					reply := run()
					require.Equal(t, "coordination_unavailable", reply.State)
					mu.Lock()
					sum := 0
					for _, n := range seen {
						sum += n
					}
					mu.Unlock()
					require.Equal(t, 4, sum, "usable budget must not bypass missing shared state")
					_, err = db.ExecContext(ctx, `ALTER TABLE hidden_group_turns RENAME TO monitoring_group_probe_turns`)
					require.NoError(t, err)
				}
				reply := run()
				require.Equal(t, http.StatusOK, reply.Status)
				require.NotEqual(t, "coordination_unavailable", reply.State)
				mu.Lock()
				unique := len(seen)
				for _, n := range seen {
					require.Equal(t, 1, n, "new processes must not spend increased budget on a previously served scope")
				}
				mu.Unlock()
				require.Equal(t, (round+1)*4, unique)
				summary, err := budget.Summary(ctx)
				require.NoError(t, err)
				require.EqualValues(t, previouslySpent+(round+1)*4, summary.DayUsed)
				require.Zero(t, summary.ActiveRequests)
				command.Tick = command.Tick.Add(5 * time.Minute)
			}
			var accountAfter string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, a.ID).Scan(&accountAfter))
			require.Equal(t, accountBefore, accountAfter)
			var usage int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&usage))
			require.Zero(t, usage)
		})
	}
}
