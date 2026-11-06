package repository

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	miniserver "github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func liveKeyTestUpstream(t *testing.T, cfg *config.Config, rdb *redis.Client, cache service.ConcurrencyCache) *httpUpstreamService {
	t.Helper()
	s, ok := newHTTPUpstream(cfg, nil, rdb, cache).(*httpUpstreamService)
	require.True(t, ok)
	return s
}

func liveKeyRedis(t *testing.T, overrides map[string]string) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	metadata := map[string]string{"appendonly": "yes", "appendfsync": "always", "no-appendfsync-on-rewrite": "no", "maxmemory-policy": "noeviction"}
	for key, value := range overrides {
		metadata[key] = value
	}
	// Only CONFIG metadata is stubbed; Lua, ZSET, TTL and cache data are real
	// miniredis behavior. The isolated Redis process exercises actual CONFIG.
	server.Server().SetPreHook(func(peer *miniserver.Peer, command string, args ...string) bool {
		if command != "CONFIG" || len(args) != 2 || !strings.EqualFold(args[0], "GET") {
			return false
		}
		if metadata["deny"] == "yes" {
			peer.WriteError("NOPERM CONFIG unavailable")
			return true
		}
		peer.WriteLen(2)
		peer.WriteBulk(args[1])
		peer.WriteBulk(metadata[args[1]])
		return true
	})
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	return server, rdb
}

// Astra wraps the production transport. HTTP, WS, Live and batch must still
// share the same authority rather than failing its optional interface checks.
func TestAstraSharedUpstreamKeyAuthority(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	testAstraSharedUpstreamKeyAuthority(t, rdb)
}

func testAstraSharedUpstreamKeyAuthority(t *testing.T, rdb *redis.Client, caches ...service.ConcurrencyCache) {
	t.Helper()
	var cache service.ConcurrencyCache
	if len(caches) != 0 {
		cache = caches[0]
	}
	cfg := upstreamKeyTestConfig(1, 1)
	cfg.Gateway.CodexGatewayPin.Enabled = true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(upstream.Close)
	transport := newHTTPUpstream(cfg, nil, rdb, cache)
	require.IsType(t, &astraRoutingUpstream{}, transport)
	ordinary, ok := transport.(service.UpstreamKeyConcurrency)
	require.True(t, ok, "wrapped transport must retain the WS authority")
	persistent, ok := transport.(service.PersistentUpstreamKeyConcurrency)
	require.True(t, ok, "wrapped transport must retain the Live authority")
	admission, ok := transport.(service.UpstreamKeyRequestAdmission)
	require.True(t, ok, "wrapped transport must retain provider-specific HTTP admission")
	_, ok = transport.(service.BatchImageUpstreamKeyAuthority)
	require.True(t, ok, "wrapped transport must retain the combined batch authority")

	request := func(path string) *http.Request {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, upstream.URL+path, strings.NewReader("{}"))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer supplier-fixture")
		return req
	}
	fp := config.UpstreamKeyFingerprint("supplier-fixture")
	_, release, err := ordinary.AcquireUpstreamKeySlots(t.Context(), []string{fp})
	require.NoError(t, err)
	require.NotNil(t, release)
	t.Cleanup(release)
	resp, err := transport.Do(request("/v1/responses"), "", 4, 4)
	require.Nil(t, resp)
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	release()
	resp, err = transport.Do(request("/v1/responses"), "", 4, 4)
	require.NoError(t, err, "a wrapped ordinary request must acquire exactly once")
	_, _, err = ordinary.AcquireUpstreamKeySlots(t.Context(), []string{fp})
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity, "an open HTTP body shares its slot with native WS")
	require.NoError(t, resp.Body.Close())

	liveRequest := request("/v1/realtime/calls")
	lease, err := persistent.AcquirePersistentUpstreamKeySlots(t.Context(), liveRequest)
	require.NoError(t, err)
	require.NotNil(t, lease)
	t.Cleanup(func() { require.NoError(t, persistent.ReleasePersistentUpstreamKeySlots(context.Background(), lease)) })
	require.NoError(t, persistent.CheckPersistentUpstreamKeySlots(t.Context(), lease))
	bound := liveRequest.WithContext(service.WithPersistentUpstreamKeyLease(t.Context(), lease))
	resp, err = transport.Do(bound, "", 4, 4)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	_, _, err = admission.BeginUpstreamKeyRequest(request("/v1/responses"))
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity, "closing the Live response must retain its shared lease")
	require.NoError(t, persistent.ReleasePersistentUpstreamKeySlots(t.Context(), lease))
	bound, finish, err := admission.BeginUpstreamKeyRequest(request("/v1/responses"))
	require.NoError(t, err)
	require.NotNil(t, bound)
	require.NotNil(t, finish)
	finish(nil, nil)
}

