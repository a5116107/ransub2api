//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func groupTurnsFixture(t *testing.T, db *sql.DB, lane string) (service.GroupMonitoringTurnRepository, service.GroupMonitoringQueueConfig) {
	t.Helper()
	if lane == "v2" {
		repo := NewChannelMonitorV2Repository(db)
		cfg, err := repo.GetConfig(t.Context())
		require.NoError(t, err)
		cfg.Enabled = true
		cfg, err = repo.UpdateConfig(t.Context(), *cfg, cfg.Version)
		require.NoError(t, err)
		return repo.(service.GroupMonitoringTurnRepository), service.GroupMonitoringQueueConfig{Version: cfg.Version}
	}
	repo := NewChannelMonitorV3Repository(db)
	cfg, err := repo.GetConfig(t.Context())
	require.NoError(t, err)
	cfg.IdleProbe = service.ChannelMonitorV3IdleProbe{Enabled: true, Groups: []service.ChannelMonitorV3IdleProbeGroup{}}
	require.NoError(t, repo.UpdateIdleProbe(t.Context(), cfg.IdleProbe))
	return repo.(service.GroupMonitoringTurnRepository), service.GroupMonitoringQueueConfig{Version: cfg.Version, IdleProbe: &cfg.IdleProbe}
}

func groupTurn(t *testing.T, queue *service.GroupMonitoringQueue, id int64) service.GroupMonitoringTurn {
	t.Helper()
	for _, turn := range queue.Turns {
		if turn.GroupID == id {
			return turn
		}
	}
	t.Fatal("missing shared scheduling turn")
	return service.GroupMonitoringTurn{}
}

