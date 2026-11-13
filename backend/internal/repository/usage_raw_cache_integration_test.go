//go:build integration

package repository

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUsageRawCacheLogicalAttribution(t *testing.T) {
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	user := mustCreateUser(t, client, &service.User{Email: "raw-cache@example.test"})
	other := mustCreateUser(t, client, &service.User{Email: "raw-cache-other@example.test"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "raw-cache-fixture", Name: "raw"})
	otherKey := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "raw-cache-other-fixture", Name: "other"})
	foreignKey := mustCreateApiKey(t, client, &service.APIKey{UserID: other.ID, Key: "raw-cache-foreign-fixture", Name: "foreign"})
	account := mustCreateAccount(t, client, &service.Account{Name: "raw-cache-account"})
	group := mustCreateGroup(t, client, &service.Group{Name: "raw-cache-product"})
	otherGroup := mustCreateGroup(t, client, &service.Group{Name: "raw-cache-other-product"})
	now := time.Now().UTC().Truncate(time.Second)
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	filters := usagestats.UsageLogFilters{UserID: user.ID, APIKeyID: key.ID, GroupID: group.ID,
		Model: "public-model", ModelFilterSource: usagestats.ModelSourceRequested,
		StartTime: &start, EndTime: &end, IncludeRawCache: true}
	create := func(userID, keyID, groupID int64, model string, at time.Time) *service.UsageLog {
		log := &service.UsageLog{UserID: userID, APIKeyID: keyID, AccountID: account.ID,
			GroupID: &groupID, Model: "private-billing-model", RequestedModel: model, RequestID: uuid.NewString(),
			InputTokens: 1, CacheReadTokens: 99999, CreatedAt: at}
		_, err := repo.Create(t.Context(), log)
		require.NoError(t, err)
		return log
	}
	write := func(log *service.UsageLog, input, read int64) {
		s := requesttiming.Snapshot{Version: 1, TraceID: uuid.NewString(), Status: 200, Outcome: "success", Terminal: "completed",
			FinalResponseAttempt: 2, RawCache: &requesttiming.CacheObservation{Status: "forged"}, Attempts: []requesttiming.Attempt{
				{Number: 1, Kind: "http", Status: 200, Terminal: "completed", CacheUsage: &requesttiming.CacheUsage{Status: "observed", InputTokens: 999, ReadTokens: 999}},
				{Number: 2, Kind: "http", Status: 200, Terminal: "completed", CacheUsage: &requesttiming.CacheUsage{Status: "observed", InputTokens: input, ReadTokens: read}},
			}}
		require.NoError(t, repo.writeTiming(t.Context(), timingWrite{requestID: log.RequestID, apiKeyID: log.APIKeyID, data: s}))
	}
	hit := create(user.ID, key.ID, group.ID, "public-model", now)
	write(hit, 100, 100)
	write(create(user.ID, key.ID, group.ID, "public-model", now), 900, 0)
	create(user.ID, key.ID, group.ID, "public-model", now) // missing diagnostics
	duplicate := create(user.ID, key.ID, group.ID, "public-model", now)
	write(duplicate, 500, 500)
	write(duplicate, 500, 500) // same logical usage, two physical client retries
	for _, log := range []*service.UsageLog{
		create(other.ID, foreignKey.ID, group.ID, "public-model", now),
		create(user.ID, otherKey.ID, group.ID, "public-model", now),
		create(user.ID, key.ID, otherGroup.ID, "public-model", now),
		create(user.ID, key.ID, group.ID, "other-model", now),
		create(user.ID, key.ID, group.ID, "public-model", start.Add(-time.Second)),
		create(user.ID, key.ID, group.ID, "public-model", end),
	} {
		write(log, 100, 100)
	}
	stats, err := repo.GetStatsWithFilters(t.Context(), filters)
	require.NoError(t, err)
	require.EqualValues(t, 4, stats.TotalRequests)
	raw := stats.RawCache
	require.NotNil(t, raw)
	require.EqualValues(t, 4, raw.TotalRequests)
	require.EqualValues(t, 2, raw.ObservedRequests)
	require.EqualValues(t, 2, raw.UnknownRequests)
	require.EqualValues(t, 1, raw.HitRequests)
	require.EqualValues(t, 1000, raw.InputTokens)
	require.EqualValues(t, 100, raw.ReadTokens)
	require.Equal(t, 0.5, *raw.Coverage)
	require.Equal(t, 0.5, *raw.RequestHitRate)
	require.Equal(t, 0.1, *raw.TokenReadRate, "token totals, not mean request percentages or rewritten billing")
	require.EqualValues(t, 399996, stats.TotalCacheReadTokens)
	filters.IncludeRawCache = false
	without, err := repo.GetStatsWithFilters(t.Context(), filters)
	require.NoError(t, err)
	require.Nil(t, without.RawCache)
	require.Equal(t, stats.TotalRequests, without.TotalRequests)
	filters.IncludeRawCache, filters.APIKeyID = true, foreignKey.ID
	empty, err := repo.GetStatsWithFilters(t.Context(), filters)
	require.NoError(t, err)
	require.Zero(t, empty.RawCache.TotalRequests, "the authenticated user's filter must survive a foreign key selection")
	require.Nil(t, empty.RawCache.Coverage)
	require.Nil(t, empty.RawCache.RequestHitRate)
	require.Nil(t, empty.RawCache.TokenReadRate)
}

