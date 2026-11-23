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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type billingProbeLifecycleTransport struct {
	HTTPUpstream
	check func(*http.Request) (*http.Response, error)
}

func (h billingProbeLifecycleTransport) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.check(req)
}

func billingProbeLifecycleFixture() (*upstreamBillingProbeAccountRepo, *upstreamBillingProbeSettingRepo) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"api_key": "fixture-key", "base_url": "https://upstream.example"},
		Extra:       map[string]any{UpstreamBillingProbeEnabledExtraKey: true}}
	return &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{1: account}},
		&upstreamBillingProbeSettingRepo{values: map[string]string{SettingKeyUpstreamBillingProbeSettings: `{"enabled":true,"interval_minutes":30}`}}
}

func TestUpstreamBillingProbeLifecycleRepeatedStopWaitsForDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo, settings := billingProbeLifecycleFixture()
		entered, canceled, drain := make(chan struct{}), make(chan struct{}), make(chan struct{})
		release := sync.OnceFunc(func() { close(drain) })
		defer release()
		svc := newUpstreamBillingProbeTestService(repo, billingProbeLifecycleTransport{check: func(req *http.Request) (*http.Response, error) {
			close(entered)
			<-req.Context().Done()
			close(canceled)
			<-drain
			return nil, req.Context().Err()
		}}, settings)
		svc.Start()
		<-entered
		first, second := make(chan struct{}), make(chan struct{})
		go func() { svc.Stop(); close(first) }()
		<-canceled
		go func() { svc.Stop(); close(second) }()
		synctest.Wait()
		select {
		case <-first:
			assert.Fail(t, "initial Stop returned while a native request was still draining")
		default:
		}
		select {
		case <-second:
			assert.Fail(t, "repeated Stop returned before the same drain completed")
		default:
		}
		release()
		<-first
		<-second
		require.Empty(t, svc.probeSlots)
		require.Nil(t, repo.accounts[1].Extra[UpstreamBillingProbeExtraKey])
	})
}

func TestUpstreamBillingProbeLifecycleExternalDueStopsAndDrains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo, settings := billingProbeLifecycleFixture()
		entered, canceled, drain := make(chan struct{}), make(chan struct{}), make(chan struct{})
		release := sync.OnceFunc(func() { close(drain) })
		defer release()
		svc := newUpstreamBillingProbeTestService(repo, billingProbeLifecycleTransport{check: func(req *http.Request) (*http.Response, error) {
			close(entered)
			select {
			case <-req.Context().Done():
				close(canceled)
				<-drain
			case <-drain:
			}
			return nil, context.Canceled
		}}, settings)
		done := make(chan error, 1)
		go func() { done <- svc.RunDue(context.Background()) }()
		<-entered
		stopped := make(chan struct{})
		go func() { svc.Stop(); close(stopped) }()
		synctest.Wait()
		select {
		case <-canceled:
		default:
			assert.Fail(t, "service Stop did not cancel the external automatic batch")
		}
		select {
		case <-stopped:
			assert.Fail(t, "service Stop did not wait for the external automatic batch")
		default:
		}
		release()
		<-stopped
		require.ErrorIs(t, <-done, context.Canceled)
		require.Empty(t, svc.probeSlots)
		require.Nil(t, repo.accounts[1].Extra[UpstreamBillingProbeExtraKey])
	})
}

func TestUpstreamBillingProbeLifecycleStoppedDueDoesNotSend(t *testing.T) {
	repo, settings := billingProbeLifecycleFixture()
	upstream := &upstreamBillingProbeHTTPStub{}
	svc := newUpstreamBillingProbeTestService(repo, upstream, settings)
	svc.Stop()
	require.ErrorIs(t, svc.RunDue(t.Context()), context.Canceled)
	require.Zero(t, upstream.calls.Load())
	require.Zero(t, upstream.balanceCalls.Load())
	require.Nil(t, repo.accounts[1].Extra[UpstreamBillingProbeExtraKey])
	result, err := svc.ProbeAccount(t.Context(), 1)
	require.NoError(t, err, "stopping the automatic runner must not disable manual probes")
	require.Equal(t, UpstreamBillingProbeStatusOK, result.Status)
	require.EqualValues(t, 1, upstream.calls.Load())
	require.EqualValues(t, 1, upstream.balanceCalls.Load())
}

