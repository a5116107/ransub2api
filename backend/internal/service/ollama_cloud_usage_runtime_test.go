package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOllamaMonitoringRuntimeLifecycleAndProjection(t *testing.T) {
	var missing *OllamaCloudUsageService
	require.Equal(t, "unavailable", missing.MonitoringRuntime().State)
	repo := &ollamaUsageTestRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{}}}
	svc := newOllamaUsageTestService(t, repo, &ollamaUsageHTTPStub{}, &upstreamBillingProbeSettingRepo{}, true)
	require.Equal(t, "not_started", svc.MonitoringRuntime().State)
	require.Nil(t, svc.MonitoringRuntime().LastScan)
	svc.start(false)
	view := svc.MonitoringRuntime()
	require.Equal(t, "running", view.State)
	require.Equal(t, "role_disabled", view.PeriodicState)
	require.Equal(t, "responding_process", view.Scope)
	require.NotEmpty(t, view.InstanceID)
	require.EqualValues(t, 60, view.ScanIntervalSeconds)
	require.EqualValues(t, 20, view.ScanLimit)
	other := newOllamaUsageTestService(t, repo, &ollamaUsageHTTPStub{}, &upstreamBillingProbeSettingRepo{}, true)
	require.NotEqual(t, view.InstanceID, other.MonitoringRuntime().InstanceID)
	policy := DefaultMonitoringProbeBudgetSettings()
	compiled := CompileMonitoringProbeCoverage(policy, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, nil, true, nil, true, nil, true, time.Now())
	stored := &monitoringRuntimeBudgetRepo{coverage: compiled}
	budget := NewMonitoringProbeBudgetService(stored)
	budget.SetOllamaUsage(svc)
	preview, err := budget.PreviewCoverage(t.Context(), policy)
	require.NoError(t, err)
	summary, err := budget.Summary(t.Context())
	require.NoError(t, err)
	require.Equal(t, view.InstanceID, preview.OllamaUsage.InstanceID)
	require.Equal(t, preview.OllamaUsage.InstanceID, summary.Coverage.OllamaUsage.InstanceID)
	require.Nil(t, compiled.OllamaUsage, "API projection must not mutate repository data")
	require.Zero(t, preview.MinimumTotalRequestsPerDay, "event queues are not physical-call forecasts")
	require.Contains(t, preview.UnestimatedDemandSources, "ollama_usage")
	svc.Stop()
	require.Equal(t, "stopped", svc.MonitoringRuntime().State)
	require.Zero(t, svc.MonitoringRuntime().PendingRateLimitProbes)
}

type ollamaRuntimeRepo struct {
	*ollamaUsageTestRepo
	list func(context.Context) ([]Account, error)
}

func (r *ollamaRuntimeRepo) ListDueOllamaCloudUsageAccounts(ctx context.Context, _ time.Time, _, _ time.Duration, _ int) ([]Account, error) {
	return r.list(ctx)
}

