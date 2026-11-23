//go:build integration

package repository_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type lifecycleCheckedMonitorRepo struct {
	service.ChannelMonitorRepository
	marked chan time.Time
}

func (r *lifecycleCheckedMonitorRepo) MarkChecked(ctx context.Context, id int64, at time.Time) error {
	err := r.ChannelMonitorRepository.MarkChecked(ctx, id, at)
	if err == nil {
		r.marked <- at
	}
	return err
}

func TestChannelMonitorRunnerLifecycleHTTPPostgresHistoryAndRecovery(t *testing.T) {
	for _, action := range []string{"stop", "unschedule"} {
		t.Run(action, func(t *testing.T) { testChannelMonitorRunnerLifecycle(t, action) })
	}
}

func testChannelMonitorRunnerLifecycle(t *testing.T, action string) {
	t.Helper()
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	var heads, generations, canceled, invalid atomic.Int32
	var block atomic.Bool
	block.Store(true)
	entered := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			heads.Add(1)
			if r.URL.Path != "/" || r.Header.Get("Authorization") != "" {
				invalid.Add(1)
			}
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/tenant/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer v1-lifecycle-fixture" {
			invalid.Add(1)
			http.Error(w, "invalid V1 lifecycle fixture", http.StatusBadRequest)
			return
		}
		generations.Add(1)
		if block.Load() {
			entered <- struct{}{}
			<-r.Context().Done()
			canceled.Add(1)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(service.SetChannelMonitorHTTPClientForTesting(server.Client()))
	cfg := &config.Config{}
	settingsRepo := repository.NewSettingRepository(client)
	require.NoError(t, settingsRepo.SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1"}))
	policyRepo := repository.NewMonitoringProbeBudgetRepository(db)
	policy := service.DefaultMonitoringProbeBudgetSettings()
	policy.Enabled, policy.RecoveryReservePercent = true, 0
	policy.RequestsPerDay, policy.RequestsPerMinute = 100000, 1000
	policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency = 10, 10, 10
	_, err := policyRepo.SaveSettings(ctx, policy)
	require.NoError(t, err)
	monitors := repository.NewChannelMonitorRepository(client, db)
	monitor := &service.ChannelMonitor{Name: "V1 lifecycle", Provider: service.MonitorProviderOpenAI,
		APIMode: service.MonitorAPIModeChatCompletions, Endpoint: server.URL + "/tenant/v1", APIKey: "v1-lifecycle-fixture",
		PrimaryModel: "model-a", ExtraModels: []string{"model-b"}, Enabled: true, IntervalSeconds: 3600,
		BodyOverrideMode: service.MonitorBodyOverrideModeReplace,
		BodyOverride:     map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}}
	require.NoError(t, monitors.Create(ctx, monitor))
	old := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	var history []*service.ChannelMonitorHistoryRow
	for _, model := range []string{"model-a", "model-b"} {
		history = append(history, &service.ChannelMonitorHistoryRow{MonitorID: monitor.ID, Model: model, Status: service.MonitorStatusOperational, CheckedAt: old})
	}
	require.NoError(t, monitors.InsertHistoryBatch(ctx, history))
	require.NoError(t, monitors.MarkChecked(ctx, monitor.ID, old))
	newService := func(marked chan time.Time) *service.ChannelMonitorService {
		var repo service.ChannelMonitorRepository = repository.NewChannelMonitorRepository(client, db)
		if marked != nil {
			repo = &lifecycleCheckedMonitorRepo{repo, marked}
		}
		return service.ProvideChannelMonitorService(repo, v1BudgetMonitorEncryptor{},
			service.NewSettingService(repository.NewSettingRepository(client), cfg), cfg, nil, repository.NewMonitoringProbeBudgetRepository(db))
	}
	firstMarked := make(chan time.Time, 1)
	firstService := newService(firstMarked)
	first := service.NewChannelMonitorRunner(firstService, service.NewSettingService(settingsRepo, cfg))
	firstService.SetScheduler(first)
	t.Cleanup(first.Stop)
	first.Start()
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for range 2 {
		select {
		case <-entered:
		case <-wait.Done():
			t.Fatal("the original V1 runner did not dispatch both model checks")
		}
	}
	usage, err := policyRepo.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, usage.ActiveRequests)
	if action == "unschedule" {
		first.Unschedule(monitor.ID)
		require.Eventually(t, func() bool { return canceled.Load() == 2 }, 3*time.Second, time.Millisecond, "retiring the monitor must cancel both real HTTP calls")
	}
	first.Stop()
	require.Empty(t, firstMarked, "the canceled automatic run never committed a new freshness timestamp")
	require.Eventually(t, func() bool { return canceled.Load() == 2 }, 3*time.Second, time.Millisecond)
	reader := repository.NewChannelMonitorRepository(client, db)
	unchanged, err := reader.GetByID(ctx, monitor.ID)
	require.NoError(t, err)
	require.NotNil(t, unchanged.LastCheckedAt)
	require.Equal(t, old, *unchanged.LastCheckedAt, "executor cancellation must not renew the previous passing observation")
	for _, model := range []string{"model-a", "model-b"} {
		rows, loadErr := reader.ListHistory(ctx, monitor.ID, model, 20)
		require.NoError(t, loadErr)
		require.Len(t, rows, 1, "canceled checks must not append provider failures or late successes")
		require.Equal(t, service.MonitorStatusOperational, rows[0].Status)
	}
	usage, err = repository.NewMonitoringProbeBudgetRepository(db).Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 3, usage.DayUsed, "the HEAD and two already-sent model calls are not refunded")
	require.EqualValues(t, 2, usage.UsageUnknown, "interrupted model bodies have no returned usage and are not free")
	require.Zero(t, usage.DayTokens)
	require.Zero(t, usage.UsagePending)
	require.Zero(t, usage.ActiveRequests)
	block.Store(false)
	// Stopping automatic work must not cancel a separate manual request.
	manual, err := newService(nil).RunCheck(ctx, monitor.ID)
	require.NoError(t, err)
	require.Len(t, manual, 2)
	for _, result := range manual {
		require.Equal(t, service.MonitorStatusOperational, result.Status)
	}
	recoveredMarked := make(chan time.Time, 1)
	recoveredService := newService(recoveredMarked)
	recovered := service.NewChannelMonitorRunner(recoveredService, service.NewSettingService(repository.NewSettingRepository(client), cfg))
	t.Cleanup(recovered.Stop)
	recoveredService.SetScheduler(recovered)
	recovered.Start()
	var recoveredAt time.Time
	select {
	case recoveredAt = <-recoveredMarked:
	case <-time.After(5 * time.Second):
		t.Fatal("the rebuilt original runner did not finish both histories and the real SQL freshness write")
	}
	recovered.Stop()
	for _, model := range []string{"model-a", "model-b"} {
		rows, loadErr := repository.NewChannelMonitorRepository(client, db).ListHistory(ctx, monitor.ID, model, 20)
		require.NoError(t, loadErr)
		require.Len(t, rows, 3)
		for _, row := range rows {
			require.Equal(t, service.MonitorStatusOperational, row.Status)
		}
	}
	latest, err := reader.GetByID(ctx, monitor.ID)
	require.NoError(t, err)
	require.WithinDuration(t, recoveredAt, *latest.LastCheckedAt, time.Microsecond)
	usage, err = policyRepo.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 9, usage.DayUsed)
	require.EqualValues(t, 28, usage.DayTokens)
	require.EqualValues(t, 4, usage.UsageReported)
	require.EqualValues(t, 2, usage.UsageUnknown)
	require.Zero(t, usage.UsagePending)
	require.Zero(t, usage.ActiveRequests)
	require.EqualValues(t, 3, heads.Load())
	require.EqualValues(t, 6, generations.Load())
	require.Zero(t, invalid.Load())
	var downstream int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&downstream))
	require.Zero(t, downstream)
	t.Logf("%s native V1 runner: canceled 2 real HTTP model calls without changing history/freshness; independent manual and rebuilt automatic checks recovered; 9 physical attempts/28 actual tokens/2 unknown; no active budget leases or downstream charges", action)
}
