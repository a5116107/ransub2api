package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCNMonitoringRuntimeStatesAndSnapshotIsolation(t *testing.T) {
	var missing *CNProviderBalanceCheckService
	require.Equal(t, "unavailable", missing.MonitoringRuntime().State)
	cfg := &config.Config{}
	cfg.Gateway.CNProviders.BalanceCheckEnabled = true
	svc := NewCNProviderBalanceCheckService(&fakeCNCheckRepo{}, &CNProviderBalanceService{}, &CNProviderQuotaService{}, cfg, 10*time.Minute)
	require.Equal(t, "not_started", svc.MonitoringRuntime().State)
	require.Nil(t, svc.MonitoringRuntime().LastScan, "startup has not observed an empty inventory")
	svc.Start()
	t.Cleanup(svc.Stop)
	view := svc.MonitoringRuntime()
	require.Equal(t, "responding_process", view.Scope)
	require.Equal(t, 1, view.Version)
	require.NotEmpty(t, view.InstanceID)
	require.Equal(t, "running", view.State)
	require.EqualValues(t, 600, *view.IntervalSeconds)
	other := NewCNProviderBalanceCheckService(nil, nil, nil, nil, time.Minute)
	require.NotEqual(t, view.InstanceID, other.MonitoringRuntime().InstanceID, "distinct process runners are not a fleet snapshot")
	svc.runOnce(t.Context())
	view = svc.MonitoringRuntime()
	require.NotNil(t, view.LastScan)
	require.True(t, view.LastScan.Complete)
	require.Zero(t, view.LastScan.Targets)
	view.LastScan.Targets = 999
	require.Zero(t, svc.MonitoringRuntime().LastScan.Targets, "API callers must not mutate runner observations")
	svc.Stop()
	require.Equal(t, "stopped", svc.MonitoringRuntime().State)
	require.NotNil(t, svc.MonitoringRuntime().LastScan, "shutdown retains the last observation, not new passing evidence")

	for _, tc := range []struct {
		name     string
		enabled  bool
		interval time.Duration
		want     string
	}{
		{"disabled", false, time.Minute, "disabled"},
		{"zero_interval", true, 0, "disabled"},
		{"no_dependencies", true, time.Minute, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.CNProviders.BalanceCheckEnabled = tc.enabled
			svc := NewCNProviderBalanceCheckService(nil, nil, nil, cfg, tc.interval)
			svc.Start()
			require.Equal(t, tc.want, svc.MonitoringRuntime().State)
			require.Nil(t, svc.MonitoringRuntime().LastScan)
			svc.Stop()
		})
	}
	gateway := &config.Config{}
	gateway.Gateway.CNProviders.BalanceCheckEnabled = true
	gateway.Runtime.Role = config.RuntimeRoleGateway
	svc = NewCNProviderBalanceCheckService(&fakeCNCheckRepo{}, &CNProviderBalanceService{}, nil, gateway, time.Minute)
	require.Equal(t, "role_disabled", svc.MonitoringRuntime().State)
	svc.Stop()
}

func TestCNMonitoringRuntimeTracksOnlyOriginalQueueTargets(t *testing.T) {
	account := func(id int64, platform string, coding, schedulable bool) Account {
		a := Account{ID: id, Platform: platform, Status: StatusActive, Type: AccountTypeAPIKey, Schedulable: schedulable,
			Credentials: map[string]any{"api_key": "private-runtime-fixture", "base_url": "https://private-runtime.invalid"}}
		if coding {
			a.Credentials["account_mode"] = "coding"
		}
		return a
	}
	kimi := account(1, PlatformKimi, true, false)
	payg := account(2, PlatformDeepseek, false, true)
	pausedPayg := account(3, PlatformDeepseek, false, false)
	ollama := account(4, PlatformKimi, true, true)
	ollama.Credentials["base_url"] = "https://ollama.com"
	inactive := account(5, PlatformZhipu, true, true)
	inactive.Status = StatusDisabled
	repo := &fakeCNCheckRepo{byPlatform: map[string][]Account{
		PlatformKimi: {kimi, kimi, ollama}, PlatformDeepseek: {payg, pausedPayg}, PlatformZhipu: {inactive},
	}}
	cfg := &config.Config{}
	cfg.Gateway.CNProviders.BalanceCheckEnabled = true
	svc := NewCNProviderBalanceCheckService(repo, &CNProviderBalanceService{}, &CNProviderQuotaService{}, cfg, time.Hour)
	targets, complete := svc.collectProbeTargets(t.Context())
	svc.observeMonitoringInventory(targets, complete)
	view := svc.MonitoringRuntime()
	require.NotNil(t, view.LastScan)
	require.EqualValues(t, 2, view.LastScan.Targets)
	require.EqualValues(t, 1, view.LastScan.BalanceTargets)
	require.EqualValues(t, 1, view.LastScan.QuotaTargets)
	require.True(t, view.LastScan.Complete)
	encoded, err := json.Marshal(view)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-runtime")
	require.NotContains(t, string(encoded), "api_key")

	svc.observeMonitoringInventory(targets[:1], false)
	view = svc.MonitoringRuntime()
	require.False(t, view.LastScan.Complete, "a partial read must replace, not continue presenting, complete inventory")
	require.EqualValues(t, 1, view.LastScan.Targets)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			for range 40 {
				svc.observeMonitoringInventory(targets, true)
				_ = svc.MonitoringRuntime()
			}
		})
	}
	wg.Wait()
}

