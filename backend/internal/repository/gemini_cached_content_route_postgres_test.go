//go:build unit

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestGeminiCachedContentRoutePostgres(t *testing.T) {
	dsn := os.Getenv("SUB2API_GEMINI_CACHE_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated SUB2API_GEMINI_CACHE_TEST_DSN")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tx.Rollback()) })
	schema := pq.QuoteIdentifier(fmt.Sprintf("gemini_cache_route_%d", time.Now().UnixNano()))
	_, err = tx.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+schema)
	require.NoError(t, err)
	apply := func(name string) {
		raw, readErr := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		require.NoError(t, readErr)
		_, execErr := tx.ExecContext(ctx, string(raw))
		require.NoError(t, execErr)
	}
	for _, name := range []string{"243_gemini_cached_contents.sql", "244_gemini_cached_contents_channel_usage.sql", "245_gemini_cached_contents_mutation_lease.sql"} {
		apply(name)
	}
	// Simulate an existing row before the additive migration. Its provenance is unknown.
	_, err = tx.ExecContext(ctx, `INSERT INTO gemini_cached_contents
 (public_id,user_id,api_key_id,group_id,account_id,upstream_name,model,upstream_model,expire_time)
 VALUES ('legacy',1,7,42,99,'cachedContents/old','gemini-pro','models/gemini-pro',NOW()+INTERVAL '1 hour')`)
	require.NoError(t, err)
	apply("328_gemini_cache_route_fingerprint.sql")
	apply("328_gemini_cache_route_fingerprint.sql")
	repo := &geminiCachedContentRepository{sql: tx}
	legacy, err := repo.GetForOwner(ctx, 7, "legacy")
	require.NoError(t, err)
	require.Empty(t, legacy.RouteFingerprint)
	account := &service.Account{ID: 99, Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "fixture-key"}}
	require.False(t, legacy.MatchesAccountRoute(account))
	record := &service.GeminiCachedContent{PublicID: "new", UserID: 1, APIKeyID: 7, GroupID: 42,
		AccountID: account.ID, UpstreamName: "cachedContents/new", Model: "gemini-pro", UpstreamModel: "models/gemini-pro",
		ExpireTime:       time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond),
		RouteFingerprint: fmt.Sprintf("%x", service.AccountRouteFingerprint(account))}
	require.NoError(t, repo.Create(ctx, record))
	got, err := repo.GetForOwner(ctx, 7, record.PublicID)
	require.NoError(t, err)
	require.Equal(t, record.RouteFingerprint, got.RouteFingerprint)
	require.True(t, got.MatchesAccountRoute(account))
	rows, err := repo.ListForOwner(ctx, 7, 42, time.Now(), 0, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, record.RouteFingerprint, rows[0].RouteFingerprint)
	acquired, err := repo.BeginMutation(ctx, record.ID, record.ExpireTime, "first", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)
	_, err = tx.ExecContext(ctx, `UPDATE gemini_cached_contents SET mutation_lease_until=NOW()-INTERVAL '1 second' WHERE id=$1`, record.ID)
	require.NoError(t, err)
	taken, err := repo.TakeOverStaleMutation(ctx, record.ID, "recovery", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, taken)
	require.Equal(t, record.RouteFingerprint, taken.RouteFingerprint)
	require.NoError(t, repo.CommitMutation(ctx, record.ID, "recovery", record.ExpireTime.Add(time.Minute)))
	// Old application inserts still work after migration and remain untrusted to the new binary.
	_, err = tx.ExecContext(ctx, `INSERT INTO gemini_cached_contents
 (public_id,user_id,api_key_id,group_id,account_id,upstream_name,model,upstream_model,expire_time)
 VALUES ('rollback',1,7,42,99,'cachedContents/rollback','gemini-pro','models/gemini-pro',NOW()+INTERVAL '1 hour')`)
	require.NoError(t, err)
	rolledBack, err := repo.GetForOwner(ctx, 7, "rollback")
	require.NoError(t, err)
	require.Empty(t, rolledBack.RouteFingerprint)
	require.NotContains(t, strings.ToLower(record.RouteFingerprint), "fixture-key")
}
