//go:build integration

package repository

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV1IndependentProcessDiscoversRemoteCreate(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"total_tokens":7}}`)
	}))
	t.Cleanup(server.Close)
	require.NoError(t, NewSettingRepository(client).SetMultiple(ctx, map[string]string{
		service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1",
	}))
	replies := make(chan monitoringBudgetNodeReply, 1)
	command := monitoringBudgetNodeCommand{Mode: "v1", DSN: monitoringBudgetNodeDSN(t, db), Site: server.URL,
		Certificate: server.Certificate().Raw, Requests: 1, TimeoutMillis: 11000}
	node := startMonitoringBudgetNode(t, command, replies)
	// The original process has no monitors at startup. The subsequent write
	// occurs through a different repository/process, with no local Schedule call.
	time.Sleep(1500 * time.Millisecond)
	m := &service.ChannelMonitor{Name: "remote-created V1", Provider: service.MonitorProviderOpenAI,
		APIMode: service.MonitorAPIModeChatCompletions, Endpoint: "https://example.com/v1", APIKey: "remote-create-fixture",
		PrimaryModel: "model-a", Enabled: true, IntervalSeconds: 3600,
		BodyOverrideMode: service.MonitorBodyOverrideModeReplace,
		BodyOverride:     map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}}
	require.NoError(t, NewChannelMonitorRepository(client, db).Create(ctx, m))
	node.wait()
	require.Equal(t, http.StatusOK, receiveMonitoringBudgetReply(t, replies).Status)
	require.EqualValues(t, 1, calls.Load(), "a running full node must discover a monitor created through another node")
}

func v1ConfigSyncMonitor() *service.ChannelMonitor {
	return &service.ChannelMonitor{Name: "V1 configuration", Provider: service.MonitorProviderOpenAI,
		APIMode: service.MonitorAPIModeChatCompletions, Endpoint: "https://example.com/v1", APIKey: "v1-config-fixture",
		PrimaryModel: "model-a", Enabled: true, IntervalSeconds: 3600,
		BodyOverrideMode: service.MonitorBodyOverrideModeMerge,
		BodyOverride:     map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}}
}

func TestChannelMonitorV1ConfigurationRevisionTransactionsAndObservations(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewChannelMonitorRepository(client, db)
	source := repo.(service.ChannelMonitorScheduleRepository)
	initial, err := source.LoadScheduleSnapshot(ctx, -1)
	require.NoError(t, err)
	require.Empty(t, initial.Monitors)
	m := v1ConfigSyncMonitor()
	require.NoError(t, repo.Create(ctx, m))
	first, err := source.LoadScheduleSnapshot(ctx, initial.Revision)
	require.NoError(t, err)
	require.Len(t, first.Monitors, 1)
	require.Greater(t, first.Revision, initial.Revision)
	claims := repo.(service.ChannelMonitorScheduledRepository)
	m = readV1ClaimSnapshot(t, repo, m)
	claim, err := claims.ClaimScheduledCheck(ctx, m, time.Minute, time.Hour)
	require.NoError(t, err)
	require.NotNil(t, claim)
	before := first.Revision
	require.NoError(t, repo.MarkChecked(ctx, m.ID, time.Now()))
	_, err = db.ExecContext(ctx, `UPDATE channel_monitors SET extra_headers=COALESCE(extra_headers,'{}'::jsonb)
 || jsonb_build_object('sub2api:budget_credential_scope','fixture-observation','sub2api:duplicate_operation_id','fixture-operation') WHERE id=$1`, m.ID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE channel_monitors SET updated_at=clock_timestamp() WHERE id=$1`, m.ID)
	require.NoError(t, err)
	unchanged, err := source.LoadScheduleSnapshot(ctx, before)
	require.NoError(t, err)
	require.Nil(t, unchanged, "history, timestamps and internal credential observations do not reload secrets or restart timers")
	observed, err := repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, m.ScheduledConfigurationKey, observed.ScheduledConfigurationKey, "the captured identity excludes the same observation fields as renewal")
	renewed, err := claims.RenewScheduledCheck(ctx, m.ID, claim, time.Minute)
	require.NoError(t, err)
	require.True(t, renewed)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `UPDATE channel_monitors SET primary_model='rolled-back' WHERE id=$1`, m.ID)
	require.NoError(t, err)
	unchanged, err = source.LoadScheduleSnapshot(ctx, before)
	require.NoError(t, err)
	require.Nil(t, unchanged, "uncommitted configuration and revision must stay invisible")
	require.NoError(t, tx.Rollback())
	renewed, err = claims.RenewScheduledCheck(ctx, m.ID, claim, time.Minute)
	require.NoError(t, err)
	require.True(t, renewed, "rollback preserves the original lease")
	for _, model := range []string{"model-a", "model-b", "model-a"} {
		m, err = repo.GetByID(ctx, m.ID)
		require.NoError(t, err)
		m.PrimaryModel = model
		require.NoError(t, repo.Update(ctx, m))
		next, err := source.LoadScheduleSnapshot(ctx, before)
		require.NoError(t, err)
		require.NotNil(t, next, "identical and ABA configuration saves are visible")
		require.Greater(t, next.Revisions[m.ID], first.Revisions[m.ID])
		require.Equal(t, model, next.Monitors[0].PrimaryModel)
		before = next.Revision
	}
	renewed, err = claims.RenewScheduledCheck(ctx, m.ID, claim, time.Minute)
	require.NoError(t, err)
	require.False(t, renewed)
	_, err = db.ExecContext(ctx, `UPDATE channel_monitors SET enabled=false WHERE id=$1`, m.ID)
	require.NoError(t, err)
	disabled, err := source.LoadScheduleSnapshot(ctx, before)
	require.NoError(t, err)
	require.Empty(t, disabled.Monitors)
	before = disabled.Revision
	require.NoError(t, repo.Delete(ctx, m.ID))
	deleted, err := source.LoadScheduleSnapshot(ctx, before)
	require.NoError(t, err)
	require.Greater(t, deleted.Revision, before)
	require.Empty(t, deleted.Monitors)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM channel_monitor_probe_claims`).Scan(&count))
	require.Zero(t, count)
}

func TestChannelMonitorV1ConfigurationTriggersCoverStoredFields(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	for _, trigger := range []string{"channel_monitor_schedule_lock", "channel_monitor_schedule_revision"} {
		rows, err := db.QueryContext(t.Context(), `SELECT a.attname FROM pg_attribute a
 WHERE a.attrelid='channel_monitors'::regclass AND a.attnum>0 AND NOT a.attisdropped
 AND a.attname NOT IN ('last_checked_at','updated_at')
 AND NOT EXISTS(SELECT 1 FROM pg_trigger t WHERE t.tgrelid=a.attrelid AND t.tgname=$1 AND a.attnum=ANY(t.tgattr))`, trigger)
		require.NoError(t, err)
		var missing []string
		for rows.Next() {
			var column string
			require.NoError(t, rows.Scan(&column))
			missing = append(missing, column)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		require.Empty(t, missing, "%s must also cover configuration fields added by future migrations", trigger)
	}
}

func TestChannelMonitorV1ConfigurationSnapshotIsRepeatableRead(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewChannelMonitorRepository(client, db)
	source := repo.(service.ChannelMonitorScheduleRepository)
	m := v1ConfigSyncMonitor()
	require.NoError(t, repo.Create(ctx, m))
	before, err := source.LoadScheduleSnapshot(ctx, -1)
	require.NoError(t, err)
	lock, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = lock.Rollback() }()
	_, err = lock.ExecContext(ctx, `LOCK TABLE channel_monitors IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	type result struct {
		snapshot *service.ChannelMonitorScheduleSnapshot
		err      error
	}
	loaded := make(chan result, 1)
	go func() {
		snapshot, err := source.LoadScheduleSnapshot(ctx, -1)
		loaded <- result{snapshot, err}
	}()
	// The loader has read its clock but cannot yet read monitor rows. Commit
	// a change before releasing the table lock to distinguish snapshot isolation.
	require.Eventually(t, func() bool {
		var blocked bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
 AND wait_event_type='Lock' AND query LIKE 'SELECT%channel_monitors%')`).Scan(&blocked)
		return err == nil && blocked
	}, 3*time.Second, 5*time.Millisecond)
	_, err = lock.ExecContext(ctx, `UPDATE channel_monitors SET primary_model='model-b' WHERE id=$1`, m.ID)
	require.NoError(t, err)
	require.NoError(t, lock.Commit())
	select {
	case got := <-loaded:
		require.NoError(t, got.err)
		require.Equal(t, before.Revision, got.snapshot.Revision)
		require.Equal(t, before.Revisions, got.snapshot.Revisions)
		require.Equal(t, "model-a", got.snapshot.Monitors[0].PrimaryModel)
	case <-time.After(5 * time.Second):
		t.Fatal("consistent snapshot did not finish after lock release")
	}
	after, err := source.LoadScheduleSnapshot(ctx, before.Revision)
	require.NoError(t, err)
	require.Greater(t, after.Revision, before.Revision)
	require.Equal(t, "model-b", after.Monitors[0].PrimaryModel)
}

func TestChannelMonitorV1ConfigurationForeignKeysAndTemplateGenerations(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewChannelMonitorRepository(client, db)
	source := repo.(service.ChannelMonitorScheduleRepository)
	var accountID int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type) VALUES('V1 config FK','openai','apikey') RETURNING id`).Scan(&accountID))
	templates := NewChannelMonitorRequestTemplateRepository(client, db)
	tpl := &service.ChannelMonitorRequestTemplate{Name: "V1 config template", Provider: service.MonitorProviderOpenAI,
		APIMode: service.MonitorAPIModeChatCompletions, BodyOverrideMode: service.MonitorBodyOverrideModeOff,
		ExtraHeaders: map[string]string{"X-Fixture": "template"}}
	require.NoError(t, templates.Create(ctx, tpl))
	m := v1ConfigSyncMonitor()
	m.TemplateID, m.AccountID = &tpl.ID, &accountID
	require.NoError(t, repo.Create(ctx, m))
	unrelated := v1ConfigSyncMonitor()
	unrelated.Name = "V1 unrelated configuration"
	require.NoError(t, repo.Create(ctx, unrelated))
	before, err := source.LoadScheduleSnapshot(ctx, -1)
	require.NoError(t, err)
	for range 2 {
		count, err := templates.ApplyToMonitors(ctx, tpl.ID, []int64{m.ID, unrelated.ID})
		require.NoError(t, err)
		require.EqualValues(t, 1, count)
		after, err := source.LoadScheduleSnapshot(ctx, before.Revision)
		require.NoError(t, err)
		require.Greater(t, after.Revisions[m.ID], before.Revisions[m.ID])
		require.Equal(t, before.Revisions[unrelated.ID], after.Revisions[unrelated.ID])
		before = after
	}
	_, err = db.ExecContext(ctx, `DELETE FROM accounts WHERE id=$1`, accountID)
	require.NoError(t, err)
	after, err := source.LoadScheduleSnapshot(ctx, before.Revision)
	require.NoError(t, err)
	require.Greater(t, after.Revisions[m.ID], before.Revisions[m.ID], "FK SET NULL also changes the committed schedule generation")
	before = after
	require.NoError(t, templates.Delete(ctx, tpl.ID))
	after, err = source.LoadScheduleSnapshot(ctx, before.Revision)
	require.NoError(t, err)
	require.Greater(t, after.Revisions[m.ID], before.Revisions[m.ID])
	current, err := repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	require.Nil(t, current.AccountID)
	require.Nil(t, current.TemplateID)
	require.Equal(t, "template", current.ExtraHeaders["X-Fixture"], "removing a template preserves the existing request snapshot")
}

