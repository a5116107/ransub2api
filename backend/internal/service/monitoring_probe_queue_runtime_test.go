package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestMonitoringProbeQueueRuntimeBudgetMissingSources(t *testing.T) {
	policy := DefaultMonitoringProbeBudgetSettings()
	coverage := CompileMonitoringProbeCoverage(policy, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, nil, true, nil, true, nil, true, time.Now())
	repo := &monitoringRuntimeBudgetRepo{coverage: coverage}
	budget := NewMonitoringProbeBudgetService(repo)
	for _, read := range []func() (any, error){
		func() (any, error) { return budget.PreviewCoverage(t.Context(), policy) },
		func() (any, error) { return budget.Summary(t.Context()) },
	} {
		out, err := read()
		require.NoError(t, err)
		raw, err := json.Marshal(out)
		require.NoError(t, err)
		var view map[string]any
		require.NoError(t, json.Unmarshal(raw, &view))
		if nested, ok := view["coverage"].(map[string]any); ok {
			view = nested
		}
		require.Contains(t, view, "group_probe_queues", "unbound sources must be explicit unknown observations, not silently absent or zero")
		queues := view["group_probe_queues"].(map[string]any)
		for _, mode := range []string{"v2", "v3"} {
			queue := queues[mode].(map[string]any)
			require.Equal(t, "unavailable", queue["state"])
			require.Nil(t, queue["last_scan"])
			require.Nil(t, queue["cycle"])
		}
	}
}

func TestMonitoringProbeQueueRuntimeDeferralAndRecovery(t *testing.T) {
	var q monitoringProbeQueue
	before := q.runtimeSnapshot()
	require.Equal(t, "unobserved", before.State)
	require.NotEmpty(t, before.InstanceID)
	require.Nil(t, before.LastScan)
	require.Nil(t, before.Cycle)
	require.Equal(t, before.InstanceID, q.runtimeSnapshot().InstanceID)
	keys := make([]monitoringProbeQueueKey, 8)
	for i := range keys {
		keys[i] = monitoringProbeQueueKey{groupID: int64(i + 1), scope: "PRIVATE_SCOPE"}
	}
	orderProbeQueue(t, &q, keys, keys)
	start := q.runtimeSnapshot()
	require.EqualValues(t, 8, start.LastScan.Targets)
	require.EqualValues(t, 8, start.Cycle.Queued)
	firstWaiting := *start.Cycle.OldestPendingAt
	var calls atomic.Int32
	denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_minute", RetryAt: time.Now().UTC().Add(time.Minute)}
	q.run(t.Context(), keys, func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun {
		if calls.Add(1) == 1 {
			return monitoringProbeQueueRun{served: true}
		}
		return monitoringProbeQueueRun{stop: true, budgetDeferral: denial}
	})
	view := q.runtimeSnapshot()
	require.Equal(t, "budget_paused", view.State)
	require.EqualValues(t, calls.Load(), view.Cycle.Finished)
	require.EqualValues(t, int64(calls.Load())-1, view.Cycle.Deferred)
	require.Zero(t, view.Cycle.Active)
	require.Positive(t, view.Cycle.Queued)
	require.EqualValues(t, 8, view.Cycle.Finished+view.Cycle.Queued)
	require.Equal(t, firstWaiting, *view.Cycle.OldestPendingAt)
	require.NotNil(t, view.Cycle.FinishedAt)
	encoded, err := json.Marshal(view)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "PRIVATE_SCOPE")
	view.LastScan.Targets = -1
	view.Cycle.BudgetDeferral.Reason = "changed"
	*view.Cycle.FinishedAt = time.Time{}
	*view.Cycle.OldestPendingAt = time.Time{}
	frozen := q.runtimeSnapshot()
	require.EqualValues(t, 8, frozen.LastScan.Targets)
	require.Equal(t, "request_limit", frozen.Cycle.BudgetDeferral.Reason)
	require.False(t, frozen.Cycle.FinishedAt.IsZero())
	require.Equal(t, firstWaiting, *frozen.Cycle.OldestPendingAt)
	orderProbeQueue(t, &q, keys, keys)
	require.Equal(t, firstWaiting, *q.runtimeSnapshot().Cycle.OldestPendingAt, "another scan must not renew an old wait")
	q.run(t.Context(), keys, func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun {
		return monitoringProbeQueueRun{served: true}
	})
	recovered := q.runtimeSnapshot()
	require.Equal(t, "idle", recovered.State)
	require.EqualValues(t, 8, recovered.Cycle.Finished)
	require.Zero(t, recovered.Cycle.Queued)
	require.Zero(t, recovered.Cycle.Deferred)
	require.Nil(t, recovered.Cycle.BudgetDeferral)
	require.Nil(t, recovered.Cycle.OldestPendingAt)
	require.Equal(t, "budget_paused", frozen.State, "a retained old read is immutable")
	orderProbeQueue(t, &q, keys, nil)
	q.run(t.Context(), nil, nil)
	quiet := q.runtimeSnapshot()
	require.EqualValues(t, 8, quiet.LastScan.Targets)
	require.Zero(t, quiet.LastScan.Due)
	require.Zero(t, quiet.Cycle.Finished)
	q.observeState("not_due")
	require.Equal(t, quiet.LastScan.ObservedAt, q.runtimeSnapshot().LastScan.ObservedAt, "reading or changing activity does not refresh inventory")
	var rebuilt monitoringProbeQueue
	require.NotEqual(t, before.InstanceID, rebuilt.runtimeSnapshot().InstanceID)
	require.Nil(t, rebuilt.runtimeSnapshot().Cycle)
}

