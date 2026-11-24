package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type monitorV3LifecycleRepo struct {
	*monitorV3RepoFake
	reads  atomic.Int32
	read   func(context.Context) (*ChannelMonitorV3Config, error)
	finish func(context.Context, ChannelMonitorV3ProbeResult) error
}

func TestChannelMonitorV3FairBudgetCoverage(t *testing.T) {
	const targets, capacity, rounds = 100, 4, 25
	steps := make([]routeStep, targets*rounds)
	for i := range steps {
		steps[i].account = account(77, "shared probe account")
	}
	var calls atomic.Int32
	var mu sync.Mutex
	covered := make(map[string]int)
	p, repo, _ := monitorV3ProberFixture(&groupTestRouterFake{steps: steps}, func(_ context.Context, _ int64, model string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
		if calls.Add(1) > capacity {
			return &ScheduledTestResult{BudgetDeferral: &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_minute"}}, nil
		}
		mu.Lock()
		covered[model]++
		mu.Unlock()
		return &ScheduledTestResult{Status: "success", ResponseText: monitorV3Answer(cfg.Prompt)}, nil
	})
	t.Cleanup(p.Stop)
	repo.components = nil
	for i := range targets {
		repo.components = append(repo.components, ChannelMonitorV3Component{ID: int64(i + 1), GroupID: 4, Model: fmt.Sprintf("probe-%03d", i), Enabled: true})
	}
	for round := range rounds {
		calls.Store(0)
		p.RunDue(t.Context(), monitorV3ProbeTick.Add(time.Duration(round)*5*time.Minute))
		require.LessOrEqual(t, int(calls.Load()), capacity+monitoringProbeQueueWorkers, "exhaustion must stop claiming the remaining queue; in-flight calls may finish")
		var latest int64
		for _, turn := range p.queue.turns {
			if turn.Served > latest {
				latest = turn.Served
			}
		}
		cohort := 0
		for _, turn := range p.queue.turns {
			if turn.Served == latest {
				cohort++
			}
		}
		require.Equal(t, capacity, cohort, "one completed round remains one tier, not one serial tier per result")
	}
	require.Len(t, covered, targets, "every stable target must receive one turn before any served target receives a second")
	for model, count := range covered {
		require.Equal(t, 1, count, model)
	}
}

func TestChannelMonitorV3FairPartialChainsYield(t *testing.T) {
	var calls atomic.Int32
	var native atomic.Int32
	steps := make([]routeStep, 4)
	for i := range steps {
		steps[i].account = account(77, "chosen")
	}
	p, repo, _ := monitorV3ProberFixture(&groupTestRouterFake{steps: steps}, func(_ context.Context, _ int64, model string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
		denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_minute"}
		if calls.Add(1) > 1 {
			return &ScheduledTestResult{BudgetDeferral: denial}, nil
		}
		if model == "partial" {
			return &ScheduledTestResult{monitoringBudgetAdmitted: true, BudgetDeferral: denial}, nil
		}
		native.Add(1)
		return &ScheduledTestResult{Status: "success", ResponseText: monitorV3Answer(cfg.Prompt)}, nil
	})
	t.Cleanup(p.Stop)
	repo.components = []ChannelMonitorV3Component{{ID: 1, GroupID: 4, Model: "partial", Enabled: true}, {ID: 2, GroupID: 4, Model: "native", Enabled: true}}
	// Dispatch the partial chain first, without relying on goroutine ordering.
	targets := channelMonitorV3ProbeTargets(repo.config.IdleProbe, repo.components)
	keys := make([]monitoringProbeQueueKey, 0, len(targets))
	for _, target := range targets {
		keys = append(keys, target.key())
	}
	require.True(t, p.queue.order(t.Context(), repo, GroupMonitoringQueueConfig{}, v3QueueIdentities(targets, repo.config.IntervalMinutes), keys))
	repo.setAge(monitoringProbeQueueKey{groupID: 4, scope: "partial"}, 1, 0)
	repo.setAge(monitoringProbeQueueKey{groupID: 4, scope: "native"}, 100, 0)
	p.RunDue(t.Context(), monitorV3ProbeTick)
	require.Zero(t, native.Load())
	calls.Store(0)
	p.RunDue(t.Context(), monitorV3ProbeTick.Add(5*time.Minute))
	require.EqualValues(t, 1, native.Load(), "a deferred chain that already spent budget must yield to pending work")
}

func TestChannelMonitorV3OlderTierDrainsBeforeRecentWork(t *testing.T) {
	entered, release, younger, done := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1), make(chan struct{})
	var releaseOnce sync.Once
	router := &groupTestRouterFake{steps: []routeStep{{account: account(77, "old")}, {account: account(78, "recent")}}}
	p, repo, _ := monitorV3ProberFixture(router, func(_ context.Context, _ int64, model string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
		if model == "old" {
			close(entered)
			<-release
			return &ScheduledTestResult{BudgetDeferral: &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day"}}, nil
		}
		younger <- struct{}{}
		return &ScheduledTestResult{Status: "success", ResponseText: monitorV3Answer(cfg.Prompt)}, nil
	})
	t.Cleanup(p.Stop)
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("tier fixture did not drain")
		}
	})
	repo.components = []ChannelMonitorV3Component{{ID: 1, GroupID: 4, Model: "old", Enabled: true}, {ID: 2, GroupID: 4, Model: "recent", Enabled: true}}
	targets := channelMonitorV3ProbeTargets(repo.config.IdleProbe, repo.components)
	keys := make([]monitoringProbeQueueKey, 0, len(targets))
	for _, target := range targets {
		keys = append(keys, target.key())
	}
	require.True(t, p.queue.order(t.Context(), repo, GroupMonitoringQueueConfig{}, v3QueueIdentities(targets, repo.config.IntervalMinutes), keys))
	repo.setAge(monitoringProbeQueueKey{groupID: 4, scope: "old"}, 1, 0)
	repo.setAge(monitoringProbeQueueKey{groupID: 4, scope: "recent"}, 100, 0)
	go func() { p.RunDue(t.Context(), monitorV3ProbeTick); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the pending target did not start")
	}
	select {
	case <-younger:
		t.Fatal("recent work raced an older in-flight target for the last budget")
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the probe did not finish")
	}
	require.Len(t, repo.claims, 1, "global exhaustion stops the younger tier before claiming it")
	require.Len(t, repo.finished, 1)
	require.Equal(t, ChannelMonitorV3ProbeDeferred, repo.finished[0].Outcome)
}

