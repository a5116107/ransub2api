package repository

import (
	"context"
	"io"
	"net"
	"os/exec"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newHTTPResponseOwnerTestCache(t *testing.T) (*gatewayCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	return &gatewayCache{rdb: client}, mr
}

func httpResponseOwnerFixture(userID, apiKeyID int64) service.OpenAIHTTPResponseOwner {
	return service.OpenAIHTTPResponseOwner{UserID: userID, APIKeyID: apiKeyID,
		Scope: service.OpenAIWSContinuationScope{AccountID: 42, RouteFingerprint: [32]byte{1}, Model: "gpt-5.1"}}
}

func TestHTTPResponseOwnerSharedExpiryAndLegacyReaders(t *testing.T) {
	cache, mr := newHTTPResponseOwnerTestCache(t)
	ctx := context.Background()
	writer := service.NewOpenAIWSStateStore(cache)
	reader := service.NewOpenAIWSStateStore(cache)
	require.NoError(t, writer.BindHTTPResponseOwner(ctx, 17, "resp_fixture", httpResponseOwnerFixture(101, 201), time.Second))
	for _, store := range []service.OpenAIWSStateStore{writer, reader} {
		owner, found, err := store.GetHTTPResponseOwner(ctx, 17, "resp_fixture")
		require.NoError(t, err)
		require.True(t, found)
		require.EqualValues(t, 101, owner.UserID)
		require.EqualValues(t, 201, owner.APIKeyID)
	}
	keys := httpResponseOwnerKeys(17, "resp_fixture")
	for _, key := range keys {
		require.NotContains(t, key, "resp_fixture", "Redis keys must only contain a response digest")
		require.Equal(t, time.Second, mr.TTL(key))
	}
	user, err := mr.Get(keys[1])
	require.NoError(t, err)
	require.Equal(t, "101", user, "old readers retain their integer wire format")
	apiKey, err := mr.Get(keys[2])
	require.NoError(t, err)
	require.Equal(t, "201", apiKey)
	_, found, err := reader.GetHTTPResponseOwner(ctx, 18, "resp_fixture")
	require.NoError(t, err)
	require.False(t, found, "groups must not share owner grants")
	mr.FastForward(2 * time.Second)
	for _, store := range []service.OpenAIWSStateStore{writer, reader, service.NewOpenAIWSStateStore(cache)} {
		_, found, err = store.GetHTTPResponseOwner(ctx, 17, "resp_fixture")
		require.NoError(t, err)
		require.False(t, found, "neither a warm node nor a restarted node may extend shared expiry")
	}
}

func TestHTTPResponseOwnerLegacyAndInvalidRecords(t *testing.T) {
	cache, mr := newHTTPResponseOwnerTestCache(t)
	ctx := context.Background()
	keys := httpResponseOwnerKeys(17, "resp_legacy")
	// Simulate the existing old writer; both compatibility keys are required.
	require.NoError(t, mr.Set(keys[1], "101"))
	_, found, err := cache.GetHTTPResponseOwner(ctx, 17, "resp_legacy")
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, mr.Set(keys[2], "201"))
	owner, found, err := cache.GetHTTPResponseOwner(ctx, 17, "resp_legacy")
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 101, owner.UserID)
	require.EqualValues(t, 201, owner.APIKeyID)
	require.False(t, owner.Valid(), "legacy ownership cannot authorize a route")
	require.ErrorIs(t, cache.SetHTTPResponseOwner(ctx, 17, "resp_legacy", httpResponseOwnerFixture(101, 201), time.Minute), errHTTPResponseOwnerUnavailable)
	for _, key := range keys {
		mr.Del(key)
	}
	require.NoError(t, cache.SetHTTPResponseOwner(ctx, 17, "resp_legacy", httpResponseOwnerFixture(101, 201), time.Minute))
	require.NoError(t, mr.Set(keys[1], "999"))
	_, found, err = cache.GetHTTPResponseOwner(ctx, 17, "resp_legacy")
	require.ErrorIs(t, err, errHTTPResponseOwnerUnavailable)
	require.False(t, found, "an old writer must not silently contradict the v1 owner")
	require.NoError(t, mr.Set(keys[1], "101"))
	for _, invalid := range []string{"v2:101:201", "v1:101", "v1:0:201", "v1:101:bad", "v1:101:201:extra", ""} {
		require.NoError(t, mr.Set(keys[0], invalid))
		_, found, err = cache.GetHTTPResponseOwner(ctx, 17, "resp_legacy")
		require.ErrorIs(t, err, errHTTPResponseOwnerUnavailable)
		require.False(t, found, "invalid v1 must not silently fall back to legacy grants")
	}
	mr.Del(keys[0])
	mr.HSet(keys[0], "wrong", "type")
	_, found, err = cache.GetHTTPResponseOwner(ctx, 17, "resp_legacy")
	require.ErrorIs(t, err, errHTTPResponseOwnerUnavailable)
	require.False(t, found)
}

