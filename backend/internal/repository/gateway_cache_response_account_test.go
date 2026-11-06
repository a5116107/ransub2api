package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestResponseAccountSharedExpiryReplacementAndRecovery(t *testing.T) {
	cache, redis := newHTTPResponseOwnerTestCache(t)
	ctx := context.Background()
	writer := service.NewOpenAIWSStateStore(cache)
	other := service.NewOpenAIWSStateStore(cache)
	const groupID = int64(17)
	const responseID = "resp_shared_account"

	require.NoError(t, writer.BindResponseAccount(ctx, groupID, responseID, 101, time.Second))
	require.NoError(t, other.BindResponseAccount(ctx, groupID, responseID, 202, time.Second))
	got, err := writer.GetResponseAccount(ctx, groupID, responseID)
	require.NoError(t, err)
	require.EqualValues(t, 202, got)
	got, err = writer.GetResponseAccount(ctx, groupID+1, responseID)
	require.NoError(t, err)
	require.Zero(t, got)

	redis.SetError("synthetic unavailable")
	got, err = writer.GetResponseAccount(ctx, groupID, responseID)
	require.Error(t, err)
	require.Zero(t, got, "a warm writer must not bypass a shared read failure")
	redis.SetError("")
	got, err = writer.GetResponseAccount(ctx, groupID, responseID)
	require.NoError(t, err)
	require.EqualValues(t, 202, got)

	redis.FastForward(2 * time.Second)
	for _, store := range []service.OpenAIWSStateStore{writer, other, service.NewOpenAIWSStateStore(cache)} {
		got, err = store.GetResponseAccount(ctx, groupID, responseID)
		require.NoError(t, err)
		require.Zero(t, got, "shared expiry must revoke warm as well as cold bindings")
	}
}

func TestResponseAccountFailedPublicationDoesNotCreateLocalBinding(t *testing.T) {
	cache, redis := newHTTPResponseOwnerTestCache(t)
	ctx := context.Background()
	store := service.NewOpenAIWSStateStore(cache)
	redis.SetError("synthetic unavailable")
	require.Error(t, store.BindResponseAccount(ctx, 17, "resp_failed_account", 101, time.Minute))
	redis.SetError("")
	got, err := store.GetResponseAccount(ctx, 17, "resp_failed_account")
	require.NoError(t, err)
	require.Zero(t, got, "a failed shared write must not publish on just one instance")
}