func TestChannelMonitorV3BudgetDispatchOutcomes(t *testing.T) {
	for _, scope := range []string{"key", "key_minute", "site", "site_minute", "plugin", "global_minute", "global_day", "coordinator", "unknown"} {
		for _, admitted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admitted=%t", scope, admitted), func(t *testing.T) {
				denial := &MonitoringProbeBudgetDenial{Reason: "fixture_denial", Scope: scope}
				p, repo, _ := monitorV3ProberFixture(&groupTestRouterFake{steps: []routeStep{{account: account(77, "chosen")}}}, func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
					return &ScheduledTestResult{monitoringBudgetAdmitted: admitted, BudgetDeferral: denial}, nil
				})
				t.Cleanup(p.Stop)
				run := p.runOne(t.Context(), channelMonitorV3ProbeTarget{GroupID: 4, Model: "gpt-probe"}, monitorV3ProbeTick, 5)
				require.Equal(t, admitted, run.served, "admitted partial chains must yield their next turn")
				require.Equal(t, !slices.Contains([]string{"key", "key_minute", "site", "site_minute", "plugin"}, scope), run.stop)
				require.Len(t, repo.finished, 1)
				require.Equal(t, ChannelMonitorV3ProbeDeferred, repo.finished[0].Outcome)
				require.Empty(t, repo.finished[0].ErrorMessage)
			})
		}
	}
}

func TestChannelMonitorV3CanceledAdmissionDoesNotPublish(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p, repo, _ := monitorV3ProberFixture(&groupTestRouterFake{steps: []routeStep{{account: account(77, "chosen")}}}, func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
		cancel()
		return &ScheduledTestResult{monitoringBudgetAdmitted: true, Status: "failed", ErrorMessage: context.Canceled.Error()}, nil
	})
	t.Cleanup(p.Stop)
	run := p.runOne(ctx, channelMonitorV3ProbeTarget{GroupID: 4, Model: "gpt-probe"}, monitorV3ProbeTick, 5)
	require.True(t, run.served, "cancellation must not erase an earlier physical admission")
	require.Empty(t, repo.finished, "queue accounting is not a late health observation")
}

func TestChannelMonitorV3LocalDeferralKeepsIndependentRoutes(t *testing.T) {
	for _, scope := range []string{"key", "key_minute", "site", "site_minute", "plugin"} {
		t.Run(scope, func(t *testing.T) {
			var calls atomic.Int32
			router := &groupTestRouterFake{steps: []routeStep{{account: account(77, "chosen")}, {account: account(78, "other")}}}
			p, repo, _ := monitorV3ProberFixture(router, func(_ context.Context, _ int64, model string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
				calls.Add(1)
				if model == "blocked" {
					return &ScheduledTestResult{BudgetDeferral: &MonitoringProbeBudgetDenial{Reason: "fixture_denial", Scope: scope}}, nil
				}
				return &ScheduledTestResult{Status: "success", ResponseText: monitorV3Answer(cfg.Prompt)}, nil
			})
			t.Cleanup(p.Stop)
			repo.components = []ChannelMonitorV3Component{{ID: 1, GroupID: 4, Model: "blocked", Enabled: true}, {ID: 2, GroupID: 4, Model: "independent", Enabled: true}}
			p.RunDue(t.Context(), monitorV3ProbeTick)
			require.EqualValues(t, 2, calls.Load())
			require.Len(t, repo.finished, 2)
			require.Equal(t, 1, len(slices.DeleteFunc(slices.Clone(repo.finished), func(r ChannelMonitorV3ProbeResult) bool { return r.Outcome != ChannelMonitorV3ProbeSuccess })))
		})
	}
}

func (r *monitorV3LifecycleRepo) FinishProbe(ctx context.Context, result ChannelMonitorV3ProbeResult) error {
	if r.finish != nil {
		return r.finish(ctx, result)
	}
	return r.monitorV3RepoFake.FinishProbe(ctx, result)
}

func (r *monitorV3LifecycleRepo) GetConfig(ctx context.Context) (*ChannelMonitorV3Config, error) {
	r.reads.Add(1)
	if r.read != nil {
		return r.read(ctx)
	}
	return r.monitorV3RepoFake.GetConfig(ctx)
}

