//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type openCodeLeaseRepo struct {
	*openCodeGoUsageTestRepo
	groupDelay time.Duration
	scans      atomic.Int32
}

func (r *openCodeLeaseRepo) ListOpenCodeGoUsageGroupAccounts(ctx context.Context, anchors []*Account) ([]Account, error) {
	if r.groupDelay > 0 {
		select {
		case <-time.After(r.groupDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.openCodeGoUsageTestRepo.ListOpenCodeGoUsageGroupAccounts(ctx, anchors)
}

func (r *openCodeLeaseRepo) ListDueOpenCodeGoUsageAccounts(ctx context.Context, now time.Time, debounce, maxWait time.Duration, limit int) ([]Account, error) {
	r.scans.Add(1)
	return r.openCodeGoUsageTestRepo.ListDueOpenCodeGoUsageAccounts(ctx, now, debounce, maxWait, limit)
}

type openCodeLeaseHTTP struct {
	HTTPUpstream
	call func(*http.Request) (*http.Response, error)
}

func (h openCodeLeaseHTTP) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return h.call(req)
}

func openCodeLeaderLeaseFixture(t *testing.T, count int) (*OpenCodeGoUsageService, *openCodeLeaseRepo) {
	t.Helper()
	accounts := make(map[int64]*Account, count)
	for i := range count {
		a := openCodeGoUsageAccount(int64(i + 1))
		a.Credentials["api_key"] = fmt.Sprintf("fixture-opencode-lease-%d", i)
		a.Extra[OpenCodeGoUsageAutoRefreshExtraKey] = true
		accounts[a.ID] = a
	}
	repo := &openCodeLeaseRepo{openCodeGoUsageTestRepo: &openCodeGoUsageTestRepo{accounts: accounts}}
	settings := &upstreamBillingProbeSettingRepo{values: map[string]string{SettingKeyOpenCodeGoUsageSettings: `{"enabled":true,"interval_minutes":60}`}}
	body := []byte(openCodeGoUsageFixture)
	svc := newOpenCodeGoUsageTestService(t, repo.openCodeGoUsageTestRepo, &openCodeGoUsageHTTPStub{body: body}, settings)
	svc.accountRepo = repo
	return svc, repo
}

func TestOpenCodeGoUsageLeaderCoordinationErrorStopsBeforeScan(t *testing.T) {
	svc, repo := openCodeLeaderLeaseFixture(t, 1)
	upstream := svc.httpUpstream.(*openCodeGoUsageHTTPStub)
	svc.lockCache = &fakeLeaderLockCache{acquireErr: errors.New("fixture coordinator unavailable")}
	err := svc.RunDue(t.Context())
	require.Error(t, err, "a configured Redis failure must not degrade to uncoordinated execution")
	require.Zero(t, repo.scans.Load())
	require.Zero(t, upstream.calls.Load())
	require.NotContains(t, repo.accounts[1].Extra, OpenCodeGoUsageSnapshotExtraKey)
}

func TestOpenCodeGoUsageLeaderLeaseCoversSlowBatch(t *testing.T) {
	body := []byte(openCodeGoUsageFixture)
	synctest.Test(t, func(t *testing.T) {
		svc, repo := openCodeLeaderLeaseFixture(t, 20)
		repo.groupDelay = 10 * time.Second
		var calls, completed atomic.Int32
		svc.httpUpstream = openCodeLeaseHTTP{call: func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			select {
			case <-time.After(9 * time.Second):
				completed.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}}
		cache := &fakeLeaderLockCache{}
		svc.lockCache = cache
		done := make(chan error, 1)
		go func() { done <- svc.RunDue(t.Context()) }()
		synctest.Wait()
		time.Sleep(opencodeGoUsageLeaderLockTTL + time.Second)
		synctest.Wait()
		acquired, err := cache.TryAcquireLeaderLock(t.Context(), opencodeGoUsageLeaderLockKey, "fixture-peer", time.Minute)
		require.NoError(t, err)
		// Join the original batch even on the unrenewed implementation.
		require.NoError(t, <-done)
		require.False(t, acquired, "a 145-second batch with bounded 9-second requests must keep its two-minute lease")
		require.EqualValues(t, 20, calls.Load())
		require.EqualValues(t, 20, completed.Load())
		require.Empty(t, cache.heldBy(opencodeGoUsageLeaderLockKey))
	})
}

func TestOpenCodeGoUsageLeaderLossCancelsQueueAndRecovers(t *testing.T) {
	for _, mode := range []string{"redis_error", "expired", "new_owner", "deadline", "late_ack"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, repo := openCodeLeaderLeaseFixture(t, 20)
				repo.groupDelay = time.Minute
				var calls atomic.Int32
				svc.httpUpstream = openCodeLeaseHTTP{call: func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					return nil, errors.New("unexpected dispatch after loss")
				}}
				cache := &billingProbeLeaseFailureCache{mode: mode, unblock: make(chan struct{})}
				unblock := sync.OnceFunc(func() { close(cache.unblock) })
				defer unblock()
				svc.lockCache = cache
				done := make(chan error, 1)
				go func() { done <- svc.RunDue(t.Context()) }()
				synctest.Wait()
				time.Sleep(opencodeGoUsageLeaderLockTTL*2/3 + time.Second)
				synctest.Wait()
				// The watchdog cancels the queue even if renewal ignores its deadline.
				active := len(svc.refreshSlots)
				unblock()
				err := <-done
				require.Error(t, err, "a lost lease must stop the original automatic batch")
				require.Zero(t, active)
				require.Zero(t, calls.Load())
				require.EqualValues(t, 1, cache.renewals.Load())
				for _, a := range repo.accounts {
					require.NotContains(t, a.Extra, OpenCodeGoUsageSnapshotExtraKey)
					require.Equal(t, true, a.Extra[OpenCodeGoUsageAutoRefreshExtraKey])
				}
				if mode == "new_owner" {
					require.Equal(t, "peer-batch", cache.heldBy(opencodeGoUsageLeaderLockKey))
				}
				time.Sleep(opencodeGoUsageLeaderLockTTL)
				synctest.Wait()
				require.EqualValues(t, 1, cache.renewals.Load(), "renewal worker must be drained")
				cache.mode, repo.groupDelay = "", 0
				svc.httpUpstream = &openCodeGoUsageHTTPStub{body: []byte(openCodeGoUsageFixture)}
				require.NoError(t, svc.RunDue(t.Context()))
				require.Empty(t, svc.refreshSlots)
				require.Empty(t, cache.heldBy(opencodeGoUsageLeaderLockKey))
				for _, a := range repo.accounts {
					require.NotNil(t, decodeOpenCodeGoUsageSnapshot(a.Extra))
				}
			})
		})
	}
}

