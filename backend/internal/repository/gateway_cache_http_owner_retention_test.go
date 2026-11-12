package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestIssue758RedisHTTPOwnerRetentionSurvivesQuotaWait(t *testing.T) {
	cache, mr := newHTTPResponseOwnerTestCache(t)
	ctx := context.Background()
	const responseID = "resp_fixture_http_758"
	store := service.NewOpenAIWSStateStore(cache)
	retention, ok := store.(service.OpenAIHTTPResponseOwnerRetentionStore)
	require.True(t, ok)
	require.NoError(t, store.BindResponseOwner(ctx, 7, responseID, httpResponseOwnerFixture(101, 201), time.Minute))
	refreshed, err := retention.RefreshHTTPResponseOwner(ctx, 7, responseID, 101, 301, 6*time.Hour)
	require.NoError(t, err)
	require.True(t, refreshed, "same-user keys must remain interoperable without rewriting the original owner")
	keys := responseOwnerKeys(7, responseID)
	record, err := mr.Get(keys[0])
	require.NoError(t, err)
	require.Contains(t, record, `"version":2`)
	for index, value := range []string{record, "101", "201"} {
		actual, err := mr.Get(keys[index])
		require.NoError(t, err)
		require.Equal(t, value, actual)
		require.Equal(t, 6*time.Hour, mr.TTL(keys[index]))
		require.NotContains(t, keys[index], responseID)
	}
	mr.FastForward(5 * time.Hour)
	reader := service.NewOpenAIWSStateStore(cache)
	owner, found, err := reader.GetResponseOwner(ctx, 7, responseID)
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 101, owner.UserID)
	require.EqualValues(t, 201, owner.APIKeyID)
	require.Equal(t, httpResponseOwnerFixture(101, 201).Scope, owner.Scope)
	refreshed, err = retention.RefreshHTTPResponseOwner(ctx, 7, responseID, 101, 301, time.Minute)
	require.NoError(t, err)
	require.True(t, refreshed)
	for _, key := range keys {
		require.Equal(t, time.Hour, mr.TTL(key), "a shorter refresh must not truncate authorization retention")
	}
	refreshed, err = retention.RefreshHTTPResponseOwner(ctx, 7, responseID, 102, 201, 8*time.Hour)
	require.NoError(t, err)
	require.False(t, refreshed, "matching an old key cannot authorize another user")
	refreshed, err = retention.RefreshHTTPResponseOwner(ctx, 8, responseID, 101, 201, 8*time.Hour)
	require.NoError(t, err)
	require.False(t, refreshed)
	require.Len(t, mr.Keys(), 3)
	mr.FastForward(2 * time.Hour)
	refreshed, err = retention.RefreshHTTPResponseOwner(ctx, 7, responseID, 101, 201, 8*time.Hour)
	require.NoError(t, err)
	require.False(t, refreshed)
	require.Empty(t, mr.Keys(), "expired tenant authorization cannot be reconstructed")
}

