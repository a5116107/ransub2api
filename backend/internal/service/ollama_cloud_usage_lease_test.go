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

type ollamaLeaseRepo struct {
	*ollamaUsageTestRepo
	groupDelay time.Duration
	scans      atomic.Int32
}

func (r *ollamaLeaseRepo) ListOllamaCloudUsageGroupAccounts(ctx context.Context, anchors []*Account) ([]Account, error) {
	if r.groupDelay > 0 {
		select {
		case <-time.After(r.groupDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.ollamaUsageTestRepo.ListOllamaCloudUsageGroupAccounts(ctx, anchors)
}

func (r *ollamaLeaseRepo) ListDueOllamaCloudUsageAccounts(ctx context.Context, now time.Time, debounce, maxWait time.Duration, limit int) ([]Account, error) {
	r.scans.Add(1)
	return r.ollamaUsageTestRepo.ListDueOllamaCloudUsageAccounts(ctx, now, debounce, maxWait, limit)
}

type ollamaLeaseHTTP struct {
	HTTPUpstream
	call func(*http.Request) (*http.Response, error)
}

func (h ollamaLeaseHTTP) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return h.call(req)
}

func ollamaLeaderLeaseFixture(t *testing.T, count int) (*OllamaCloudUsageService, *ollamaLeaseRepo) {
	t.Helper()
	accounts := make(map[int64]*Account, count)
	for i := range count {
		a := ollamaUsageAccount(int64(i + 1))
		a.Credentials["api_key"] = fmt.Sprintf("fixture-ollama-lease-%d", i)
		a.Extra[OllamaCloudUsageSessionExtraKey] = fmt.Sprintf("cipher:wos-session=fixture-lease-%d", i)
		a.Extra[OllamaCloudUsageAutoRefreshExtraKey] = true
		accounts[a.ID] = a
	}
	repo := &ollamaLeaseRepo{ollamaUsageTestRepo: &ollamaUsageTestRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: accounts}}}
	settings := &upstreamBillingProbeSettingRepo{values: map[string]string{SettingKeyOllamaCloudUsageSettings: `{"enabled":true,"interval_minutes":60}`}}
	body := ollamaUsageFixture(t)
	svc := newOllamaUsageTestService(t, repo.ollamaUsageTestRepo, &ollamaUsageHTTPStub{body: body}, settings, true)
	svc.accountRepo = repo
	return svc, repo
}

func TestOllamaCloudUsageLeaderCoordinationErrorStopsBeforeScan(t *testing.T) {
	svc, repo := ollamaLeaderLeaseFixture(t, 1)
	upstream := svc.httpUpstream.(*ollamaUsageHTTPStub)
	svc.lockCache = &fakeLeaderLockCache{acquireErr: errors.New("fixture coordinator unavailable")}
	err := svc.RunDue(t.Context())
	require.Error(t, err, "a configured Redis failure must not degrade to uncoordinated execution")
	require.Zero(t, repo.scans.Load())
	require.Zero(t, upstream.calls.Load())
	require.NotContains(t, repo.accounts[1].Extra, OllamaCloudUsageSnapshotExtraKey)
	scan := svc.MonitoringRuntime().LastScan
	require.Equal(t, "coordination_unavailable", scan.State)
	require.Nil(t, scan.Candidates)
	require.Nil(t, scan.DueGroups)
}

