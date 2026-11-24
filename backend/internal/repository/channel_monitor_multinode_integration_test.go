//go:build integration

package repository

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV1IndependentProcessesDoNotDuplicate(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	var generations, heads atomic.Int64
	entered, gate := make(chan struct{}, 4), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			heads.Add(1)
			return
		}
		generations.Add(1)
		entered <- struct{}{}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(release)
	require.NoError(t, NewSettingRepository(client).SetMultiple(ctx, map[string]string{
		service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1",
	}))
	budget := NewMonitoringProbeBudgetRepository(db)
	_, err := budget.SaveSettings(ctx, monitoringBudgetPolicy())
	require.NoError(t, err)
	monitors := NewChannelMonitorRepository(client, db)
	m := &service.ChannelMonitor{Name: "V1 process fixture", Provider: service.MonitorProviderOpenAI,
		APIMode: service.MonitorAPIModeChatCompletions, Endpoint: "https://example.com/tenant/v1", APIKey: "v1-process-fixture",
		PrimaryModel: "model-a", Enabled: true, IntervalSeconds: 3600,
		BodyOverrideMode: service.MonitorBodyOverrideModeReplace,
		BodyOverride:     map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}}
	require.NoError(t, monitors.Create(ctx, m))
	command := monitoringBudgetNodeCommand{Mode: "v1", DSN: monitoringBudgetNodeDSN(t, db),
		Site: server.URL, Certificate: server.Certificate().Raw, Requests: 1, TimeoutMillis: 5000}
	replies := make(chan monitoringBudgetNodeReply, 2)
	leader := startMonitoringBudgetNode(t, command, replies)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first V1 process did not dispatch its native model check")
	}
	command.TimeoutMillis = 1000
	peer := startMonitoringBudgetNode(t, command, replies)
	require.NotEqual(t, leader.pid, peer.pid)
	peer.wait()
	require.Equal(t, http.StatusOK, receiveMonitoringBudgetReply(t, replies).Status)
	release()
	leader.wait()
	require.Equal(t, http.StatusOK, receiveMonitoringBudgetReply(t, replies).Status)
	require.EqualValues(t, 1, generations.Load(), "independent V1 runners must not duplicate one monitor's in-flight check")
	require.EqualValues(t, 1, heads.Load())
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, summary.DayUsed)
	require.EqualValues(t, 7, summary.DayTokens)
	require.Zero(t, summary.ActiveRequests)
	// Restart only the original runner, without altering due times or clearing
	// counts: a fast completed batch is still one shared cycle.
	command.TimeoutMillis = 500
	restarted := startMonitoringBudgetNode(t, command, replies)
	restarted.wait()
	require.Equal(t, http.StatusOK, receiveMonitoringBudgetReply(t, replies).Status)
	require.EqualValues(t, 1, generations.Load())
	require.EqualValues(t, 1, heads.Load())
	history, err := monitors.ListHistory(ctx, m.ID, "", 20)
	require.NoError(t, err)
	require.Len(t, history, 1)
	current, err := monitors.GetByID(ctx, m.ID)
	require.NoError(t, err)
	require.NotNil(t, current.LastCheckedAt)
	var downstream int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&downstream))
	require.Zero(t, downstream)
	t.Log("three independent V1 runners: one physical model/HEAD pair, one history, actual tokens=7; restart did not replay the completed cycle")
}

