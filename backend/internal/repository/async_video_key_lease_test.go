package repository

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func asyncVideoLeaseRequest(t *testing.T, handoff *service.AsyncVideoKeyHandoff) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://upstream.example/create", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer supplier-fixture")
	ctx := service.WithAsyncVideoKeyGuard(req.Context(), true)
	return req.WithContext(service.WithAsyncVideoKeyHandoff(ctx, handoff))
}

func TestAsyncVideoKeyLeaseHandoffPersistsBindsAndReleasesAcrossInstances(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	cfg := upstreamKeyTestConfig(1, 1)
	first := liveKeyTestUpstream(t, cfg, rdb, nil)
	second := liveKeyTestUpstream(t, cfg, rdb, nil)
	controller, ok := any(first).(service.AsyncVideoKeyLeaseController)
	require.True(t, ok)

	handoff := service.NewAsyncVideoKeyHandoff()
	bound, finish, err := first.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.NoError(t, err)
	require.NotNil(t, bound)
	require.NotNil(t, handoff.Lease())
	require.Eventually(t, func() bool {
		return rdb.Exists(context.Background(), asyncVideoMemberKey(handoff.Member())).Val() == 1
	}, time.Second, 10*time.Millisecond, "the provisional member must be durable before the POST")

	require.NoError(t, controller.BindAsyncVideoKeyLease(t.Context(), handoff, "grok-task"))
	require.NoError(t, controller.BindAsyncVideoKeyLease(t.Context(), handoff, "grok-task"), "task binding must be idempotent during response handoff retries")
	finish(nil, nil)
	competitor := service.NewAsyncVideoKeyHandoff()
	_, err = first.AcquirePersistentUpstreamKeySlots(t.Context(), asyncVideoLeaseRequest(t, competitor))
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)

	// A fresh transport instance can release the bound task with only the
	// durable task record, which is the cross-instance recovery contract.
	require.NoError(t, second.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), "grok-task"))
	require.Eventually(t, func() bool {
		return rdb.ZCard(context.Background(), upstreamKeySlotKeyPrefix+config.UpstreamKeyFingerprint("supplier-fixture")).Val() == 0 &&
			rdb.Exists(context.Background(), asyncVideoTaskKey("grok-task")).Val() == 0
	}, time.Second, 10*time.Millisecond)

	lease, err := first.AcquirePersistentUpstreamKeySlots(t.Context(), asyncVideoLeaseRequest(t, competitor))
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.NoError(t, first.ReleasePersistentUpstreamKeySlots(t.Context(), lease))
}

func TestAsyncVideoKeyLeaseUnknownResultRetainsExactMemberUntilRelease(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	recovery := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	_, ok := any(upstream).(service.AsyncVideoKeyLeaseController)
	require.True(t, ok)
	handoff := service.NewAsyncVideoKeyHandoff()
	_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.NoError(t, err)
	require.NotNil(t, handoff.Lease())

	finish(nil, errors.New("connection reset after POST"))
	require.Eventually(t, func() bool {
		return rdb.ZScore(context.Background(), asyncVideoUnknownKey(), handoff.Member()).Val() > 0
	}, time.Second, 10*time.Millisecond, "unknown results must remain indexed for recovery")
	require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()))
	recoveryController, ok := any(recovery).(service.AsyncVideoKeyLeaseController)
	require.True(t, ok)
	require.NoError(t, recoveryController.BindAsyncVideoKeyLeaseByMember(t.Context(), handoff.Member(), "recovered-task"))
	require.NoError(t, recoveryController.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), "recovered-task"))
	require.Eventually(t, func() bool {
		return rdb.ZScore(context.Background(), asyncVideoUnknownKey(), handoff.Member()).Err() != nil &&
			rdb.ZCard(context.Background(), upstreamKeySlotKeyPrefix+config.UpstreamKeyFingerprint("supplier-fixture")).Val() == 0
	}, time.Second, 10*time.Millisecond)
}