func TestHTTPResponseOwnerFailedPublicationAndSharedDeletion(t *testing.T) {
	cache, mr := newHTTPResponseOwnerTestCache(t)
	ctx := context.Background()
	store := service.NewOpenAIWSStateStore(cache)
	mr.SetError("synthetic-private-topology")
	err := store.BindHTTPResponseOwner(ctx, 17, "resp_failed", httpResponseOwnerFixture(101, 201), time.Minute)
	require.ErrorIs(t, err, errHTTPResponseOwnerUnavailable)
	require.NotContains(t, err.Error(), "synthetic-private")
	mr.SetError("")
	_, found, err := store.GetHTTPResponseOwner(ctx, 17, "resp_failed")
	require.NoError(t, err)
	require.False(t, found, "failed shared publication must not grant from local memory")
	require.NoError(t, store.BindHTTPResponseOwner(ctx, 17, "resp_failed", httpResponseOwnerFixture(101, 201), time.Minute))
	_, found, err = store.GetHTTPResponseOwner(ctx, 17, "resp_failed")
	require.NoError(t, err)
	require.True(t, found)
	mr.SetError("synthetic-private-topology")
	_, found, err = store.GetHTTPResponseOwner(ctx, 17, "resp_failed")
	require.ErrorIs(t, err, errHTTPResponseOwnerUnavailable)
	require.False(t, found, "a warm process must not bypass shared owner availability")
	mr.SetError("")
	for _, key := range httpResponseOwnerKeys(17, "resp_failed") {
		require.True(t, mr.Del(key))
	}
	_, found, err = store.GetHTTPResponseOwner(ctx, 17, "resp_failed")
	require.NoError(t, err)
	require.False(t, found, "deleting all shared formats must revoke warm process grants")
}

func TestHTTPResponseOwnerSharedScopeCannotBeRelabeled(t *testing.T) {
	cache, mr := newHTTPResponseOwnerTestCache(t)
	writer := service.NewOpenAIWSStateStore(cache)
	reader := service.NewOpenAIWSStateStore(cache)
	original := httpResponseOwnerFixture(9007199254740993, 9007199254740995)
	original.Scope.AccountID = 9007199254740997
	require.NoError(t, writer.BindHTTPResponseOwner(t.Context(), 17, "resp_immutable", original, time.Second))
	for _, change := range []string{"user", "account", "route", "model", "tier"} {
		t.Run(change, func(t *testing.T) {
			changed := original
			switch change {
			case "user":
				changed.UserID++
			case "account":
				changed.Scope.AccountID++
			case "route":
				changed.Scope.RouteFingerprint[0]++
			case "model":
				changed.Scope.Model = "gpt-5.4"
			case "tier":
				changed.Scope.ServiceTier = "priority"
			}
			require.Error(t, reader.BindHTTPResponseOwner(t.Context(), 17, "resp_immutable", changed, time.Minute))
			owner, found, err := writer.GetHTTPResponseOwner(t.Context(), 17, "resp_immutable")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, original, owner)
			require.Equal(t, time.Second, mr.TTL(httpResponseOwnerKeys(17, "resp_immutable")[0]), "a rejected scope must not extend expiry")
		})
	}
	otherKey := original
	otherKey.APIKeyID++
	require.NoError(t, reader.BindHTTPResponseOwner(t.Context(), 17, "resp_immutable", otherKey, time.Second))
	owner, found, err := reader.GetHTTPResponseOwner(t.Context(), 17, "resp_immutable")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, original, owner, "the same tenant may use another key without relabeling the first observation")
	mr.FastForward(2 * time.Second)
	otherKey.Scope.Model = "gpt-5.4"
	require.NoError(t, reader.BindHTTPResponseOwner(t.Context(), 17, "resp_immutable", otherKey, time.Minute))
	owner, found, err = writer.GetHTTPResponseOwner(t.Context(), 17, "resp_immutable")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, otherKey, owner)
}

func TestHTTPResponseOwnerRealRedisScopeAndRecovery(t *testing.T) {
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
	cmd := exec.Command(binary, "--bind", "127.0.0.1", "--port", port, "--dir", t.TempDir(), "--save", "", "--appendonly", "no")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client := redis.NewClient(&redis.Options{Addr: address, MaxRetries: -1, DialTimeout: 200 * time.Millisecond, ContextTimeoutEnabled: true})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	require.Eventually(t, func() bool { return client.Ping(t.Context()).Err() == nil }, 3*time.Second, 10*time.Millisecond)
	writer := service.NewOpenAIWSStateStore(NewGatewayCache(client))
	reader := service.NewOpenAIWSStateStore(NewGatewayCache(client))
	original := httpResponseOwnerFixture(101, 201)
	require.NoError(t, writer.BindHTTPResponseOwner(t.Context(), 17, "resp_real", original, time.Minute))
	owner, found, err := reader.GetHTTPResponseOwner(t.Context(), 17, "resp_real")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, original, owner)
	changed := original
	changed.Scope.RouteFingerprint[0]++
	require.Error(t, reader.BindHTTPResponseOwner(t.Context(), 17, "resp_real", changed, time.Minute))
	owner, found, err = writer.GetHTTPResponseOwner(t.Context(), 17, "resp_real")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, original, owner)
	require.NoError(t, client.Do(t.Context(), "CLIENT", "PAUSE", 300).Err())
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, found, err = writer.GetHTTPResponseOwner(ctx, 17, "resp_real")
	require.Error(t, err)
	require.False(t, found, "a warm node must honor the shared I/O failure")
	require.Eventually(t, func() bool { return client.Ping(t.Context()).Err() == nil }, 3*time.Second, 10*time.Millisecond)
	owner, found, err = writer.GetHTTPResponseOwner(t.Context(), 17, "resp_real")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, original, owner)
	require.NoError(t, client.Del(t.Context(), httpResponseOwnerKeys(17, "resp_real")...).Err())
	_, found, err = writer.GetHTTPResponseOwner(t.Context(), 17, "resp_real")
	require.NoError(t, err)
	require.False(t, found)
}
