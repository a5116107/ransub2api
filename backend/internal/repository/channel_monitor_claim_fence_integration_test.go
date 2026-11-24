//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV1ClaimAndFinishLockMonitorBeforeLease(t *testing.T) {
	for _, operation := range []string{"claim", "finish"} {
		t.Run(operation, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			ctx := t.Context()
			repo := NewChannelMonitorRepository(client, db)
			m := v1ConfigSyncMonitor()
			require.NoError(t, repo.Create(ctx, m))
			m = readV1ClaimSnapshot(t, repo, m)
			claims := repo.(service.ChannelMonitorScheduledRepository)
			var claim *service.ChannelMonitorScheduledClaim
			if operation == "finish" {
				var err error
				claim, err = claims.ClaimScheduledCheck(ctx, m, time.Minute, time.Hour)
				require.NoError(t, err)
				require.NotNil(t, claim)
			}
			lock, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = lock.Rollback() }()
			_, err = lock.ExecContext(ctx, `SELECT id FROM channel_monitors WHERE id=$1 FOR NO KEY UPDATE`, m.ID)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() {
				var err error
				if operation == "claim" {
					_, err = claims.ClaimScheduledCheck(ctx, m, time.Minute, time.Hour)
				} else {
					_, err = claims.FinishScheduledCheck(ctx, m.ID, claim, nil)
				}
				done <- err
			}()
			require.Eventually(t, func() bool {
				var blocked bool
				err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
 AND state='active' AND wait_event_type='Lock' AND query LIKE '%channel_monitor_probe_claims%'
 AND query NOT LIKE '%pg_stat_activity%')`).Scan(&blocked)
				return err == nil && blocked
			}, 2*time.Second, 5*time.Millisecond)
			// While the contender waits for the monitor, it must not hold the
			// lease row that this monitor owner would next update.
			var id int64
			require.NoError(t, lock.QueryRowContext(ctx, `SELECT monitor_id FROM channel_monitor_probe_claims
 WHERE monitor_id=$1 FOR UPDATE NOWAIT`, m.ID).Scan(&id))
			require.Equal(t, m.ID, id)
			require.NoError(t, lock.Rollback())
			require.NoError(t, <-done)
		})
	}
}

func TestChannelMonitorV1StaleLeaseWaitIsNotAnotherOwnersDelay(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewChannelMonitorRepository(client, db)
	m := v1ConfigSyncMonitor()
	require.NoError(t, repo.Create(ctx, m))
	old := readV1ClaimSnapshot(t, repo, m)
	_, err := db.ExecContext(ctx, `UPDATE channel_monitors SET primary_model='model-b' WHERE id=$1`, m.ID)
	require.NoError(t, err)
	current := readV1ClaimSnapshot(t, repo, m)
	claims := repo.(service.ChannelMonitorScheduledRepository)
	winner, err := claims.ClaimScheduledCheck(ctx, current, time.Minute, time.Hour)
	require.NoError(t, err)
	require.NotNil(t, winner)
	stale, err := claims.ClaimScheduledCheck(ctx, old, time.Minute, time.Hour)
	require.NoError(t, err)
	require.Nil(t, stale, "old configuration cannot inherit the current owner's hour-long retry hint")
	var owner string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT lease_id::text FROM channel_monitor_probe_claims WHERE monitor_id=$1`, m.ID).Scan(&owner))
	require.Equal(t, winner.ID, owner)
	for _, invalid := range []*service.ChannelMonitor{nil, {}, {ID: m.ID}, {ID: m.ID, ScheduledConfigurationKey: "invalid"}} {
		claim, err := claims.ClaimScheduledCheck(ctx, invalid, time.Minute, time.Hour)
		require.Error(t, err)
		require.Nil(t, claim)
	}
}

type v1ClaimResult struct {
	claim *service.ChannelMonitorScheduledClaim
	err   error
}

