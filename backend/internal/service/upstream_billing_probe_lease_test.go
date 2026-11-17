package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func slowBillingProbeLeaseFixture(t *testing.T) (*UpstreamBillingProbeService, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	first, auth, upstream, accounts := newAPIConfigFixture()
	_, err := first.SaveNewAPIConfig(context.Background(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
	require.NoError(t, err)
	original := *accounts.accounts[1]
	binding := *auth.bindings[1]
	accounts.accounts = make(map[int64]*Account)
	auth.bindings = make(map[int64]*NewAPIAccountBinding)
	for id := int64(1); id <= upstreamBillingProbeMaxPerCycle; id++ {
		a := original
		a.ID, a.Status = id, StatusActive
		a.Extra = map[string]any{UpstreamBillingProbeEnabledExtraKey: true}
		accounts.accounts[id] = &a
		b := binding
		b.AccountID = id
		auth.bindings[id] = &b
	}
	probes, writes := &atomic.Int64{}, &atomic.Int64{}
	upstream.check = func(req *http.Request) {
		var delay time.Duration
		switch req.URL.Path {
		case "/api/status":
			probes.Add(1)
			delay = 8 * time.Second
		case "/api/user/self", "/api/pricing":
			delay = 8 * time.Second
		case "/api/token/":
			delay = 4 * time.Second
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-req.Context().Done():
		}
	}
	auth.writeCheck = func(ctx context.Context) {
		require.NoError(t, ctx.Err(), "lost leadership must not manufacture failed or healthy evidence")
		writes.Add(1)
	}
	return first, probes, writes
}

// Five waves of four New API accounts take 140 seconds, even though every
// request fits its 10-second limit and every account fits its 30-second budget.
func TestUpstreamBillingProbeLeaderLeaseCoversSlowNewAPIBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, probes, writes := slowBillingProbeLeaseFixture(t)
		cache := &fakeLeaderLockCache{}
		first.SetLeaderLock(cache, nil)
		result := make(chan error, 1)
		go func() { result <- first.RunDue(context.Background()) }()
		synctest.Wait()
		require.Equal(t, int64(4), probes.Load())
		time.Sleep(upstreamBillingProbeLeaderLockTTL + time.Second)
		synctest.Wait()
		peer := newUpstreamBillingProbeTestService(first.accountRepo, first.accountTestService.httpUpstream, nil)
		peer.SetLeaderLock(cache, nil)
		release, acquired, err := peer.tryAcquireLeaderLock(context.Background(), upstreamBillingProbeLeaderLockKey, peer.instanceID)
		if acquired {
			release()
		}
		require.NoError(t, err)
		// Wait before asserting so the old batch always joins, including the red run.
		time.Sleep(20 * time.Second)
		require.NoError(t, <-result)
		require.False(t, acquired, "a second node must not acquire the main lock while the first batch is still running")
		require.Equal(t, int64(20), probes.Load())
		require.Equal(t, int64(20), writes.Load())
		require.Empty(t, cache.heldBy(upstreamBillingProbeLeaderLockKey))
	})
}

type billingProbeLeaseFailureCache struct {
	fakeLeaderLockCache
	mode     string
	renewals atomic.Int64
	unblock  chan struct{}
}

func (c *billingProbeLeaseFailureCache) RenewLeaderLock(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	c.renewals.Add(1)
	switch c.mode {
	case "redis_error":
		return false, errors.New("redis unavailable")
	case "expired":
		c.mu.Lock()
		delete(c.owners, key)
		c.mu.Unlock()
		return false, nil
	case "new_owner":
		c.mu.Lock()
		c.owners[key], c.expires[key] = "peer-batch", time.Now().Add(ttl)
		c.mu.Unlock()
		return false, nil
	case "deadline":
		<-ctx.Done()
		return false, ctx.Err()
	case "late_ack":
		<-c.unblock // Model a client that returns after its context expired.
		return true, nil
	}
	return c.fakeLeaderLockCache.RenewLeaderLock(ctx, key, owner, ttl)
}