func TestMonitoringProbeQueueRuntimeConcurrentReadsDoNotProbe(t *testing.T) {
	var q monitoringProbeQueue
	keys := make([]monitoringProbeQueueKey, 10)
	for i := range keys {
		keys[i].groupID = int64(i + 1)
	}
	orderProbeQueue(t, &q, keys, keys)
	entered, release, done := make(chan struct{}, monitoringProbeQueueWorkers), make(chan struct{}), make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		once.Do(func() { close(release) })
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("the observed queue did not drain")
		}
	})
	var calls atomic.Int32
	go func() {
		defer close(done)
		q.run(ctx, keys, func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun {
			calls.Add(1)
			entered <- struct{}{}
			<-release
			return monitoringProbeQueueRun{served: true}
		})
	}()
	for range monitoringProbeQueueWorkers {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("the observed probes did not start")
		}
	}
	require.Eventually(t, func() bool { return q.runtimeSnapshot().Cycle.Active == monitoringProbeQueueWorkers }, time.Second, time.Millisecond)
	initial := q.runtimeSnapshot()
	var readers sync.WaitGroup
	for range 20 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 100 {
				view := q.runtimeSnapshot()
				if view.InstanceID != initial.InstanceID || view.LastScan.ObservedAt != initial.LastScan.ObservedAt ||
					view.Cycle.Queued+view.Cycle.Active+view.Cycle.Finished != view.LastScan.Due {
					t.Error("inconsistent runtime snapshot")
					return
				}
			}
		}()
	}
	readers.Wait()
	require.EqualValues(t, monitoringProbeQueueWorkers, calls.Load(), "2000 reads must not dispatch probes")
	cancel()
	once.Do(func() { close(release) })
	<-done
	final := q.runtimeSnapshot()
	require.Equal(t, "canceled", final.State)
	require.Zero(t, final.Cycle.Active)
	require.EqualValues(t, monitoringProbeQueueWorkers, final.Cycle.Finished)
	require.EqualValues(t, len(keys)-monitoringProbeQueueWorkers, final.Cycle.Queued)
}

func TestMonitoringProbeQueueRuntimeKeepsStoppingReason(t *testing.T) {
	var q monitoringProbeQueue
	keys := []monitoringProbeQueueKey{{groupID: 1}, {groupID: 2}, {groupID: 3}, {groupID: 4}, {groupID: 5}}
	orderProbeQueue(t, &q, keys, keys)
	release, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("the stopping-reason fixture did not drain")
		}
	})
	go func() {
		defer close(done)
		q.run(t.Context(), keys, func(_ context.Context, key monitoringProbeQueueKey) monitoringProbeQueueRun {
			if key.groupID == 1 {
				return monitoringProbeQueueRun{stop: true, budgetDeferral: &MonitoringProbeBudgetDenial{Reason: "token_limit", Scope: "global_day"}}
			}
			<-release
			return monitoringProbeQueueRun{budgetDeferral: &MonitoringProbeBudgetDenial{Reason: "plugin_metering_unavailable", Scope: "plugin"}}
		})
	}()
	require.Eventually(t, func() bool {
		view := q.runtimeSnapshot()
		return view.Cycle.BudgetDeferral != nil && view.Cycle.BudgetDeferral.Scope == "global_day"
	}, 3*time.Second, time.Millisecond)
	once.Do(func() { close(release) })
	<-done
	view := q.runtimeSnapshot()
	require.Equal(t, "budget_paused", view.State)
	require.Equal(t, "global_day", view.Cycle.BudgetDeferral.Scope)
	require.EqualValues(t, 4, view.Cycle.Deferred)
	require.EqualValues(t, 1, view.Cycle.Queued)
}