func TestAstraSharedUpstreamKeyAuthorityMissingDelegateFailsClosed(t *testing.T) {
	for _, delegate := range []service.HTTPUpstream{nil, gatewayPinDelegate{}} {
		transport := &astraRoutingUpstream{delegate: delegate}
		_, release, err := transport.AcquireUpstreamKeySlots(t.Context(), []string{config.UpstreamKeyFingerprint("fixture")})
		require.Nil(t, release)
		require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
		lease, err := transport.AcquirePersistentUpstreamKeySlots(t.Context(), httptest.NewRequest(http.MethodPost, "/v1/realtime/calls", nil))
		require.Nil(t, lease)
		require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
		require.ErrorIs(t, transport.CheckPersistentUpstreamKeySlots(t.Context(), nil), service.ErrUpstreamKeyPolicy)
		require.ErrorIs(t, transport.ReleasePersistentUpstreamKeySlots(t.Context(), nil), service.ErrUpstreamKeyPolicy)
		_, finish, err := transport.BeginUpstreamKeyRequest(httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
		require.Nil(t, finish)
		require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	}
}

func TestAstraSharedUpstreamKeyAuthorityDisabledKeepsOriginalBehavior(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin.Enabled = true
	transport := newHTTPUpstream(cfg, nil, nil, nil)
	ordinary, ok := transport.(service.UpstreamKeyConcurrency)
	require.True(t, ok)
	ctx, release, err := ordinary.AcquireUpstreamKeySlots(t.Context(), []string{config.UpstreamKeyFingerprint("unlimited")})
	require.NoError(t, err)
	require.Same(t, t.Context(), ctx)
	require.Nil(t, release)
	persistent, ok := transport.(service.PersistentUpstreamKeyConcurrency)
	require.True(t, ok)
	req := httptest.NewRequest(http.MethodPost, "/v1/realtime/calls", nil)
	lease, err := persistent.AcquirePersistentUpstreamKeySlots(t.Context(), req)
	require.NoError(t, err)
	require.Nil(t, lease)
	admission, ok := transport.(service.UpstreamKeyRequestAdmission)
	require.True(t, ok)
	bound, finish, err := admission.BeginUpstreamKeyRequest(req)
	require.NoError(t, err)
	require.Same(t, req, bound)
	require.NotNil(t, finish)
	finish(nil, nil)
}

func TestUpstreamKeyLivePersistsAcrossBodyCloseAndProcessHandoff(t *testing.T) {
	server, rdb := liveKeyRedis(t, nil)
	testUpstreamKeyLivePersistence(t, rdb, func() { server.FastForward(2 * upstreamKeyLeaseTTL) })
}

func testUpstreamKeyLivePersistence(t *testing.T, rdb *redis.Client, advance func()) {
	t.Helper()
	cfg := upstreamKeyTestConfig(1, 2)
	a := liveKeyTestUpstream(t, cfg, rdb, nil)
	b := liveKeyTestUpstream(t, cfg, rdb, nil)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/calls/call_fixture")
		_, _ = io.WriteString(w, "v=0\r\n")
	}))
	t.Cleanup(upstream.Close)
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/v1/realtime/calls", strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer supplier-fixture")
	lease, err := a.AcquirePersistentUpstreamKeySlots(ctx, req)
	require.NoError(t, err)
	require.NoError(t, lease.Validate())
	t.Cleanup(func() { require.NoError(t, b.ReleasePersistentUpstreamKeySlots(context.Background(), lease)) })
	resp, err := a.Do(req.WithContext(service.WithPersistentUpstreamKeyLease(ctx, lease)), "", 10, 10)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	cancel()

	key := upstreamKeySlotKeyPrefix + config.UpstreamKeyFingerprint("supplier-fixture")
	require.EqualValues(t, 1, rdb.ZCard(t.Context(), key).Val(), "SDP/body close must not end directly connected media")
	require.Equal(t, -time.Nanosecond, rdb.TTL(t.Context(), key).Val(), "Live ownership must have no automatic key expiry")
	store, ok := NewGatewayCache(rdb).(service.LiveCallStore)
	require.True(t, ok)
	record := &service.LiveCallRecord{CallID: "call_fixture", CallHash: HashLiveCallID("call_fixture"), LeaseID: "joint-fixture", UpstreamKeyLease: lease, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, store.SaveLiveCall(t.Context(), record, time.Hour))
	restored, err := store.GetLiveCall(t.Context(), record.CallHash)
	require.NoError(t, err)
	require.Equal(t, lease, restored.UpstreamKeyLease)
	require.NoError(t, b.CheckPersistentUpstreamKeySlots(t.Context(), restored.UpstreamKeyLease))

	ordinaryCtx, ordinaryCancel := context.WithCancelCause(t.Context())
	defer ordinaryCancel(nil)
	ordinary, err := b.upstreamKeyLimiter.acquire(ordinaryCtx, lease.Resources[0].Fingerprint, ordinaryCancel)
	require.NoError(t, err)
	require.NotNil(t, ordinary)
	defer ordinary.release()
	_, err = a.AcquirePersistentUpstreamKeySlots(t.Context(), req)
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	_, _, err = a.AcquireUpstreamKeySlots(t.Context(), []string{lease.Resources[0].Fingerprint})
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity, "HTTP and native WS share the Live budget")
	_, err = refreshUpstreamKeyScript.Run(t.Context(), rdb, []string{key}, ordinary.member, upstreamKeyLeaseTTL.Milliseconds()).Int()
	require.NoError(t, err)
	require.Equal(t, -time.Nanosecond, rdb.TTL(t.Context(), key).Val(), "ordinary renewal must not install a TTL over Live")
	ordinary.release()
	advance()
	require.NoError(t, b.CheckPersistentUpstreamKeySlots(t.Context(), restored.UpstreamKeyLease))
	require.NoError(t, b.ReleasePersistentUpstreamKeySlots(t.Context(), restored.UpstreamKeyLease))
	require.NoError(t, b.ReleasePersistentUpstreamKeySlots(t.Context(), restored.UpstreamKeyLease))
	require.Zero(t, rdb.ZCard(t.Context(), key).Val())
	require.ErrorIs(t, a.CheckPersistentUpstreamKeySlots(t.Context(), lease), service.ErrUpstreamKeyLease)
	require.Zero(t, rdb.ZCard(t.Context(), key).Val(), "a handoff check must not resurrect released ownership")
}

