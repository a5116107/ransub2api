package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ollamaLifecycleFixture(t *testing.T, upstream HTTPUpstream) (*OllamaCloudUsageService, *ollamaUsageTestRepo) {
	t.Helper()
	account := ollamaUsageAccount(9101)
	account.Extra[OllamaCloudUsageSessionExtraKey] = "cipher:wos-session=lifecycle-fixture"
	account.Extra[OllamaCloudUsageAutoRefreshExtraKey] = true
	repo := &ollamaUsageTestRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}, due: []Account{*account}}
	settings := &upstreamBillingProbeSettingRepo{values: map[string]string{SettingKeyOllamaCloudUsageSettings: `{"enabled":true,"interval_minutes":60,"debounce_minutes":1}`}}
	return newOllamaUsageTestService(t, repo, upstream, settings, true), repo
}

func TestOllamaCloudUsageLifecycleExternalDueStopsAndDrains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, canceled, drain := make(chan struct{}), make(chan struct{}), make(chan struct{})
		release := sync.OnceFunc(func() { close(drain) })
		defer release()
		upstream := &ollamaUsageHTTPStub{body: ollamaUsageFixture(t), beforeResponse: func(req *http.Request) {
			close(entered)
			select {
			case <-req.Context().Done():
				close(canceled)
				<-drain
			case <-drain:
			}
		}}
		svc, repo := ollamaLifecycleFixture(t, upstream)
		done := make(chan error, 1)
		go func() { done <- svc.RunDue(t.Context()) }()
		<-entered
		first, second := make(chan struct{}), make(chan struct{})
		go func() { svc.Stop(); close(first) }()
		synctest.Wait()
		go func() { svc.Stop(); close(second) }()
		synctest.Wait()
		select {
		case <-canceled:
		default:
			assert.Fail(t, "Stop did not cancel the external automatic refresh")
		}
		for _, stopped := range []<-chan struct{}{first, second} {
			select {
			case <-stopped:
				assert.Fail(t, "Stop returned while the automatic refresh was still draining")
			default:
			}
		}
		assert.Equal(t, "stopping", svc.MonitoringRuntime().State)
		release()
		<-first
		<-second
		require.ErrorIs(t, <-done, context.Canceled)
		require.Nil(t, repo.accounts[9101].Extra[OllamaCloudUsageSnapshotExtraKey])
		require.Empty(t, svc.refreshSlots)
		require.Equal(t, "stopped", svc.MonitoringRuntime().State)
	})
}

func TestOllamaCloudUsageLifecycleStoppedDueDoesNotSend(t *testing.T) {
	upstream := &ollamaUsageHTTPStub{body: ollamaUsageFixture(t)}
	svc, repo := ollamaLifecycleFixture(t, upstream)
	svc.Stop()
	assert.ErrorIs(t, svc.RunDue(t.Context()), context.Canceled)
	assert.Zero(t, upstream.calls.Load(), "stopped automatic work must not send")
	assert.Nil(t, repo.accounts[9101].Extra[OllamaCloudUsageSnapshotExtraKey])
}

func TestOllamaCloudUsageLifecycleCanceledRefreshPublishesNothing(t *testing.T) {
	for _, phase := range []string{"response", "body"} {
		for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusServiceUnavailable} {
			t.Run(phase+"/"+http.StatusText(status), func(t *testing.T) {
				cause := errors.New("refresh caller retired")
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				var upstream HTTPUpstream = &ollamaUsageHTTPStub{body: ollamaUsageFixture(t), status: status,
					beforeResponse: func(*http.Request) { cancel(cause) }}
				if phase == "body" {
					upstream = ollamaLifecycleLateBodyUpstream{body: ollamaUsageFixture(t), status: status, onRead: func() { cancel(cause) }}
				}
				svc, repo := ollamaLifecycleFixture(t, upstream)
				old := time.Now().UTC().Add(-2 * time.Hour)
				previous := &OllamaCloudUsageSnapshot{Status: OllamaCloudUsageStatusOK, Data: &OllamaCloudUsageData{Plan: "previous"},
					FetchedAt: &old, LastAttemptAt: old, NextRefreshAt: old}
				repo.accounts[9101].Extra[OllamaCloudUsageSnapshotExtraKey] = previous
				_, err := svc.Refresh(ctx, 9101)
				assert.ErrorIs(t, err, cause)
				assert.Same(t, previous, repo.accounts[9101].Extra[OllamaCloudUsageSnapshotExtraKey], "late transport replies must not replace prior evidence or renew its freshness")
				require.Equal(t, "cipher:wos-session=lifecycle-fixture", repo.accounts[9101].Extra[OllamaCloudUsageSessionExtraKey])
				require.Equal(t, true, repo.accounts[9101].Extra[OllamaCloudUsageAutoRefreshExtraKey])
				if stub, ok := upstream.(*ollamaUsageHTTPStub); ok {
					require.EqualValues(t, 1, stub.calls.Load())
				}
				require.Empty(t, svc.refreshSlots)
			})
		}
	}
}

