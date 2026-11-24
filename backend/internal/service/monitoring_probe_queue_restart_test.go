package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Test-only shared store: rebuilding an executor does not rebuild this state.
// PostgreSQL/CAS/config fencing are exercised by repository integration tests.
type groupMonitoringTurnsFake struct {
	mu                           sync.Mutex
	sequence                     int64
	turns                        map[string]GroupMonitoringTurn
	syncErr, claimErr, finishErr error
	staleClaim                   bool
}

func groupTurnKey(turn GroupMonitoringTurn) string {
	return fmt.Sprintf("%d/%s", turn.GroupID, turn.ScopeHash)
}

func (r *groupMonitoringTurnsFake) SyncGroupMonitoringTurns(ctx context.Context, _ GroupMonitoringQueueConfig, input []GroupMonitoringTurn) (*GroupMonitoringQueue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := errors.Join(ctx.Err(), r.syncErr); err != nil {
		return nil, err
	}
	r.sequence++
	turns := make(map[string]GroupMonitoringTurn, len(input))
	for _, target := range input {
		key := groupTurnKey(target)
		turn, ok := r.turns[key]
		if !ok || turn.IdentityHash != target.IdentityHash {
			turn = target
			turn.Served, turn.Tried = r.sequence, 0
		}
		if !target.Due {
			turn.WaitingSince = time.Time{}
		} else if turn.WaitingSince.IsZero() {
			turn.WaitingSince = time.Now().UTC()
		}
		turns[key] = turn
	}
	r.turns = turns
	r.sequence++
	out := &GroupMonitoringQueue{Round: r.sequence}
	for _, turn := range turns {
		out.Turns = append(out.Turns, turn)
	}
	return out, nil
}

func (r *groupMonitoringTurnsFake) ClaimGroupMonitoringTurn(ctx context.Context, turn GroupMonitoringTurn) (GroupMonitoringTurn, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := errors.Join(ctx.Err(), r.claimErr); err != nil {
		return turn, false, err
	}
	current, ok := r.turns[groupTurnKey(turn)]
	if r.staleClaim || !ok || !current.ClaimUntil.IsZero() && time.Now().Before(current.ClaimUntil) || current.IdentityHash != turn.IdentityHash || current.Served != turn.Served || current.Tried != turn.Tried {
		return turn, false, nil
	}
	r.sequence++
	turn.Tried = r.sequence
	turn.ClaimUntil = time.Now().UTC().Add(GroupMonitoringTurnLeaseTTL)
	r.turns[groupTurnKey(turn)] = turn
	return turn, true, nil
}

func (r *groupMonitoringTurnsFake) FinishGroupMonitoringTurn(ctx context.Context, turn GroupMonitoringTurn, round int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := errors.Join(ctx.Err(), r.finishErr); err != nil {
		return false, err
	}
	current, ok := r.turns[groupTurnKey(turn)]
	if !ok || current.IdentityHash != turn.IdentityHash || current.Served != turn.Served || current.Tried != turn.Tried || current.ClaimUntil.IsZero() {
		return false, nil
	}
	turn.ClaimUntil = time.Time{}
	if round > 0 {
		turn.Served, turn.WaitingSince = round, time.Time{}
	}
	r.turns[groupTurnKey(turn)] = turn
	return true, nil
}

func orderProbeQueue(t *testing.T, q *monitoringProbeQueue, targets, due []monitoringProbeQueueKey) {
	t.Helper()
	store := q.store
	if store == nil {
		store = &groupMonitoringTurnsFake{}
	}
	identities := make(map[monitoringProbeQueueKey]string, len(targets))
	for _, key := range targets {
		identities[key] = key.scope
	}
	require.True(t, q.order(t.Context(), store, GroupMonitoringQueueConfig{}, identities, due))
}

func (r *groupMonitoringTurnsFake) setAge(key monitoringProbeQueueKey, served, tried int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(key.scope)))
	k := groupTurnKey(GroupMonitoringTurn{GroupID: key.groupID, ScopeHash: hash})
	turn := r.turns[k]
	turn.Served, turn.Tried = served, tried
	r.turns[k] = turn
	if served > r.sequence {
		r.sequence = served
	}
	if tried > r.sequence {
		r.sequence = tried
	}
}

func v3QueueIdentities(targets []channelMonitorV3ProbeTarget, interval int) map[monitoringProbeQueueKey]string {
	out := make(map[monitoringProbeQueueKey]string, len(targets))
	for _, target := range targets {
		out[target.key()] = fmt.Sprintf("%s\x00%s\x00%d", target.Model, target.ReasoningEffort, interval)
	}
	return out
}