func TestGroupMonitoringTurnsPersistenceCASAndInventory(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	a := mustCreateGroup(t, client, &service.Group{Name: "shared-turn-a", Platform: service.PlatformOpenAI, Status: service.StatusActive})
	b := mustCreateGroup(t, client, &service.Group{Name: "shared-turn-b", Platform: service.PlatformOpenAI, Status: service.StatusActive})
	hash := func(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
	for _, lane := range []string{"v2", "v3"} {
		t.Run(lane, func(t *testing.T) {
			repo, cfg := groupTurnsFixture(t, db, lane)
			input := []service.GroupMonitoringTurn{{GroupID: a.ID, ScopeHash: hash("scope"), IdentityHash: hash("model/effort/period"), Due: true},
				{GroupID: b.ID, ScopeHash: hash("scope"), IdentityHash: hash("model/effort/period"), Due: true}}
			queue, err := repo.SyncGroupMonitoringTurns(ctx, cfg, input)
			require.NoError(t, err)
			before := groupTurn(t, queue, a.ID)
			require.Equal(t, before.Served, groupTurn(t, queue, b.ID).Served)
			require.False(t, before.WaitingSince.IsZero())
			// Two independent repositories compete for exactly the same snapshot.
			var wg sync.WaitGroup
			type reply struct {
				turn service.GroupMonitoringTurn
				ok   bool
				err  error
			}
			done := make(chan reply, 2)
			for range 2 {
				wg.Go(func() {
					var peer service.GroupMonitoringTurnRepository
					if lane == "v2" {
						peer = NewChannelMonitorV2Repository(db).(service.GroupMonitoringTurnRepository)
					} else {
						peer = NewChannelMonitorV3Repository(db).(service.GroupMonitoringTurnRepository)
					}
					turn, ok, err := peer.ClaimGroupMonitoringTurn(ctx, before)
					done <- reply{turn, ok, err}
				})
			}
			wg.Wait()
			close(done)
			var won service.GroupMonitoringTurn
			count := 0
			for result := range done {
				require.NoError(t, result.err)
				if result.ok {
					count++
					won = result.turn
				}
			}
			require.Equal(t, 1, count)
			ok, err := repo.FinishGroupMonitoringTurn(ctx, won, queue.Round)
			require.NoError(t, err)
			require.True(t, ok)
			ok, err = repo.FinishGroupMonitoringTurn(ctx, won, queue.Round)
			require.NoError(t, err)
			require.False(t, ok)
			next, err := repo.SyncGroupMonitoringTurns(ctx, cfg, input)
			require.NoError(t, err)
			require.Equal(t, queue.Round, groupTurn(t, next, a.ID).Served)
			require.Less(t, groupTurn(t, next, b.ID).Served, groupTurn(t, next, a.ID).Served)
			require.Equal(t, groupTurn(t, queue, b.ID).WaitingSince, groupTurn(t, next, b.ID).WaitingSince, "rescans do not refresh pending time")
			unserved, ok, err := repo.ClaimGroupMonitoringTurn(ctx, groupTurn(t, next, a.ID))
			require.NoError(t, err)
			require.True(t, ok)
			require.InDelta(t, service.GroupMonitoringTurnLeaseTTL.Seconds(), time.Until(unserved.ClaimUntil).Seconds(), 3)
			ok, err = repo.FinishGroupMonitoringTurn(ctx, unserved, 0)
			require.NoError(t, err)
			require.True(t, ok)
			ok, err = repo.FinishGroupMonitoringTurn(ctx, unserved, 0)
			require.NoError(t, err)
			require.False(t, ok, "duplicate neutral release must not act as a live owner")
			released, err := repo.SyncGroupMonitoringTurns(ctx, cfg, input)
			require.NoError(t, err)
			require.Equal(t, groupTurn(t, next, a.ID).Served, groupTurn(t, released, a.ID).Served)
			require.Equal(t, groupTurn(t, next, a.ID).WaitingSince, groupTurn(t, released, a.ID).WaitingSince, "pure budget deferral preserves pending origin")
			stale, ok, err := repo.ClaimGroupMonitoringTurn(ctx, groupTurn(t, next, b.ID))
			require.NoError(t, err)
			require.True(t, ok)
			// A live lease prevents another scan stealing in-flight ownership. A crashed
			// owner can be replaced after its database deadline, fencing late completion.
			current, err := repo.SyncGroupMonitoringTurns(ctx, cfg, input)
			require.NoError(t, err)
			_, ok, err = repo.ClaimGroupMonitoringTurn(ctx, groupTurn(t, current, b.ID))
			require.NoError(t, err)
			require.False(t, ok, "in-flight owner keeps its completion authority")
			_, err = db.ExecContext(ctx, `UPDATE monitoring_group_probe_turns SET claim_until=clock_timestamp()-interval '1 second' WHERE lane=$1 AND group_id=$2`, lane, b.ID)
			require.NoError(t, err)
			_, ok, err = repo.ClaimGroupMonitoringTurn(ctx, groupTurn(t, current, b.ID))
			require.NoError(t, err)
			require.True(t, ok)
			ok, err = repo.FinishGroupMonitoringTurn(ctx, stale, next.Round)
			require.NoError(t, err)
			require.False(t, ok)
			input[1].IdentityHash = hash("changed-effort/period")
			changed, err := repo.SyncGroupMonitoringTurns(ctx, cfg, input)
			require.NoError(t, err)
			require.Greater(t, groupTurn(t, changed, b.ID).Served, groupTurn(t, next, b.ID).Served)
			require.Zero(t, groupTurn(t, changed, b.ID).Tried)
			ok, err = repo.FinishGroupMonitoringTurn(ctx, stale, changed.Round)
			require.NoError(t, err)
			require.False(t, ok)
			input[1].Due = false
			quiet, err := repo.SyncGroupMonitoringTurns(ctx, cfg, input)
			require.NoError(t, err)
			require.True(t, groupTurn(t, quiet, b.ID).WaitingSince.IsZero())
			require.Equal(t, groupTurn(t, changed, b.ID).Served, groupTurn(t, quiet, b.ID).Served)
			small, err := repo.SyncGroupMonitoringTurns(ctx, cfg, input[:1])
			require.NoError(t, err)
			require.Len(t, small.Turns, 1)
			empty, err := repo.SyncGroupMonitoringTurns(ctx, cfg, nil)
			require.NoError(t, err)
			require.Empty(t, empty.Turns)
		})
	}
	var changed int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM usage_logs)+(SELECT count(*) FROM monitoring_probe_budget_counters)+(SELECT count(*) FROM channel_monitor_v3_probe_results)+(SELECT count(*) FROM channel_monitor_v2_candy_results)`).Scan(&changed))
	require.Zero(t, changed, "queue coordination must not create evidence, physical usage, or downstream billing")
}

func TestGroupMonitoringTurnsConfigMigrationFailureAndRecovery(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	g := mustCreateGroup(t, client, &service.Group{Name: "group-turn-migration", Platform: service.PlatformOpenAI, Status: service.StatusActive})
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("fixture")))
	input := []service.GroupMonitoringTurn{{GroupID: g.ID, ScopeHash: hash, IdentityHash: hash, Due: true}}
	ddl, err := os.ReadFile("../../migrations/355_monitoring_group_probe_turns.sql")
	require.NoError(t, err)
	for _, lane := range []string{"v2", "v3"} {
		t.Run(lane, func(t *testing.T) {
			repo, cfg := groupTurnsFixture(t, db, lane)
			queue, err := repo.SyncGroupMonitoringTurns(ctx, cfg, input)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, string(ddl))
			require.NoError(t, err)
			next, err := repo.SyncGroupMonitoringTurns(ctx, cfg, input)
			require.NoError(t, err)
			require.Equal(t, queue.Turns, next.Turns, "replayed DDL must not reset age or waiting origin")
			require.Greater(t, next.Round, queue.Round)
			stale := cfg
			stale.Version++
			_, err = repo.SyncGroupMonitoringTurns(ctx, stale, nil)
			require.Error(t, err)
			if lane == "v3" {
				other := *cfg.IdleProbe
				other.Groups = []service.ChannelMonitorV3IdleProbeGroup{{GroupID: g.ID, Model: "new-model"}}
				require.NoError(t, NewChannelMonitorV3Repository(db).UpdateIdleProbe(ctx, other))
				_, err = repo.SyncGroupMonitoringTurns(ctx, cfg, nil)
				require.Error(t, err, "idle settings change without version bump must fence old inventory")
				require.NoError(t, NewChannelMonitorV3Repository(db).UpdateIdleProbe(ctx, *cfg.IdleProbe))
			}
			_, err = repo.SyncGroupMonitoringTurns(ctx, cfg, append(input, input...))
			require.Error(t, err)
			bad := input[0]
			bad.ScopeHash = "private-raw-model"
			_, err = repo.SyncGroupMonitoringTurns(ctx, cfg, []service.GroupMonitoringTurn{bad})
			require.Error(t, err)
			lock, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = lock.Rollback() }()
			id := int64(0x47525050524f4232)
			if lane == "v3" {
				id++
			}
			_, err = lock.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, id)
			require.NoError(t, err)
			limited, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			_, err = repo.SyncGroupMonitoringTurns(limited, cfg, input)
			cancel()
			require.Error(t, err)
			require.NoError(t, lock.Commit())
			after, err := repo.SyncGroupMonitoringTurns(ctx, cfg, input)
			require.NoError(t, err)
			require.Equal(t, next.Turns, after.Turns)
			_, err = db.ExecContext(ctx, `ALTER TABLE monitoring_group_probe_turns RENAME TO hidden_group_turns`)
			require.NoError(t, err)
			_, err = repo.SyncGroupMonitoringTurns(ctx, cfg, input)
			require.Error(t, err)
			_, _, err = repo.ClaimGroupMonitoringTurn(ctx, groupTurn(t, after, g.ID))
			require.Error(t, err)
			_, err = db.ExecContext(ctx, `ALTER TABLE hidden_group_turns RENAME TO monitoring_group_probe_turns`)
			require.NoError(t, err)
			recovered, err := repo.SyncGroupMonitoringTurns(ctx, cfg, input)
			require.NoError(t, err)
			require.Equal(t, after.Turns, recovered.Turns)
		})
	}
	// Rollback is confined to this private fixture; procurement already spent stays spent.
	budget := NewMonitoringProbeBudgetRepository(db)
	_, err = budget.SaveSettings(ctx, monitoringBudgetPolicy())
	require.NoError(t, err)
	id, err := budget.Acquire(ctx, service.MonitoringProbeBudgetRequest{SiteHash: "site", KeyHashes: []string{"key"}})
	require.NoError(t, err)
	require.NoError(t, budget.Release(ctx, id, service.MonitoringProbeTokenUsage{}))
	_, err = db.ExecContext(ctx, `DROP TABLE monitoring_group_probe_turns`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(ddl))
	require.NoError(t, err)
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, summary.DayUsed)
	require.Zero(t, summary.ActiveRequests)
}

func TestGroupMonitoringNativeReviewClocks(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	group := mustCreateGroup(t, client, &service.Group{Name: "native-clock", Platform: service.PlatformOpenAI, Status: service.StatusActive})
	other := mustCreateGroup(t, client, &service.Group{Name: "native-clock-other", Platform: service.PlatformOpenAI, Status: service.StatusActive})
	hash := func(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }
	now := time.Now().UTC().Truncate(time.Second)
	slot := now.Truncate(5 * time.Minute).Add(-15 * time.Minute)
	for _, lane := range []string{"v2", "v3"} {
		t.Run(lane, func(t *testing.T) {
			repo, cfg := groupTurnsFixture(t, db, lane)
			cfg.ObservedAt = now
			key, scope := hash("candy-v1:native-model:medium:5"), ""
			identity := hash(key)
			scope = identity
			if lane == "v3" {
				key, scope = service.ChannelMonitorV3ProbeConfigKey("native-model", "medium", 5), hash("native-model")
				identity = key
			}
			target := service.GroupMonitoringTurn{GroupID: group.ID, ScopeHash: scope, IdentityHash: identity, Due: true, EvidenceKey: key, IntervalMinutes: 5}
			cases := []string{"success", "failure", "deferred", "running", "unfinished", "future_finished", "future_slot", "zero_clock", "reversed_clock", "wrong_key", "wrong_group", "wrong_model", "wrong_effort", "wrong_interval", "legacy", "deferred_keeps_old", "future_keeps_old"}
			if lane == "v2" {
				cases = append(cases, "interrupted")
			}
			for _, scenario := range cases {
				t.Run(scenario, func(t *testing.T) {
					table := "channel_monitor_v2_candy_results"
					if lane == "v3" {
						table = "channel_monitor_v3_probe_results"
					}
					_, err := db.ExecContext(ctx, "DELETE FROM "+table)
					require.NoError(t, err)
					seed := func(old bool) {
						t.Helper()
						groupID, configKey, model := group.ID, any(key), "native-model"
						effort := "medium"
						minutes, stamp, started, finished := 5, slot, slot.Add(time.Minute), any(slot.Add(2*time.Minute))
						status, reason, deferral := "success", "", any(nil)
						if lane == "v2" {
							status = "correct"
						}
						if !old {
							switch scenario {
							case "failure":
								if lane == "v2" {
									status = "incorrect"
								} else {
									status = "failure"
								}
							case "deferred", "deferred_keeps_old":
								status, deferral = "deferred", `{"reason":"request_limit","scope":"global_day","retry_at":"2026-12-01T00:00:00Z"}`
							case "running":
								status, finished = "running", nil
							case "unfinished":
								finished = nil
							case "future_finished", "future_keeps_old":
								finished = now.Add(time.Hour)
							case "future_slot":
								stamp = now.Add(time.Hour)
							case "zero_clock":
								started = time.Time{}
							case "reversed_clock":
								finished = started.Add(-time.Second)
							case "wrong_key":
								configKey = hash("other-configuration")
							case "wrong_group":
								groupID = other.ID
							case "wrong_interval":
								minutes = 10
								configKey = hash("interval-10")
							case "legacy":
								if lane == "v3" {
									configKey = nil
								} else {
									configKey = "old-unknown-key"
								}
							case "interrupted":
								status, reason = "error", "interrupted"
							case "wrong_model":
								model = "other-model"
							case "wrong_effort":
								effort = "low"
								if lane == "v3" {
									configKey = service.ChannelMonitorV3ProbeConfigKey(model, effort, 5)
								}
							}
						} else {
							stamp, started, finished = slot.Add(-5*time.Minute), slot.Add(-4*time.Minute), slot.Add(-3*time.Minute)
						}
						if lane == "v2" {
							_, err = db.ExecContext(ctx, `INSERT INTO channel_monitor_v2_candy_results(group_id,config_key,model,reasoning_effort,slot,checked_at,finished_at,verdict,reason,budget_deferral) VALUES($1,$2,$3,$10,$4,$5,$6,$7,$8,$9::jsonb)`, groupID, configKey, model, stamp, started, finished, status, reason, deferral, effort)
						} else {
							_, err = db.ExecContext(ctx, `INSERT INTO channel_monitor_v3_probe_results(group_id,config_key,model,slot_start,slot_minutes,started_at,finished_at,outcome,budget_deferral) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb)`, groupID, configKey, model, stamp, minutes, started, finished, status, deferral)
						}
						require.NoError(t, err)
					}
					keepsOld := scenario == "deferred_keeps_old" || scenario == "future_keeps_old"
					if keepsOld {
						seed(true)
					}
					seed(false)
					queue, err := repo.SyncGroupMonitoringTurns(ctx, cfg, []service.GroupMonitoringTurn{target})
					require.NoError(t, err)
					turn := groupTurn(t, queue, group.ID)
					if scenario == "success" || scenario == "failure" || keepsOld {
						due := slot.Add(5 * time.Minute)
						if lane == "v3" {
							due = slot.Add(9 * time.Minute)
						}
						if keepsOld {
							due = due.Add(-5 * time.Minute)
						}
						require.Equal(t, due, turn.ReviewDueAt)
						rebuilt, err := repo.SyncGroupMonitoringTurns(ctx, cfg, []service.GroupMonitoringTurn{target})
						require.NoError(t, err)
						require.Equal(t, turn, groupTurn(t, rebuilt, group.ID), "reading the native clock must not renew age or evidence")
					} else {
						require.True(t, turn.ReviewDueAt.IsZero(), "invalid, unknown or deferred evidence is not a review")
					}
					var evidence string
					require.NoError(t, db.QueryRowContext(ctx, "SELECT json_agg(r ORDER BY id)::text FROM "+table+" r").Scan(&evidence))
					if scenario == "success" {
						_, err = db.ExecContext(ctx, "ALTER TABLE "+table+" RENAME TO hidden_native_results")
						require.NoError(t, err)
						_, err = repo.SyncGroupMonitoringTurns(ctx, cfg, []service.GroupMonitoringTurn{target})
						require.Error(t, err, "native history failures must not become routine fallback")
						_, err = db.ExecContext(ctx, "ALTER TABLE hidden_native_results RENAME TO "+table)
						require.NoError(t, err)
						recovered, err := repo.SyncGroupMonitoringTurns(ctx, cfg, []service.GroupMonitoringTurn{target})
						require.NoError(t, err)
						require.Equal(t, turn, groupTurn(t, recovered, group.ID))
					}
					var unchanged string
					require.NoError(t, db.QueryRowContext(ctx, "SELECT json_agg(r ORDER BY id)::text FROM "+table+" r").Scan(&unchanged))
					require.Equal(t, evidence, unchanged, "classification never writes the original verdict or time")
				})
			}
			invalid := cfg
			invalid.ObservedAt = time.Time{}
			_, err := repo.SyncGroupMonitoringTurns(ctx, invalid, []service.GroupMonitoringTurn{target})
			require.Error(t, err)
			target.IntervalMinutes = 0
			_, err = repo.SyncGroupMonitoringTurns(ctx, cfg, []service.GroupMonitoringTurn{target})
			require.Error(t, err)
		})
	}
	var usage, counters int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM usage_logs),(SELECT count(*) FROM monitoring_probe_budget_counters)`).Scan(&usage, &counters))
	require.Zero(t, usage)
	require.Zero(t, counters)
}