func TestChannelMonitorV1ClaimWaitsForConfigurationCommit(t *testing.T) {
	for _, change := range []string{"saved_configuration", "same_timestamp", "disabled", "deleted", "observation_only"} {
		t.Run(change, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			ctx := t.Context()
			repo := NewChannelMonitorRepository(client, db)
			m := v1ConfigSyncMonitor()
			require.NoError(t, repo.Create(ctx, m))
			stale, err := repo.GetByID(ctx, m.ID)
			require.NoError(t, err)
			lock, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = lock.Rollback() }()
			query := `UPDATE channel_monitors SET primary_model='model-b',api_key_encrypted='rotated-fixture',updated_at=clock_timestamp() WHERE id=$1`
			switch change {
			case "same_timestamp":
				query = `UPDATE channel_monitors SET primary_model='model-b',api_key_encrypted='rotated-fixture' WHERE id=$1`
			case "disabled":
				query = `UPDATE channel_monitors SET enabled=false WHERE id=$1`
			case "deleted":
				query = `DELETE FROM channel_monitors WHERE id=$1`
			case "observation_only":
				query = `UPDATE channel_monitors SET extra_headers=COALESCE(extra_headers,'{}'::jsonb)
 || jsonb_build_object('sub2api:duplicate_operation_id','observation-fixture') WHERE id=$1`
			}
			_, err = lock.ExecContext(ctx, query, m.ID)
			require.NoError(t, err)
			if change == "observation_only" {
				// Observation publication intentionally does not touch the claim.
				// Hold its row explicitly so the old implementation also waits.
				_, err = lock.ExecContext(ctx, `SELECT monitor_id FROM channel_monitor_probe_claims WHERE monitor_id=$1 FOR UPDATE`, m.ID)
				require.NoError(t, err)
			}
			finished := make(chan v1ClaimResult, 1)
			claimCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			go func() {
				claim, err := repo.(service.ChannelMonitorScheduledRepository).ClaimScheduledCheck(claimCtx, stale, time.Minute, time.Hour)
				finished <- v1ClaimResult{claim, err}
			}()
			require.Eventually(t, func() bool {
				var waiting bool
				err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
 AND state='active' AND wait_event_type='Lock' AND query LIKE '%channel_monitor_probe_claims AS c%'
 AND query NOT LIKE '%pg_stat_activity%')`).Scan(&waiting)
				return err == nil && waiting
			}, 3*time.Second, 5*time.Millisecond, "claim must be blocked before the configuration transaction commits")
			require.NoError(t, lock.Commit())
			got := <-finished
			require.NoError(t, got.err)
			if change == "observation_only" {
				require.NotNil(t, got.claim, "observation metadata cannot invalidate the scheduling configuration")
				return
			}
			require.Nil(t, got.claim, "a stale snapshot must not acquire or overwrite the newer configuration's lease after waiting")
			if change == "deleted" {
				return
			}
			var active int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM channel_monitor_probe_claims WHERE monitor_id=$1 AND lease_id IS NOT NULL`, m.ID).Scan(&active))
			require.Zero(t, active)
			if change == "disabled" {
				return
			}
			current, err := repo.GetByID(ctx, m.ID)
			require.NoError(t, err)
			claim, err := repo.(service.ChannelMonitorScheduledRepository).ClaimScheduledCheck(ctx, current, time.Minute, time.Hour)
			require.NoError(t, err)
			require.NotNil(t, claim, "the current committed snapshot can still acquire immediately")
		})
	}
}

func TestChannelMonitorV1ClaimDoesNotSerializeIndependentMonitors(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewChannelMonitorRepository(client, db)
	first, other := v1ConfigSyncMonitor(), v1ConfigSyncMonitor()
	other.Name = "independent V1 configuration"
	require.NoError(t, repo.Create(ctx, first))
	require.NoError(t, repo.Create(ctx, other))
	other = readV1ClaimSnapshot(t, repo, other)
	lock, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = lock.Rollback() }()
	_, err = lock.ExecContext(ctx, `UPDATE channel_monitors SET primary_model='model-b' WHERE id=$1`, first.ID)
	require.NoError(t, err)
	claimCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	claims := repo.(service.ChannelMonitorScheduledRepository)
	claim, err := claims.ClaimScheduledCheck(claimCtx, other, time.Minute, time.Hour)
	require.NoError(t, err, "holding another monitor and the configuration clock must not serialize claims globally")
	require.NotNil(t, claim)
	renewed, err := claims.RenewScheduledCheck(claimCtx, other.ID, claim, time.Minute)
	require.NoError(t, err)
	require.True(t, renewed)
}