func TestOllamaMonitoringRuntimeDueScanBoundaries(t *testing.T) {
	first, sibling, other := ollamaUsageAccount(701), ollamaUsageAccount(702), ollamaUsageAccount(703)
	for _, a := range []*Account{first, sibling, other} {
		a.Extra[OllamaCloudUsageSessionExtraKey] = "cipher:wos-session=runtime-private"
		a.Extra[OllamaCloudUsageAutoRefreshExtraKey] = true
		a.Credentials["api_key"] = "runtime-private-key"
	}
	other.Credentials["api_key"] = "runtime-private-other"
	base := &ollamaUsageTestRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{first.ID: first, sibling.ID: sibling, other.ID: other}}}
	rows := []Account{*first, *sibling, *other}
	var listErr error
	repo := &ollamaRuntimeRepo{ollamaUsageTestRepo: base, list: func(context.Context) ([]Account, error) { return rows, listErr }}
	settings := &upstreamBillingProbeSettingRepo{values: map[string]string{SettingKeyOllamaCloudUsageSettings: `{"enabled":true,"interval_minutes":60,"debounce_minutes":1}`}}
	svc := NewOllamaCloudUsageService(repo, &ollamaUsageHTTPStub{body: ollamaUsageFixture(t)}, NewSettingService(settings, nil), ollamaUsageTestEncryptor{}, true)
	t.Cleanup(svc.Stop)
	svc.lockCache = &fakeLeaderLockCache{}
	require.NoError(t, svc.RunDue(t.Context()))
	scan := svc.MonitoringRuntime().LastScan
	require.Equal(t, "observed", scan.State)
	require.EqualValues(t, 3, *scan.Candidates)
	require.EqualValues(t, 2, *scan.DueGroups)
	require.False(t, *scan.LimitReached)
	require.EqualValues(t, 60, *scan.IntervalMinutes)
	require.EqualValues(t, 1, *scan.DebounceMinutes)
	stamp := scan.ObservedAt
	scan.Candidates = nil
	require.NotNil(t, svc.MonitoringRuntime().LastScan.Candidates)
	require.Equal(t, stamp, svc.MonitoringRuntime().LastScan.ObservedAt, "reading does not renew the last scan")
	encoded, err := json.Marshal(svc.MonitoringRuntime())
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "runtime-private")
	rows = nil
	require.NoError(t, svc.RunDue(t.Context()))
	require.Zero(t, *svc.MonitoringRuntime().LastScan.Candidates, "a successful empty bounded scan is explicit zero")
	require.Zero(t, *svc.MonitoringRuntime().LastScan.DueGroups)
	rows = make([]Account, ollamaCloudUsageMaxPerCycle)
	require.NoError(t, svc.RunDue(t.Context()))
	require.True(t, *svc.MonitoringRuntime().LastScan.LimitReached, "exact limit is not proof of full inventory")
	listErr = errors.New("private inventory error")
	require.Error(t, svc.RunDue(t.Context()))
	scan = svc.MonitoringRuntime().LastScan
	require.Equal(t, "inventory_unavailable", scan.State)
	require.Nil(t, scan.Candidates, "a failed read must not report zero or the old scan as current")
	require.Nil(t, scan.DueGroups)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, svc.RunDue(ctx), context.Canceled)
	require.Equal(t, scan.ObservedAt, svc.MonitoringRuntime().LastScan.ObservedAt, "cancellation does not renew observations")
	require.NoError(t, settings.Set(t.Context(), SettingKeyOllamaCloudUsageSettings, `{"enabled":false,"interval_minutes":60}`))
	require.NoError(t, svc.RunDue(t.Context()))
	require.Equal(t, "disabled", svc.MonitoringRuntime().LastScan.State)
	require.Nil(t, svc.MonitoringRuntime().LastScan.Candidates)
	require.NoError(t, settings.Set(t.Context(), SettingKeyOllamaCloudUsageSettings, `{"enabled":true,"interval_minutes":60}`))
	cache := &fakeLeaderLockCache{}
	_, acquired := tryAcquireSingletonLeaderLock(t.Context(), cache, nil, ollamaCloudUsageLeaderLockKey, "peer", time.Minute)
	require.True(t, acquired)
	svc.lockCache = cache
	require.NoError(t, svc.RunDue(t.Context()))
	require.Equal(t, "not_leader", svc.MonitoringRuntime().LastScan.State)
	require.Nil(t, svc.MonitoringRuntime().LastScan.Candidates)
	require.NoError(t, settings.Set(t.Context(), SettingKeyOllamaCloudUsageSettings, `{`))
	require.Error(t, svc.RunDue(t.Context()))
	require.Equal(t, "settings_unavailable", svc.MonitoringRuntime().LastScan.State)
	require.Nil(t, svc.MonitoringRuntime().LastScan.Enabled)
}

