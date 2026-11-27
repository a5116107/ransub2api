package service

import (
	"context"
	"encoding/json"
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/require"
)

func ownedModerationLifecycleService(t *testing.T, repo ContentModerationRepository) *ContentModerationService {
	t.Helper()
	svc := NewContentModerationService(&contentModerationTestSettingRepo{}, repo, nil, nil, nil, nil, nil, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, svc.Stop(ctx))
	})
	return svc
}

func TestContentModerationStopDrainsAuditQueue(t *testing.T) {
	repo := &contentModerationTestRepo{}
	svc := ownedModerationLifecycleService(t, repo)
	for range 40 {
		svc.enqueueRecord(ContentModerationCheckInput{}, nil, &ContentModerationLog{}, "", false, false)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, svc.Stop(ctx))
	require.Len(t, repo.snapshotLogs(), 40)
	require.Empty(t, svc.asyncQueue)
	require.EqualValues(t, 40, svc.asyncProcessed.Load())
	svc.enqueueRecord(ContentModerationCheckInput{}, nil, &ContentModerationLog{}, "", false, false)
	svc.enqueueAsync(ContentModerationCheckInput{}, nil, ContentModerationInput{}, "")
	require.EqualValues(t, 2, svc.asyncDropped.Load())
	require.Empty(t, svc.asyncQueue)
}

type moderationBlockedAuditRepo struct {
	contentModerationTestRepo
	entered, exited chan struct{}
	start, finish   sync.Once
}

func (r *moderationBlockedAuditRepo) CreateLog(ctx context.Context, _ *ContentModerationLog) error {
	r.start.Do(func() { close(r.entered) })
	<-ctx.Done()
	r.finish.Do(func() { close(r.exited) })
	return ctx.Err()
}

func TestContentModerationStopDeadlineCancelsActiveAudit(t *testing.T) {
	repo := &moderationBlockedAuditRepo{entered: make(chan struct{}), exited: make(chan struct{})}
	svc := ownedModerationLifecycleService(t, repo)
	svc.enqueueRecord(ContentModerationCheckInput{}, nil, &ContentModerationLog{}, "", false, false)
	select {
	case <-repo.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("audit did not enter the repository")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, svc.Stop(ctx), context.DeadlineExceeded)
	select {
	case <-repo.exited:
	case <-time.After(time.Second):
		t.Fatal("shutdown deadline did not cancel the audit")
	}
	ctx, cancel = context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	require.NoError(t, svc.Stop(ctx))
}

func TestContentModerationStopConcurrentProducers(t *testing.T) {
	svc := ownedModerationLifecycleService(t, &contentModerationTestRepo{})
	var workers sync.WaitGroup
	for range 12 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 30 {
				svc.enqueueRecord(ContentModerationCheckInput{}, nil, &ContentModerationLog{}, "", false, false)
			}
		}()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, svc.Stop(ctx))
	workers.Wait()
	require.EqualValues(t, 360, svc.asyncProcessed.Load()+svc.asyncDropped.Load())
	require.Empty(t, svc.asyncQueue)
}

func TestContentModerationStopWakesInactiveWorker(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.WorkerCount = 1
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	settings := &contentModerationRuntimeSettingRepo{values: map[string]string{SettingKeyContentModerationConfig: string(raw)}}
	svc := runtimeCacheTestService(settings, time.Hour)
	svc.asyncQueue = make(chan contentModerationTask, 4)
	svc.asyncStop, svc.asyncDone = make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	svc.asyncCancel = cancel
	t.Cleanup(cancel)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(t, svc.Stop(ctx))
	})
	start := func(id int) {
		svc.asyncWorkers.Add(1)
		go func() { defer svc.asyncWorkers.Done(); svc.runWorker(ctx, id) }()
	}
	svc.enqueueRecord(ContentModerationCheckInput{}, nil, &ContentModerationLog{}, "", false, false)
	start(1)
	require.Eventually(t, func() bool { _, calls := settings.calls(); return calls > 0 }, time.Second, time.Millisecond)
	start(0)
	repo := svc.repo.(*contentModerationTestRepo)
	require.Eventually(t, func() bool { return len(repo.snapshotLogs()) == 1 }, time.Second, time.Millisecond)
	stopCtx, stopCancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer stopCancel()
	require.NoError(t, svc.Stop(stopCtx), "an inactive worker must wake on shutdown, not wait for its normal one-second retry")
}

func TestContentModerationStopReleasesWorkersAndQueue(t *testing.T) {
	var pointer weak.Pointer[ContentModerationService]
	t.Run("fixture", func(t *testing.T) {
		svc := ownedModerationLifecycleService(t, &contentModerationTestRepo{})
		pointer = weak.Make(svc)
	})
	require.Eventually(t, func() bool {
		runtime.GC()
		return pointer.Value() == nil
	}, 3*time.Second, 20*time.Millisecond)
}