func TestChannelMonitorV3StoppedProberStartsNoNewWork(t *testing.T) {
	p, base, _ := monitorV3ProberFixture(&groupTestRouterFake{}, nil)
	repo := &monitorV3LifecycleRepo{monitorV3RepoFake: base}
	p.repo = repo
	p.Stop()
	p.Start()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.RunDue(t.Context(), monitorV3ProbeTick)
			p.Stop()
		}()
	}
	wg.Wait()
	require.Zero(t, repo.reads.Load(), "a stopped executor must not query configuration or reserve new probes")
	require.Empty(t, base.claims)
}

func TestChannelMonitorV3StopCancelsAndDrainsEveryCaller(t *testing.T) {
	p, base, _ := monitorV3ProberFixture(&groupTestRouterFake{}, nil)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("the active probe did not drain during cleanup")
		}
		p.Stop()
	})
	repo := &monitorV3LifecycleRepo{monitorV3RepoFake: base, read: func(ctx context.Context) (*ChannelMonitorV3Config, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release // A storage driver may still be returning after cancellation.
		return nil, ctx.Err()
	}}
	p.repo = repo
	go func() { p.RunDue(ctx, monitorV3ProbeTick); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the probe did not enter its configuration read")
	}
	// Overlapping ticks still skip instead of starting a second inventory read.
	p.RunDue(t.Context(), monitorV3ProbeTick)
	require.EqualValues(t, 1, repo.reads.Load())
	returned := make(chan struct{}, 20)
	for range 20 {
		go func() { p.Stop(); returned <- struct{}{} }()
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the active direct RunDue call")
	}
	select {
	case <-returned:
		t.Fatal("Stop returned before the active configuration read exited")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	for range 20 {
		select {
		case <-returned:
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent Stop callers did not await the same drain")
		}
	}
	p.RunDue(t.Context(), monitorV3ProbeTick)
	require.EqualValues(t, 1, repo.reads.Load())
	require.Empty(t, base.claims)
}

func TestChannelMonitorV3StopCancelsPhysicalHTTPAndQueuedTargets(t *testing.T) {
	entered, release := make(chan struct{}, monitoringProbeQueueWorkers), make(chan struct{})
	var releaseOnce sync.Once
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	steps := make([]routeStep, monitoringProbeQueueWorkers)
	for i := range steps {
		steps[i] = routeStep{account: account(int64(i+1), "physical probe")}
	}
	router := &groupTestRouterFake{steps: steps}
	var returned atomic.Int32
	p, repo, _ := monitorV3ProberFixture(router, func(ctx context.Context, _ int64, _ string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
		defer returned.Add(1)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader("{}"))
		if err != nil {
			return nil, err
		}
		response, err := server.Client().Do(request)
		if response != nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		return nil, err
	})
	t.Cleanup(p.Stop)
	repo.components = nil
	for i := range 100 {
		repo.components = append(repo.components, ChannelMonitorV3Component{ID: int64(i + 1), GroupID: 4, Model: "probe-" + strconv.Itoa(i), Enabled: true})
	}
	runDone := make(chan struct{})
	go func() { p.RunDue(t.Context(), monitorV3ProbeTick); close(runDone) }()
	for range monitoringProbeQueueWorkers {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the bounded worker pool did not reach its physical requests")
		}
	}
	stopped := make(chan struct{})
	go func() { p.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not cancel and await the physical requests")
	}
	<-runDone
	require.EqualValues(t, monitoringProbeQueueWorkers, sends.Load())
	require.EqualValues(t, monitoringProbeQueueWorkers, returned.Load())
	require.Len(t, repo.claims, monitoringProbeQueueWorkers, "queued targets must not acquire a slot after cancellation")
	require.Empty(t, repo.finished, "shutdown is not a failed or passing observation")
	require.Len(t, router.released, monitoringProbeQueueWorkers)
	p.RunDue(t.Context(), monitorV3ProbeTick.Add(time.Hour))
	require.Len(t, repo.claims, monitoringProbeQueueWorkers)
}

func TestChannelMonitorV3StopWaitsForBoundedResultSave(t *testing.T) {
	router := &groupTestRouterFake{steps: []routeStep{{account: account(77, "completed")}}}
	p, base, _ := monitorV3ProberFixture(router, func(_ context.Context, _ int64, _ string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
		return &ScheduledTestResult{Status: "success", ResponseText: monitorV3Answer(cfg.Prompt)}, nil
	})
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); p.Stop() })
	var saveCanceled atomic.Bool
	p.repo = &monitorV3LifecycleRepo{monitorV3RepoFake: base, finish: func(ctx context.Context, result ChannelMonitorV3ProbeResult) error {
		close(entered)
		<-release
		saveCanceled.Store(ctx.Err() != nil)
		return base.FinishProbe(ctx, result)
	}}
	done := make(chan struct{})
	go func() { p.RunDue(t.Context(), monitorV3ProbeTick); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("a completed answer did not reach result persistence")
	}
	stopped := make(chan struct{})
	go func() { p.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned before the independent bounded save completed")
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("the prober did not drain after its save completed")
	}
	<-done
	require.False(t, saveCanceled.Load())
	require.Len(t, base.finished, 1)
	require.Equal(t, ChannelMonitorV3ProbeSuccess, base.finished[0].Outcome)
}

