package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2CandyNativeReviewPriority(t *testing.T) {
	repo := &candyRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: candyConfigFixture()}}
	repo.config.CandyProbes = nil
	repo.nativeReviewDue = map[string]time.Time{}
	groupRepo := groupTestGroupsFake{}
	for id := int64(1); id <= 8; id++ {
		probe := candyProbeFixture()
		probe.GroupID = id
		repo.config.CandyProbes = append(repo.config.CandyProbes, probe)
		groupRepo[id] = &Group{ID: id, Platform: PlatformOpenAI, Status: StatusActive}
		if id > 4 {
			hash := fmt.Sprintf("%x", sha256.Sum256([]byte(probe.key())))
			repo.nativeReviewDue[fmt.Sprintf("%d/%s", id, hash)] = groupTestNow.Add(-time.Minute)
		}
	}
	var calls atomic.Int32
	var mu sync.Mutex
	served := []int64{}
	groups := &PelicanGroupTestService{groups: groupRepo, router: &candyRouterSpy{}, now: time.Now,
		runAccount: func(_ context.Context, id int64, _ string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
			if calls.Add(1) > 4 {
				return &ScheduledTestResult{BudgetDeferral: &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day"}}, nil
			}
			mu.Lock()
			served = append(served, id)
			mu.Unlock()
			return &ScheduledTestResult{Status: "success", ResponseText: "21"}, nil
		}}
	candyServiceFixture(repo, groups).RunDue(t.Context(), groupTestNow)
	require.ElementsMatch(t, []int64{5, 6, 7, 8}, served, "same-age native reviews must use the budget before unexplored groups")
}

func TestChannelMonitorV2CandyFairBudgetCoverage(t *testing.T) {
	const targets, capacity = channelCandyMaxProbes, 4
	repo := &candyRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: candyConfigFixture()}}
	repo.config.CandyProbes = nil
	for i := range targets {
		probe := candyProbeFixture()
		probe.Model = fmt.Sprintf("probe-%03d", i)
		repo.config.CandyProbes = append(repo.config.CandyProbes, probe)
	}
	steps := make([]routeStep, targets*targets)
	for i := range steps {
		steps[i].account = account(77, "shared probe account")
	}
	var calls atomic.Int32
	var mu sync.Mutex
	covered := make(map[string]int)
	groups, _ := newGroupTestService(newGroupTestRepoFake(), &groupTestRouterFake{steps: steps}, func(_ context.Context, _ int64, model string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
		if calls.Add(1) > capacity {
			return &ScheduledTestResult{BudgetDeferral: &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_minute"}}, nil
		}
		mu.Lock()
		covered[model]++
		mu.Unlock()
		return &ScheduledTestResult{Status: "success", ResponseText: "21"}, nil
	})
	s := candyServiceFixture(repo, groups)
	for round := range targets / capacity {
		calls.Store(0)
		s.RunDue(t.Context(), groupTestNow.Add(time.Duration(round)*time.Minute))
		require.LessOrEqual(t, int(calls.Load()), capacity+4, "global exhaustion must stop queued claims; only in-flight probes may finish")
	}
	require.Len(t, covered, targets, "all stable probes must get a turn before a served probe repeats")
	for model, count := range covered {
		require.Equal(t, 1, count, model)
	}
}

func TestChannelMonitorV2CandyBudgetDispatchOutcomes(t *testing.T) {
	for _, scope := range []string{"key", "key_minute", "site", "site_minute", "plugin", "global_minute", "global_day", "coordinator", "unknown"} {
		for _, admitted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admitted=%t", scope, admitted), func(t *testing.T) {
				repo := &candyRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: candyConfigFixture()}}
				denial := &MonitoringProbeBudgetDenial{Reason: "fixture_denial", Scope: scope}
				groups, _ := newGroupTestService(newGroupTestRepoFake(), &groupTestRouterFake{steps: []routeStep{{account: account(77, "chosen")}}}, func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
					return &ScheduledTestResult{monitoringBudgetAdmitted: admitted, BudgetDeferral: denial, ResponseText: "21"}, nil
				})
				s := candyServiceFixture(repo, groups)
				run := s.runOne(t.Context(), candyProbeFixture(), &repo.config, groupTestNow)
				require.Equal(t, admitted, run.served)
				require.Equal(t, !slices.Contains([]string{"key", "key_minute", "site", "site_minute", "plugin"}, scope), run.stop)
				require.Len(t, repo.results, 1)
				require.Equal(t, "deferred", repo.results[0].Verdict)
				require.Empty(t, repo.results[0].AnswerPreview)
			})
		}
	}
}

func TestChannelMonitorV2CandyLocalDeferralKeepsIndependentRoutes(t *testing.T) {
	for _, scope := range []string{"key", "key_minute", "site", "site_minute", "plugin"} {
		t.Run(scope, func(t *testing.T) {
			repo := &candyRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: candyConfigFixture()}}
			second := candyProbeFixture()
			second.Model = "independent"
			repo.config.CandyProbes = append(repo.config.CandyProbes, second)
			router := &groupTestRouterFake{steps: []routeStep{{account: account(77, "first")}, {account: account(78, "second")}}}
			groups, _ := newGroupTestService(newGroupTestRepoFake(), router, func(_ context.Context, _ int64, model string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
				if model != second.Model {
					return &ScheduledTestResult{BudgetDeferral: &MonitoringProbeBudgetDenial{Reason: "fixture_denial", Scope: scope}}, nil
				}
				return &ScheduledTestResult{Status: "success", ResponseText: "21"}, nil
			})
			s := candyServiceFixture(repo, groups)
			s.RunDue(t.Context(), groupTestNow)
			require.Len(t, repo.results, 2)
			require.Len(t, router.released, 2)
			require.Len(t, slices.DeleteFunc(slices.Clone(repo.results), func(r ChannelMonitorV2CandyResult) bool { return r.Verdict != "correct" }), 1)
		})
	}
}

