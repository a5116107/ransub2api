//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func sharedConcurrencyRedisClients(t *testing.T, count int) ([]*redis.Client, string) {
	t.Helper()
	// Share keys, not connection pools. The clients use the isolated Docker
	// Redis supplied by TestMain, never an application configuration or URL.
	prefix := fmt.Sprintf("it:%s:%d:%d:", sanitizeRedisNamespace(t.Name()), time.Now().UnixNano(), atomic.AddUint64(&redisNamespaceSeq, 1))
	clients := make([]*redis.Client, count)
	connectionIDs := make(map[int64]struct{}, count)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := range clients {
		opts := *integrationRedis.Options()
		opts.PoolSize = 1
		opts.ContextTimeoutEnabled = true
		client := NewNamespacedRedisClient(&opts, prefix)
		t.Cleanup(func() { require.NoError(t, client.Close()) })
		id, err := client.ClientID(ctx).Result()
		require.NoError(t, err)
		require.NotContains(t, connectionIDs, id, "the contenders must use distinct Redis connections")
		connectionIDs[id] = struct{}{}
		clients[i] = client
	}
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		var cursor uint64
		for {
			keys, next, err := integrationRedis.Scan(cleanupCtx, cursor, prefix+"*", 100).Result()
			require.NoError(t, err)
			if len(keys) > 0 {
				require.NoError(t, integrationRedis.Unlink(cleanupCtx, keys...).Err())
			}
			cursor = next
			if cursor == 0 {
				return
			}
		}
	})
	return clients, prefix
}

// NewNamespacedRedisClient exposes the same test hook to external-package
// process fixtures without duplicating its command-key rules.
func NewNamespacedRedisClient(opts *redis.Options, prefix string) *redis.Client {
	client := redis.NewClient(opts)
	client.AddHook(prefixHook{prefix: prefix})
	return client
}

func TestSharedAccountAndUserConcurrencyAcrossIndependentClients(t *testing.T) {
	clients, _ := sharedConcurrencyRedisClients(t, 2)
	var caches [2]service.ConcurrencyCache
	for i := range caches {
		caches[i] = NewConcurrencyCache(clients[i], 15, 60)
	}

	for _, kind := range []string{"account", "user"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			const resourceID int64 = 71001
			acquire := func(node int, requestID string) (bool, error) {
				if kind == "account" {
					return caches[node].AcquireAccountSlot(ctx, resourceID, 1, requestID)
				}
				return caches[node].AcquireUserSlot(ctx, resourceID, 1, requestID)
			}
			release := func(node int, requestID string) error {
				if kind == "account" {
					return caches[node].ReleaseAccountSlot(ctx, resourceID, requestID)
				}
				return caches[node].ReleaseUserSlot(ctx, resourceID, requestID)
			}
			assertCount := func(want int) {
				t.Helper()
				for _, cache := range caches {
					var count int
					var err error
					if kind == "account" {
						count, err = cache.GetAccountConcurrency(ctx, resourceID)
					} else {
						count, err = cache.GetUserConcurrency(ctx, resourceID)
					}
					require.NoError(t, err)
					require.Equal(t, want, count, "both nodes must observe the same shared occupancy")
				}
			}
			type result struct {
				node     int
				acquired bool
				err      error
			}
			start := make(chan struct{})
			ready := make(chan struct{}, len(caches))
			results := make(chan result, len(caches))
			requestIDs := [2]string{"node-a-request", "node-b-request"}
			var workers sync.WaitGroup
			for node := range caches {
				workers.Add(1)
				go func(node int) {
					defer workers.Done()
					ready <- struct{}{}
					<-start
					acquired, err := acquire(node, requestIDs[node])
					results <- result{node: node, acquired: acquired, err: err}
				}(node)
			}
			for range caches {
				select {
				case <-ready:
				case <-ctx.Done():
					close(start)
					workers.Wait()
					t.Fatal("independent Redis contenders did not reach the start barrier")
				}
			}
			close(start)
			// Join workers before fixture cleanup even when an assertion fails.
			defer workers.Wait()
			var received []result
			for range caches {
				select {
				case value := <-results:
					received = append(received, value)
				case <-ctx.Done():
					t.Fatal("independent Redis contenders did not finish before the deadline")
				}
			}
			winner, successes := -1, 0
			for _, value := range received {
				require.NoError(t, value.err)
				if value.acquired {
					winner = value.node
					successes++
				}
			}
			require.Equal(t, 1, successes, "one shared slot must admit exactly one contender")
			assertCount(1)
			loser := 1 - winner
			acquired, err := acquire(loser, requestIDs[loser])
			require.NoError(t, err)
			require.False(t, acquired, "the losing node must remain blocked while the winner holds its slot")
			assertCount(1)
			require.NoError(t, release(winner, requestIDs[winner]))
			assertCount(0)
			acquired, err = acquire(loser, requestIDs[loser])
			require.NoError(t, err)
			require.True(t, acquired, "the original losing node must observe the released capacity")
			assertCount(1)
			require.NoError(t, release(loser, requestIDs[loser]))
			assertCount(0)
		})
	}
}