func TestCNMonitoringRuntimeStopDoesNotReportDrainedEarly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Gateway.CNProviders.BalanceCheckEnabled = true
		release, cancelled, stopped := make(chan struct{}), make(chan struct{}, 4), make(chan struct{})
		svc := NewCNProviderBalanceCheckService(cnQueueAccounts(t, 100), &CNProviderBalanceService{}, nil, cfg, time.Minute)
		svc.quotaService = cnQueueQuotaProber{query: func(ctx context.Context, _ int64) (*CNProviderQuotaProbeResult, error) {
			<-ctx.Done()
			cancelled <- struct{}{}
			<-release
			return nil, ctx.Err()
		}}
		svc.Start()
		time.Sleep(time.Minute)
		synctest.Wait()
		require.EqualValues(t, 100, svc.MonitoringRuntime().LastScan.Targets)
		go func() { svc.Stop(); close(stopped) }()
		<-cancelled
		synctest.Wait()
		require.Equal(t, "stopping", svc.MonitoringRuntime().State)
		select {
		case <-stopped:
			t.Error("Stop returned before dispatched work exited")
		default:
		}
		close(release)
		<-stopped
		require.Equal(t, "stopped", svc.MonitoringRuntime().State)
	})
}

func TestCNMonitoringRuntimePartialEmptyAndCancelledCollection(t *testing.T) {
	base := cnQueueAccounts(t, 12)
	failed := false
	repo := cnQueueInventoryRepo{list: func(ctx context.Context, platform string) ([]Account, error) {
		if failed && platform == PlatformCline {
			return nil, errors.New("private inventory fixture failure")
		}
		return base.ListByPlatform(ctx, platform)
	}}
	svc := NewCNProviderBalanceCheckService(repo, &CNProviderBalanceService{}, nil, &config.Config{}, time.Minute)
	svc.quotaService = &fakeCNQuotaProber{}
	svc.runOnce(t.Context())
	require.True(t, svc.MonitoringRuntime().LastScan.Complete)
	failed = true
	svc.runOnce(t.Context())
	require.False(t, svc.MonitoringRuntime().LastScan.Complete)
	require.EqualValues(t, 8, svc.MonitoringRuntime().LastScan.Targets)
	prior := *svc.MonitoringRuntime().LastScan
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	svc.runOnce(ctx)
	require.Equal(t, prior, *svc.MonitoringRuntime().LastScan, "a cancelled run must not renew the previous scan")
	failed = false
	clear(base.byPlatform)
	svc.runOnce(t.Context())
	require.True(t, svc.MonitoringRuntime().LastScan.Complete)
	require.Zero(t, svc.MonitoringRuntime().LastScan.Targets)
}

type monitoringRuntimeBudgetRepo struct {
	MonitoringProbeBudgetRepository
	coverage *MonitoringProbeCoverage
	err      error
}

func (r *monitoringRuntimeBudgetRepo) PreviewCoverage(context.Context, MonitoringProbeBudgetSettings) (*MonitoringProbeCoverage, error) {
	return r.coverage, r.err
}

func (r *monitoringRuntimeBudgetRepo) Summary(context.Context) (*MonitoringProbeBudgetSummary, error) {
	return &MonitoringProbeBudgetSummary{Coverage: r.coverage}, r.err
}

func TestCNMonitoringRuntimeBudgetProjectionAndProviderWiring(t *testing.T) {
	policy := DefaultMonitoringProbeBudgetSettings()
	coverage := CompileMonitoringProbeCoverage(policy, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, nil, true, nil, true, nil, true, time.Now())
	repo := &monitoringRuntimeBudgetRepo{coverage: coverage}
	cn := NewCNProviderBalanceCheckService(nil, nil, nil, &config.Config{}, time.Minute)
	probe := ProvideUpstreamBillingProbeService(nil, nil, nil, nil, nil,
		&config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}}, nil, nil, repo, cn)
	t.Cleanup(probe.Stop)
	preview, err := probe.MonitoringBudget().PreviewCoverage(t.Context(), policy)
	require.NoError(t, err)
	summary, err := probe.MonitoringBudget().Summary(t.Context())
	require.NoError(t, err)
	require.Equal(t, cn.MonitoringRuntime().InstanceID, preview.CNQuotaCycle.InstanceID)
	require.Equal(t, preview.CNQuotaCycle.InstanceID, summary.Coverage.CNQuotaCycle.InstanceID)
	require.Nil(t, repo.coverage.CNQuotaCycle, "projection must not mutate shared repository results")
	require.Zero(t, preview.MinimumTotalRequestsPerDay, "inventory targets are not physical-call forecasts")
	require.Contains(t, preview.UnestimatedDemandSources, "account_quota_cycle")

	unknown, err := NewMonitoringProbeBudgetService(repo).PreviewCoverage(t.Context(), policy)
	require.NoError(t, err)
	require.Equal(t, "unavailable", unknown.CNQuotaCycle.State, "legacy or unbound construction stays unknown")
	repo.err = ErrMonitoringProbeBudgetConflict
	_, err = probe.MonitoringBudget().PreviewCoverage(t.Context(), policy)
	require.ErrorIs(t, err, ErrMonitoringProbeBudgetConflict)
	_, err = probe.MonitoringBudget().Summary(t.Context())
	require.ErrorIs(t, err, ErrMonitoringProbeBudgetConflict)
}