func TestUpstreamBillingProbeLeaderLeaseLossStopsBatch(t *testing.T) {
	for _, mode := range []string{"redis_error", "expired", "new_owner", "deadline", "late_ack"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, probes, writes := slowBillingProbeLeaseFixture(t)
				cache := &billingProbeLeaseFailureCache{mode: mode, unblock: make(chan struct{})}
				unblock := sync.OnceFunc(func() { close(cache.unblock) })
				defer unblock()
				svc.SetLeaderLock(cache, nil)
				result := make(chan error, 1)
				go func() { result <- svc.RunDue(context.Background()) }()
				synctest.Wait()
				time.Sleep(upstreamBillingProbeLeaderLockTTL*2/3 + time.Second)
				synctest.Wait()
				// All queued probes and in-flight network requests must have joined
				// before the 120-second acknowledged lease could be reused.
				require.Empty(t, svc.probeSlots)
				require.Less(t, probes.Load(), int64(upstreamBillingProbeMaxPerCycle))
				require.Less(t, writes.Load(), probes.Load())
				unblock()
				require.ErrorIs(t, <-result, errUpstreamBillingProbeLeaderLockLost)
				require.Equal(t, int64(1), cache.renewals.Load())
				if mode == "new_owner" {
					require.Equal(t, "peer-batch", cache.heldBy(upstreamBillingProbeLeaderLockKey), "stale release must preserve the successor")
				}
				before := probes.Load()
				time.Sleep(upstreamBillingProbeLeaderLockTTL)
				synctest.Wait()
				require.Equal(t, before, probes.Load())
				require.Equal(t, int64(1), cache.renewals.Load(), "renewal worker must stop")
				cache.mode = ""
				require.NoError(t, svc.RunDue(context.Background()), "a recovered backend must allow the next batch")
				require.Equal(t, before+upstreamBillingProbeMaxPerCycle, probes.Load())
				require.Empty(t, svc.probeSlots)
				require.Empty(t, cache.heldBy(upstreamBillingProbeLeaderLockKey))
			})
		})
	}
}

func TestUpstreamBillingProbeLeaderLeaseNewBatchHasDistinctOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, writes := slowBillingProbeLeaseFixture(t)
		cache := &fakeLeaderLockCache{}
		svc.SetLeaderLock(cache, nil)
		start := func() (context.CancelFunc, <-chan error) {
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() { result <- svc.RunDue(ctx) }()
			synctest.Wait()
			return cancel, result
		}
		cancelFirst, first := start()
		firstOwner := cache.heldBy(upstreamBillingProbeLeaderLockKey)
		require.NotEmpty(t, firstOwner)
		cancelFirst()
		require.ErrorIs(t, <-first, context.Canceled)
		time.Sleep(upstreamBillingProbeCycleInterval + time.Second)
		cancelSecond, second := start()
		defer cancelSecond()
		secondOwner := cache.heldBy(upstreamBillingProbeLeaderLockKey)
		require.NotEmpty(t, secondOwner)
		require.NotEqual(t, firstOwner, secondOwner)
		owned, err := cache.RenewLeaderLock(context.Background(), upstreamBillingProbeLeaderLockKey, firstOwner, time.Hour)
		require.NoError(t, err)
		require.False(t, owned)
		require.NoError(t, cache.ReleaseLeaderLock(context.Background(), upstreamBillingProbeLeaderLockKey, firstOwner))
		require.Equal(t, secondOwner, cache.heldBy(upstreamBillingProbeLeaderLockKey))
		cancelSecond()
		require.ErrorIs(t, <-second, context.Canceled)
		require.Zero(t, writes.Load())
	})
}

func TestUpstreamBillingProbeLeaderLeaseCallerCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, probes, writes := slowBillingProbeLeaseFixture(t)
		cache := &billingProbeLeaseFailureCache{}
		svc.SetLeaderLock(cache, nil)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- svc.RunDue(ctx) }()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-result, context.Canceled)
		require.Equal(t, int64(4), probes.Load())
		require.Zero(t, writes.Load())
		require.Empty(t, svc.probeSlots)
		require.Empty(t, cache.heldBy(upstreamBillingProbeLeaderLockKey))
		time.Sleep(upstreamBillingProbeLeaderLockTTL * 2)
		synctest.Wait()
		require.Zero(t, cache.renewals.Load())
	})
}

type billingProbeCancelingTransport struct {
	*upstreamBillingProbeHTTPStub
	cancel context.CancelFunc
	fail   bool
}

func (h billingProbeCancelingTransport) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	response, err := h.upstreamBillingProbeHTTPStub.DoWithTLS(req, proxy, id, concurrency, profile)
	h.cancel()
	if h.fail {
		_ = response.Body.Close()
		return nil, errors.New("transport canceled")
	}
	return response, err
}

func TestUpstreamBillingProbeLeaderLeaseCanceledSub2APIWritesNothing(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy", true: "failure"}[fail], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
				Credentials: map[string]any{"api_key": "fixture-key", "base_url": "https://upstream.example"},
				Extra:       map[string]any{UpstreamBillingProbeEnabledExtraKey: true}}
			repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{1: account}}
			http := &upstreamBillingProbeHTTPStub{}
			svc := newUpstreamBillingProbeTestService(repo, billingProbeCancelingTransport{upstreamBillingProbeHTTPStub: http, cancel: cancel, fail: fail}, nil)
			require.ErrorIs(t, svc.RunDue(ctx), context.Canceled)
			require.Nil(t, account.Extra[UpstreamBillingProbeExtraKey])
			require.Zero(t, http.balanceCalls.Load(), "do not send the second physical request after cancellation")
		})
	}
}