func TestMonitoringProbeQueueSharedFailureStopsAndRecovers(t *testing.T) {
	for _, failure := range []string{"missing", "read", "claim", "finish", "stale"} {
		t.Run(failure, func(t *testing.T) {
			keys := make([]monitoringProbeQueueKey, 12)
			for i := range keys {
				keys[i].groupID = int64(i + 1)
			}
			var q monitoringProbeQueue
			store := &groupMonitoringTurnsFake{}
			err := errors.New("private-storage-fixture")
			var source any = store
			if failure == "missing" {
				source = struct{}{}
			}
			if failure == "read" {
				store.syncErr = err
			}
			if failure == "claim" {
				store.claimErr = err
			}
			if failure == "finish" {
				store.finishErr = err
			}
			if failure == "stale" {
				store.staleClaim = true
			}
			identities := map[monitoringProbeQueueKey]string{}
			for _, key := range keys {
				identities[key] = key.scope
			}
			var calls atomic.Int32
			if q.order(t.Context(), source, GroupMonitoringQueueConfig{}, identities, keys) {
				q.run(t.Context(), keys, func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun {
					calls.Add(1)
					return monitoringProbeQueueRun{served: true}
				})
			}
			if failure == "finish" {
				require.EqualValues(t, monitoringProbeQueueWorkers, calls.Load(), "save failure drains the original four but dispatches no more")
			} else {
				require.Zero(t, calls.Load(), "failed or stale shared state must not start a physical attempt")
			}
			if failure != "stale" {
				require.Equal(t, "coordination_unavailable", q.runtimeSnapshot().State)
			}
			store.syncErr, store.claimErr, store.finishErr, store.staleClaim = nil, nil, nil, false
			require.True(t, q.order(t.Context(), store, GroupMonitoringQueueConfig{}, identities, keys))
			q.run(t.Context(), keys, func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun {
				return monitoringProbeQueueRun{served: true}
			})
			require.Equal(t, "idle", q.runtimeSnapshot().State)
			if failure == "finish" {
				require.NotNil(t, q.runtimeSnapshot().Cycle.OldestPendingAt, "uncertain saves retain pending age until the original lease expires")
				store.mu.Lock()
				for key, turn := range store.turns {
					turn.ClaimUntil = time.Now().UTC().Add(-time.Second)
					store.turns[key] = turn
				}
				store.mu.Unlock()
				require.True(t, q.order(t.Context(), store, GroupMonitoringQueueConfig{}, identities, keys))
				q.run(t.Context(), keys, func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun {
					return monitoringProbeQueueRun{served: true}
				})
			}
			require.Nil(t, q.runtimeSnapshot().Cycle.OldestPendingAt)
		})
	}
}

func TestMonitoringProbeQueueCanceledAdmissionPersistsAge(t *testing.T) {
	var q monitoringProbeQueue
	keys := []monitoringProbeQueueKey{{groupID: 1}}
	orderProbeQueue(t, &q, keys, keys)
	before := q.turns[keys[0]]
	ctx, cancel := context.WithCancel(t.Context())
	q.run(ctx, keys, func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun {
		cancel()
		return monitoringProbeQueueRun{served: true}
	})
	var rebuilt monitoringProbeQueue
	rebuilt.store = q.store
	orderProbeQueue(t, &rebuilt, keys, keys)
	require.Greater(t, rebuilt.turns[keys[0]].Served, before.Served, "consumed age must survive both cancellation and executor rebuild")
	require.NotEqual(t, q.runtimeSnapshot().InstanceID, rebuilt.runtimeSnapshot().InstanceID)
}