func TestChannelMonitorV3CanceledCallerDoesNotStopService(t *testing.T) {
	p, base, _ := monitorV3ProberFixture(&groupTestRouterFake{}, nil)
	t.Cleanup(p.Stop)
	repo := &monitorV3LifecycleRepo{monitorV3RepoFake: base}
	p.repo = repo
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p.RunDue(ctx, monitorV3ProbeTick)
	require.Zero(t, repo.reads.Load())
	p.RunDue(t.Context(), monitorV3ProbeTick)
	require.EqualValues(t, 1, repo.reads.Load(), "caller cancellation must not terminate the service lifetime")
	require.Len(t, base.claims, 1)
}

func TestChannelMonitorV3ConcurrentStartStopAndTicks(t *testing.T) {
	for range 50 {
		p, base, _ := monitorV3ProberFixture(&groupTestRouterFake{}, nil)
		repo := &monitorV3LifecycleRepo{monitorV3RepoFake: base}
		p.repo = repo
		var wg sync.WaitGroup
		for range 10 {
			wg.Add(3)
			go func() { defer wg.Done(); p.Start() }()
			go func() { defer wg.Done(); p.Stop() }()
			go func() { defer wg.Done(); p.RunDue(t.Context(), monitorV3ProbeTick) }()
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent startup, ticks and shutdown did not drain")
		}
		before := repo.reads.Load()
		p.Start()
		p.RunDue(t.Context(), monitorV3ProbeTick.Add(time.Hour))
		p.Stop()
		require.Equal(t, before, repo.reads.Load(), "a terminated service must not be restarted by repeated Start")
	}
}

func TestChannelMonitorV3ProbesJudgeOnlyQuietSlots(t *testing.T) {
	cfg := monitorV3Config()
	cfg.MinRequests = 3
	component := ChannelMonitorV3Component{ID: 1, GroupID: 4}
	first := monitorV3Slot(7)
	slot := func(i int) time.Time { return first.Add(time.Duration(i) * 5 * time.Minute) }
	probe := func(i int, category string, latency int64) ChannelMonitorV3ProbeFact {
		return ChannelMonitorV3ProbeFact{GroupID: 4, Model: "gpt-probe", Slot: slot(i), Category: category, Count: 1, LatencyP50Ms: latency}
	}
	facts := &ChannelMonitorV3Facts{
		Metrics: []ChannelMonitorV3Fact{
			{GroupID: 4, Model: "gpt", Slot: slot(0), Success: 10},
			{GroupID: 4, Model: "gpt", Slot: slot(5), Success: 1},
			{GroupID: 4, Model: "gpt", Slot: slot(7), Success: 2},
		},
		Probes: []ChannelMonitorV3ProbeFact{
			probe(0, "upstream_5xx", 0), // real traffic was enough; the probe is ignored
			probe(1, "", 2000),
			probe(2, "", 12000),
			probe(3, "upstream_5xx", 0),
			probe(4, "model_unsupported", 0), // the probe asked for something the group does not serve
			probe(5, "", 1500),               // one real request is below the minimum
			probe(6, "authentication", 0),    // user-error filters do not cover probes
		},
	}
	cells := channelMonitorV3Cells(cfg, component, facts, first, 8, 5*time.Minute, true)
	statuses := make([]string, len(cells))
	for i, cell := range cells {
		if cell != nil {
			statuses[i] = cell.Status
		}
	}
	require.Equal(t, []string{
		ChannelMonitorV3StatusOperational,
		ChannelMonitorV3StatusOperational,
		ChannelMonitorV3StatusDegraded,
		ChannelMonitorV3StatusDown,
		"",
		ChannelMonitorV3StatusOperational,
		ChannelMonitorV3StatusDown,
		ChannelMonitorV3StatusInsufficient,
	}, statuses)
	require.False(t, cells[0].Probed)
	require.Equal(t, int64(10), cells[0].Requests)
	require.Equal(t, int64(1), cells[0].Probes, "admins still see that a probe ran")
	require.True(t, cells[1].Probed)
	require.Equal(t, int64(2000), *cells[1].ProbeLatencyMs)
	require.Nil(t, cells[1].TTFTP50Ms)
	require.InDelta(t, 1.0, cells[1].SuccessRate, 1e-9)
	require.Equal(t, "upstream_5xx", cells[3].TopError)
	require.Zero(t, cells[3].SuccessRate)
	require.Equal(t, int64(1), cells[3].ProbeErrors)
	require.True(t, cells[5].Probed)
	require.Equal(t, int64(1), cells[5].Requests)
	require.Equal(t, "authentication", cells[6].TopError)

	// The component threshold applies to probe latency too.
	component.DegradedTTFTMs = 15000
	require.Equal(t, ChannelMonitorV3StatusOperational, channelMonitorV3Cells(cfg, component, facts, first, 8, 5*time.Minute, true)[2].Status)
	// Users learn a slot was probed, never the volumes.
	userCell := channelMonitorV3Cells(cfg, component, facts, first, 8, 5*time.Minute, false)[3]
	require.True(t, userCell.Probed)
	require.Zero(t, userCell.Probes)
	require.Zero(t, userCell.ProbeErrors)
}

