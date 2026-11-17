//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLeaderLockRedisConcurrentRenewalAndRecovery(t *testing.T) {
	cache := &leaderLockCache{rdb: testRedis(t)}
	ctx := context.Background()
	const key = "billing:lease:integration"
	type result struct {
		owner    string
		acquired bool
		err      error
	}
	results := make(chan result, 20)
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			owner := fmt.Sprintf("node-%d:batch-1", i)
			acquired, err := cache.TryAcquireLeaderLock(ctx, key, owner, 2*time.Minute)
			results <- result{owner, acquired, err}
		}()
	}
	close(start)
	group.Wait()
	close(results)
	owners := []string{}
	for r := range results {
		require.NoError(t, r.err)
		if r.acquired {
			owners = append(owners, r.owner)
		}
	}
	require.Len(t, owners, 1, "exactly one of twenty contenders must acquire the batch")
	first := owners[0]
	owned, err := cache.RenewLeaderLock(ctx, key, first, time.Hour)
	require.NoError(t, err)
	require.True(t, owned)
	ttl, err := cache.rdb.PTTL(ctx, leaderLockKeyPrefix+key).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 30*time.Minute)
	owned, err = cache.TryAcquireLeaderLock(ctx, key, "other-node", time.Minute)
	require.NoError(t, err)
	require.False(t, owned)
	// Exercise actual Redis expiry, not an emulator's clock or a manual delete.
	require.NoError(t, cache.rdb.PExpire(ctx, leaderLockKeyPrefix+key, time.Millisecond).Err())
	require.Eventually(t, func() bool {
		count, queryErr := cache.rdb.Exists(ctx, leaderLockKeyPrefix+key).Result()
		return queryErr == nil && count == 0
	}, 3*time.Second, 5*time.Millisecond)
	owned, err = cache.RenewLeaderLock(ctx, key, first, time.Hour)
	require.NoError(t, err)
	require.False(t, owned, "a missing lease must never be recreated by renewal")
	second := first + ":batch-2"
	owned, err = cache.TryAcquireLeaderLock(ctx, key, second, time.Minute)
	require.NoError(t, err)
	require.True(t, owned)
	owned, err = cache.RenewLeaderLock(ctx, key, first, time.Hour)
	require.NoError(t, err)
	require.False(t, owned)
	require.NoError(t, cache.ReleaseLeaderLock(ctx, key, first))
	value, err := cache.rdb.Get(ctx, leaderLockKeyPrefix+key).Result()
	require.NoError(t, err)
	require.Equal(t, second, value, "the expired batch must neither extend nor delete its successor")
	ttl, err = cache.rdb.PTTL(ctx, leaderLockKeyPrefix+key).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, time.Minute)
	require.NoError(t, cache.ReleaseLeaderLock(ctx, key, second))
}