func TestChannelMonitorV1ClaimSnapshotReaderPreservesExistingMapping(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewChannelMonitorRepository(client, db)
	m := v1ConfigSyncMonitor()
	m.Name, m.GroupName, m.JitterSeconds = "读取一致性", "fixture group", 15
	m.ExtraModels = []string{"model-b", "model-c"}
	m.ExtraHeaders = map[string]string{"X-Fixture": "header"}
	m.DuplicateOperationID = "snapshot-fixture"
	require.NoError(t, repo.Create(ctx, m))
	require.NoError(t, repo.MarkChecked(ctx, m.ID, time.Now().UTC().Truncate(time.Microsecond)))
	db.SetMaxOpenConns(1)
	for _, zone := range []string{"UTC", "Asia/Kathmandu"} {
		_, err := db.ExecContext(ctx, `SELECT set_config('TimeZone',$1,false)`, zone)
		require.NoError(t, err)
		for _, body := range []map[string]any{m.BodyOverride, nil, {}, {"nested": map[string]any{"value": 3.5}, "list": []any{"a", true}}} {
			m.BodyOverride = body
			require.NoError(t, repo.Update(ctx, m))
			// The original Ent read remains the independent mapping oracle. The
			// production getter is replaced, not maintained as a second reader.
			stored, err := client.ChannelMonitor.Get(ctx, m.ID)
			require.NoError(t, err)
			want := entToServiceMonitor(stored)
			got, err := repo.GetByID(ctx, m.ID)
			require.NoError(t, err)
			require.Len(t, got.ScheduledConfigurationKey, 32)
			require.Equal(t, m.APIKey, got.APIKey, "the private ciphertext column must survive JSON decoding")
			encoded, err := json.Marshal(got)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), got.ScheduledConfigurationKey)
			require.NotContains(t, string(encoded), "ScheduledConfigurationKey")
			got.ScheduledConfigurationKey = ""
			require.Equal(t, want, got, "all existing public and internal field mappings must remain identical")
			claim, err := repo.(service.ChannelMonitorScheduledRepository).ClaimScheduledCheck(ctx, got, time.Minute, time.Hour)
			require.Error(t, err, "a missing stored snapshot identity must fail before creating a lease")
			require.Nil(t, claim)
		}
	}
	_, err := repo.GetByID(ctx, -1)
	require.ErrorIs(t, err, service.ErrChannelMonitorNotFound)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = repo.GetByID(canceled, m.ID)
	require.ErrorIs(t, err, context.Canceled)
}

type v1ClaimReadFence struct {
	service.ChannelMonitorRepository
	service.ChannelMonitorScheduledRepository
	service.ChannelMonitorScheduleRepository
	once    sync.Once
	loaded  chan struct{}
	release chan struct{}
	claims  chan v1ClaimResult
}

func (r *v1ClaimReadFence) ClaimScheduledCheck(ctx context.Context, m *service.ChannelMonitor, ttl, delay time.Duration) (*service.ChannelMonitorScheduledClaim, error) {
	claim, err := r.ChannelMonitorScheduledRepository.ClaimScheduledCheck(ctx, m, ttl, delay)
	r.claims <- v1ClaimResult{claim, err}
	return claim, err
}

func (r *v1ClaimReadFence) GetByID(ctx context.Context, id int64) (*service.ChannelMonitor, error) {
	m, err := r.ChannelMonitorRepository.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	r.once.Do(func() {
		close(r.loaded)
		select {
		case <-r.release:
		case <-ctx.Done():
		}
	})
	return m, ctx.Err()
}