func TestMonitoringProbeQueuePeerCannotStealInFlightAge(t *testing.T) {
	store := &groupMonitoringTurnsFake{}
	left := monitoringProbeQueue{store: store}
	keys := make([]monitoringProbeQueueKey, 8)
	for i := range keys {
		keys[i].groupID = int64(i + 1)
	}
	orderProbeQueue(t, &left, keys, keys)
	entered, release, done := make(chan struct{}, 4), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	var once sync.Once
	t.Cleanup(func() { cancel(); once.Do(func() { close(release) }); <-done })
	go func() {
		defer close(done)
		left.run(ctx, keys, func(context.Context, monitoringProbeQueueKey) monitoringProbeQueueRun {
			entered <- struct{}{}
			<-release
			return monitoringProbeQueueRun{served: true}
		})
	}()
	for range 4 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("original four probes did not start")
		}
	}
	right := monitoringProbeQueue{store: store}
	peerKeys := append([]monitoringProbeQueueKey(nil), keys...)
	orderProbeQueue(t, &right, peerKeys, peerKeys)
	var peers atomic.Int32
	right.run(t.Context(), peerKeys, func(_ context.Context, key monitoringProbeQueueKey) monitoringProbeQueueRun {
		require.Greater(t, key.groupID, int64(4), "a peer must skip a live owner's original four")
		peers.Add(1)
		return monitoringProbeQueueRun{served: true}
	})
	require.EqualValues(t, 4, peers.Load())
	cancel()
	once.Do(func() { close(release) })
	<-done
	rebuilt := monitoringProbeQueue{store: store}
	orderProbeQueue(t, &rebuilt, keys, keys)
	for _, key := range keys[:4] {
		require.Equal(t, left.round, rebuilt.turns[key].Served, "original owner must persist its consumed age even after a peer scan")
	}
}

func TestMonitoringProbeQueueDeletedGroupDoesNotBlockIndependentSource(t *testing.T) {
	repo := &candyRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: candyConfigFixture()}}
	missing := candyProbeFixture()
	missing.GroupID = 999
	repo.config.CandyProbes = append(repo.config.CandyProbes, missing)
	var calls atomic.Int32
	groups, _ := newGroupTestService(newGroupTestRepoFake(), &groupTestRouterFake{steps: []routeStep{{account: account(77, "fixture")}}}, func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
		calls.Add(1)
		return &ScheduledTestResult{Status: "success", ResponseText: "21"}, nil
	})
	svc := candyServiceFixture(repo, groups)
	svc.RunDue(t.Context(), groupTestNow)
	require.EqualValues(t, 1, calls.Load())
	require.EqualValues(t, 1, svc.MonitoringRuntime().LastScan.Targets, "deleted/inactive groups are not live shared scheduling targets")
	require.Equal(t, "idle", svc.MonitoringRuntime().State)
}

func TestMonitoringProbeGroupQueuesRetainAgeAcrossServiceRebuild(t *testing.T) {
	for _, lane := range []string{"v2", "v3"} {
		t.Run(lane, func(t *testing.T) {
			const targets, capacity = 12, 4
			var attempts atomic.Int64
			var mu sync.Mutex
			seen := make(map[string]int)
			router := &groupTestRouterFake{}
			for range targets * 4 {
				router.steps = append(router.steps, routeStep{account: account(77, "fixture")})
			}
			run := func(_ context.Context, _ int64, model string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
				if attempts.Add(1) > capacity {
					return &ScheduledTestResult{BudgetDeferral: &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day"}}, nil
				}
				mu.Lock()
				seen[model]++
				mu.Unlock()
				answer := "21"
				if lane == "v3" {
					answer = monitorV3Answer(cfg.Prompt)
				}
				return &ScheduledTestResult{Status: "success", ResponseText: answer}, nil
			}
			groups, _ := newGroupTestService(newGroupTestRepoFake(), router, run)
			v2repo := &candyRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: candyConfigFixture()}}
			v2repo.config.CandyProbes = nil
			_, v3repo, _ := monitorV3ProberFixture(router, run)
			v3repo.components = nil
			for i := range targets {
				probe := candyProbeFixture()
				probe.Model = fmt.Sprintf("rebuild-model-%02d", i)
				v2repo.config.CandyProbes = append(v2repo.config.CandyProbes, probe)
				v3repo.components = append(v3repo.components, ChannelMonitorV3Component{ID: int64(i + 1), GroupID: 4, Model: probe.Model, Enabled: true})
			}
			for round := range targets / capacity {
				attempts.Store(0)
				if lane == "v2" {
					candyServiceFixture(v2repo, groups).RunDue(t.Context(), groupTestNow.Add(time.Duration(round)*time.Minute))
				} else {
					prober := NewChannelMonitorV3Prober(v3repo, groups, channelMonitorV2RuntimeStub{rt: ChannelMonitorRuntime{Enabled: true, Mode: ChannelMonitorModeV3}})
					prober.RunDue(t.Context(), monitorV3ProbeTick.Add(time.Duration(round)*5*time.Minute))
					prober.Stop()
				}
				mu.Lock()
				unique := len(seen)
				mu.Unlock()
				require.Equal(t, (round+1)*capacity, unique, "fresh executor instances must retain older pending peers before repeating served scopes")
			}
			for model, n := range seen {
				require.Equal(t, 1, n, model)
			}
		})
	}
}