func TestIssue758RedisHTTPOwnerRetentionRejectsRacesAndIncompleteRecords(t *testing.T) {
	cache, mr := newHTTPResponseOwnerTestCache(t)
	ctx := context.Background()
	const responseID = "resp_fixture_http_758"
	keys := responseOwnerKeys(7, responseID)
	require.NoError(t, cache.SetResponseOwner(ctx, 7, responseID, httpResponseOwnerFixture(102, 202), time.Hour))
	refreshed, err := cache.RefreshHTTPResponseOwner(ctx, 7, responseID, httpResponseOwnerFixture(101, 201), 8*time.Hour)
	require.NoError(t, err)
	require.False(t, refreshed, "an owner changed between read and retention cannot be overwritten")
	for _, key := range keys {
		require.Equal(t, time.Hour, mr.TTL(key))
	}

	for _, missing := range keys {
		for _, key := range keys {
			mr.Del(key)
		}
		require.NoError(t, cache.SetResponseOwner(ctx, 7, responseID, httpResponseOwnerFixture(101, 201), time.Hour))
		mr.Del(missing)
		refreshed, err = cache.RefreshHTTPResponseOwner(ctx, 7, responseID, httpResponseOwnerFixture(101, 201), 8*time.Hour)
		require.NoError(t, err)
		require.False(t, refreshed, "retention requires every existing representation to match")
		require.False(t, mr.Exists(missing), "a missing authorization mirror cannot be recreated")
		for _, key := range keys {
			if key != missing {
				require.Equal(t, time.Hour, mr.TTL(key), "a rejected multi-record CAS must extend nothing")
			}
		}
	}
	for _, key := range keys {
		mr.Del(key)
	}
	require.NoError(t, cache.SetResponseOwner(ctx, 7, responseID, httpResponseOwnerFixture(101, 201), time.Hour))
	require.NoError(t, mr.Set(keys[2], "202"))
	mr.SetTTL(keys[2], time.Hour)
	refreshed, err = cache.RefreshHTTPResponseOwner(ctx, 7, responseID, httpResponseOwnerFixture(101, 201), 8*time.Hour)
	require.NoError(t, err)
	require.False(t, refreshed, "inconsistent versioned and legacy records cannot be retained")
	for _, key := range keys {
		require.Equal(t, time.Hour, mr.TTL(key))
	}
}

func TestIssue758RedisHTTPOwnerRetentionKeepsPersistentRecordsAndStoreErrors(t *testing.T) {
	cache, mr := newHTTPResponseOwnerTestCache(t)
	ctx := context.Background()
	const responseID = "resp_fixture_http_758"
	require.NoError(t, cache.SetResponseOwner(ctx, 7, responseID, httpResponseOwnerFixture(101, 201), time.Hour))
	keys := responseOwnerKeys(7, responseID)
	for _, key := range keys {
		require.NoError(t, cache.rdb.Persist(ctx, key).Err())
	}
	refreshed, err := cache.RefreshHTTPResponseOwner(ctx, 7, responseID, httpResponseOwnerFixture(101, 201), 8*time.Hour)
	require.NoError(t, err)
	require.True(t, refreshed)
	for _, key := range keys {
		require.Zero(t, mr.TTL(key), "persistent authorization records cannot gain an earlier expiry")
	}
	mr.Close()
	refreshed, err = cache.RefreshHTTPResponseOwner(ctx, 7, responseID, httpResponseOwnerFixture(101, 201), time.Hour)
	require.Error(t, err)
	require.False(t, refreshed)
}

func TestIssue758RedisHTTPOwnerRetentionRequiresExactCreationScope(t *testing.T) {
	cache, mr := newHTTPResponseOwnerTestCache(t)
	ctx := t.Context()
	const responseID = "resp_retention_scope"
	original := httpResponseOwnerFixture(101, 201)
	require.NoError(t, cache.SetResponseOwner(ctx, 7, responseID, original, time.Hour))
	for _, field := range []string{"account", "route", "model", "tier"} {
		t.Run(field, func(t *testing.T) {
			stale := original
			switch field {
			case "account":
				stale.Scope.AccountID++
			case "route":
				stale.Scope.RouteFingerprint[0]++
			case "model":
				stale.Scope.Model = "other-model"
			case "tier":
				stale.Scope.ServiceTier = "priority"
			}
			refreshed, err := cache.RefreshHTTPResponseOwner(ctx, 7, responseID, stale, 8*time.Hour)
			require.NoError(t, err)
			require.False(t, refreshed)
			for _, key := range responseOwnerKeys(7, responseID) {
				require.Equal(t, time.Hour, mr.TTL(key), "scope mismatch must extend no authorization record")
			}
		})
	}
	owner, found, err := cache.GetResponseOwner(ctx, 7, responseID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, original, owner)
}