func TestChannelMonitorV3ProbesFollowTheComponentModel(t *testing.T) {
	cfg := monitorV3Config()
	first := monitorV3Slot(0)
	facts := &ChannelMonitorV3Facts{Probes: []ChannelMonitorV3ProbeFact{
		{GroupID: 4, Model: "gpt-a", Slot: first, Count: 1, LatencyP50Ms: 1000},
		{GroupID: 4, Model: "gpt-b", Slot: first, Category: "timeout", Count: 1},
	}}
	whole := channelMonitorV3Cells(cfg, ChannelMonitorV3Component{GroupID: 4}, facts, first, 1, 5*time.Minute, true)[0]
	require.Equal(t, ChannelMonitorV3StatusDown, whole.Status, "half the group's probes failed")
	require.Equal(t, int64(2), whole.Probes)
	one := channelMonitorV3Cells(cfg, ChannelMonitorV3Component{GroupID: 4, Model: "gpt-a"}, facts, first, 1, 5*time.Minute, true)[0]
	require.Equal(t, ChannelMonitorV3StatusOperational, one.Status)
	require.Equal(t, int64(1), one.Probes)
}

func monitorV3ProbeStatusFixture() *monitorV3RepoFake {
	repo := monitorV3StatusFixture()
	repo.config.IdleProbe = ChannelMonitorV3IdleProbe{Enabled: true, Groups: []ChannelMonitorV3IdleProbeGroup{{GroupID: 4, Model: "gpt"}}}
	// Group 4 has no real traffic in slot 2; group 5 is not checked.
	repo.probeSlots = []ChannelMonitorV3ProbeFact{
		{GroupID: 4, Model: "gpt", Slot: monitorV3Slot(2), Category: "timeout", Count: 1},
		{GroupID: 5, Model: "claude", Slot: monitorV3Slot(2), Count: 1, LatencyP50Ms: 900},
	}
	repo.probeTotals = []ChannelMonitorV3ProbeFact{{GroupID: 4, Model: "gpt", Count: 150, LatencyP50Ms: 900}}
	return repo
}

func TestChannelMonitorV3StatusReadsProbesOfCheckedGroups(t *testing.T) {
	repo := monitorV3ProbeStatusFixture()
	status, err := monitorV3Service(repo).Status(context.Background(), ChannelMonitorV3Viewer{Admin: true}, nil)
	require.NoError(t, err)
	require.True(t, status.IdleProbe)
	for _, call := range repo.probeCalls {
		require.Equal(t, []int64{4}, call.groups, "only checked groups are read")
	}
	require.Len(t, repo.probeCalls, 2, "slots and availability totals")
	require.Zero(t, repo.probeCalls[1].interval)
	codex := status.Categories[1].Components[0]
	require.Equal(t, "Codex", codex.Name)
	require.True(t, codex.Cells[27].Probed)
	require.Equal(t, ChannelMonitorV3StatusDown, codex.Cells[27].Status)
	// Totals: 800 answered of 1000 counted real requests (50 cancellations are
	// not counted), plus 150 successful probes.
	require.InDelta(t, 950.0*100/1150, *codex.Availability, 1e-9)
	require.Equal(t, int64(150), codex.Probes)
	public := status.Categories[1].Components[1]
	require.Nil(t, public.Cells[27], "probes of unchecked groups are not shown")

	// Turning the switch off brings the page back to real traffic only.
	repo.config.IdleProbe.Enabled = false
	repo.probeCalls = nil
	status, err = monitorV3Service(repo).Status(context.Background(), ChannelMonitorV3Viewer{Admin: true}, nil)
	require.NoError(t, err)
	require.False(t, status.IdleProbe)
	require.Empty(t, repo.probeCalls)
	require.Nil(t, status.Categories[1].Components[0].Cells[27])
}

func TestChannelMonitorV3ProbeFailuresBecomeIncidents(t *testing.T) {
	repo := &monitorV3RepoFake{config: monitorV3Config()}
	repo.config.IdleProbe = ChannelMonitorV3IdleProbe{Enabled: true, Groups: []ChannelMonitorV3IdleProbeGroup{{GroupID: 4, Model: "gpt"}}}
	repo.components = []ChannelMonitorV3Component{{ID: 10, Name: "Quiet", GroupID: 4, Enabled: true, Visibility: ChannelMonitorV3VisibilityPublic}}
	for _, back := range []int{4, 3, 1} {
		repo.probeSlots = append(repo.probeSlots, ChannelMonitorV3ProbeFact{GroupID: 4, Model: "gpt", Slot: monitorV3Slot(back), Category: "upstream_5xx", Count: 1})
	}
	repo.probeSlots = append(repo.probeSlots, ChannelMonitorV3ProbeFact{GroupID: 4, Model: "gpt", Slot: monitorV3Slot(2), Count: 1, LatencyP50Ms: 800})
	s := monitorV3Service(repo)
	page, err := s.Incidents(context.Background(), ChannelMonitorV3Viewer{Admin: true}, 1, 20)
	require.NoError(t, err)
	require.Len(t, page.Items, 1, "one probed red slot alone is a blip")
	require.Equal(t, monitorV3Slot(4), page.Items[0].StartedAt)
	require.Equal(t, monitorV3Slot(2), *page.Items[0].EndedAt)
	require.Equal(t, int64(2), page.Items[0].Probes)
	require.Equal(t, "upstream_5xx", page.Items[0].TopError)
	page, err = s.Incidents(context.Background(), ChannelMonitorV3Viewer{AllowedGroups: map[int64]bool{4: true}}, 1, 20)
	require.NoError(t, err)
	require.Zero(t, page.Items[0].Probes, "users never get volumes")
}