func TestChannelMonitorV2CandyPartialChainYields(t *testing.T) {
	repo := &candyRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: candyConfigFixture()}}
	partial := candyProbeFixture()
	partial.Model = "partial"
	native := partial
	native.Model = "native"
	repo.config.CandyProbes = []ChannelMonitorV2CandyProbe{partial, native}
	steps := []routeStep{{account: account(77, "first")}, {account: account(78, "second")}, {account: account(79, "later")}}
	var calls atomic.Int32
	var completed atomic.Int32
	groups, _ := newGroupTestService(newGroupTestRepoFake(), &groupTestRouterFake{steps: steps}, func(_ context.Context, _ int64, model string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
		denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_minute"}
		if calls.Add(1) > 1 {
			return &ScheduledTestResult{BudgetDeferral: denial}, nil
		}
		if model == partial.Model {
			return &ScheduledTestResult{monitoringBudgetAdmitted: true, BudgetDeferral: denial}, nil
		}
		completed.Add(1)
		return &ScheduledTestResult{Status: "success", ResponseText: "21"}, nil
	})
	s := candyServiceFixture(repo, groups)
	partialKey := monitoringProbeQueueKey{groupID: partial.GroupID, scope: partial.key()}
	nativeKey := monitoringProbeQueueKey{groupID: native.GroupID, scope: native.key()}
	require.True(t, s.queue.order(t.Context(), repo, GroupMonitoringQueueConfig{}, map[monitoringProbeQueueKey]monitoringProbeQueueTarget{partialKey: {identity: partial.key()}, nativeKey: {identity: native.key()}}, []monitoringProbeQueueKey{partialKey, nativeKey}))
	repo.setAge(partialKey, 1, 0)
	repo.setAge(nativeKey, 100, 0)
	s.RunDue(t.Context(), groupTestNow)
	require.Zero(t, completed.Load())
	require.Len(t, repo.claims, 1, "a younger tier must not race the older in-flight chain")
	calls.Store(0)
	s.RunDue(t.Context(), groupTestNow.Add(time.Minute))
	require.EqualValues(t, 1, completed.Load(), "partial physical work consumes its turn even though its verdict is deferred")
}

type candyQueueConfigRepo struct {
	*candyRepoFake
	read func(context.Context) (*ChannelMonitorV2Config, error)
}

func (r candyQueueConfigRepo) GetConfig(ctx context.Context) (*ChannelMonitorV2Config, error) {
	return r.read(ctx)
}

func TestChannelMonitorV2CandyQueueInventoryLifecycle(t *testing.T) {
	repo := &candyRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: candyConfigFixture()}}
	groups, _ := newGroupTestService(newGroupTestRepoFake(), &groupTestRouterFake{}, nil)
	s := candyServiceFixture(repo, groups)
	probe := candyProbeFixture()
	key := monitoringProbeQueueKey{groupID: probe.GroupID, scope: probe.key()}
	s.queue.turns = map[monitoringProbeQueueKey]GroupMonitoringTurn{key: {Served: 1, Tried: 2}}
	s.config = candyQueueConfigRepo{candyRepoFake: repo, read: func(context.Context) (*ChannelMonitorV2Config, error) { return nil, errors.New("fixture read failed") }}
	s.RunDue(t.Context(), groupTestNow)
	require.Equal(t, GroupMonitoringTurn{Served: 1, Tried: 2}, s.queue.turns[key], "a failed read must not erase pending work")
	s.config = repo
	repo.config.CandyProbes = nil
	s.RunDue(t.Context(), groupTestNow)
	require.Empty(t, s.queue.turns)
	repo.config.CandyProbes = []ChannelMonitorV2CandyProbe{probe}
	probe.IntervalMinutes = 5
	s.queue.turns[key] = GroupMonitoringTurn{Served: 1}
	repo.config.CandyProbes[0] = probe
	s.RunDue(t.Context(), groupTestNow)
	require.NotContains(t, s.queue.turns, key, "a changed interval is a different native config key")
	require.Contains(t, s.queue.turns, monitoringProbeQueueKey{groupID: probe.GroupID, scope: probe.key()})
	repo.config.Enabled = false
	s.RunDue(t.Context(), groupTestNow)
	require.Empty(t, s.queue.turns)
}

func TestChannelMonitorV2CandyCanceledQueueAndOverlap(t *testing.T) {
	repo := &candyRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: candyConfigFixture()}}
	groups, _ := newGroupTestService(newGroupTestRepoFake(), &groupTestRouterFake{}, nil)
	s := candyServiceFixture(repo, groups)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.RunDue(ctx, groupTestNow)
	require.Zero(t, repo.prunes, "already canceled work must not touch storage")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("configuration fixture did not drain")
		}
	})
	s.config = candyQueueConfigRepo{candyRepoFake: repo, read: func(context.Context) (*ChannelMonitorV2Config, error) {
		close(entered)
		<-release
		return nil, errors.New("fixture stopped")
	}}
	go func() { defer close(done); s.RunDue(t.Context(), groupTestNow) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the active inventory read did not start")
	}
	s.RunDue(t.Context(), groupTestNow)
	require.Equal(t, 1, repo.prunes, "overlapping ticks must skip before reading storage")
	once.Do(func() { close(release) })
	<-done
	require.Empty(t, repo.claims)
}
