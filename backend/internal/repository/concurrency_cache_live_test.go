package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// lostSlotReplyCache injects a transport failure after the real Redis adapter
// has committed its write, then cancels the original request before rollback.
type lostSlotReplyCache struct {
	service.ConcurrencyCache
	cancel context.CancelFunc
}

func (c *lostSlotReplyCache) AcquireAccountSlot(ctx context.Context, id int64, limit int, request string) (bool, error) {
	acquired, err := c.ConcurrencyCache.AcquireAccountSlot(ctx, id, limit, request)
	if err != nil || !acquired {
		return acquired, err
	}
	c.cancel()
	return false, context.DeadlineExceeded
}

func (c *lostSlotReplyCache) AcquireUserSlot(ctx context.Context, id int64, limit int, request string) (bool, error) {
	acquired, err := c.ConcurrencyCache.AcquireUserSlot(ctx, id, limit, request)
	if err != nil || !acquired {
		return acquired, err
	}
	c.cancel()
	return false, context.DeadlineExceeded
}

func TestConcurrencyLostReplyRestoresSharedCapacity(t *testing.T) {
	for _, dimension := range []string{"account", "user"} {
		t.Run(dimension, func(t *testing.T) {
			server := miniredis.RunT(t)
			first := redis.NewClient(&redis.Options{Addr: server.Addr()})
			second := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { require.NoError(t, first.Close()); require.NoError(t, second.Close()) })
			base := NewConcurrencyCache(first, 15, 900)
			peer := service.NewConcurrencyService(base)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			faulty := service.NewConcurrencyService(&lostSlotReplyCache{ConcurrencyCache: NewConcurrencyCache(second, 15, 900), cancel: cancel})
			acquire := func(s *service.ConcurrencyService, ctx context.Context) (*service.AcquireResult, error) {
				if dimension == "account" {
					return s.AcquireAccountSlot(ctx, 71, 2)
				}
				return s.AcquireUserSlot(ctx, 71, 2)
			}
			count := func() int {
				t.Helper()
				var n int
				var err error
				if dimension == "account" {
					n, err = base.GetAccountConcurrency(context.Background(), 71)
				} else {
					n, err = base.GetUserConcurrency(context.Background(), 71)
				}
				require.NoError(t, err)
				return n
			}
			held, err := acquire(peer, context.Background())
			require.NoError(t, err)
			require.True(t, held.Acquired)
			t.Cleanup(held.ReleaseFunc)
			result, err := acquire(faulty, ctx)
			require.Nil(t, result)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.Equal(t, 1, count(), "only the healthy peer's reservation must remain")
			peerKey := accountSlotKey(71)
			if dimension == "user" {
				peerKey = userSlotKey(71)
			}
			require.NoError(t, first.ZScore(context.Background(), peerKey, held.RequestID).Err())
			next, err := acquire(peer, context.Background())
			require.NoError(t, err)
			require.True(t, next.Acquired, "another instance must immediately reuse the recovered capacity")
			t.Cleanup(next.ReleaseFunc)
			require.Equal(t, 2, count())
			next.ReleaseFunc()
			held.ReleaseFunc()
			require.Zero(t, count())
		})
	}
}

func TestLiveLeaseReplacesRegularSlotsAndCountsTowardLimits(t *testing.T) {
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	regular := NewConcurrencyCache(client, 15, 900)
	live, ok := regular.(service.LiveConcurrencyCache)
	require.True(t, ok)
	ctx := context.Background()

	accountAcquired, err := regular.AcquireAccountSlot(ctx, 10, 1, "regular-account")
	require.NoError(t, err)
	require.True(t, accountAcquired)
	userAcquired, err := regular.AcquireUserSlot(ctx, 20, 1, "regular-user")
	require.NoError(t, err)
	require.True(t, userAcquired)

	acquired, err := live.AcquireLiveLease(ctx, 10, 1, 20, 1, 30, 0, "live-lease", true)
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, regular.ReleaseAccountSlot(ctx, 10, "regular-account"))
	require.NoError(t, regular.ReleaseUserSlot(ctx, 20, "regular-user"))

	accountCount, err := regular.GetAccountConcurrency(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, accountCount)
	userCount, err := regular.GetUserConcurrency(ctx, 20)
	require.NoError(t, err)
	require.Equal(t, 1, userCount)
	accountAcquired, err = regular.AcquireAccountSlot(ctx, 10, 1, "ordinary-blocked")
	require.NoError(t, err)
	require.False(t, accountAcquired)

	refreshed, err := live.RefreshLiveLease(ctx, 10, 20, 30, "live-lease")
	require.NoError(t, err)
	require.True(t, refreshed)
	require.NoError(t, live.ReleaseLiveLease(ctx, 10, 20, 30, "live-lease"))
	accountAcquired, err = regular.AcquireAccountSlot(ctx, 10, 1, "ordinary-allowed")
	require.NoError(t, err)
	require.True(t, accountAcquired)
}

func TestLiveLeaseExpiresWithoutRefresh(t *testing.T) {
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	regular := NewConcurrencyCache(client, 15, 900)
	live, ok := regular.(service.LiveConcurrencyCache)
	require.True(t, ok)
	ctx := context.Background()

	acquired, err := live.AcquireLiveLease(ctx, 10, 1, 20, 1, 30, 0, "expired-live", false)
	require.NoError(t, err)
	require.True(t, acquired)

	redisServer.FastForward(61 * time.Second)
	acquired, err = regular.AcquireAccountSlot(ctx, 10, 1, "ordinary-after-expiry")
	require.NoError(t, err)
	require.True(t, acquired)
	refreshed, err := live.RefreshLiveLease(ctx, 10, 20, 30, "expired-live")
	require.NoError(t, err)
	require.False(t, refreshed)
}
