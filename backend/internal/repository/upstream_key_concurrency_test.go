package repository

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func upstreamKeyTestConfig(revision int64, limit int) *config.Config {
	cfg := &config.Config{}
	cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: revision, Limits: map[string]int{config.UpstreamKeyFingerprint("supplier-fixture"): limit}}
	return cfg
}

func TestUpstreamKeyHTTPAdmissionSharedLifetime(t *testing.T) {
	server := miniredis.RunT(t)
	first := redis.NewClient(&redis.Options{Addr: server.Addr()})
	second := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, first.Close()); require.NoError(t, second.Close()) })
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	a := newHTTPUpstream(upstreamKeyTestConfig(1, 1), nil, first, nil)
	b := newHTTPUpstream(upstreamKeyTestConfig(1, 1), nil, second, nil)
	request := func(credential string) *http.Request {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, upstream.URL+"/v1/responses", strings.NewReader("{}"))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+credential)
		return req
	}
	response, err := a.Do(request("supplier-fixture"), "", 10, 8)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, response.Body.Close()) })
	blocked, err := b.Do(request("supplier-fixture"), "", 20, 100)
	require.Nil(t, blocked)
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	require.EqualValues(t, 1, requests.Load(), "full Key must not send a second upstream request")
	other, err := b.Do(request("different-fixture"), "", 30, 8)
	require.NoError(t, err, "unconfigured credential remains independent")
	require.NoError(t, other.Body.Close())
	require.NoError(t, response.Body.Close())
	next, err := b.Do(request("supplier-fixture"), "", 20, 100)
	require.NoError(t, err)
	require.NoError(t, next.Body.Close())
	require.Zero(t, first.ZCard(t.Context(), "concurrency:upstream_key:slots:"+config.UpstreamKeyFingerprint("supplier-fixture")).Val())
}

func TestUpstreamKeyAtomicCompetitionAndPolicyRevision(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	testUpstreamKeyCompetition(t, client)
}

func TestUpstreamKeySharedAuthorityDeduplicatesCredentials(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			s := &httpUpstreamService{upstreamKeyLimiter: newUpstreamKeyLimiter(upstreamKeyTestConfig(1, limit), client, nil)}
			fingerprint := config.UpstreamKeyFingerprint("supplier-fixture")
			fingerprints := []string{fingerprint, fingerprint, fingerprint}
			ctx, release, err := s.AcquireUpstreamKeySlots(t.Context(), fingerprints)
			require.NoError(t, err, "one active generation must not compete against its own duplicate credential")
			require.NotNil(t, release)
			defer release()
			require.NoError(t, ctx.Err())
			require.Equal(t, []string{fingerprint, fingerprint, fingerprint}, fingerprints, "the caller owns its handshake evidence")
			key := "concurrency:upstream_key:slots:" + fingerprint
			require.EqualValues(t, 1, client.ZCard(t.Context(), key).Val())
			release()
			release()
			require.Zero(t, client.ZCard(t.Context(), key).Val())
		})
	}
}

func TestUpstreamKeySharedAuthorityPartialFailurePreservesPeer(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	first, second := config.UpstreamKeyFingerprint("first-fixture"), config.UpstreamKeyFingerprint("second-fixture")
	cfg := upstreamKeyTestConfig(1, 1)
	cfg.Gateway.UpstreamKeyConcurrency.Limits = map[string]int{first: 1, second: 1}
	s := &httpUpstreamService{upstreamKeyLimiter: newUpstreamKeyLimiter(cfg, client, nil)}
	_, peerRelease, err := s.AcquireUpstreamKeySlots(t.Context(), []string{second})
	require.NoError(t, err)
	require.NotNil(t, peerRelease)
	defer peerRelease()
	key := "concurrency:upstream_key:slots:" + second
	peerMembers, err := client.ZRange(t.Context(), key, 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, peerMembers, 1)
	_, release, err := s.AcquireUpstreamKeySlots(t.Context(), []string{first, first, second, second})
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	require.Nil(t, release)
	require.Zero(t, client.ZCard(t.Context(), "concurrency:upstream_key:slots:"+first).Val(), "a later refusal must roll back this attempt's earlier acquisition")
	members, err := client.ZRange(t.Context(), key, 0, -1).Result()
	require.NoError(t, err)
	require.Equal(t, peerMembers, members, "rollback must preserve the exact peer lease")
	peerRelease()
	_, release, err = s.AcquireUpstreamKeySlots(t.Context(), []string{second, first, second, first})
	require.NoError(t, err, "both credentials must be usable after recovery")
	require.NotNil(t, release)
	release()
	require.Zero(t, client.ZCard(t.Context(), key).Val())
	require.Zero(t, client.ZCard(t.Context(), "concurrency:upstream_key:slots:"+first).Val())
}

func TestUpstreamKeyRealRedisAtomicCompetition(t *testing.T) {
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server is required for this isolated adapter check")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	_, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	cmd := exec.Command(binary, "--bind", "127.0.0.1", "--port", port, "--dir", t.TempDir(), "--save", "", "--appendonly", "yes", "--appendfsync", "always", "--no-appendfsync-on-rewrite", "no", "--maxmemory-policy", "noeviction")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client := redis.NewClient(&redis.Options{Addr: address, MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	require.Eventually(t, func() bool { return client.Ping(t.Context()).Err() == nil }, 3*time.Second, 10*time.Millisecond)
	testUpstreamKeyCompetition(t, client)
	liveClient := redis.NewClient(&redis.Options{Addr: address, DB: 1})
	t.Cleanup(func() { require.NoError(t, liveClient.Close()) })
	testUpstreamKeyLivePersistence(t, liveClient, func() {})
	batchClient := redis.NewClient(&redis.Options{Addr: address, DB: 2})
	t.Cleanup(func() { require.NoError(t, batchClient.Close()) })
	testUpstreamKeyBatchPersistence(t, batchClient, func() {})
}