func TestUsageRawCacheProtocolAndUnknownDiagnostics(t *testing.T) {
	for _, protocol := range []string{"responses", "messages", "gemini", "chat_completions", "ws_turn"} {
		t.Run(protocol, func(t *testing.T) {
			tx := testEntTx(t)
			client := tx.Client()
			repo := newUsageLogRepositoryWithSQL(client, tx)
			user := mustCreateUser(t, client, &service.User{Email: "raw-protocol@example.test"})
			key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "raw-protocol-fixture", Name: "raw"})
			account := mustCreateAccount(t, client, &service.Account{Name: "raw-protocol-account"})
			log := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID,
				RequestID: uuid.NewString(), Model: "model", CreatedAt: time.Now()}
			_, err := repo.Create(t.Context(), log)
			require.NoError(t, err)
			usage := &requesttiming.CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 70}
			a := requesttiming.Attempt{Number: 1, Kind: "http", Status: 200, Terminal: "completed", CacheUsage: usage}
			s := requesttiming.Snapshot{Version: 1, TraceID: uuid.NewString(), Status: 200, Outcome: "success", Terminal: "completed", FinalResponseAttempt: 1}
			if protocol == "ws_turn" {
				a.Kind, a.Status, s.Status, s.TTFTMode = "ws_turn", 0, 0, "ws_turn"
			} else if protocol != "responses" {
				usage.Protocol = protocol
				a.TextStream = &requesttiming.TextStreamTiming{Protocol: protocol}
				if protocol == "messages" {
					usage.WriteReported, usage.InputReported, usage.ReadReported, usage.UncachedTokens = true, true, true, 30
				}
			}
			s.Attempts = []requesttiming.Attempt{a}
			require.NoError(t, repo.writeTiming(t.Context(), timingWrite{requestID: log.RequestID, apiKeyID: key.ID, data: s}))
			get := func() *usagestats.RawCacheStats {
				stats, getErr := repo.GetStatsWithFilters(t.Context(), usagestats.UsageLogFilters{UserID: user.ID, IncludeRawCache: true})
				require.NoError(t, getErr)
				return stats.RawCache
			}
			raw := get()
			require.EqualValues(t, 1, raw.ObservedRequests)
			require.Equal(t, 0.7, *raw.TokenReadRate)
			var detail []byte
			require.NoError(t, scanSingleRow(t.Context(), tx, "SELECT detail FROM request_timing_details WHERE usage_log_id=$1", []any{log.ID}, &detail))
			var original map[string]any
			require.NoError(t, json.Unmarshal(detail, &original))
			cache, ok := original["raw_cache"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "observed", cache["status"])
			for _, change := range []struct{ field, value string }{
				{"status", `"partial"`}, {"source", `"billing"`}, {"scope", `"all_suppliers"`},
				{"input_tokens", `0`}, {"input_tokens", `-1`}, {"input_tokens", `1.5`}, {"input_tokens", `1e100`},
				{"input_tokens", `9007199254740992`}, {"read_tokens", `"70"`}, {"read_tokens", `101`},
				{"read_tokens", `null`}, {"write_tokens", `31`}, {"raw_cache", `null`},
			} {
				t.Run(fmt.Sprintf("%s_%s", change.field, change.value), func(t *testing.T) {
					path := "{raw_cache," + change.field + "}"
					if change.field == "raw_cache" {
						path = "{raw_cache}"
					}
					_, updateErr := tx.ExecContext(t.Context(), `UPDATE request_timing_details SET detail=jsonb_set($1::jsonb,$2::text[],$3::jsonb) WHERE usage_log_id=$4`, string(detail), path, change.value, log.ID)
					require.NoError(t, updateErr)
					unknown := get()
					require.Zero(t, unknown.ObservedRequests)
					require.EqualValues(t, 1, unknown.UnknownRequests)
					require.Equal(t, 0.0, *unknown.Coverage)
					require.Nil(t, unknown.TokenReadRate)
					require.Nil(t, unknown.RequestHitRate)
				})
			}
			_, err = tx.ExecContext(t.Context(), `UPDATE request_timing_details SET detail=$1::jsonb,created_at=NOW()-INTERVAL '31 days' WHERE usage_log_id=$2`, string(detail), log.ID)
			require.NoError(t, err)
			require.Zero(t, get().ObservedRequests, "expired diagnostics stay unknown before asynchronous retention runs")
			_, err = tx.ExecContext(t.Context(), `UPDATE request_timing_details SET created_at=NOW() WHERE usage_log_id=$1`, log.ID)
			require.NoError(t, err)
			require.EqualValues(t, 1, get().ObservedRequests)
			_, err = tx.ExecContext(t.Context(), `UPDATE usage_logs SET created_at=NOW()-INTERVAL '31 days' WHERE id=$1`, log.ID)
			require.NoError(t, err)
			require.Zero(t, get().ObservedRequests, "a newer retry cannot restore unique ownership after old traces expired")
		})
	}
}