func TestOllamaMonitoringRuntimeQueueAndConcurrentSnapshots(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	a := ollamaUsageAccount(801)
	a.Extra[OllamaCloudUsageSessionExtraKey] = "cipher:wos-session=fixture"
	repo := &ollamaUsageTestRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{a.ID: a}}}
	svc := NewOllamaCloudUsageService(repo, &ollamaUsageHTTPStub{body: ollamaCloudProbeUsageBody(100, now.Add(time.Hour).Format(time.RFC3339))}, NewSettingService(&upstreamBillingProbeSettingRepo{}, nil), ollamaUsageTestEncryptor{}, true)
	svc.now = func() time.Time { return now }
	svc.start(false)
	entered, release := make(chan struct{}), make(chan struct{})
	require.True(t, svc.ScheduleOllamaCloudUsageRateLimitProbe(a.ID, func(int64, time.Time) { close(entered); <-release }))
	<-entered
	for id := int64(900); id < 910; id++ {
		require.True(t, svc.ScheduleOllamaCloudUsageRateLimitProbe(id, func(int64, time.Time) {}))
	}
	view := svc.MonitoringRuntime()
	require.EqualValues(t, 10, view.PendingRateLimitProbes)
	require.EqualValues(t, 1, view.ActiveRateLimitProbes)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for range 20 {
				_ = svc.MonitoringRuntime()
			}
		})
	}
	wg.Wait()
	done := make(chan struct{})
	go func() { svc.Stop(); close(done) }()
	require.Eventually(t, func() bool { return svc.MonitoringRuntime().State == "stopping" }, time.Second, time.Millisecond)
	require.False(t, svc.ScheduleOllamaCloudUsageRateLimitProbe(999, func(int64, time.Time) {}))
	close(release)
	<-done
	view = svc.MonitoringRuntime()
	require.Equal(t, "stopped", view.State)
	require.Zero(t, view.PendingRateLimitProbes)
	require.Zero(t, view.ActiveRateLimitProbes)
	require.Zero(t, view.ActiveRefreshes)
	require.Nil(t, view.LastScan, "gateway event handling is not a periodic inventory scan")
}

func TestOllamaMonitoringRuntimeBackgroundConfigIsObservedNotInferred(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &ollamaUsageTestRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{}}}
		settings := &upstreamBillingProbeSettingRepo{values: map[string]string{}}
		svc := NewOllamaCloudUsageService(repo, &ollamaUsageHTTPStub{}, NewSettingService(settings, nil), ollamaUsageTestEncryptor{}, true)
		svc.start(true)
		synctest.Wait()
		require.Equal(t, "running", svc.MonitoringRuntime().State)
		require.Equal(t, "disabled", svc.MonitoringRuntime().PeriodicState)
		require.Equal(t, "disabled", svc.MonitoringRuntime().LastScan.State)
		require.NoError(t, settings.Set(t.Context(), SettingKeyOllamaCloudUsageSettings, `{"enabled":true,"interval_minutes":60,"debounce_minutes":1}`))
		time.Sleep(time.Minute)
		synctest.Wait()
		require.Equal(t, "enabled", svc.MonitoringRuntime().PeriodicState)
		require.Equal(t, "observed", svc.MonitoringRuntime().LastScan.State)
		require.Zero(t, *svc.MonitoringRuntime().LastScan.Candidates)
		require.NoError(t, settings.Set(t.Context(), SettingKeyOllamaCloudUsageSettings, `{`))
		time.Sleep(time.Minute)
		synctest.Wait()
		require.Equal(t, "unknown", svc.MonitoringRuntime().PeriodicState)
		require.Equal(t, "settings_unavailable", svc.MonitoringRuntime().LastScan.State)
		svc.Stop()
		require.Equal(t, "stopped", svc.MonitoringRuntime().State)
		require.Equal(t, "stopped", svc.MonitoringRuntime().PeriodicState)
	})
}
