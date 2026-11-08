package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type failNextVideoSlotRelease struct{ fail atomic.Bool }

func (*failNextVideoSlotRelease) DialHook(next redis.DialHook) redis.DialHook { return next }
func (*failNextVideoSlotRelease) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *failNextVideoSlotRelease) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "zrem" && h.fail.CompareAndSwap(true, false) {
			return errors.New("injected release failure")
		}
		return next(ctx, cmd)
	}
}

type blockVideoSlotRelease struct{ calls atomic.Int32 }

func (*blockVideoSlotRelease) DialHook(next redis.DialHook) redis.DialHook { return next }
func (*blockVideoSlotRelease) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *blockVideoSlotRelease) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "zrem" {
			h.calls.Add(1)
			<-ctx.Done()
			return ctx.Err()
		}
		return next(ctx, cmd)
	}
}

func asyncVideoLeaseRequest(t *testing.T, handoff *service.AsyncVideoKeyHandoff) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://upstream.example/create", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer supplier-fixture")
	ctx := service.WithAsyncVideoKeyGuard(req.Context(), true)
	return req.WithContext(service.WithAsyncVideoKeyHandoff(ctx, handoff))
}

func asyncVideoLeaseOwner() service.AsyncVideoKeyOwner {
	return service.AsyncVideoKeyOwner{UserID: 10, APIKeyID: 20, GroupID: 30, AccountID: 7, Provider: "grok", RouteFingerprint: [32]byte{1}}
}

func TestAsyncVideoKeyLeaseHandoffPersistsBindsAndReleasesAcrossInstances(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	cfg := upstreamKeyTestConfig(1, 1)
	first := liveKeyTestUpstream(t, cfg, rdb, nil)
	second := liveKeyTestUpstream(t, cfg, rdb, nil)
	controller, ok := any(first).(service.AsyncVideoKeyLeaseController)
	require.True(t, ok)

	owner := asyncVideoLeaseOwner()
	handoff := service.NewAsyncVideoKeyHandoff(owner)
	bound, finish, err := first.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.NoError(t, err)
	require.NotNil(t, bound)
	require.NotNil(t, handoff.Lease())
	require.Eventually(t, func() bool {
		return rdb.Exists(context.Background(), asyncVideoMemberKey(handoff.Member())).Val() == 1
	}, time.Second, 10*time.Millisecond, "the provisional member must be durable before the POST")
	require.NoError(t, rdb.ZScore(t.Context(), asyncVideoUnknownKey(), handoff.Member()).Err(), "provisional members must be discoverable after a crash")
	_, _, err = first.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "the same handoff cannot submit twice")

	require.NoError(t, controller.BindAsyncVideoKeyLease(t.Context(), handoff, "grok-task"))
	require.NoError(t, controller.BindAsyncVideoKeyLease(t.Context(), handoff, "grok-task"), "task binding must be idempotent during response handoff retries")
	require.Error(t, controller.BindAsyncVideoKeyLease(t.Context(), handoff, "different-task"), "one provisional member must never be rebound to a second task")
	require.NoError(t, controller.MarkAsyncVideoKeySubmissionUnknown(t.Context(), handoff), "a late transport error must not replace a bound task")
	require.Error(t, rdb.ZScore(t.Context(), asyncVideoUnknownKey(), handoff.Member()).Err())
	finish(nil, nil)
	competitor := service.NewAsyncVideoKeyHandoff(owner)
	_, err = first.AcquirePersistentUpstreamKeySlots(t.Context(), asyncVideoLeaseRequest(t, competitor))
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)

	// A fresh transport instance can release the bound task with only the
	// durable task record, which is the cross-instance recovery contract.
	require.NoError(t, second.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), owner, "grok-task"))
	require.Eventually(t, func() bool {
		return rdb.ZCard(context.Background(), upstreamKeySlotKeyPrefix+config.UpstreamKeyFingerprint("supplier-fixture")).Val() == 0 &&
			rdb.Exists(context.Background(), asyncVideoTaskKey(owner, "grok-task")).Val() == 0
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
	owner := asyncVideoLeaseOwner()
	handoff := service.NewAsyncVideoKeyHandoff(owner)
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
	require.NoError(t, recoveryController.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), owner, "recovered-task"))
	require.Eventually(t, func() bool {
		return rdb.ZScore(context.Background(), asyncVideoUnknownKey(), handoff.Member()).Err() != nil &&
			rdb.ZCard(context.Background(), upstreamKeySlotKeyPrefix+config.UpstreamKeyFingerprint("supplier-fixture")).Val() == 0
	}, time.Second, 10*time.Millisecond)
}

