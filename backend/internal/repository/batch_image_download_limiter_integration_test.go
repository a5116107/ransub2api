//go:build integration

package repository

import (
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestBatchImageDownloadLimiterRealRedisOwnershipAndRecovery(t *testing.T) {
	rdb := testRedis(t)
	first := &batchImageDownloadLimiter{rdb: rdb, activePrefix: defaultBatchImageDownloadActivePrefix, maxActive: 2, ttl: time.Minute}
	second := *first
	stale, err := first.Acquire(t.Context(), "11", "zip")
	require.NoError(t, err)
	key := first.activeKey("11")
	require.NoError(t, rdb.PExpire(t.Context(), key, 30*time.Millisecond).Err())
	require.Eventually(t, func() bool { return rdb.Exists(t.Context(), key).Val() == 0 }, time.Second, 10*time.Millisecond)

	const contenders = 24
	permits := make(chan service.BatchImageDownloadPermit, contenders)
	errors := make(chan error, contenders)
	var workers sync.WaitGroup
	for i := range contenders {
		workers.Go(func() {
			owner := first
			if i%2 != 0 {
				owner = &second
			}
			permit, acquireErr := owner.Acquire(t.Context(), "11", "item")
			if permit != nil {
				permits <- permit
			}
			errors <- acquireErr
		})
	}
	workers.Wait()
	close(permits)
	close(errors)
	var active []service.BatchImageDownloadPermit
	for permit := range permits {
		active = append(active, permit)
	}
	require.Len(t, active, 2, "independent limiters share one atomic ceiling")
	for acquireErr := range errors {
		if acquireErr != nil {
			require.ErrorIs(t, acquireErr, service.ErrBatchImageDownloadLimited)
		}
	}
	require.NoError(t, stale.Release(t.Context()))
	require.EqualValues(t, 2, rdb.ZCard(t.Context(), key).Val())
	_, err = second.Acquire(t.Context(), "11", "zip")
	require.ErrorIs(t, err, service.ErrBatchImageDownloadLimited)
	other, err := second.Acquire(t.Context(), "12", "zip")
	require.NoError(t, err, "another user keeps an independent ceiling")
	require.NoError(t, other.Release(t.Context()))

	// Expire only one member while keeping the key and the other owner alive.
	member, ok := active[0].(*batchImageDownloadPermit)
	require.True(t, ok)
	clock, err := rdb.Time(t.Context()).Result()
	require.NoError(t, err)
	expired := clock.Add(-time.Millisecond).UnixMilli()
	require.NoError(t, rdb.ZAdd(t.Context(), key, redis.Z{Score: float64(expired), Member: member.member}).Err())
	replacement, err := second.Acquire(t.Context(), "11", "zip")
	require.NoError(t, err)
	require.EqualValues(t, 2, rdb.ZCard(t.Context(), key).Val())
	require.NoError(t, active[0].Release(t.Context()))
	require.EqualValues(t, 2, rdb.ZCard(t.Context(), key).Val(), "an expired member cannot subtract its successor")
	releaseErrors := make(chan error, 8)
	for range 8 {
		workers.Go(func() { releaseErrors <- active[1].Release(t.Context()) })
	}
	workers.Wait()
	close(releaseErrors)
	for releaseErr := range releaseErrors {
		require.NoError(t, releaseErr)
	}
	require.EqualValues(t, 1, rdb.ZCard(t.Context(), key).Val())
	require.NoError(t, replacement.Release(t.Context()))
	require.Zero(t, rdb.Exists(t.Context(), key).Val())

	// Old versions used a string counter. Neither acquisition nor a late
	// new-version release may reinterpret or delete that still-live counter.
	require.NoError(t, rdb.Set(t.Context(), key, "1", 50*time.Millisecond).Err())
	_, err = first.Acquire(t.Context(), "11", "zip")
	require.ErrorIs(t, err, service.ErrBatchImageDownloadLimited)
	legacyRelease := &batchImageDownloadPermit{rdb: rdb, key: key, member: "synthetic-stale-member"}
	require.NoError(t, legacyRelease.Release(t.Context()))
	require.Equal(t, "1", rdb.Get(t.Context(), key).Val())
	require.Eventually(t, func() bool { return rdb.Exists(t.Context(), key).Val() == 0 }, time.Second, 10*time.Millisecond)
	recovered, err := second.Acquire(t.Context(), "11", "zip")
	require.NoError(t, err)
	require.NoError(t, recovered.Release(t.Context()))
}