type billingProbeLifecycleDueRepo struct {
	*upstreamBillingProbeAccountRepo
	scans atomic.Int64
}

func (r *billingProbeLifecycleDueRepo) ListDueUpstreamBillingProbeAccounts(ctx context.Context, _ time.Time, _ int) ([]Account, error) {
	r.scans.Add(1)
	account, err := r.GetByID(ctx, 1)
	if err != nil {
		return nil, err
	}
	return []Account{*account}, nil
}

// Observing caller-context attachment gives the test a real admission barrier,
// without using synthetic-clock waits on goroutines blocked in cycleMu.Lock.
type billingProbeLifecycleContext struct {
	context.Context
	attached chan struct{}
	once     sync.Once
}

func (c *billingProbeLifecycleContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.attached) })
	return c.Context.Done()
}

func TestUpstreamBillingProbeLifecycleQueuedDueStopsWithoutScanning(t *testing.T) {
	accounts, settings := billingProbeLifecycleFixture()
	repo := &billingProbeLifecycleDueRepo{upstreamBillingProbeAccountRepo: accounts}
	entered, canceled, drain := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(drain) })
	defer release()
	svc := newUpstreamBillingProbeTestService(repo, billingProbeLifecycleTransport{check: func(req *http.Request) (*http.Response, error) {
		close(entered)
		<-req.Context().Done()
		close(canceled)
		<-drain
		return nil, req.Context().Err()
	}}, settings)
	t.Cleanup(svc.Stop)
	results := make(chan error, 21)
	go func() { results <- svc.RunDue(t.Context()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("native billing batch did not enter its physical request")
	}
	for range 20 {
		ctx := &billingProbeLifecycleContext{Context: t.Context(), attached: make(chan struct{})}
		go func() { results <- svc.RunDue(ctx) }()
		select {
		case <-ctx.attached:
		case <-time.After(5 * time.Second):
			t.Fatal("queued automatic batch did not attach its caller lifetime")
		}
	}
	stopped := make(chan struct{})
	go func() { svc.Stop(); close(stopped) }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not cancel the active billing request")
	}
	require.EqualValues(t, 1, repo.scans.Load())
	select {
	case <-stopped:
		assert.Fail(t, "Stop returned before queued automatic work had drained")
	default:
	}
	release()
	for range 21 {
		select {
		case err := <-results:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("queued automatic batch did not finish cancellation")
		}
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not complete the shared drain")
	}
	require.EqualValues(t, 1, repo.scans.Load(), "retired queued batches must not rescan inventory")
	require.Empty(t, svc.probeSlots)
	require.Nil(t, accounts.accounts[1].Extra[UpstreamBillingProbeExtraKey])
}

func TestUpstreamBillingProbeLifecycleConcurrentStartRunStop(t *testing.T) {
	for range 25 {
		_, settings := billingProbeLifecycleFixture()
		svc := newUpstreamBillingProbeTestService(&upstreamBillingProbeAccountRepo{}, &upstreamBillingProbeHTTPStub{}, settings)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); <-start; svc.Start() }()
		go func() { defer wg.Done(); <-start; _ = svc.RunDue(t.Context()) }()
		go func() { defer wg.Done(); <-start; svc.Stop() }()
		close(start)
		wg.Wait()
		svc.Stop()
		svc.Start()
		require.ErrorIs(t, svc.RunDue(t.Context()), context.Canceled)
		require.Empty(t, svc.probeSlots)
	}
}

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