func TestChannelMonitorV1IndependentProcessesPeriodicBudgetRecovery(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	var generations, heads, invalid atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			if r.URL.Path != "/" || r.Header.Get("Authorization") != "" {
				invalid.Add(1)
			}
			heads.Add(1)
			return
		}
		if r.URL.Path != "/tenant/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer v1-periodic-fixture" {
			invalid.Add(1)
			http.Error(w, "unexpected V1 fixture", http.StatusBadRequest)
			return
		}
		generations.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)
	}))
	t.Cleanup(server.Close)
	require.NoError(t, NewSettingRepository(client).SetMultiple(ctx, map[string]string{
		service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1",
	}))
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 20000
	policy.TokensPerDay = 1
	policy, err := budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	repo := NewChannelMonitorRepository(client, db)
	m := &service.ChannelMonitor{Name: "V1 periodic budget", Provider: service.MonitorProviderOpenAI,
		APIMode: service.MonitorAPIModeChatCompletions, Endpoint: "https://example.com/tenant/v1", APIKey: "v1-periodic-fixture",
		PrimaryModel: "model-a", Enabled: true, IntervalSeconds: 15,
		BodyOverrideMode: service.MonitorBodyOverrideModeReplace,
		BodyOverride:     map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}}
	require.NoError(t, repo.Create(ctx, m))
	command := monitoringBudgetNodeCommand{Mode: "v1", DSN: monitoringBudgetNodeDSN(t, db), Site: server.URL,
		Certificate: server.Certificate().Raw, Requests: 1, TimeoutMillis: 34000}
	replies := make(chan monitoringBudgetNodeReply, 2)
	first := startMonitoringBudgetNode(t, command, replies)
	require.Eventually(t, func() bool { return heads.Load() == 1 }, 8*time.Second, 10*time.Millisecond)
	command.TimeoutMillis = 32000
	second := startMonitoringBudgetNode(t, command, replies)
	require.NotEqual(t, first.pid, second.pid)
	// Actual usage is not known before dispatch. The first model reports seven
	// tokens, then the next cycle defers generation without changing evidence.
	require.Eventually(t, func() bool {
		rows, err := repo.ListHistory(ctx, m.ID, "", 20)
		return err == nil && len(rows) == 1
	}, 5*time.Second, 10*time.Millisecond)
	previous, err := repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	require.NotNil(t, previous.LastCheckedAt)
	require.Eventually(t, func() bool { return heads.Load() == 2 }, 23*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		s, err := budget.Summary(ctx)
		return err == nil && s.DayUsed == 3 && s.ActiveRequests == 0
	}, 3*time.Second, 10*time.Millisecond)
	current, err := repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, previous.LastCheckedAt, current.LastCheckedAt)
	history, err := repo.ListHistory(ctx, m.ID, "", 20)
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.EqualValues(t, 1, generations.Load())
	policy.TokensPerDay = 100
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	first.wait()
	second.wait()
	for range 2 {
		require.Equal(t, http.StatusOK, receiveMonitoringBudgetReply(t, replies).Status)
	}
	require.EqualValues(t, 2, generations.Load(), "original 15-second timers must run the next two due cycles, not every other interval")
	require.EqualValues(t, 3, heads.Load(), "each shared cycle has one HEAD despite two process-local timers")
	history, err = repo.ListHistory(ctx, m.ID, "", 20)
	require.NoError(t, err)
	require.Len(t, history, 2)
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 5, summary.DayUsed)
	require.EqualValues(t, 14, summary.DayTokens)
	require.Zero(t, summary.UsageUnknown)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.ActiveRequests)
	require.Zero(t, invalid.Load())
	var downstream int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&downstream))
	require.Zero(t, downstream)
	t.Log("two independent original 15-second timers / three real cycles: budget-deferral kept history and freshness unchanged; recovery sent two model requests plus three HEADs, actual tokens=14, downstream=0")
}

// Compile-time assertion: coordination remains on the original repository.
var _ service.ChannelMonitorScheduledRepository = (*channelMonitorRepository)(nil)

func TestChannelMonitorV1ScheduledClaimsFailureClosed(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewChannelMonitorRepository(client, db)
	m := &service.ChannelMonitor{Name: "V1 missing claim table", Provider: service.MonitorProviderOpenAI,
		Endpoint: "https://example.com/v1", APIKey: "fixture", PrimaryModel: "model", Enabled: true, IntervalSeconds: 60}
	require.NoError(t, repo.Create(ctx, m))
	m = readV1ClaimSnapshot(t, repo, m)
	_, err := db.ExecContext(ctx, `ALTER TABLE channel_monitor_probe_claims RENAME TO fixture_held_claims`)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, restoreErr := db.ExecContext(cleanup, `ALTER TABLE fixture_held_claims RENAME TO channel_monitor_probe_claims`)
		require.NoError(t, restoreErr)
	})
	claim, err := repo.(service.ChannelMonitorScheduledRepository).ClaimScheduledCheck(ctx, m, 2*time.Minute, time.Minute)
	require.Error(t, err)
	require.Nil(t, claim)
	current, err := repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	require.Nil(t, current.LastCheckedAt)
}