func TestAsyncVideoKeyLeaseAllowsStatusAtCapacity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, rdb := liveKeyRedis(t, nil)
	var creates, polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.Method == http.MethodPost {
			creates.Add(1)
			_, _ = io.WriteString(w, `{"id":"job-123"}`)
			return
		}
		polls.Add(1)
		_, _ = io.WriteString(w, `{"status":"succeeded","usage":{"completion_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	cfg := upstreamKeyTestConfig(1, 1)
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	transport := newHTTPUpstream(cfg, nil, rdb, nil)
	svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, transport, nil, nil, nil, nil, nil, nil, nil, nil)
	account := &service.Account{ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "supplier-fixture", "base_url": server.URL, "openai_capabilities": []string{"seedance"}}}
	owner := service.AsyncVideoKeyOwner{UserID: 10, APIKeyID: 20, GroupID: 30, AccountID: account.ID,
		Provider: "seedance", RouteFingerprint: service.AccountRouteFingerprint(account)}
	ctx := service.WithAsyncVideoKeyOwner(t.Context(), owner)
	makeContext := func(method string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(method, "/fixture", nil).WithContext(ctx)
		return c
	}
	created, err := svc.ForwardSeedance(ctx, makeContext(http.MethodPost), account, service.SeedanceEndpointCreate, "", []byte(`{"model":"video","content":[{"type":"text","text":"waves"}]}`))
	require.NoError(t, err)
	require.Equal(t, service.SeedanceTaskKey("job-123"), created.ResponseID)
	require.EqualValues(t, 1, creates.Load())
	status, err := svc.ForwardSeedance(ctx, makeContext(http.MethodGet), account, service.SeedanceEndpointStatus, created.ResponseID, nil)
	require.NoError(t, err, "the task must be able to observe its terminal state while its Key slot is held")
	require.True(t, status.AsyncVideoTerminal)
	require.EqualValues(t, 1, polls.Load())
	require.NoError(t, svc.ReleaseAsyncVideoTaskLease(ctx, created.ResponseID))
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/competing", strings.NewReader("{}"))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer supplier-fixture")
	response, err := transport.Do(request, "", 8, 1)
	require.NoError(t, err, "confirmed terminal state must release the member")
	require.NoError(t, response.Body.Close())
}

func TestAsyncVideoKeyLeaseTaskOwnership(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	owner := asyncVideoLeaseOwner()
	handoff := service.NewAsyncVideoKeyHandoff(owner)
	_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.NoError(t, err)
	require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), handoff, "same-id"))
	finish(nil, nil)
	for _, other := range []service.AsyncVideoKeyOwner{
		{UserID: owner.UserID + 1, APIKeyID: owner.APIKeyID, GroupID: owner.GroupID, AccountID: owner.AccountID, Provider: owner.Provider, RouteFingerprint: owner.RouteFingerprint},
		{UserID: owner.UserID, APIKeyID: owner.APIKeyID + 1, GroupID: owner.GroupID, AccountID: owner.AccountID, Provider: owner.Provider, RouteFingerprint: owner.RouteFingerprint},
		{UserID: owner.UserID, APIKeyID: owner.APIKeyID, GroupID: owner.GroupID + 1, AccountID: owner.AccountID, Provider: owner.Provider, RouteFingerprint: owner.RouteFingerprint},
		{UserID: owner.UserID, APIKeyID: owner.APIKeyID, GroupID: owner.GroupID, AccountID: owner.AccountID, Provider: "seedance", RouteFingerprint: owner.RouteFingerprint},
	} {
		lease, err := upstream.LoadAsyncVideoKeyLeaseByTask(t.Context(), other, "same-id")
		require.NoError(t, err)
		require.Nil(t, lease)
		require.NoError(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), other, "same-id"))
		require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()))
	}
	principal := owner
	principal.AccountID = 0
	principal.RouteFingerprint = [32]byte{}
	resolved, err := upstream.ResolveAsyncVideoKeyOwner(t.Context(), principal, "same-id")
	require.NoError(t, err)
	require.Equal(t, owner, *resolved)
	wrongAccount := owner
	wrongAccount.AccountID++
	_, err = upstream.LoadAsyncVideoKeyLeaseByTask(t.Context(), wrongAccount, "same-id")
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	require.ErrorIs(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), wrongAccount, "same-id"), service.ErrUpstreamKeyPolicy)
	changedRoute := owner
	changedRoute.RouteFingerprint[0]++
	_, err = upstream.LoadAsyncVideoKeyLeaseByTask(t.Context(), changedRoute, "same-id")
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "an edited account must not inherit the previous task route")
	require.ErrorIs(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), changedRoute, "same-id"), service.ErrUpstreamKeyPolicy)
	require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()))
	raw, err := rdb.Get(t.Context(), asyncVideoMemberKey(handoff.Member())).Bytes()
	require.NoError(t, err)
	fenced, err := fenceAsyncVideoKeyLeaseScript.Run(t.Context(), rdb,
		[]string{asyncVideoFenceKey(handoff.Member()), asyncVideoMemberKey(handoff.Member()), asyncVideoUnknownKey(), asyncVideoTaskKey(owner, "same-id"), asyncVideoReleaseKey()},
		handoff.Member(), raw, "0").Int()
	require.NoError(t, err)
	require.Equal(t, 1, fenced)
	_, err = upstream.LoadAsyncVideoKeyLeaseByTask(t.Context(), owner, "same-id")
	require.ErrorIs(t, err, service.ErrUpstreamKeyLease, "fenced tasks cannot send additional lookups")
	require.NoError(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), owner, "same-id"))
}

func TestAsyncVideoKeyLookupRejectsChangedRequest(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	owner := asyncVideoLeaseOwner()
	handoff := service.NewAsyncVideoKeyHandoff(owner)
	_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.NoError(t, err)
	require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), handoff, "task"))
	finish(nil, nil)

	request := httptest.NewRequest(http.MethodGet, "https://upstream.example/status/task", nil)
	request.Header.Set("Authorization", "Bearer supplier-fixture")
	bound := service.WithAsyncVideoKeyLookup(request, handoff.Lease())
	_, done, err := upstream.BeginUpstreamKeyRequest(bound)
	require.NoError(t, err)
	done(nil, nil)

	changedAuth := bound.Clone(bound.Context())
	changedAuth.Header = bound.Header.Clone()
	changedAuth.Header.Set("Authorization", "Bearer another-key")
	_, _, err = upstream.BeginUpstreamKeyRequest(changedAuth)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	changedURL := bound.Clone(bound.Context())
	changedURL.URL.Path = "/status/another-task"
	_, _, err = upstream.BeginUpstreamKeyRequest(changedURL)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	changedMethod := bound.Clone(bound.Context())
	changedMethod.Method = http.MethodPost
	_, _, err = upstream.BeginUpstreamKeyRequest(changedMethod)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()))
	require.NoError(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), owner, "task"))
}

func TestAsyncVideoKeyLeaseFencesDelayedAcquisition(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	owner := asyncVideoLeaseOwner()
	handoff := service.NewAsyncVideoKeyHandoff(owner)
	fingerprint := config.UpstreamKeyFingerprint("supplier-fixture")
	lease := &service.PersistentUpstreamKeyLease{Authority: "redis", Member: handoff.Member(),
		Fingerprints: []string{fingerprint}, Resources: []service.PersistentUpstreamKeyResource{{Fingerprint: fingerprint}}}
	handoff.SetLease(lease)
	require.NoError(t, upstream.PersistAsyncVideoKeyLease(t.Context(), handoff))
	require.NoError(t, rdb.ZScore(t.Context(), asyncVideoUnknownKey(), handoff.Member()).Err(), "a crash before acquisition retains the provisional owner")
	_, result, err := upstream.upstreamKeyLimiter.take(t.Context(), fingerprint, handoff.Member(), "video")
	require.NoError(t, err)
	require.Equal(t, 1, result)

	restarted := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	raw, err := rdb.Get(t.Context(), asyncVideoMemberKey(handoff.Member())).Bytes()
	require.NoError(t, err)
	fenced, err := fenceAsyncVideoKeyLeaseScript.Run(t.Context(), rdb,
		[]string{asyncVideoFenceKey(handoff.Member()), asyncVideoMemberKey(handoff.Member()), asyncVideoUnknownKey(), "", asyncVideoReleaseKey()},
		handoff.Member(), raw, "0").Int()
	require.NoError(t, err)
	require.Equal(t, 1, fenced)
	_, _, err = restarted.upstreamKeyLimiter.take(t.Context(), fingerprint, handoff.Member(), "video")
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "a delayed acquisition must not cross a release fence")
	require.NoError(t, restarted.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), handoff.Member()))
	_, _, err = restarted.upstreamKeyLimiter.take(t.Context(), fingerprint, handoff.Member(), "video")
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "a delayed acquisition must not recreate a deleted member")
	require.Zero(t, rdb.ZCard(t.Context(), upstreamKeySlotKeyPrefix+fingerprint).Val())
}

func TestAsyncVideoKeyLeasePartialAcquisitionRollsBack(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	first := config.UpstreamKeyFingerprint("supplier-fixture")
	second := config.UpstreamKeyFingerprint("second-fixture")
	cfg := &config.Config{}
	cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{first: 1, second: 1}}
	upstream := liveKeyTestUpstream(t, cfg, rdb, nil)
	_, release, err := upstream.AcquireUpstreamKeySlots(t.Context(), []string{second})
	require.NoError(t, err)
	require.NotNil(t, release)
	handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	request := asyncVideoLeaseRequest(t, handoff)
	request.Header.Set("X-Api-Key", "second-fixture")
	_, _, err = upstream.BeginUpstreamKeyRequest(request)
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	require.Zero(t, rdb.ZCard(t.Context(), upstreamKeySlotKeyPrefix+first).Val(), "the first credential must be returned")
	require.EqualValues(t, 1, rdb.ZCard(t.Context(), upstreamKeySlotKeyPrefix+second).Val(), "another request's member must remain")
	require.Zero(t, rdb.Exists(t.Context(), asyncVideoMemberKey(handoff.Member()), asyncVideoFenceKey(handoff.Member())).Val())
	require.Error(t, rdb.ZScore(t.Context(), asyncVideoUnknownKey(), handoff.Member()).Err())
	release()
	newHandoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	request = asyncVideoLeaseRequest(t, newHandoff)
	request.Header.Set("X-Api-Key", "second-fixture")
	_, finish, err := upstream.BeginUpstreamKeyRequest(request)
	require.NoError(t, err)
	finish(nil, nil)
	require.Len(t, newHandoff.Lease().Resources, 2)
	require.NoError(t, upstream.ReleaseAsyncVideoKeyLease(t.Context(), newHandoff))
}

func TestAsyncVideoKeyLeaseTaskIDCollisionDoesNotReplaceOwner(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 2), rdb, nil)
	owner := asyncVideoLeaseOwner()
	first := service.NewAsyncVideoKeyHandoff(owner)
	_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, first))
	require.NoError(t, err)
	require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), first, "same-task"))
	finish(nil, nil)
	other := owner
	other.AccountID++
	other.RouteFingerprint[0]++
	second := service.NewAsyncVideoKeyHandoff(other)
	_, finish, err = upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, second))
	require.NoError(t, err)
	require.Error(t, upstream.BindAsyncVideoKeyLease(t.Context(), second, "same-task"))
	finish(nil, nil)
	principal := owner
	principal.AccountID = 0
	principal.RouteFingerprint = [32]byte{}
	resolved, err := upstream.ResolveAsyncVideoKeyOwner(t.Context(), principal, "same-task")
	require.NoError(t, err)
	require.Equal(t, owner, *resolved)
	require.NoError(t, upstream.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), second.Member()))
	require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), first.Lease()), "the conflicting task must not release the original member")
	require.NoError(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), owner, "same-task"))
}

func TestAsyncVideoKeyReleaseRejectsMismatchedAndLegacyRecords(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			_, rdb := liveKeyRedis(t, nil)
			upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 2), rdb, nil)
			first := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
			_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, first))
			require.NoError(t, err)
			finish(nil, nil)
			second := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
			_, finish, err = upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, second))
			require.NoError(t, err)
			finish(nil, nil)

			raw, err := rdb.Get(t.Context(), asyncVideoMemberKey(first.Member())).Bytes()
			require.NoError(t, err)
			var record asyncVideoKeyLeaseRecord
			require.NoError(t, json.Unmarshal(raw, &record))
			if legacy {
				record.Owner = service.AsyncVideoKeyOwner{}
				record.State, record.TaskID = asyncVideoLeaseStateBound, "v1-task"
			} else {
				record.Lease = second.Lease()
			}
			damaged, err := json.Marshal(record)
			require.NoError(t, err)
			if legacy {
				var fields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(damaged, &fields))
				delete(fields, "owner")
				damaged, err = json.Marshal(fields)
				require.NoError(t, err)
				require.NoError(t, rdb.Set(t.Context(), asyncVideoKeyLeasePrefix+"task:v1-task", damaged, 0).Err())
			}
			require.NoError(t, rdb.Set(t.Context(), asyncVideoMemberKey(first.Member()), damaged, 0).Err())

			require.ErrorIs(t, upstream.BindAsyncVideoKeyLeaseByMember(t.Context(), first.Member(), "damaged-task"), service.ErrUpstreamKeyPolicy)
			require.ErrorIs(t, upstream.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), first.Member()), service.ErrUpstreamKeyPolicy)
			require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), first.Lease()))
			require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), second.Lease()))
			require.Equal(t, string(damaged), rdb.Get(t.Context(), asyncVideoMemberKey(first.Member())).Val())
			if legacy {
				resolved, resolveErr := upstream.ResolveAsyncVideoKeyOwner(t.Context(), first.Owner(), "v1-task")
				require.NoError(t, resolveErr)
				require.Nil(t, resolved, "an ownerless v1 task cannot be attributed to a principal")
				require.Equal(t, string(damaged), rdb.Get(t.Context(), asyncVideoKeyLeasePrefix+"task:v1-task").Val())
			}
		})
	}
}

func TestAsyncVideoKeyReleaseRequiresExistingFenceRecord(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.NoError(t, err)
	finish(nil, nil)
	require.NoError(t, rdb.Del(t.Context(), asyncVideoMemberKey(handoff.Member())).Err())

	err = upstream.ReleaseAsyncVideoKeyLease(t.Context(), handoff)
	require.ErrorIs(t, err, service.ErrUpstreamKeyLease)
	require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()), "missing ownership must not release a persistent slot")
}

func TestAsyncVideoKeyFenceRejectsStaleSnapshotAndMissingTaskIndex(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.NoError(t, err)
	finish(nil, nil)
	oldRaw, err := rdb.Get(t.Context(), asyncVideoMemberKey(handoff.Member())).Bytes()
	require.NoError(t, err)
	require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), handoff, "task"))

	result, err := fenceAsyncVideoKeyLeaseScript.Run(t.Context(), rdb,
		[]string{asyncVideoFenceKey(handoff.Member()), asyncVideoMemberKey(handoff.Member()), asyncVideoUnknownKey(), "", asyncVideoReleaseKey()},
		handoff.Member(), oldRaw, "0").Int()
	require.NoError(t, err)
	require.Zero(t, result, "a concurrent task binding must not be released from a stale snapshot")
	require.Zero(t, rdb.Exists(t.Context(), asyncVideoFenceKey(handoff.Member())).Val())
	require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()))

	require.NoError(t, rdb.Del(t.Context(), asyncVideoTaskKey(handoff.Owner(), "task")).Err())
	require.ErrorIs(t, upstream.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), handoff.Member()), service.ErrUpstreamKeyLease)
	require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()), "a missing task index must preserve the member")
}

func TestAsyncVideoKeyReleaseFailureKeepsFencedRecordForRetry(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.NoError(t, err)
	require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), handoff, "task"))
	finish(nil, nil)
	hook := &failNextVideoSlotRelease{}
	hook.fail.Store(true)
	rdb.AddHook(hook)

	err = upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), handoff.Owner(), "task")
	require.ErrorContains(t, err, "injected release failure")
	require.EqualValues(t, 1, rdb.Exists(t.Context(), asyncVideoFenceKey(handoff.Member())).Val())
	require.NoError(t, rdb.ZScore(t.Context(), asyncVideoUnknownKey(), handoff.Member()).Err())
	require.NoError(t, rdb.ZScore(t.Context(), asyncVideoKeyLeasePrefix+"release", handoff.Member()).Err(), "a failed fenced release must be discoverable separately from unknown submissions")
	require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()))
	_, err = upstream.LoadAsyncVideoKeyLeaseByTask(t.Context(), handoff.Owner(), "task")
	require.ErrorIs(t, err, service.ErrUpstreamKeyLease)

	restarted := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	require.NoError(t, restarted.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), handoff.Member()))
	require.Zero(t, rdb.Exists(t.Context(), asyncVideoMemberKey(handoff.Member()), asyncVideoTaskKey(handoff.Owner(), "task"), asyncVideoFenceKey(handoff.Member())).Val())
	require.Error(t, rdb.ZScore(t.Context(), asyncVideoUnknownKey(), handoff.Member()).Err())
	require.Error(t, restarted.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()))
	require.Error(t, rdb.ZScore(t.Context(), asyncVideoReleaseKey(), handoff.Member()).Err())
}

func TestAsyncVideoKeyRecoveryRetriesOnlyFencedMembers(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 2), rdb, nil)
	unknown := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, unknown))
	require.NoError(t, err)
	finish(nil, errors.New("submission result unknown"))
	pending := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	_, finish, err = upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, pending))
	require.NoError(t, err)
	require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), pending, "task"))
	finish(nil, nil)
	hook := &failNextVideoSlotRelease{}
	hook.fail.Store(true)
	rdb.AddHook(hook)
	require.Error(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), pending.Owner(), "task"))

	// A corrupt queue entry without a release fence must never release an
	// unknown submission, nor prevent the next fenced entry from recovering.
	require.NoError(t, rdb.ZAdd(t.Context(), asyncVideoReleaseKey(), redis.Z{Score: float64(time.Now().Add(-time.Minute).UnixMilli()), Member: unknown.Member()}).Err())
	recovered, err := upstream.RecoverAsyncVideoKeyReleases(t.Context(), 2)
	require.ErrorIs(t, err, service.ErrUpstreamKeyLease)
	require.Equal(t, 1, recovered)
	require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), unknown.Lease()))
	require.Error(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), pending.Lease()))
	require.Error(t, rdb.ZScore(t.Context(), asyncVideoReleaseKey(), pending.Member()).Err())
	score, err := rdb.ZScore(t.Context(), asyncVideoReleaseKey(), unknown.Member()).Result()
	require.NoError(t, err)
	require.Greater(t, score, float64(time.Now().UnixMilli()), "a failed item must back off")
	recovered, err = upstream.RecoverAsyncVideoKeyReleases(t.Context(), 2)
	require.NoError(t, err)
	require.Zero(t, recovered, "a deferred item must not be retried immediately")
}

func TestAsyncVideoKeyRecoveryRetriesFailedRelease(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.NoError(t, err)
	require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), handoff, "task"))
	finish(nil, nil)
	hook := &failNextVideoSlotRelease{}
	hook.fail.Store(true)
	rdb.AddHook(hook)
	require.Error(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), handoff.Owner(), "task"))
	hook.fail.Store(true)
	recovered, err := upstream.RecoverAsyncVideoKeyReleases(t.Context(), 1)
	require.ErrorContains(t, err, "injected release failure")
	require.Zero(t, recovered)
	require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()))
	score, err := rdb.ZScore(t.Context(), asyncVideoReleaseKey(), handoff.Member()).Result()
	require.NoError(t, err)
	require.Greater(t, score, float64(time.Now().UnixMilli()))
	require.NoError(t, rdb.ZAdd(t.Context(), asyncVideoReleaseKey(), redis.Z{Score: float64(time.Now().Add(-time.Second).UnixMilli()), Member: handoff.Member()}).Err())
	restarted := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	recovered, err = restarted.RecoverAsyncVideoKeyReleases(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, recovered)
	require.Error(t, restarted.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()))
	require.Error(t, rdb.ZScore(t.Context(), asyncVideoReleaseKey(), handoff.Member()).Err())
}

func TestAsyncVideoKeyRecoveryConcurrentWorkers(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.NoError(t, err)
	require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), handoff, "task"))
	finish(nil, nil)
	hook := &failNextVideoSlotRelease{}
	hook.fail.Store(true)
	rdb.AddHook(hook)
	require.Error(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), handoff.Owner(), "task"))

	workers := []*httpUpstreamService{upstream, liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)}
	start := make(chan struct{})
	errs := make(chan error, len(workers))
	var group sync.WaitGroup
	for _, worker := range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, recoverErr := worker.RecoverAsyncVideoKeyReleases(t.Context(), 1)
			errs <- recoverErr
		}()
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Error(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()))
	require.Error(t, rdb.ZScore(t.Context(), asyncVideoReleaseKey(), handoff.Member()).Err())
	require.Zero(t, rdb.Exists(t.Context(), asyncVideoMemberKey(handoff.Member()), asyncVideoFenceKey(handoff.Member())).Val())
}

func TestAsyncVideoKeyRecoveryUsesRedisClock(t *testing.T) {
	server, rdb := liveKeyRedis(t, nil)
	server.SetTime(time.Now().Add(6 * time.Hour))
	upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.NoError(t, err)
	require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), handoff, "task"))
	finish(nil, nil)
	hook := &failNextVideoSlotRelease{}
	hook.fail.Store(true)
	rdb.AddHook(hook)
	require.Error(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), handoff.Owner(), "task"))
	serverTime, err := rdb.Time(t.Context()).Result()
	require.NoError(t, err)
	score, err := rdb.ZScore(t.Context(), asyncVideoReleaseKey(), handoff.Member()).Result()
	require.NoError(t, err)
	require.Equal(t, float64(serverTime.UnixMilli()), score, "initial enqueue must also use the Redis clock")

	recovered, err := upstream.RecoverAsyncVideoKeyReleases(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, recovered, "the Redis clock, not this process clock, determines when a release is due")
	require.Error(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()))
}

func TestAsyncVideoKeyRecoveryReclaimsAbandonedClaim(t *testing.T) {
	server, rdb := liveKeyRedis(t, nil)
	upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.NoError(t, err)
	require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), handoff, "task"))
	finish(nil, nil)
	hook := &failNextVideoSlotRelease{}
	hook.fail.Store(true)
	rdb.AddHook(hook)
	require.Error(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), handoff.Owner(), "task"))

	now, err := rdb.Time(t.Context()).Result()
	require.NoError(t, err)
	claimUntil := now.Add(2 * time.Minute).UnixMilli()
	claimed, err := claimAsyncVideoKeyReleaseScript.Run(t.Context(), rdb, []string{asyncVideoReleaseKey()}, handoff.Member(), now.UnixMilli(), claimUntil).Int()
	require.NoError(t, err)
	require.Equal(t, 1, claimed)
	score, err := rdb.ZScore(t.Context(), asyncVideoReleaseKey(), handoff.Member()).Result()
	require.NoError(t, err, "claim must retain a recoverable queue entry if the worker exits")
	require.Equal(t, float64(claimUntil), score)
	recovered, err := upstream.RecoverAsyncVideoKeyReleases(t.Context(), 1)
	require.NoError(t, err)
	require.Zero(t, recovered, "an unexpired claim remains exclusive")
	server.SetTime(now.Add(2*time.Minute + time.Second))
	recovered, err = upstream.RecoverAsyncVideoKeyReleases(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, recovered, "another worker must recover after the claim expires")
	require.Error(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()))
}

func TestAsyncVideoKeyRecoveryDoesNotRequeueCompletedRelease(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	upstream := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.NoError(t, err)
	require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), handoff, "task"))
	finish(nil, nil)
	raw, err := rdb.Get(t.Context(), asyncVideoMemberKey(handoff.Member())).Bytes()
	require.NoError(t, err)
	fenced, err := fenceAsyncVideoKeyLeaseScript.Run(t.Context(), rdb,
		[]string{asyncVideoFenceKey(handoff.Member()), asyncVideoMemberKey(handoff.Member()), asyncVideoUnknownKey(), asyncVideoTaskKey(handoff.Owner(), "task"), asyncVideoReleaseKey()},
		handoff.Member(), raw, "0").Int()
	require.NoError(t, err)
	require.Equal(t, 1, fenced)
	now, err := rdb.Time(t.Context()).Result()
	require.NoError(t, err)
	claimUntil := now.Add(asyncVideoReleaseClaimTTL).UnixMilli()
	claimed, err := claimAsyncVideoKeyReleaseScript.Run(t.Context(), rdb, []string{asyncVideoReleaseKey()}, handoff.Member(), now.UnixMilli(), claimUntil).Int()
	require.NoError(t, err)
	require.Equal(t, 1, claimed)
	require.NoError(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), handoff.Owner(), "task"))
	deferred, err := deferAsyncVideoKeyReleaseScript.Run(t.Context(), rdb, []string{asyncVideoReleaseKey()}, handoff.Member(), claimUntil, 30000).Int()
	require.NoError(t, err)
	require.Zero(t, deferred, "a late worker must not recreate a completed queue entry")
	require.Error(t, rdb.ZScore(t.Context(), asyncVideoReleaseKey(), handoff.Member()).Err())
}

func TestAsyncVideoKeyRecoveryHonorsCancellation(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	cfg := upstreamKeyTestConfig(1, 1)
	cfg.Gateway.UpstreamKeyConcurrency.Limits[config.UpstreamKeyFingerprint("second-fixture")] = 1
	upstream := liveKeyTestUpstream(t, cfg, rdb, nil)
	handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	request := asyncVideoLeaseRequest(t, handoff)
	request.Header.Set("X-Api-Key", "second-fixture")
	_, finish, err := upstream.BeginUpstreamKeyRequest(request)
	require.NoError(t, err)
	require.Len(t, handoff.Lease().Resources, 2)
	finish(nil, nil)
	raw, err := rdb.Get(t.Context(), asyncVideoMemberKey(handoff.Member())).Bytes()
	require.NoError(t, err)
	fenced, err := fenceAsyncVideoKeyLeaseScript.Run(t.Context(), rdb,
		[]string{asyncVideoFenceKey(handoff.Member()), asyncVideoMemberKey(handoff.Member()), asyncVideoUnknownKey(), "", asyncVideoReleaseKey()},
		handoff.Member(), raw, "0").Int()
	require.NoError(t, err)
	require.Equal(t, 1, fenced)
	hook := &blockVideoSlotRelease{}
	rdb.AddHook(hook)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = upstream.RecoverAsyncVideoKeyReleases(ctx, 1)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), 1500*time.Millisecond, "shutdown must not wait for each resource's independent timeout")
	require.EqualValues(t, 1, hook.calls.Load())
	require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()))
	require.NoError(t, rdb.ZScore(t.Context(), asyncVideoReleaseKey(), handoff.Member()).Err(), "cancelled recovery must remain retryable")
}
