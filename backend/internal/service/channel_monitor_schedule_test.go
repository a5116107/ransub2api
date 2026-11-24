//go:build unit

package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type monitorScheduleSourceFunc func(context.Context, int64) (*ChannelMonitorScheduleSnapshot, error)

func (f monitorScheduleSourceFunc) LoadScheduleSnapshot(ctx context.Context, known int64) (*ChannelMonitorScheduleSnapshot, error) {
	return f(ctx, known)
}

func monitorScheduleFixture(revision int64, generations map[int64]int64) *ChannelMonitorScheduleSnapshot {
	snapshot := &ChannelMonitorScheduleSnapshot{Revision: revision, Revisions: generations}
	for id := range generations {
		snapshot.Monitors = append(snapshot.Monitors, &ChannelMonitor{ID: id, Enabled: true, IntervalSeconds: 3600})
	}
	return snapshot
}

func TestV1RunnerConfigurationSnapshotReconcilesOnlyChangedGenerations(t *testing.T) {
	svc := &stubMonitorSvc{runCalled: make(chan int64, 16)}
	r := newRunnerForTest(svc)
	t.Cleanup(r.Stop)
	snapshot := monitorScheduleFixture(1, map[int64]int64{1: 1})
	r.scheduleSource = monitorScheduleSourceFunc(func(_ context.Context, known int64) (*ChannelMonitorScheduleSnapshot, error) {
		if snapshot.Revision == known {
			return nil, nil
		}
		return snapshot, nil
	})
	// Exercise reconciliation directly without adding a second background loop.
	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
	r.reconcileSchedule()
	first := runnerTaskPtr(r, 1)
	require.NotNil(t, first)
	require.EqualValues(t, 1, first.revision)
	r.reconcileSchedule()
	require.Same(t, first, runnerTaskPtr(r, 1), "unchanged reads cannot restart the immediate probe")
	snapshot = monitorScheduleFixture(2, map[int64]int64{1: 1, 2: 2})
	r.reconcileSchedule()
	require.Same(t, first, runnerTaskPtr(r, 1), "another monitor's change cannot reset this cadence")
	second := runnerTaskPtr(r, 2)
	require.NotNil(t, second)
	snapshot = monitorScheduleFixture(4, map[int64]int64{1: 4, 2: 2})
	r.reconcileSchedule()
	require.NotSame(t, first, runnerTaskPtr(r, 1), "same-value and ABA saves still replace the generation")
	require.Same(t, second, runnerTaskPtr(r, 2))
	snapshot = monitorScheduleFixture(5, map[int64]int64{2: 2})
	r.reconcileSchedule()
	require.Nil(t, runnerTaskPtr(r, 1), "remote disable/delete removes the original timer")
	snapshot = monitorScheduleFixture(6, map[int64]int64{2: 6})
	snapshot.Monitors[0].APIKeyDecryptFailed = true
	r.reconcileSchedule()
	require.Zero(t, runnerTaskCount(r))
	snapshot = monitorScheduleFixture(7, map[int64]int64{2: 7})
	r.reconcileSchedule()
	require.NotNil(t, runnerTaskPtr(r, 2), "a repaired key is scheduled after its committed save")
}

func TestV1RunnerConfigurationReadFailureAndInvalidSnapshotsPreserveTasks(t *testing.T) {
	r := newRunnerForTest(&stubMonitorSvc{})
	t.Cleanup(r.Stop)
	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
	snapshot := monitorScheduleFixture(10, map[int64]int64{1: 10})
	var readErr error
	r.scheduleSource = monitorScheduleSourceFunc(func(context.Context, int64) (*ChannelMonitorScheduleSnapshot, error) {
		return snapshot, readErr
	})
	r.reconcileSchedule()
	first := runnerTaskPtr(r, 1)
	require.NotNil(t, first)
	for _, invalid := range []struct {
		name     string
		snapshot *ChannelMonitorScheduleSnapshot
		err      error
	}{
		{"SQL unavailable", nil, errors.New("fixture read failure")},
		{"unchanged", nil, nil},
		{"stale", monitorScheduleFixture(9, nil), nil},
		{"missing generation", &ChannelMonitorScheduleSnapshot{Revision: 11, Monitors: snapshot.Monitors}, nil},
		{"future generation", monitorScheduleFixture(11, map[int64]int64{2: 12}), nil},
		{"nil row", &ChannelMonitorScheduleSnapshot{Revision: 11, Monitors: []*ChannelMonitor{nil}}, nil},
		{"duplicate rows", &ChannelMonitorScheduleSnapshot{Revision: 11, Monitors: []*ChannelMonitor{snapshot.Monitors[0], snapshot.Monitors[0]}, Revisions: map[int64]int64{1: 10}}, nil},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			snapshot, readErr = invalid.snapshot, invalid.err
			r.reconcileSchedule()
			require.Same(t, first, runnerTaskPtr(r, 1))
			require.EqualValues(t, 10, r.scheduleRevision)
		})
	}
	snapshot, readErr = monitorScheduleFixture(12, nil), nil
	r.reconcileSchedule()
	require.Zero(t, runnerTaskCount(r), "a successfully loaded empty snapshot does remove old tasks")
}