func TestOpenCodeGoUsageLeaderStopJoinsRenewalAndManualStaysIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, repo := openCodeLeaderLeaseFixture(t, 20)
		repo.groupDelay = time.Minute
		cache := &billingProbeLeaseFailureCache{}
		svc.lockCache = cache
		done := make(chan error, 1)
		go func() { done <- svc.RunDue(t.Context()) }()
		synctest.Wait()
		firstOwner := cache.heldBy(opencodeGoUsageLeaderLockKey)
		require.NotEmpty(t, firstOwner)
		time.Sleep(41 * time.Second)
		synctest.Wait()
		require.EqualValues(t, 1, cache.renewals.Load())
		svc.Stop()
		svc.Stop()
		require.ErrorIs(t, <-done, context.Canceled)
		require.Empty(t, svc.refreshSlots)
		require.Empty(t, cache.heldBy(opencodeGoUsageLeaderLockKey))
		require.ErrorIs(t, svc.RunDue(t.Context()), context.Canceled)
		time.Sleep(2 * opencodeGoUsageLeaderLockTTL)
		synctest.Wait()
		require.EqualValues(t, 1, cache.renewals.Load())
		repo.groupDelay = 0
		state, err := svc.Refresh(t.Context(), 1)
		require.NoError(t, err)
		require.Equal(t, OpenCodeGoUsageStatusOK, state.Snapshot.Status)
		require.Empty(t, cache.heldBy(opencodeGoUsageLeaderLockKey), "manual refresh must not acquire an automatic-cycle lease")
	})
}