func TestChannelMonitorV1ClaimCommitFenceHTTPAndRecovery(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	var heads, models, invalid atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			heads.Add(1)
			return
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "model-b" || r.Header.Get("Authorization") != "Bearer rotated-fixture" {
			invalid.Add(1)
			http.Error(w, "invalid fixture snapshot", http.StatusBadRequest)
			return
		}
		if len(body.Messages) == 0 {
			invalid.Add(1)
			http.Error(w, "missing fixture challenge", http.StatusBadRequest)
			return
		}
		prompt := body.Messages[len(body.Messages)-1].Content
		start := strings.LastIndex(prompt, "Q:")
		var a, b int
		var operator string
		if start < 0 {
			invalid.Add(1)
			http.Error(w, "missing fixture question", http.StatusBadRequest)
			return
		}
		if n, err := fmt.Sscanf(prompt[start:], "Q: %d %s %d = ?", &a, &operator, &b); err != nil || n != 3 || operator != "+" && operator != "-" {
			invalid.Add(1)
			http.Error(w, "invalid fixture question", http.StatusBadRequest)
			return
		}
		if operator == "-" {
			b = -b
		}
		models.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"%d"}}],"usage":{"total_tokens":7}}`, a+b)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(service.SetChannelMonitorHTTPClientForTesting(server.Client()))
	settings := service.NewSettingService(NewSettingRepository(client), &config.Config{})
	require.NoError(t, NewSettingRepository(client).SetMultiple(ctx, map[string]string{
		service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1"}))
	budget := NewMonitoringProbeBudgetRepository(db)
	_, err := budget.SaveSettings(ctx, monitoringBudgetPolicy())
	require.NoError(t, err)
	repo := NewChannelMonitorRepository(client, db)
	m := v1ConfigSyncMonitor()
	m.Endpoint = server.URL + "/v1"
	require.NoError(t, repo.Create(ctx, m))
	fence := &v1ClaimReadFence{ChannelMonitorRepository: repo,
		ChannelMonitorScheduledRepository: repo.(service.ChannelMonitorScheduledRepository),
		ChannelMonitorScheduleRepository:  repo.(service.ChannelMonitorScheduleRepository),
		loaded:                            make(chan struct{}), release: make(chan struct{}), claims: make(chan v1ClaimResult, 4)}
	svc := service.ProvideChannelMonitorService(fence, ollamaBudgetFixtureEncryptor{}, settings, &config.Config{}, nil, budget)
	runner := service.NewChannelMonitorRunner(svc, settings)
	svc.SetScheduler(runner)
	release := sync.OnceFunc(func() { close(fence.release) })
	t.Cleanup(func() { release(); runner.Stop() })
	runner.Start()
	select {
	case <-fence.loaded:
	case <-time.After(3 * time.Second):
		t.Fatal("original runner did not load the fixture monitor")
	}
	lock, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = lock.Rollback() }()
	_, err = lock.ExecContext(ctx, `UPDATE channel_monitors SET primary_model='model-b',api_key_encrypted='rotated-fixture' WHERE id=$1`, m.ID)
	require.NoError(t, err)
	release()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
 AND state='active' AND wait_event_type='Lock' AND query LIKE '%channel_monitor_probe_claims AS c%'
 AND query NOT LIKE '%pg_stat_activity%')`).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, 5*time.Millisecond)
	require.NoError(t, lock.Commit())
	select {
	case stale := <-fence.claims:
		require.NoError(t, stale.err)
		require.Nil(t, stale.claim, "the original automatic attempt must finish its stale claim before recovery is scheduled")
	case <-time.After(time.Second):
		t.Fatal("the original stale claim did not exit after the commit")
	}
	current, err := repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	runner.Schedule(current)
	require.Eventually(t, func() bool {
		history, err := repo.ListHistory(ctx, m.ID, "", 10)
		return err == nil && len(history) == 1 && history[0].Model == "model-b"
	}, 4*time.Second, 10*time.Millisecond)
	history, err := repo.ListHistory(ctx, m.ID, "", 20)
	require.NoError(t, err)
	require.Equal(t, service.MonitorStatusOperational, history[0].Status)
	runner.Stop()
	require.EqualValues(t, 1, heads.Load())
	require.EqualValues(t, 1, models.Load())
	require.Zero(t, invalid.Load(), "the stale credential/model must never reach the physical client")
	results, err := svc.RunCheck(ctx, m.ID)
	require.NoError(t, err, "manual diagnostics remain independent after automatic Stop")
	require.Len(t, results, 1)
	require.Equal(t, service.MonitorStatusOperational, results[0].Status, "fixture manual result: %s", results[0].Message)
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 4, summary.DayUsed)
	require.EqualValues(t, 14, summary.DayTokens)
	require.Zero(t, summary.ActiveRequests)
	require.EqualValues(t, 2, models.Load())
	require.Zero(t, invalid.Load())
	var downstream int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&downstream))
	require.Zero(t, downstream)
}
