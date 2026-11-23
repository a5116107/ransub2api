//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type cnBalanceCheckLeaseCache struct {
	fakeLeaderLockCache
	acquire func(context.Context, string, string, time.Duration) (bool, error)
	renew   func(context.Context, string, string, time.Duration) (bool, error)
}

func (c *cnBalanceCheckLeaseCache) TryAcquireLeaderLock(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	if c.acquire != nil {
		return c.acquire(ctx, key, owner, ttl)
	}
	return c.fakeLeaderLockCache.TryAcquireLeaderLock(ctx, key, owner, ttl)
}

func (c *cnBalanceCheckLeaseCache) RenewLeaderLock(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	if c.renew != nil {
		return c.renew(ctx, key, owner, ttl)
	}
	return c.fakeLeaderLockCache.RenewLeaderLock(ctx, key, owner, ttl)
}

func TestCNProviderBalanceCheckSharedCycleAndRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, prober := &fakeLeaderLockCache{}, &fakeCNQuotaProber{}
		makeRunner := func() *CNProviderBalanceCheckService {
			s := NewCNProviderBalanceCheckService(cnQueueAccounts(t, 4), &CNProviderBalanceService{}, nil, &config.Config{}, time.Minute)
			s.quotaService = prober
			s.SetLeaderLock(cache)
			return s
		}
		first := makeRunner()
		first.runOnce(t.Context())
		require.Len(t, prober.probed, 4)
		require.Empty(t, cache.heldBy(cnBalanceCheckLeaderLockKey))
		key := fmt.Sprintf("%s:%d:%d", cnBalanceCheckLeaderLockKey, time.Minute, time.Now().UTC().Truncate(time.Minute).UnixNano())
		firstOwner := cache.heldBy(key)
		require.NotEmpty(t, firstOwner)
		first.runOnce(t.Context())
		makeRunner().runOnce(t.Context())
		require.Len(t, prober.probed, 4, "a fast finish or rebuilt process must not replay this UTC window")
		time.Sleep(time.Minute)
		first.runOnce(t.Context())
		require.Len(t, prober.probed, 8)
		nextKey := fmt.Sprintf("%s:%d:%d", cnBalanceCheckLeaderLockKey, time.Minute, time.Now().UTC().Truncate(time.Minute).UnixNano())
		require.NotEqual(t, firstOwner, cache.heldBy(nextKey), "successive runs need distinct owner tokens")
		require.Empty(t, cache.heldBy(cnBalanceCheckLeaderLockKey))
	})
}

func TestCNProviderBalanceCheckCoordinationFailureIsClosed(t *testing.T) {
	for _, mode := range []string{"main_error", "main_held", "cadence_error", "cadence_held", "canceled_after_main", "invalid_interval"} {
		t.Run(mode, func(t *testing.T) {
			cache := &cnBalanceCheckLeaseCache{}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cache.acquire = func(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
				main := key == cnBalanceCheckLeaderLockKey
				if (main && mode == "main_error") || (!main && mode == "cadence_error") {
					return false, errors.New("fixture coordination unavailable")
				}
				if (main && mode == "main_held") || (!main && mode == "cadence_held") {
					return false, nil
				}
				if err := ctx.Err(); err != nil {
					return false, err
				}
				ok, err := cache.fakeLeaderLockCache.TryAcquireLeaderLock(ctx, key, owner, ttl)
				if main && mode == "canceled_after_main" {
					cancel()
				}
				return ok, err
			}
			prober := &fakeCNQuotaProber{}
			var scans int
			repo := cnQueueInventoryRepo{list: func(context.Context, string) ([]Account, error) {
				scans++
				return nil, nil
			}}
			s := NewCNProviderBalanceCheckService(repo, &CNProviderBalanceService{}, nil, &config.Config{}, time.Minute)
			s.quotaService = prober
			s.SetLeaderLock(cache)
			if mode == "invalid_interval" {
				s.interval = 0
			}
			s.runOnce(ctx)
			require.Zero(t, scans, "coordination failure must precede inventory and all dispatch")
			require.Empty(t, prober.probed)
			require.Empty(t, cache.heldBy(cnBalanceCheckLeaderLockKey), "cancellation after acquisition still releases ownership")
		})
	}
}

func TestCNProviderBalanceCheckLeaderLeaseCoversSlowQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &fakeLeaderLockCache{}
		var calls, finished atomic.Int32
		prober := cnQueueQuotaProber{query: func(ctx context.Context, _ int64) (*CNProviderQuotaProbeResult, error) {
			calls.Add(1)
			select {
			case <-time.After(13 * time.Second):
				finished.Add(1)
				return &CNProviderQuotaProbeResult{Success: true}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}
		s := NewCNProviderBalanceCheckService(cnQueueAccounts(t, 40), &CNProviderBalanceService{}, nil, &config.Config{}, time.Minute)
		s.quotaService = prober
		s.SetLeaderLock(cache)
		done := make(chan struct{})
		go func() { defer close(done); s.runOnce(t.Context()) }()
		synctest.Wait()
		time.Sleep(cnBalanceCheckLeaderLockTTL + time.Second)
		synctest.Wait()
		peer := NewCNProviderBalanceCheckService(s.accountRepo, s.balanceService, nil, s.cfg, time.Minute)
		peer.quotaService = prober
		peer.SetLeaderLock(cache)
		peer.runOnce(t.Context())
		require.Empty(t, peer.turns, "a peer must not scan after the original lease TTL while the queue is running")
		require.NotEmpty(t, cache.heldBy(cnBalanceCheckLeaderLockKey))
		<-done
		require.EqualValues(t, 40, calls.Load())
		require.EqualValues(t, 40, finished.Load())
		require.Empty(t, cache.heldBy(cnBalanceCheckLeaderLockKey))
	})
}