func TestChannelMonitorV3UpdateIdleProbe(t *testing.T) {
	repo := &monitorV3RepoFake{config: monitorV3Config()}
	repo.components = []ChannelMonitorV3Component{
		{ID: 1, GroupID: 4, Enabled: true},
		{ID: 2, GroupID: 5, Model: "claude-opus-5-5", Enabled: true},
	}
	s := monitorV3Service(repo)
	ctx := context.Background()
	for name, input := range map[string]ChannelMonitorV3IdleProbe{
		"duplicate":       {Groups: []ChannelMonitorV3IdleProbeGroup{{GroupID: 5}, {GroupID: 5}}},
		"no group":        {Groups: []ChannelMonitorV3IdleProbeGroup{{GroupID: 0}}},
		"unknown group":   {Groups: []ChannelMonitorV3IdleProbeGroup{{GroupID: 99, Model: "x"}}},
		"bad effort":      {Groups: []ChannelMonitorV3IdleProbeGroup{{GroupID: 5, ReasoningEffort: "turbo"}}},
		"long model":      {Groups: []ChannelMonitorV3IdleProbeGroup{{GroupID: 4, Model: string(make([]rune, 101))}}},
		"model for whole": {Enabled: true, Groups: []ChannelMonitorV3IdleProbeGroup{{GroupID: 4}}},
	} {
		_, err := s.UpdateIdleProbe(ctx, input)
		require.ErrorIs(t, err, ErrChannelMonitorV3Invalid, name)
	}
	saved, err := s.UpdateIdleProbe(ctx, ChannelMonitorV3IdleProbe{Enabled: true, Groups: []ChannelMonitorV3IdleProbeGroup{
		{GroupID: 4, Model: " gpt-5.5 ", ReasoningEffort: " LOW "},
		{GroupID: 5}, // its component names the model to probe
	}})
	require.NoError(t, err)
	want := ChannelMonitorV3IdleProbe{Enabled: true, Groups: []ChannelMonitorV3IdleProbeGroup{{GroupID: 4, Model: "gpt-5.5", ReasoningEffort: "low"}, {GroupID: 5}}}
	require.Equal(t, want, *saved)
	require.Equal(t, want, repo.config.IdleProbe)
	require.Equal(t, 1, repo.config.Version, "the page config version is left alone")

	settings, err := s.Settings(ctx)
	require.NoError(t, err)
	require.Equal(t, want, settings.IdleProbe)
	require.NotNil(t, settings.IdleProbeStats)
}

// monitorV3ProbeTick is 30 seconds before the five-minute slot that started at 09:30 ends.
var monitorV3ProbeTick = time.Date(2026, 10, 11, 9, 34, 30, 0, time.UTC)

var monitorV3ChallengePattern = regexp.MustCompile(`(\d+) ([+-]) (\d+) = \?\nA:$`)

// monitorV3Answer solves the V1 challenge in a probe prompt.
func monitorV3Answer(prompt string) string {
	match := monitorV3ChallengePattern.FindStringSubmatch(prompt)
	a, _ := strconv.Atoi(match[1])
	b, _ := strconv.Atoi(match[3])
	if match[2] == "-" {
		return strconv.Itoa(a - b)
	}
	return strconv.Itoa(a + b)
}

type monitorV3AccountRun func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error)

func TestChannelMonitorV3MonitoringBudgetIsNeutral(t *testing.T) {
	router := &groupTestRouterFake{steps: []routeStep{{account: account(77, "chosen")}, {account: account(88, "unused")}}}
	denial := &MonitoringProbeBudgetDenial{Reason: "token_limit", Scope: "global_day", RetryAt: groupTestNow.Add(time.Hour)}
	prober, repo, _ := monitorV3ProberFixture(router, func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
		return &ScheduledTestResult{Status: "failed", QualityJudgment: &QualityJudgment{Verdict: "unknown", BudgetDeferral: denial}}, nil
	})
	prober.runOne(t.Context(), channelMonitorV3ProbeTarget{GroupID: 4, Model: "gpt-probe"}, groupTestNow, 5)
	require.Len(t, repo.finished, 1)
	require.Equal(t, "deferred", repo.finished[0].Outcome)
	require.Equal(t, "monitoring_budget_deferred", repo.finished[0].Category)
	require.Empty(t, repo.finished[0].ErrorMessage)
	require.Len(t, router.excluded, 1)
}

func monitorV3ProberFixture(router *groupTestRouterFake, run monitorV3AccountRun) (*ChannelMonitorV3Prober, *monitorV3RepoFake, *PelicanGroupTestService) {
	repo := &monitorV3RepoFake{config: monitorV3Config()}
	repo.config.IdleProbe = ChannelMonitorV3IdleProbe{Enabled: true, Groups: []ChannelMonitorV3IdleProbeGroup{{GroupID: 4, Model: "gpt-probe", ReasoningEffort: "low"}}}
	repo.components = []ChannelMonitorV3Component{
		{ID: 1, Name: "Whole group", GroupID: 4, Enabled: true, GroupStatus: StatusActive},
		{ID: 2, Name: "Unchecked", GroupID: 5, Enabled: true, GroupStatus: StatusActive},
	}
	groups, _ := newGroupTestService(newGroupTestRepoFake(), router, run)
	p := NewChannelMonitorV3Prober(repo, groups, channelMonitorV2RuntimeStub{rt: ChannelMonitorRuntime{Enabled: true, Mode: ChannelMonitorModeV3}})
	p.now = func() time.Time { return monitorV3ProbeTick }
	return p, repo, groups
}