func TestChannelMonitorV1ConfigurationConcurrentSavesAndCompletion(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewChannelMonitorRepository(client, db)
	claims := repo.(service.ChannelMonitorScheduledRepository)
	m := v1ConfigSyncMonitor()
	require.NoError(t, repo.Create(ctx, m))
	for range 20 {
		current, err := repo.GetByID(ctx, m.ID)
		require.NoError(t, err)
		claim, err := claims.ClaimScheduledCheck(ctx, current, time.Minute, time.Hour)
		require.NoError(t, err)
		require.NotNil(t, claim)
		start := make(chan struct{})
		done := make(chan error, 2)
		go func() {
			<-start
			_, err := claims.FinishScheduledCheck(ctx, m.ID, claim, []*service.ChannelMonitorHistoryRow{{MonitorID: m.ID,
				Model: m.PrimaryModel, Status: service.MonitorStatusOperational, CheckedAt: time.Now()}})
			done <- err
		}()
		go func() {
			<-start
			done <- repo.Update(ctx, current)
		}()
		close(start)
		for range 2 {
			require.NoError(t, <-done, "configuration and observation completion must use a compatible row lock order")
		}
		renewed, err := claims.RenewScheduledCheck(ctx, m.ID, claim, time.Minute)
		require.NoError(t, err)
		require.False(t, renewed)
	}
}