func TestCNProviderBalanceCheckLeaderLossStopsAndRecovers(t *testing.T) {
	for _, mode := range []string{"redis_error", "expired", "new_owner", "deadline", "late_ack"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cache := &cnBalanceCheckLeaseCache{}
				unblock := make(chan struct{})
				finishRenewal := sync.OnceFunc(func() { close(unblock) })
				defer finishRenewal()
				var renewals, calls, active, writes atomic.Int32
				cache.renew = func(ctx context.Context, key, _ string, ttl time.Duration) (bool, error) {
					renewals.Add(1)
					switch mode {
					case "redis_error":
						return false, errors.New("fixture Redis unavailable")
					case "expired":
						return false, nil
					case "new_owner":
						cache.mu.Lock()
						cache.owners[key], cache.expires[key] = "peer-cycle", time.Now().Add(ttl)
						cache.mu.Unlock()
						return false, nil
					case "deadline":
						<-ctx.Done()
						return false, ctx.Err()
					default:
						<-unblock // A noncooperative client must not defeat the watchdog.
						return true, nil
					}
				}
				var recovering atomic.Bool
				s := NewCNProviderBalanceCheckService(cnQueueAccounts(t, 40), &CNProviderBalanceService{}, nil, &config.Config{}, time.Minute)
				s.SetLeaderLock(cache)
				s.quotaService = cnQueueQuotaProber{query: func(ctx context.Context, _ int64) (*CNProviderQuotaProbeResult, error) {
					calls.Add(1)
					active.Add(1)
					defer active.Add(-1)
					if !recovering.Load() {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					writes.Add(1)
					return &CNProviderQuotaProbeResult{Success: true}, nil
				}}
				done := make(chan struct{})
				go func() { defer close(done); s.runOnce(t.Context()) }()
				synctest.Wait()
				time.Sleep(cnBalanceCheckLeaderLockTTL*2/3 + time.Second)
				synctest.Wait()
				require.Zero(t, active.Load())
				require.EqualValues(t, 4, calls.Load(), "loss of ownership must cancel the original workers, not dispatch the queue")
				require.Zero(t, writes.Load(), "cancellation is not a fresh passing observation")
				finishRenewal()
				<-done
				require.EqualValues(t, 1, renewals.Load())
				if mode == "new_owner" {
					require.Equal(t, "peer-cycle", cache.heldBy(cnBalanceCheckLeaderLockKey), "stale cleanup must preserve the successor")
				}
				time.Sleep(cnBalanceCheckLeaderLockTTL)
				synctest.Wait()
				require.EqualValues(t, 1, renewals.Load(), "the renewal worker must be drained")
				cache.renew = nil
				recovering.Store(true)
				s.runOnce(t.Context())
				require.EqualValues(t, 44, calls.Load())
				require.EqualValues(t, 40, writes.Load())
				require.Empty(t, cache.heldBy(cnBalanceCheckLeaderLockKey))
			})
		})
	}
}

func TestCNProviderBalanceCheckLeaderStopDrainsAndCancelsRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &cnBalanceCheckLeaseCache{}
		var calls, renewals atomic.Int32
		cache.renew = func(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
			renewals.Add(1)
			return cache.fakeLeaderLockCache.RenewLeaderLock(ctx, key, owner, ttl)
		}
		cfg := &config.Config{}
		cfg.Gateway.CNProviders.BalanceCheckEnabled = true
		s := NewCNProviderBalanceCheckService(cnQueueAccounts(t, 40), &CNProviderBalanceService{}, nil, cfg, time.Minute)
		s.SetLeaderLock(cache)
		s.quotaService = cnQueueQuotaProber{query: func(ctx context.Context, _ int64) (*CNProviderQuotaProbeResult, error) {
			calls.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		s.Start()
		s.Start()
		synctest.Wait()
		time.Sleep(time.Minute + 41*time.Second)
		synctest.Wait()
		require.EqualValues(t, 4, calls.Load())
		require.EqualValues(t, 1, renewals.Load())
		s.Stop()
		s.Stop()
		require.True(t, s.drained)
		require.Empty(t, cache.heldBy(cnBalanceCheckLeaderLockKey))
		time.Sleep(2 * cnBalanceCheckLeaderLockTTL)
		synctest.Wait()
		require.EqualValues(t, 4, calls.Load(), "Stop must drain the original ticker and queue")
		require.EqualValues(t, 1, renewals.Load(), "Stop must also join the lease renewal worker")
	})
}