func testUpstreamKeyCompetition(t *testing.T, client *redis.Client) {
	t.Helper()
	fingerprint := config.UpstreamKeyFingerprint("supplier-fixture")
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	const limit = 4
	limiter := newUpstreamKeyLimiter(upstreamKeyTestConfig(1, limit), client, nil)
	var group sync.WaitGroup
	results := make(chan *upstreamKeyLease, 32)
	errors := make(chan error, 32)
	for range 32 {
		group.Go(func() { lease, err := limiter.acquire(ctx, fingerprint, cancel); results <- lease; errors <- err })
	}
	group.Wait()
	close(results)
	close(errors)
	var leases []*upstreamKeyLease
	for lease := range results {
		if lease != nil {
			leases = append(leases, lease)
		}
	}
	defer func() {
		for _, lease := range leases {
			lease.release()
		}
	}()
	require.Len(t, leases, limit)
	for err := range errors {
		if err != nil {
			require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
		}
	}
	conflict := newUpstreamKeyLimiter(upstreamKeyTestConfig(1, 8), client, nil)
	_, err := conflict.acquire(ctx, fingerprint, cancel)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	shrink := newUpstreamKeyLimiter(upstreamKeyTestConfig(2, 1), client, nil)
	_, err = shrink.acquire(ctx, fingerprint, cancel)
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity, "lowering policy retains existing occupancy")
	_, err = limiter.acquire(ctx, fingerprint, cancel)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "old process must not restore its higher limit")
	for _, lease := range leases {
		lease.release()
		lease.release()
	}
	next, err := shrink.acquire(ctx, fingerprint, cancel)
	require.NoError(t, err)
	require.NotNil(t, next)
	next.release()
}

func TestUpstreamKeyLeaseLossCancelsRequest(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	limiter := newUpstreamKeyLimiter(upstreamKeyTestConfig(1, 1), client, nil)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	lease, err := limiter.acquire(ctx, config.UpstreamKeyFingerprint("supplier-fixture"), cancel)
	require.NoError(t, err)
	defer lease.release()
	require.NoError(t, client.Del(t.Context(), lease.key).Err())
	select {
	case <-ctx.Done():
		require.ErrorIs(t, context.Cause(ctx), service.ErrUpstreamKeyLease)
	case <-time.After(7 * time.Second):
		t.Fatal("lost ownership did not cancel request")
	}
	require.Zero(t, client.ZCard(t.Context(), lease.key).Val(), "renewal must never recreate a deleted lease")
}

func TestUpstreamKeyCredentialsAndFailureBoundaries(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://fixture/v1/responses?key=query-fixture", nil)
	req.Header.Set("Authorization", "Bearer supplier-fixture")
	req.Header.Set("X-Api-Key", "supplier-fixture")
	req.Header.Set("X-Goog-Api-Key", "google-fixture")
	require.ElementsMatch(t, []string{config.UpstreamKeyFingerprint("supplier-fixture"), config.UpstreamKeyFingerprint("query-fixture"), config.UpstreamKeyFingerprint("google-fixture")}, config.UpstreamCredentialFingerprints(req))
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	s := &httpUpstreamService{upstreamKeyLimiter: newUpstreamKeyLimiter(upstreamKeyTestConfig(1, 1), client, nil)}
	_, finish, err := s.BeginUpstreamKeyRequest(req)
	require.NoError(t, err)
	finish(nil, io.ErrUnexpectedEOF)
	require.Zero(t, client.ZCard(t.Context(), "concurrency:upstream_key:slots:"+config.UpstreamKeyFingerprint("supplier-fixture")).Val())
	req.URL.Path = "/v1/realtime/calls"
	_, _, err = s.BeginUpstreamKeyRequest(req)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	req.URL.Path = "/v1/responses"
	s.upstreamKeyLimiter = newUpstreamKeyLimiter(upstreamKeyTestConfig(1, 1), nil, nil)
	_, _, err = s.BeginUpstreamKeyRequest(req)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "missing shared authority must fail closed")
}

func TestUpstreamKeyCancellationReleasesOnlyOwnedLease(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	s := &httpUpstreamService{upstreamKeyLimiter: newUpstreamKeyLimiter(upstreamKeyTestConfig(1, 2), client, nil)}
	fingerprint := config.UpstreamKeyFingerprint("supplier-fixture")
	peerCtx, peerCancel := context.WithCancelCause(t.Context())
	defer peerCancel(nil)
	peer, err := s.upstreamKeyLimiter.acquire(peerCtx, fingerprint, peerCancel)
	require.NoError(t, err)
	defer peer.release()
	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequest(http.MethodGet, "http://fixture/v1/responses", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer supplier-fixture")
	_, finish, err := s.BeginUpstreamKeyRequest(req)
	require.NoError(t, err)
	response := &http.Response{Body: io.NopCloser(strings.NewReader("done"))}
	finish(response, nil)
	cancel()
	require.Eventually(t, func() bool { return client.ZCard(t.Context(), peer.key).Val() == 1 }, time.Second, time.Millisecond)
	require.NoError(t, client.ZScore(t.Context(), peer.key, peer.member).Err())
	require.NoError(t, response.Body.Close())
	require.NoError(t, response.Body.Close())
}
