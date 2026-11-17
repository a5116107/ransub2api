//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newLeaderLockTestCache(t *testing.T) (*leaderLockCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &leaderLockCache{rdb: rdb}, mr
}

func TestLeaderLockCache_AcquireContendedRelease(t *testing.T) {
	cache, _ := newLeaderLockTestCache(t)
	ctx := context.Background()
	const key = "dashboard:aggregation:leader"

	ok, err := cache.TryAcquireLeaderLock(ctx, key, "A", time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "first owner should acquire")

	ok, err = cache.TryAcquireLeaderLock(ctx, key, "B", time.Minute)
	require.NoError(t, err)
	require.False(t, ok, "peer must be locked out while held")

	require.NoError(t, cache.ReleaseLeaderLock(ctx, key, "A"))

	ok, err = cache.TryAcquireLeaderLock(ctx, key, "B", time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "peer should acquire after release")
}

// A stale owner whose lock expired and was re-acquired by a peer must not delete
// the peer's lock when its late release fires (compare-and-delete by owner).
func TestLeaderLockCache_ReleaseIsCompareAndDelete(t *testing.T) {
	cache, _ := newLeaderLockTestCache(t)
	ctx := context.Background()
	const key = "payment:order:expiry:leader"

	ok, err := cache.TryAcquireLeaderLock(ctx, key, "A", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	// Simulate A's lock expiring and peer B taking ownership.
	require.NoError(t, cache.rdb.Set(ctx, leaderLockKeyPrefix+key, "B", time.Minute).Err())

	// A's stale release must be a no-op against B's lock.
	require.NoError(t, cache.ReleaseLeaderLock(ctx, key, "A"))

	val, err := cache.rdb.Get(ctx, leaderLockKeyPrefix+key).Result()
	require.NoError(t, err)
	require.Equal(t, "B", val, "stale owner must not delete the new owner's lock")
}

func TestLeaderLockCache_TTLExpires(t *testing.T) {
	cache, mr := newLeaderLockTestCache(t)
	ctx := context.Background()
	const key = "subscription:expiry:reminder:leader"

	ok, err := cache.TryAcquireLeaderLock(ctx, key, "A", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	mr.FastForward(2 * time.Minute)

	ok, err = cache.TryAcquireLeaderLock(ctx, key, "B", time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "lock should be re-acquirable after the TTL expires")
}

func TestLeaderLockCache_RenewUsesCurrentOwnerAndTTL(t *testing.T) {
	cache, mr := newLeaderLockTestCache(t)
	ctx := context.Background()
	const key = "billing:renew"
	owned, err := cache.TryAcquireLeaderLock(ctx, key, "batch-A", time.Minute)
	require.NoError(t, err)
	require.True(t, owned)
	mr.FastForward(50 * time.Second)
	owned, err = cache.RenewLeaderLock(ctx, key, "batch-A", time.Minute)
	require.NoError(t, err)
	require.True(t, owned)
	require.Equal(t, time.Minute, mr.TTL(leaderLockKeyPrefix+key))
	mr.FastForward(20 * time.Second)
	owned, err = cache.TryAcquireLeaderLock(ctx, key, "batch-B", time.Minute)
	require.NoError(t, err)
	require.False(t, owned, "renewal must outlive the original expiry")
	mr.FastForward(41 * time.Second)
	owned, err = cache.TryAcquireLeaderLock(ctx, key, "batch-B", time.Minute)
	require.NoError(t, err)
	require.True(t, owned, "renewal must retain crash expiry")
}

func TestLeaderLockCache_RenewNeverRevivesOrExtendsSuccessor(t *testing.T) {
	for _, mode := range []string{"missing", "expired", "foreign_owner", "same_instance_new_batch"} {
		t.Run(mode, func(t *testing.T) {
			cache, mr := newLeaderLockTestCache(t)
			ctx := context.Background()
			const key = "billing:renew:stale"
			if mode != "missing" {
				owned, err := cache.TryAcquireLeaderLock(ctx, key, "instance-A:batch-1", time.Minute)
				require.NoError(t, err)
				require.True(t, owned)
				mr.FastForward(61 * time.Second)
			}
			successor := "instance-B:batch-1"
			if mode == "same_instance_new_batch" {
				successor = "instance-A:batch-2"
			}
			if mode == "foreign_owner" || mode == "same_instance_new_batch" {
				owned, err := cache.TryAcquireLeaderLock(ctx, key, successor, time.Minute)
				require.NoError(t, err)
				require.True(t, owned)
				mr.FastForward(10 * time.Second)
			}
			owned, err := cache.RenewLeaderLock(ctx, key, "instance-A:batch-1", time.Hour)
			require.NoError(t, err)
			require.False(t, owned)
			require.NoError(t, cache.ReleaseLeaderLock(ctx, key, "instance-A:batch-1"))
			if mode == "foreign_owner" || mode == "same_instance_new_batch" {
				value, err := cache.rdb.Get(ctx, leaderLockKeyPrefix+key).Result()
				require.NoError(t, err)
				require.Equal(t, successor, value)
				require.Equal(t, 50*time.Second, mr.TTL(leaderLockKeyPrefix+key))
			} else {
				require.False(t, mr.Exists(leaderLockKeyPrefix+key))
			}
		})
	}
}

func TestLeaderLockCache_RenewRejectsInvalidTTL(t *testing.T) {
	cache, mr := newLeaderLockTestCache(t)
	ctx := context.Background()
	owned, err := cache.TryAcquireLeaderLock(ctx, "billing:invalid-ttl", "batch-A", time.Minute)
	require.NoError(t, err)
	require.True(t, owned)
	for _, ttl := range []time.Duration{-time.Second, 0, time.Microsecond} {
		owned, err = cache.RenewLeaderLock(ctx, "billing:invalid-ttl", "batch-A", ttl)
		require.Error(t, err)
		require.False(t, owned)
		require.Equal(t, time.Minute, mr.TTL(leaderLockKeyPrefix+"billing:invalid-ttl"))
	}
}