func TestUpstreamKeyLivePartialFailureAndFinalCredentialBinding(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	first, second := config.UpstreamKeyFingerprint("supplier-fixture"), config.UpstreamKeyFingerprint("second-fixture")
	cfg := upstreamKeyTestConfig(1, 1)
	cfg.Gateway.UpstreamKeyConcurrency.Limits[second] = 1
	s := liveKeyTestUpstream(t, cfg, rdb, nil)
	_, release, err := s.AcquireUpstreamKeySlots(t.Context(), []string{second})
	require.NoError(t, err)
	defer release()
	req := httptest.NewRequest(http.MethodPost, "http://fixture/v1/realtime/calls", nil)
	req.Header.Set("Authorization", "Bearer supplier-fixture")
	req.Header.Set("X-Api-Key", "second-fixture")
	lease, err := s.AcquirePersistentUpstreamKeySlots(t.Context(), req)
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	require.Nil(t, lease)
	require.Zero(t, rdb.ZCard(t.Context(), upstreamKeySlotKeyPrefix+first).Val())
	require.EqualValues(t, 1, rdb.ZCard(t.Context(), upstreamKeySlotKeyPrefix+second).Val(), "partial rollback preserves another request")
	release()
	lease, err = s.AcquirePersistentUpstreamKeySlots(t.Context(), req)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.ReleasePersistentUpstreamKeySlots(context.Background(), lease)) })
	bound := req.Clone(service.WithPersistentUpstreamKeyLease(t.Context(), lease))
	_, _, err = s.BeginUpstreamKeyRequest(bound)
	require.NoError(t, err)
	bound.Header.Set("Authorization", "Bearer changed-fixture")
	_, _, err = s.BeginUpstreamKeyRequest(bound)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	bound = req.Clone(service.WithPersistentUpstreamKeyLease(t.Context(), lease))
	bound.URL.Path = "/v1/responses"
	_, _, err = s.BeginUpstreamKeyRequest(bound)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "a Live member must not exempt a new HTTP generation")
	bound.URL.Path = "/v1/realtime/calls"
	require.NoError(t, rdb.ZRem(t.Context(), upstreamKeySlotKeyPrefix+first, lease.Member).Err())
	_, _, err = s.BeginUpstreamKeyRequest(bound)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "missing ownership is a local pre-send refusal")
}