func TestOllamaCloudUsageLeaderLeaseCoversSlowBatch(t *testing.T) {
	body := ollamaUsageFixture(t)
	synctest.Test(t, func(t *testing.T) {
		svc, repo := ollamaLeaderLeaseFixture(t, 20)
		repo.groupDelay = 10 * time.Second
		var calls, completed atomic.Int32
		svc.httpUpstream = ollamaLeaseHTTP{call: func(req *http.Request) (*http.Response, error) {
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
		time.Sleep(ollamaCloudUsageLeaderLockTTL + time.Second)
		synctest.Wait()
		acquired, err := cache.TryAcquireLeaderLock(t.Context(), ollamaCloudUsageLeaderLockKey, "fixture-peer", time.Minute)
		require.NoError(t, err)
		// Join the original batch even on the unrenewed implementation.
		require.NoError(t, <-done)
		require.False(t, acquired, "a 145-second batch with bounded 9-second requests must keep its two-minute lease")
		require.EqualValues(t, 20, calls.Load())
		require.EqualValues(t, 20, completed.Load())
		require.Empty(t, cache.heldBy(ollamaCloudUsageLeaderLockKey))
	})
}

func TestOllamaCloudUsageLeaderLossCancelsQueueAndRecovers(t *testing.T) {
	for _, mode := range []string{"redis_error", "expired", "new_owner", "deadline", "late_ack"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, repo := ollamaLeaderLeaseFixture(t, 20)
				repo.groupDelay = time.Minute
				var calls atomic.Int32
				svc.httpUpstream = ollamaLeaseHTTP{call: func(*http.Request) (*http.Response, error) {
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
				time.Sleep(ollamaCloudUsageLeaderLockTTL*2/3 + time.Second)
				synctest.Wait()
				// The watchdog cancels the queue even if renewal ignores its deadline.
				active := len(svc.refreshSlots)
				unblock()
				err := <-done
				require.ErrorIs(t, err, errOllamaCloudUsageLeaderLockLost, "a lost lease must stop the original automatic batch")
				require.Zero(t, active)
				require.Zero(t, calls.Load())
				require.EqualValues(t, 1, cache.renewals.Load())
				require.Equal(t, "coordination_unavailable", svc.MonitoringRuntime().LastScan.State)
				require.Nil(t, svc.MonitoringRuntime().LastScan.Candidates)
				for _, a := range repo.accounts {
					require.NotContains(t, a.Extra, OllamaCloudUsageSnapshotExtraKey)
					require.Equal(t, true, a.Extra[OllamaCloudUsageAutoRefreshExtraKey])
				}
				if mode == "new_owner" {
					require.Equal(t, "peer-batch", cache.heldBy(ollamaCloudUsageLeaderLockKey))
				}
				time.Sleep(ollamaCloudUsageLeaderLockTTL)
				synctest.Wait()
				require.EqualValues(t, 1, cache.renewals.Load(), "renewal worker must be drained")
				cache.mode, repo.groupDelay = "", 0
				svc.httpUpstream = &ollamaUsageHTTPStub{body: ollamaUsageFixture(t)}
				require.NoError(t, svc.RunDue(t.Context()))
				require.Equal(t, "observed", svc.MonitoringRuntime().LastScan.State)
				require.Empty(t, svc.refreshSlots)
				require.Empty(t, cache.heldBy(ollamaCloudUsageLeaderLockKey))
				for _, a := range repo.accounts {
					require.NotNil(t, decodeOllamaCloudUsageSnapshot(a.Extra))
				}
			})
		})
	}
}

func TestOllamaCloudUsageLeaderStopJoinsRenewalAndManualStaysIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, repo := ollamaLeaderLeaseFixture(t, 20)
		repo.groupDelay = time.Minute
		cache := &billingProbeLeaseFailureCache{}
		svc.lockCache = cache
		done := make(chan error, 1)
		go func() { done <- svc.RunDue(t.Context()) }()
		synctest.Wait()
		firstOwner := cache.heldBy(ollamaCloudUsageLeaderLockKey)
		require.NotEmpty(t, firstOwner)
		time.Sleep(41 * time.Second)
		synctest.Wait()
		require.EqualValues(t, 1, cache.renewals.Load())
		svc.Stop()
		svc.Stop()
		require.ErrorIs(t, <-done, context.Canceled)
		require.Empty(t, svc.refreshSlots)
		require.Empty(t, cache.heldBy(ollamaCloudUsageLeaderLockKey))
		require.ErrorIs(t, svc.RunDue(t.Context()), context.Canceled)
		time.Sleep(2 * ollamaCloudUsageLeaderLockTTL)
		synctest.Wait()
		require.EqualValues(t, 1, cache.renewals.Load())
		repo.groupDelay = 0
		state, err := svc.Refresh(t.Context(), 1)
		require.NoError(t, err)
		require.Equal(t, OllamaCloudUsageStatusOK, state.Snapshot.Status)
		require.Empty(t, cache.heldBy(ollamaCloudUsageLeaderLockKey), "manual refresh must not acquire an automatic-cycle lease")
	})
}