func TestOllamaCloudUsageLifecycleManualRefreshAfterStop(t *testing.T) {
	upstream := &ollamaUsageHTTPStub{body: ollamaUsageFixture(t)}
	svc, _ := ollamaLifecycleFixture(t, upstream)
	svc.Stop()
	state, err := svc.Refresh(t.Context(), 9101)
	require.NoError(t, err)
	require.Equal(t, OllamaCloudUsageStatusOK, state.Snapshot.Status)
	require.EqualValues(t, 1, upstream.calls.Load())
}

type ollamaLifecycleLateBody struct {
	io.ReadCloser
	ctx    context.Context
	onRead func()
}

func (b *ollamaLifecycleLateBody) Read(p []byte) (int, error) {
	if b.onRead != nil {
		b.onRead()
	}
	<-b.ctx.Done()
	return b.ReadCloser.Read(p)
}

type ollamaLifecycleLateBodyUpstream struct {
	HTTPUpstream
	body   []byte
	status int
	onRead func()
}

func (u ollamaLifecycleLateBodyUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	status := u.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/html"}},
		Body: &ollamaLifecycleLateBody{ReadCloser: io.NopCloser(strings.NewReader(string(u.body))), ctx: req.Context(), onRead: u.onRead}, Request: req}, nil
}

func TestOllamaCloudUsageLifecycleRequestTimeoutIsNotPassingEvidence(t *testing.T) {
	for _, phase := range []string{"response", "body"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				body := ollamaUsageFixture(t)
				var upstream HTTPUpstream = &ollamaUsageHTTPStub{body: body, beforeResponse: func(req *http.Request) { <-req.Context().Done() }}
				if phase == "body" {
					upstream = ollamaLifecycleLateBodyUpstream{body: body}
				}
				svc, _ := ollamaLifecycleFixture(t, upstream)
				state, err := svc.Refresh(t.Context(), 9101)
				require.NoError(t, err, "the transport deadline is a supplier failure, not caller cancellation")
				require.Equal(t, OllamaCloudUsageStatusFailed, state.Snapshot.Status)
				require.Equal(t, 1, state.Snapshot.FailureCount)
				require.Nil(t, state.Snapshot.FetchedAt)
				require.NoError(t, t.Context().Err())
				require.Empty(t, svc.refreshSlots)
			})
		})
	}
}

func TestOllamaCloudUsageLifecycleQueuedDueStopsWithoutRescan(t *testing.T) {
	entered, canceled, drain := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(drain) })
	defer release()
	upstream := &ollamaUsageHTTPStub{body: ollamaUsageFixture(t), beforeResponse: func(req *http.Request) {
		close(entered)
		<-req.Context().Done()
		close(canceled)
		<-drain
	}}
	svc, base := ollamaLifecycleFixture(t, upstream)
	var scans atomic.Int64
	svc.accountRepo = &ollamaRuntimeRepo{ollamaUsageTestRepo: base, list: func(context.Context) ([]Account, error) {
		scans.Add(1)
		account, err := base.GetByID(t.Context(), 9101)
		if err != nil {
			return nil, err
		}
		return []Account{*account}, nil
	}}
	results := make(chan error, 21)
	go func() { results <- svc.RunDue(t.Context()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Ollama automatic refresh did not reach its request")
	}
	for range 20 {
		ctx := &billingProbeLifecycleContext{Context: t.Context(), attached: make(chan struct{})}
		go func() { results <- svc.RunDue(ctx) }()
		select {
		case <-ctx.attached:
		case <-time.After(5 * time.Second):
			t.Fatal("automatic refresh did not register its caller context")
		}
	}
	stopped := make(chan struct{})
	go func() { svc.Stop(); close(stopped) }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic refresh did not receive shutdown cancellation")
	}
	require.EqualValues(t, 1, scans.Load())
	release()
	for range 21 {
		select {
		case err := <-results:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("queued automatic refresh did not drain")
		}
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic refresh shutdown did not complete")
	}
	require.EqualValues(t, 1, scans.Load())
	require.EqualValues(t, 1, upstream.calls.Load())
	require.Empty(t, svc.refreshSlots)
	require.Nil(t, base.accounts[9101].Extra[OllamaCloudUsageSnapshotExtraKey])
}

func TestOllamaCloudUsageLifecycleConcurrentStartRunStop(t *testing.T) {
	for range 25 {
		repo := &ollamaUsageTestRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{}}}
		svc := newOllamaUsageTestService(t, repo, &ollamaUsageHTTPStub{}, &upstreamBillingProbeSettingRepo{}, true)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); <-start; svc.Start() }()
		go func() { defer wg.Done(); <-start; _ = svc.RunDue(t.Context()) }()
		go func() { defer wg.Done(); <-start; svc.Stop() }()
		close(start)
		wg.Wait()
		svc.Stop()
		svc.Start()
		require.ErrorIs(t, svc.RunDue(t.Context()), context.Canceled)
		require.Empty(t, svc.refreshSlots)
	}
}

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