func TestUpstreamKeyLivePolicyChangeRejectsNewSendButRetainsMedia(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	s := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
	req := httptest.NewRequest(http.MethodPost, "http://fixture/v1/realtime/calls", nil)
	req.Header.Set("Authorization", "Bearer supplier-fixture")
	lease, err := s.AcquirePersistentUpstreamKeySlots(t.Context(), req)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.ReleasePersistentUpstreamKeySlots(context.Background(), lease)) })
	newer := liveKeyTestUpstream(t, upstreamKeyTestConfig(2, 1), rdb, nil)
	_, _, err = newer.AcquireUpstreamKeySlots(t.Context(), lease.Fingerprints)
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	bound := req.Clone(service.WithPersistentUpstreamKeyLease(t.Context(), lease))
	_, _, err = s.BeginUpstreamKeyRequest(bound)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	require.NoError(t, s.CheckPersistentUpstreamKeySlots(t.Context(), lease), "policy changes must not forget already active media")
	require.NoError(t, newer.CheckPersistentUpstreamKeySlots(t.Context(), lease))
}

func TestUpstreamKeyLiveRejectsVolatileOrUnconfirmedRedis(t *testing.T) {
	for field, value := range map[string]string{"appendonly": "no", "appendfsync": "everysec", "no-appendfsync-on-rewrite": "yes", "maxmemory-policy": "allkeys-lru", "deny": "yes"} {
		t.Run(field, func(t *testing.T) {
			_, rdb := liveKeyRedis(t, map[string]string{field: value})
			s := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
			req := httptest.NewRequest(http.MethodPost, "http://fixture/v1/realtime/calls", nil)
			req.Header.Set("Authorization", "Bearer supplier-fixture")
			lease, err := s.AcquirePersistentUpstreamKeySlots(t.Context(), req)
			require.Nil(t, lease)
			require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
			require.Zero(t, rdb.ZCard(t.Context(), upstreamKeySlotKeyPrefix+config.UpstreamKeyFingerprint("supplier-fixture")).Val())
		})
	}
}

func TestRegionalUpstreamKeyLiveDurableHandoff(t *testing.T) {
	for _, mode := range []string{"primary", "region"} {
		t.Run(mode, func(t *testing.T) {
			rdb := regionalKeyRedis(t)
			cfg := upstreamKeyTestConfig(1, 2)
			primary, region, _ := regionalKeyFixture(t, cfg, rdb, 1)
			owner := primary
			if mode == "region" {
				owner = region
			}
			s := liveKeyTestUpstream(t, cfg, rdb, owner)
			req := httptest.NewRequest(http.MethodPost, "http://fixture/v1/realtime/calls", nil)
			req.Header.Set("Authorization", "Bearer supplier-fixture")
			lease, err := s.AcquirePersistentUpstreamKeySlots(t.Context(), req)
			require.NoError(t, err)
			require.NoError(t, lease.Validate())
			restartedCache, err := NewRegionalConcurrencyCache(owner.ConcurrencyCache, owner.primary, owner.local)
			require.NoError(t, err)
			restarted := liveKeyTestUpstream(t, cfg, rdb, restartedCache)
			require.NoError(t, restarted.CheckPersistentUpstreamKeySlots(t.Context(), lease))
			_, _, err = s.AcquireUpstreamKeySlots(t.Context(), lease.Fingerprints)
			require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
			newPolicy := upstreamKeyTestConfig(2, 2)
			require.NoError(t, primary.primary.PublishUpstreamPolicy(t.Context(), newPolicy.Gateway.UpstreamKeyConcurrency))
			snapshot, err := primary.primary.ReadUpstreamPolicy(t.Context())
			require.NoError(t, err)
			require.NoError(t, region.local.InstallUpstreamPolicy(t.Context(), snapshot))
			bound := req.Clone(service.WithPersistentUpstreamKeyLease(t.Context(), lease))
			_, _, err = s.BeginUpstreamKeyRequest(bound)
			require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
			require.NoError(t, restarted.CheckPersistentUpstreamKeySlots(t.Context(), lease), "active media retains its durable member across a policy revision")
			require.NoError(t, restarted.ReleasePersistentUpstreamKeySlots(t.Context(), lease))
			require.NoError(t, restarted.ReleasePersistentUpstreamKeySlots(t.Context(), lease))
			require.ErrorIs(t, s.CheckPersistentUpstreamKeySlots(t.Context(), lease), service.ErrUpstreamKeyLease)
			latest := liveKeyTestUpstream(t, newPolicy, rdb, restartedCache)
			_, release, err := latest.AcquireUpstreamKeySlots(t.Context(), lease.Fingerprints)
			require.NoError(t, err)
			require.NotNil(t, release)
			release()
		})
	}
}