const v1ScheduleRevisionRollbackSQL = `DROP TRIGGER channel_monitor_schedule_revision ON channel_monitors;
 DROP TRIGGER channel_monitor_schedule_lock ON channel_monitors;
 DROP FUNCTION advance_channel_monitor_schedule_revision(); DROP FUNCTION lock_channel_monitor_schedule_clock();
 DROP TABLE channel_monitor_schedule_clock; ALTER TABLE channel_monitor_probe_claims DROP COLUMN schedule_revision;`

func TestChannelMonitorV1ConfigurationMigrationRollbackPreservesData(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewChannelMonitorRepository(client, db)
	m := v1ConfigSyncMonitor()
	require.NoError(t, repo.Create(ctx, m))
	require.NoError(t, repo.InsertHistoryBatch(ctx, []*service.ChannelMonitorHistoryRow{{MonitorID: m.ID, Model: m.PrimaryModel,
		Status: service.MonitorStatusOperational, CheckedAt: time.Now()}}))
	claims := repo.(service.ChannelMonitorScheduledRepository)
	m = readV1ClaimSnapshot(t, repo, m)
	claim, err := claims.ClaimScheduledCheck(ctx, m, time.Minute, time.Hour)
	require.NoError(t, err)
	require.NotNil(t, claim)
	observe := func() string {
		t.Helper()
		var value string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT json_build_array((SELECT row_to_json(m) FROM channel_monitors m WHERE id=$1),
 (SELECT json_agg(h) FROM channel_monitor_histories h WHERE monitor_id=$1),
 (SELECT json_agg(b) FROM monitoring_probe_budget_counters b),
 (SELECT json_agg(a) FROM accounts a))::text`, m.ID).Scan(&value))
		return value
	}
	before := observe()
	migration, err := os.ReadFile("../../migrations/352_channel_monitor_schedule_revision.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	renewed, err := claims.RenewScheduledCheck(ctx, m.ID, claim, time.Minute)
	require.NoError(t, err)
	require.True(t, renewed, "idempotent migration must preserve live claim ownership and due time")
	_, err = db.ExecContext(ctx, v1ScheduleRevisionRollbackSQL)
	require.NoError(t, err, "rollback runs only in the disposable private database")
	require.Equal(t, before, observe())
	_, err = repo.(service.ChannelMonitorScheduleRepository).LoadScheduleSnapshot(ctx, -1)
	require.Error(t, err, "missing clock fails closed rather than returning empty inventory")
	renewed, err = claims.RenewScheduledCheck(ctx, m.ID, claim, time.Minute)
	require.NoError(t, err)
	require.True(t, renewed, "351 scheduling state is not removed with 352")
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	require.Equal(t, before, observe())
	snapshot, err := repo.(service.ChannelMonitorScheduleRepository).LoadScheduleSnapshot(ctx, -1)
	require.NoError(t, err)
	require.Len(t, snapshot.Monitors, 1)
}

func TestChannelMonitorV1IndependentProcessReconcilesRemoteChanges(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	var mu sync.Mutex
	type observed struct{ Model, Auth, Path, Template string }
	var seen []observed
	observations := func() []observed {
		mu.Lock()
		defer mu.Unlock()
		return append([]observed(nil), seen...)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid fixture JSON", http.StatusBadRequest)
			return
		}
		mu.Lock()
		seen = append(seen, observed{body.Model, r.Header.Get("Authorization"), r.URL.Path, r.Header.Get("X-Fixture")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"total_tokens":7}}`)
	}))
	t.Cleanup(server.Close)
	require.NoError(t, NewSettingRepository(client).SetMultiple(ctx, map[string]string{
		service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1"}))
	budget := NewMonitoringProbeBudgetRepository(db)
	_, err := budget.SaveSettings(ctx, monitoringBudgetPolicy())
	require.NoError(t, err)
	repo := NewChannelMonitorRepository(client, db)
	templates := NewChannelMonitorRequestTemplateRepository(client, db)
	tpl := &service.ChannelMonitorRequestTemplate{Name: "V1 process template", Provider: service.MonitorProviderOpenAI,
		APIMode: service.MonitorAPIModeChatCompletions, BodyOverrideMode: service.MonitorBodyOverrideModeOff,
		ExtraHeaders: map[string]string{"X-Fixture": "applied"}}
	require.NoError(t, templates.Create(ctx, tpl))
	m := v1ConfigSyncMonitor()
	m.TemplateID = &tpl.ID
	require.NoError(t, repo.Create(ctx, m))
	command := monitoringBudgetNodeCommand{Mode: "v1", DSN: monitoringBudgetNodeDSN(t, db), Site: server.URL,
		Certificate: server.Certificate().Raw, Requests: 1, TimeoutMillis: 36000}
	replies := make(chan monitoringBudgetNodeReply, 1)
	node := startMonitoringBudgetNode(t, command, replies)
	awaitHistory := func(count int) {
		t.Helper()
		require.Eventually(t, func() bool {
			history, err := repo.ListHistory(ctx, m.ID, "", 20)
			return err == nil && len(history) == count
		}, 8*time.Second, 10*time.Millisecond)
	}
	awaitHistory(1)
	// These are committed through a different process's repository. No local
	// callback, due-time override or direct claim mutation nudges the child.
	m.PrimaryModel = "model-b"
	m.APIKey = "v1-config-rotated-fixture"
	m.Endpoint = "https://example.com/tenant/v1"
	m.IntervalSeconds, m.JitterSeconds = 1800, 15
	require.NoError(t, repo.Update(ctx, m))
	awaitHistory(2)
	count, err := templates.ApplyToMonitors(ctx, tpl.ID, []int64{m.ID})
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	awaitHistory(3)
	m, err = repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	m.Enabled = false
	require.NoError(t, repo.Update(ctx, m))
	// Leave the disabled generation across a complete coordinator interval.
	time.Sleep(5500 * time.Millisecond)
	expected := []observed{{"model-a", "Bearer v1-config-fixture", "/v1/chat/completions", ""},
		{"model-b", "Bearer v1-config-rotated-fixture", "/tenant/v1/chat/completions", ""},
		{"model-b", "Bearer v1-config-rotated-fixture", "/tenant/v1/chat/completions", "applied"}}
	require.Equal(t, expected, observations())
	m.Enabled = true
	m.PrimaryModel = "model-c"
	require.NoError(t, repo.Update(ctx, m))
	awaitHistory(4)
	require.NoError(t, repo.Delete(ctx, m.ID))
	node.wait()
	require.Equal(t, http.StatusOK, receiveMonitoringBudgetReply(t, replies).Status)
	expected = append(expected, observed{"model-c", "Bearer v1-config-rotated-fixture", "/tenant/v1/chat/completions", "applied"})
	require.Equal(t, expected, observations())
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 8, summary.DayUsed)
	require.EqualValues(t, 28, summary.DayTokens)
	require.Zero(t, summary.ActiveRequests)
	// The offline/restarted node sees only the current committed inventory,
	// not the deleted monitor or the intermediate model saved while offline.
	offline := v1ConfigSyncMonitor()
	offline.PrimaryModel = "offline-intermediate"
	require.NoError(t, repo.Create(ctx, offline))
	offline.PrimaryModel = "offline-current"
	require.NoError(t, repo.Update(ctx, offline))
	command.TimeoutMillis = 1000
	restarted := startMonitoringBudgetNode(t, command, replies)
	restarted.wait()
	require.Equal(t, http.StatusOK, receiveMonitoringBudgetReply(t, replies).Status)
	summary, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 10, summary.DayUsed)
	require.EqualValues(t, 35, summary.DayTokens)
	expected = append(expected, observed{"offline-current", "Bearer v1-config-fixture", "/v1/chat/completions", ""})
	require.Equal(t, expected, observations())
	var downstream int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&downstream))
	require.Zero(t, downstream)
}