func monitorV3Claims(repo *monitorV3RepoFake) []string {
	keys := []string{}
	for key := range repo.claims {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func TestChannelMonitorV3ProberProbesQuietSlotsOnce(t *testing.T) {
	asked := []*PelicanTestConfig{}
	router := &groupTestRouterFake{steps: []routeStep{{account: account(77, "scheduler choice")}}}
	p, repo, _ := monitorV3ProberFixture(router, func(_ context.Context, id int64, model string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
		require.Equal(t, int64(77), id)
		require.Equal(t, "gpt-probe", model)
		asked = append(asked, cfg)
		return &ScheduledTestResult{Status: "success", ResponseText: monitorV3Answer(cfg.Prompt)}, nil
	})
	p.RunDue(context.Background(), monitorV3ProbeTick)

	require.Equal(t, []string{"4/gpt-probe/2026-10-11T09:30:00Z/5"}, monitorV3Claims(repo))
	require.Len(t, repo.finished, 1)
	require.Equal(t, ChannelMonitorV3ProbeSuccess, repo.finished[0].Outcome)
	require.Empty(t, repo.finished[0].Category)
	require.Equal(t, int64(77), repo.finished[0].AccountID)
	require.Len(t, asked, 1)
	require.Equal(t, ChannelMonitorV3ProbeQuestionKind, asked[0].QuestionKind)
	require.Equal(t, "low", asked[0].ReasoningEffort)
	require.Contains(t, asked[0].Prompt, "Calculate and respond with ONLY the number")
	require.Equal(t, []monitorV3SlotCall{{groups: []int64{4}, from: time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC), to: monitorV3ProbeTick}}, repo.countCalls)
	require.Equal(t, []int64{77}, router.released)

	// The claim keeps a slot from being probed twice, also across instances.
	p.RunDue(context.Background(), monitorV3ProbeTick)
	require.Len(t, repo.finished, 1)
	require.Len(t, asked, 1)
}

func TestChannelMonitorV3ProberGates(t *testing.T) {
	for name, mutate := range map[string]func(*ChannelMonitorV3Prober, *monitorV3RepoFake, *PelicanGroupTestService){
		"not the last minute": func(p *ChannelMonitorV3Prober, _ *monitorV3RepoFake, _ *PelicanGroupTestService) {
			p.now = func() time.Time { return monitorV3ProbeTick.Add(-2 * time.Minute) }
		},
		"enough real traffic": func(_ *ChannelMonitorV3Prober, r *monitorV3RepoFake, _ *PelicanGroupTestService) {
			r.successCounts = []ChannelMonitorV3Fact{{GroupID: 4, Model: "gpt-5.5", Success: 1}}
		},
		"switched off": func(_ *ChannelMonitorV3Prober, r *monitorV3RepoFake, _ *PelicanGroupTestService) {
			r.config.IdleProbe.Enabled = false
		},
		"group unchecked": func(_ *ChannelMonitorV3Prober, r *monitorV3RepoFake, _ *PelicanGroupTestService) {
			r.config.IdleProbe.Groups[0].GroupID = 5
		},
		"no model to ask": func(_ *ChannelMonitorV3Prober, r *monitorV3RepoFake, _ *PelicanGroupTestService) {
			r.config.IdleProbe.Groups[0].Model = ""
		},
		"site shows v2": func(p *ChannelMonitorV3Prober, _ *monitorV3RepoFake, _ *PelicanGroupTestService) {
			p.settings = channelMonitorV2RuntimeStub{rt: ChannelMonitorRuntime{Enabled: true, Mode: ChannelMonitorModeV2}}
		},
		"monitor off": func(p *ChannelMonitorV3Prober, _ *monitorV3RepoFake, _ *PelicanGroupTestService) {
			p.settings = channelMonitorV2RuntimeStub{rt: ChannelMonitorRuntime{Enabled: false, Mode: ChannelMonitorModeV3}}
		},
		"component off": func(_ *ChannelMonitorV3Prober, r *monitorV3RepoFake, _ *PelicanGroupTestService) {
			r.components[0].Enabled = false
		},
		"group disabled": func(_ *ChannelMonitorV3Prober, r *monitorV3RepoFake, _ *PelicanGroupTestService) {
			r.components[0].GroupStatus = "disabled"
		},
		"group deleted": func(_ *ChannelMonitorV3Prober, r *monitorV3RepoFake, _ *PelicanGroupTestService) {
			r.components[0].GroupDeleted = true
		},
		"group turned off since listing": func(_ *ChannelMonitorV3Prober, _ *monitorV3RepoFake, g *PelicanGroupTestService) {
			g.groups = groupTestGroupsFake{4: {ID: 4, Status: "disabled"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, repo, groups := monitorV3ProberFixture(&groupTestRouterFake{steps: []routeStep{{account: account(77, "a")}}}, func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
				t.Fatal("no probe expected")
				return nil, nil
			})
			mutate(p, repo, groups)
			p.RunDue(context.Background(), p.now())
			require.Empty(t, repo.claims)
			require.Len(t, repo.prunedBefore, 1, "old probes are pruned whatever the switch")
		})
	}
}

func TestChannelMonitorV3ProberCountsOnlyTheComponentModel(t *testing.T) {
	probed := 0
	p, repo, _ := monitorV3ProberFixture(&groupTestRouterFake{steps: []routeStep{{account: account(77, "a")}, {account: account(78, "b")}}},
		func(_ context.Context, _ int64, model string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
			require.Equal(t, "gpt-a", model, "a component narrowed to a model probes that model")
			probed++
			return &ScheduledTestResult{Status: "success", ResponseText: monitorV3Answer(cfg.Prompt)}, nil
		})
	repo.components = []ChannelMonitorV3Component{{ID: 1, GroupID: 4, Model: "gpt-a", Enabled: true}}
	repo.successCounts = []ChannelMonitorV3Fact{{GroupID: 4, Model: "gpt-b", Success: 9}}
	p.RunDue(context.Background(), monitorV3ProbeTick)
	require.Equal(t, 1, probed, "traffic of another model does not make this one busy")

	repo.claims = nil
	repo.successCounts = []ChannelMonitorV3Fact{{GroupID: 4, Model: "gpt-a", Success: 1}}
	p.RunDue(context.Background(), monitorV3ProbeTick)
	require.Equal(t, 1, probed)
}

func TestChannelMonitorV3ProberVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		steps    []routeStep
		reply    func(cfg *PelicanTestConfig) *ScheduledTestResult
		category string
	}{
		{"wrong answer", []routeStep{{account: account(77, "a")}}, func(cfg *PelicanTestConfig) *ScheduledTestResult {
			wrong, _ := strconv.Atoi(monitorV3Answer(cfg.Prompt))
			return &ScheduledTestResult{Status: "success", ResponseText: strconv.Itoa(wrong + 1)}
		}, ChannelMonitorV3ProbeWrongAnswer},
		{"upstream 5xx", []routeStep{{account: account(77, "a")}}, func(*PelicanTestConfig) *ScheduledTestResult {
			return &ScheduledTestResult{Status: "failed", ResponseText: "", ErrorMessage: "API returned 503: upstream unavailable"}
		}, "upstream_5xx"},
		{"expired upstream login", []routeStep{{account: account(77, "a")}}, func(*PelicanTestConfig) *ScheduledTestResult {
			return &ScheduledTestResult{Status: "failed", ErrorMessage: "API returned 401: token expired"}
		}, "authentication"},
		{"no account", nil, nil, "account_pool_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, repo, _ := monitorV3ProberFixture(&groupTestRouterFake{steps: tc.steps}, func(_ context.Context, _ int64, _ string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
				return tc.reply(cfg), nil
			})
			p.RunDue(context.Background(), monitorV3ProbeTick)
			require.Len(t, repo.finished, 1)
			require.Equal(t, ChannelMonitorV3ProbeFailure, repo.finished[0].Outcome)
			require.Equal(t, tc.category, repo.finished[0].Category, repo.finished[0].ErrorMessage)
			require.NotEmpty(t, repo.finished[0].ErrorMessage)
		})
	}
}