func TestMonitoringProbeQueueRuntimeOriginalExecutorsAndReadOnlyProjection(t *testing.T) {
	policy := DefaultMonitoringProbeBudgetSettings()
	coverage := CompileMonitoringProbeCoverage(policy, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, nil, true, nil, true, nil, true, time.Now())
	repo := &monitoringRuntimeBudgetRepo{coverage: coverage}
	router := &groupTestRouterFake{steps: []routeStep{{account: account(77, "chosen")}, {account: account(78, "chosen")}}}
	p, v3Repo, groups := monitorV3ProberFixture(router, func(_ context.Context, _ int64, _ string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
		answer := "21"
		if cfg.QuestionKind == ChannelMonitorV3ProbeQuestionKind {
			answer = monitorV3Answer(cfg.Prompt)
		}
		return &ScheduledTestResult{Status: "success", ResponseText: answer}, nil
	})
	t.Cleanup(p.Stop)
	candyRepo := &candyRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: candyConfigFixture()}}
	candy := candyServiceFixture(candyRepo, groups)
	probe := ProvideUpstreamBillingProbeService(nil, nil, nil, nil, nil,
		&config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}}, nil, nil, repo, nil, nil, &ChannelMonitorV2Service{candy: candy}, p)
	t.Cleanup(probe.Stop)
	budget := probe.MonitoringBudget()
	preview, err := budget.PreviewCoverage(t.Context(), policy)
	require.NoError(t, err)
	require.Equal(t, "unobserved", preview.GroupProbeQueues.V2.State)
	require.Equal(t, "unobserved", preview.GroupProbeQueues.V3.State)
	require.Nil(t, coverage.GroupProbeQueues, "projecting runtime must not mutate repository results")
	require.Empty(t, v3Repo.claims)
	require.Empty(t, candyRepo.claims)
	candy.RunDue(t.Context(), groupTestNow)
	p.RunDue(t.Context(), monitorV3ProbeTick)
	summary, err := budget.Summary(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, summary.Coverage.GroupProbeQueues.V2.Cycle.Finished)
	require.EqualValues(t, 1, summary.Coverage.GroupProbeQueues.V3.Cycle.Finished)
	require.Nil(t, preview.GroupProbeQueues.V2.Cycle, "an earlier GET cannot acquire a later cycle")
	require.Zero(t, summary.Coverage.MinimumTotalRequestsPerDay, "queue scopes do not invent physical demand")
	v3Scan := p.MonitoringRuntime().LastScan.ObservedAt
	p.RunDue(t.Context(), monitorV3ProbeTick.Add(time.Minute))
	require.Equal(t, "not_due", p.MonitoringRuntime().State)
	require.Equal(t, v3Scan, p.MonitoringRuntime().LastScan.ObservedAt)
	p.repo = &monitorV3LifecycleRepo{monitorV3RepoFake: v3Repo, read: func(context.Context) (*ChannelMonitorV3Config, error) { return nil, errors.New("fixture read failed") }}
	p.RunDue(t.Context(), monitorV3ProbeTick.Add(5*time.Minute))
	require.Equal(t, "inventory_unavailable", p.MonitoringRuntime().State)
	require.Equal(t, v3Scan, p.MonitoringRuntime().LastScan.ObservedAt)
	p.repo = v3Repo
	v3Repo.components = nil
	p.RunDue(t.Context(), monitorV3ProbeTick.Add(10*time.Minute))
	require.Zero(t, p.MonitoringRuntime().LastScan.Targets, "only a real complete empty read proves zero targets")
	require.Equal(t, "idle", p.MonitoringRuntime().State)
	require.Len(t, router.released, 2, "snapshot reads and empty scans must not reserve an account")
	require.True(t, slices.Contains(summary.Coverage.UnestimatedDemandSources, "v3_idle_probes"))
}
