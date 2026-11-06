//go:build unit

package repository

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	miniserver "github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestBatchImageDownloadLimiter_StaleReleasePreservesNewOwner(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	limiter := &batchImageDownloadLimiter{rdb: rdb, activePrefix: defaultBatchImageDownloadActivePrefix, maxActive: 1, ttl: time.Minute}
	stale, err := limiter.Acquire(t.Context(), "11", "zip")
	require.NoError(t, err)
	mr.FastForward(2 * time.Minute)
	current, err := limiter.Acquire(t.Context(), "11", "item")
	require.NoError(t, err)
	require.NoError(t, stale.Release(t.Context()))
	_, err = limiter.Acquire(t.Context(), "11", "zip")
	require.ErrorIs(t, err, service.ErrBatchImageDownloadLimited, "an expired owner must not release a newer download")
	require.NoError(t, current.Release(t.Context()))
}

func TestBatchImageDownloadLimiter_ReleaseRetriesAfterFailure(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	limiter := &batchImageDownloadLimiter{rdb: rdb, activePrefix: defaultBatchImageDownloadActivePrefix, maxActive: 1, ttl: time.Minute}
	permit, err := limiter.Acquire(t.Context(), "11", "zip")
	require.NoError(t, err)
	var fail atomic.Bool
	fail.Store(true)
	mr.Server().SetPreHook(func(peer *miniserver.Peer, command string, _ ...string) bool {
		if fail.Load() && strings.HasPrefix(strings.ToUpper(command), "EVAL") {
			peer.WriteError("ERR injected download-release failure")
			return true
		}
		return false
	})
	require.Error(t, permit.Release(t.Context()))
	fail.Store(false)
	require.NoError(t, permit.Release(t.Context()), "a failed release must remain retryable")
	require.NoError(t, permit.Release(t.Context()))
	require.False(t, mr.Exists(limiter.activeKey("11")))
}

func TestBatchImageDownloadLimiter_IndividualExpiryAndMixedDurations(t *testing.T) {
	mr := miniredis.RunT(t)
	clock := time.Unix(1_800_000_000, 0)
	mr.SetTime(clock)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	long := &batchImageDownloadLimiter{rdb: rdb, activePrefix: defaultBatchImageDownloadActivePrefix, maxActive: 2, ttl: 2 * time.Minute}
	short := *long
	short.ttl = time.Minute
	first, err := long.Acquire(t.Context(), "11", "zip")
	require.NoError(t, err)
	mr.SetTime(clock.Add(30 * time.Second))
	second, err := short.Acquire(t.Context(), "11", "item")
	require.NoError(t, err)
	require.GreaterOrEqual(t, mr.TTL(long.activeKey("11")), 90*time.Second, "a shorter node deadline must not expire another node's longer member")
	mr.SetTime(clock.Add(91 * time.Second))
	third, err := short.Acquire(t.Context(), "11", "zip")
	require.NoError(t, err, "only the short member expired")
	require.NoError(t, second.Release(t.Context()))
	_, err = long.Acquire(t.Context(), "11", "zip")
	require.ErrorIs(t, err, service.ErrBatchImageDownloadLimited)
	require.NoError(t, first.Release(t.Context()))
	require.NoError(t, third.Release(t.Context()))
}

func TestBatchImageDownloadLimiter_AcquireDenyReleaseAndTTL(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	limiter := &batchImageDownloadLimiter{
		rdb:          rdb,
		activePrefix: defaultBatchImageDownloadActivePrefix,
		maxActive:    1,
		ttl:          time.Minute,
	}

	permit, err := limiter.Acquire(ctx, "11", "zip")
	require.NoError(t, err)
	require.NotNil(t, permit)
	require.True(t, mr.TTL(limiter.activeKey("11")) > 0)

	_, err = limiter.Acquire(ctx, "11", "zip")
	require.ErrorIs(t, err, service.ErrBatchImageDownloadLimited)

	require.NoError(t, permit.Release(ctx))
	require.NoError(t, permit.Release(ctx))
	require.False(t, mr.Exists(limiter.activeKey("11")))

	permit, err = limiter.Acquire(ctx, "11", "zip")
	require.NoError(t, err)
	require.NotNil(t, permit)
}