func TestChannelMonitorV3ProbeConfigMigrationAndLegacy(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	group := mustCreateGroup(t, client, &service.Group{Name: "v3-config-migration", Platform: service.PlatformOpenAI, Status: service.StatusActive})
	slot := time.Now().UTC().Truncate(5 * time.Minute).Add(-time.Hour)
	_, err := db.ExecContext(ctx, `INSERT INTO channel_monitor_v3_probe_results(group_id,model,slot_start,slot_minutes,outcome) VALUES($1,'native-model',$2,5,'failure')`, group.ID, slot)
	require.NoError(t, err)
	ddl, err := os.ReadFile("../../migrations/356_channel_monitor_v3_probe_config.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = db.ExecContext(ctx, string(ddl))
		require.NoError(t, err)
	}
	var unknown int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM channel_monitor_v3_probe_results WHERE config_key IS NULL AND outcome='failure'`).Scan(&unknown))
	require.Equal(t, 1, unknown, "migration must not guess the legacy effort")
	repo := NewChannelMonitorV3Repository(db)
	key := service.ChannelMonitorV3ProbeConfigKey("native-model", "low", 5)
	high := service.ChannelMonitorV3ProbeConfigKey("native-model", "high", 5)
	require.NotEqual(t, key, high)
	id, err := repo.ClaimProbe(ctx, group.ID, "native-model", slot.Add(5*time.Minute), 5, key)
	require.NoError(t, err)
	require.Positive(t, id)
	again, err := repo.ClaimProbe(ctx, group.ID, "native-model", slot.Add(5*time.Minute), 5, high)
	require.NoError(t, err)
	require.Zero(t, again, "effort change must not bypass the original one-per-slot claim")
	var saved string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT config_key FROM channel_monitor_v3_probe_results WHERE id=$1`, id).Scan(&saved))
	require.Equal(t, key, saved)
	budget := NewMonitoringProbeBudgetRepository(db)
	_, err = budget.SaveSettings(ctx, monitoringBudgetPolicy())
	require.NoError(t, err)
	lease, err := budget.Acquire(ctx, service.MonitoringProbeBudgetRequest{SiteHash: "site", KeyHashes: []string{"key"}})
	require.NoError(t, err)
	require.NoError(t, budget.Release(ctx, lease, service.MonitoringProbeTokenUsage{}))
	_, err = db.ExecContext(ctx, `ALTER TABLE channel_monitor_v3_probe_results DROP COLUMN config_key`)
	require.NoError(t, err)
	var outcomes string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT string_agg(outcome,',' ORDER BY id) FROM channel_monitor_v3_probe_results`).Scan(&outcomes))
	require.Equal(t, "failure,running", outcomes)
	_, err = db.ExecContext(ctx, string(ddl))
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM channel_monitor_v3_probe_results WHERE config_key IS NULL`).Scan(&unknown))
	require.Equal(t, 2, unknown)
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, summary.DayUsed)
	require.Zero(t, summary.ActiveRequests)
}