func TestV1RunnerConfigurationStartupRetryAndLocalWake(t *testing.T) {
	svc := &stubMonitorSvc{runCalled: make(chan int64, 4)}
	r := newRunnerForTest(svc)
	t.Cleanup(r.Stop)
	r.scheduleInterval = 10 * time.Millisecond
	var reads atomic.Int64
	r.scheduleSource = monitorScheduleSourceFunc(func(_ context.Context, known int64) (*ChannelMonitorScheduleSnapshot, error) {
		if reads.Add(1) == 1 {
			return nil, errors.New("fixture startup unavailable")
		}
		if known == 1 {
			return nil, nil
		}
		return monitorScheduleFixture(1, map[int64]int64{1: 1}), nil
	})
	r.Start()
	waitFor(t, time.Second, "startup inventory retry", func() bool { return runnerTaskPtr(r, 1) != nil })
	first := runnerTaskPtr(r, 1)
	// Captured callback data is deliberately stale. Only storage controls tasks.
	r.Schedule(&ChannelMonitor{ID: 99, Enabled: true, IntervalSeconds: 15})
	r.Unschedule(1)
	waitFor(t, time.Second, "same snapshot refreshed after callbacks", func() bool { return reads.Load() >= 3 })
	require.Nil(t, runnerTaskPtr(r, 99))
	require.Same(t, first, runnerTaskPtr(r, 1))
}

func TestV1RunnerConfigurationLocalSaveFencesSlowSnapshot(t *testing.T) {
	svc := &stubMonitorSvc{runCalled: make(chan int64, 4)}
	r := newRunnerForTest(svc)
	t.Cleanup(r.Stop)
	entered, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int64
	r.scheduleSource = monitorScheduleSourceFunc(func(ctx context.Context, known int64) (*ChannelMonitorScheduleSnapshot, error) {
		if reads.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return monitorScheduleFixture(1, map[int64]int64{1: 1}), nil
		}
		if known == 2 {
			return nil, nil
		}
		return monitorScheduleFixture(2, map[int64]int64{2: 2}), nil
	})
	started := make(chan struct{})
	go func() { r.Start(); close(started) }()
	<-entered
	r.Schedule(&ChannelMonitor{ID: 2, Enabled: true, IntervalSeconds: 3600})
	close(release)
	<-started
	waitFor(t, time.Second, "committed replacement after slow startup", func() bool { return runnerTaskPtr(r, 2) != nil })
	require.Nil(t, runnerTaskPtr(r, 1))
	select {
	case id := <-svc.runCalled:
		require.EqualValues(t, 2, id, "discarded slow snapshot must not submit an old immediate probe")
	case <-time.After(time.Second):
		t.Fatal("replacement was not dispatched")
	}
}

func TestV1RunnerConfigurationStopCancelsAndDrainsReads(t *testing.T) {
	for _, phase := range []string{"startup", "refresh"} {
		t.Run(phase, func(t *testing.T) {
			r := newRunnerForTest(&stubMonitorSvc{})
			t.Cleanup(r.Stop)
			r.scheduleInterval = 10 * time.Millisecond
			entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var reads atomic.Int64
			r.scheduleSource = monitorScheduleSourceFunc(func(ctx context.Context, _ int64) (*ChannelMonitorScheduleSnapshot, error) {
				if reads.Add(1) == 1 && phase == "refresh" {
					return monitorScheduleFixture(0, nil), nil
				}
				close(entered)
				<-ctx.Done()
				close(canceled)
				<-release
				return monitorScheduleFixture(1, map[int64]int64{1: 1}), nil
			})
			started := make(chan struct{})
			go func() { r.Start(); close(started) }()
			<-entered
			done := make(chan struct{})
			go func() { r.Stop(); close(done) }()
			<-canceled
			select {
			case <-done:
				t.Fatal("Stop returned while its snapshot reader was still running")
			default:
			}
			r.Schedule(&ChannelMonitor{ID: 1})
			r.Unschedule(1)
			close(release)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("Stop did not drain its configuration reader")
			}
			<-started
			require.Zero(t, runnerTaskCount(r))
			expectedReads := int64(1)
			if phase == "refresh" {
				expectedReads = 2
			}
			require.Equal(t, expectedReads, reads.Load())
		})
	}
}

func TestV1RunnerConfigurationGatewayRoleDoesNotStart(t *testing.T) {
	svc := NewChannelMonitorService(nil, nil)
	svc.requireScheduledClaims = true
	r := ProvideChannelMonitorRunner(svc, nil, nil, &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}})
	t.Cleanup(r.Stop)
	require.False(t, r.started)
	require.EqualValues(t, -1, r.scheduleRevision)
	require.NotNil(t, r.scheduleSource, "full-node snapshots remain required even if the role skips starting")
}

func TestV1RunnerConfigurationMissingRepositoryFailsClosed(t *testing.T) {
	svc := NewChannelMonitorService(nil, nil)
	svc.requireScheduledClaims = true
	r := NewChannelMonitorRunner(svc, nil)
	t.Cleanup(r.Stop)
	r.Start()
	require.EqualValues(t, -1, r.scheduleRevision)
	require.Zero(t, runnerTaskCount(r))
	r.Schedule(&ChannelMonitor{ID: 1, Enabled: true, IntervalSeconds: 15})
	r.Stop()
	require.Zero(t, runnerTaskCount(r), "a missing production source never falls back to local captured credentials")
}