func TestChannelMonitorV3ProberAbandonsProbesOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, repo, _ := monitorV3ProberFixture(&groupTestRouterFake{steps: []routeStep{{account: account(77, "a")}}}, func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
		cancel()
		return &ScheduledTestResult{Status: "failed", ErrorMessage: "context canceled"}, nil
	})
	p.RunDue(ctx, monitorV3ProbeTick)
	require.Len(t, repo.claims, 1)
	require.Empty(t, repo.finished, "an interrupted probe must not paint the slot red")
}

func TestChannelMonitorV3ProberPrunesHourlyByRange(t *testing.T) {
	p, repo, _ := monitorV3ProberFixture(&groupTestRouterFake{}, nil)
	repo.config.IdleProbe.Enabled = false
	p.RunDue(context.Background(), monitorV3ProbeTick)
	p.RunDue(context.Background(), monitorV3ProbeTick.Add(30*time.Minute))
	require.Equal(t, []time.Time{monitorV3ProbeTick.Add(-8 * 24 * time.Hour)}, repo.prunedBefore)

	repo.config.AvailabilityRange = "30d"
	p.RunDue(context.Background(), monitorV3ProbeTick.Add(time.Hour))
	require.Equal(t, monitorV3ProbeTick.Add(time.Hour-31*24*time.Hour), repo.prunedBefore[1])
	require.Equal(t, 8*24*time.Hour, channelMonitorV3ProbeRetention("24h"), "bars still page back seven days")
}

func TestChannelMonitorV3ProbeCategories(t *testing.T) {
	for message, want := range map[string]string{
		"answer_mismatch: expected 12, got \"13\"":     ChannelMonitorV3ProbeWrongAnswer,
		"API returned 502: bad gateway":                "upstream_5xx",
		"Grok Realtime WS handshake failed (HTTP 429)": "rate_or_capacity",
		"API returned 404: The model does not exist":   "not_found",
		"timeout: no answer within 90 seconds":         "timeout",
		"no_available_account: no available accounts":  "account_pool_unavailable",
		"probe_failed: no result":                      "other",
	} {
		require.Equal(t, want, channelMonitorV3ProbeCategory(message), message)
	}
	require.True(t, channelMonitorV3ProbeIgnored("not_found"))
	require.False(t, channelMonitorV3ProbeIgnored("authentication"))
}

func TestPelicanProbeKindAsksOnlyTheChallenge(t *testing.T) {
	cfg := &PelicanTestConfig{QuestionKind: ChannelMonitorV3ProbeQuestionKind, Prompt: "Q: 1 + 2 = ?\nA:"}
	require.Equal(t, "Q: 1 + 2 = ?\nA:", intelligenceTestPrompt(cfg), "no HTML or integer contract is appended")
	require.Empty(t, intelligenceTestOutputError(cfg, "3"), "the answer is not required to be HTML")
	require.Equal(t, pelicanErrEmptyOutput, intelligenceTestOutputError(cfg, " "))
}
