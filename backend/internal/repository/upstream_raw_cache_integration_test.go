//go:build integration

package repository

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type upstreamCacheMetrics struct {
	Requests         int
	ObservedRequests int   `json:"observed_requests"`
	UnknownRequests  int   `json:"unknown_requests"`
	Unattributed     int   `json:"unattributed_requests"`
	InputTokens      int64 `json:"input_tokens"`
	ReadTokens       int64 `json:"read_tokens"`
	Coverage         *float64
	RequestHitRate   *float64   `json:"request_hit_rate"`
	TokenReadRate    *float64   `json:"token_read_rate"`
	LatestSampleAt   *time.Time `json:"latest_sample_at"`
}

type upstreamCacheResult struct {
	Summary upstreamCacheMetrics
	Total   int
	Rows    []struct {
		upstreamCacheMetrics
		Credential string `json:"credential_fingerprint"`
		Route      string `json:"route_fingerprint"`
		Group      string `json:"upstream_group_status"`
		Protocol   string
		Transport  string
		Waits      struct {
			Version        int
			Source         string
			FirstByte      upstreamWaitMetrics `json:"first_byte"`
			FirstEvent     upstreamWaitMetrics `json:"first_event"`
			FirstReasoning upstreamWaitMetrics `json:"first_reasoning"`
			FirstText      upstreamWaitMetrics `json:"first_text"`
		}
	}
}

func readUpstreamCache(t *testing.T, f *channelOperationsFixture, q service.UpstreamOperationsFilter) upstreamCacheResult {
	t.Helper()
	out, err := f.repo.ReadUpstreamOperations(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, "ready", out.RawCache.Status, string(out.RawCache.Data))
	var data upstreamCacheResult
	require.NoError(t, json.Unmarshal(out.RawCache.Data, &data))
	return data
}

func TestUpstreamRawCacheFinalIdentityWeightedPagingAndFilters(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	q := service.UpstreamOperationsFilter{From: now.Add(-time.Hour), Until: now.Add(30 * time.Second), AccountID: f.account,
		GroupID: f.group, Model: "public-model", ModelScope: "public", Dimension: "model", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20}
	repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	wireModel := "wire-model"
	create := func(account, group int64, model string, at time.Time) *service.UsageLog {
		log := &service.UsageLog{UserID: f.user, APIKeyID: f.key, AccountID: account, GroupID: &group,
			Model: "billing-model", RequestedModel: model, UpstreamModel: &wireModel, RequestID: uuid.NewString(),
			InputTokens: 1, CacheReadTokens: 99999, CreatedAt: at}
		_, err := repo.Create(t.Context(), log)
		require.NoError(t, err)
		return log
	}
	write := func(log *service.UsageLog, input, read int64, credential, route string) {
		s := requesttiming.Snapshot{Version: 1, TraceID: uuid.NewString(), Status: 200, Outcome: "success", Terminal: "completed",
			FinalResponseAttempt: 2, Attempts: []requesttiming.Attempt{
				{Number: 1, Kind: "http", AccountID: f.outsider, Status: 500, Terminal: "failed", RouteFingerprint: strings.Repeat("e", 64),
					UpstreamCredential: &requesttiming.UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: strings.Repeat("f", 64)}},
				{Number: 2, Kind: "http", AccountID: log.AccountID, RouteFingerprint: route, Status: 200, Terminal: "completed",
					UpstreamCredential: &requesttiming.UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: credential},
					CacheUsage:         &requesttiming.CacheUsage{Status: "observed", InputTokens: input, ReadTokens: read}},
			}}
		require.NoError(t, repo.writeTiming(t.Context(), timingWrite{requestID: log.RequestID, apiKeyID: f.key, data: s}))
	}
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	write(create(f.account, f.group, "public-model", now), 100, 100, a, b)
	write(create(f.account, f.group, "public-model", now), 900, 0, a, b)
	create(f.account, f.group, "public-model", now)
	duplicate := create(f.account, f.group, "public-model", now)
	write(duplicate, 500, 500, a, b)
	write(duplicate, 500, 500, a, b)
	for _, log := range []*service.UsageLog{
		create(f.outsider, f.group, "public-model", now),
		create(f.account, f.group, "other-model", now),
		create(f.account, f.group, "public-model", q.From.Add(-time.Microsecond)),
		create(f.account, f.group, "public-model", q.Until),
	} {
		write(log, 100, 100, a, b)
	}
	got := readUpstreamCache(t, f, q)
	require.Equal(t, 4, got.Summary.Requests)
	require.Equal(t, 2, got.Summary.ObservedRequests)
	require.Equal(t, 2, got.Summary.UnknownRequests)
	require.Equal(t, 2, got.Summary.Unattributed)
	require.EqualValues(t, 1000, got.Summary.InputTokens)
	require.EqualValues(t, 100, got.Summary.ReadTokens)
	require.Equal(t, .1, *got.Summary.TokenReadRate)
	require.Equal(t, .5, *got.Summary.RequestHitRate)
	require.Equal(t, .5, *got.Summary.Coverage)
	require.NotNil(t, got.Summary.LatestSampleAt)
	require.Equal(t, 2, got.Total)
	// Current configuration cannot replace the immutable historical scope.
	_, err := integrationDB.Exec(`UPDATE accounts SET credentials='{"api_key":"changed","base_url":"https://changed.invalid/v1"}' WHERE id=$1`, f.account)
	require.NoError(t, err)
	require.Equal(t, got, readUpstreamCache(t, f, q))
	q.PageSize = 1
	first := readUpstreamCache(t, f, q)
	q.Page = 2
	second := readUpstreamCache(t, f, q)
	require.Equal(t, first.Summary, second.Summary)
	require.Len(t, first.Rows, 1)
	require.Len(t, second.Rows, 1)
	require.NotEqual(t, first.Rows[0].Credential, second.Rows[0].Credential)
	for _, row := range append(first.Rows, second.Rows...) {
		require.Equal(t, "not_observed", row.Group)
		if row.Credential != "" {
			require.Equal(t, a, row.Credential)
			require.Equal(t, b, row.Route)
		}
	}
	q.Page, q.PageSize = 1, 20
	write(create(f.account, f.group, "public-model", now), 100, 100, strings.Repeat("c", 64), b)
	write(create(f.account, f.group, "public-model", now), 100, 100, a, strings.Repeat("d", 64))
	separate := readUpstreamCache(t, f, q)
	require.Equal(t, 4, separate.Total, "credential rotations and route revisions cannot share a cache bucket")
	require.Equal(t, 4, separate.Summary.ObservedRequests)
	q.Page = 1
	q.ModelScope, q.Model = "upstream", "does-not-exist"
	empty := readUpstreamCache(t, f, q)
	require.Zero(t, empty.Summary.Requests)
	require.Nil(t, empty.Summary.Coverage)
	require.Nil(t, empty.Summary.TokenReadRate)
	require.Nil(t, empty.Summary.RequestHitRate)
}

func TestUpstreamRawCacheProtocolsAndTransport(t *testing.T) {
	for _, protocol := range []string{"responses", "messages", "gemini", "chat_completions", "ws_turn"} {
		t.Run(protocol, func(t *testing.T) {
			f := newChannelOperationsFixture(t)
			now := time.Now().UTC()
			q := service.UpstreamOperationsFilter{From: now.Add(-time.Hour), Until: now.Add(30 * time.Second), AccountID: f.account,
				Dimension: "model", ModelScope: "public", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20}
			repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
			log := &service.UsageLog{UserID: f.user, APIKeyID: f.key, AccountID: f.account, Model: "model", RequestID: uuid.NewString(), CreatedAt: now}
			_, err := repo.Create(t.Context(), log)
			require.NoError(t, err)
			usage := &requesttiming.CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 70}
			a := requesttiming.Attempt{Number: 1, Kind: "http", AccountID: f.account, Status: 200, Terminal: "completed", CacheUsage: usage,
				RouteFingerprint: strings.Repeat("a", 64), UpstreamCredential: &requesttiming.UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: strings.Repeat("b", 64)}}
			s := requesttiming.Snapshot{Version: 1, TraceID: uuid.NewString(), Status: 200, Outcome: "success", Terminal: "completed", FinalResponseAttempt: 1}
			wantProtocol, wantTransport := protocol, "http"
			if protocol == "ws_turn" {
				a.Kind, a.Status, s.Status, s.TTFTMode = "ws_turn", 0, 0, "ws_turn"
				a.UpstreamCredential.Source = "websocket_handshake_credentials"
				wantProtocol, wantTransport = "responses", "ws_turn"
			} else if protocol != "responses" {
				usage.Protocol = protocol
				a.TextStream = &requesttiming.TextStreamTiming{Protocol: protocol}
				if protocol == "messages" {
					usage.WriteReported, usage.InputReported, usage.ReadReported, usage.UncachedTokens = true, true, true, 30
				}
			}
			s.Attempts = []requesttiming.Attempt{a}
			require.NoError(t, repo.writeTiming(t.Context(), timingWrite{requestID: log.RequestID, apiKeyID: f.key, data: s}))
			got := readUpstreamCache(t, f, q)
			require.Equal(t, 1, got.Summary.ObservedRequests)
			require.Equal(t, .7, *got.Summary.TokenReadRate)
			require.Len(t, got.Rows, 1)
			require.Equal(t, wantProtocol, got.Rows[0].Protocol)
			require.Equal(t, wantTransport, got.Rows[0].Transport)
		})
	}
}

func TestUpstreamRawCacheUnknownAndRetentionRecovery(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC()
	q := service.UpstreamOperationsFilter{From: now.Add(-30*24*time.Hour - 12*time.Hour), Until: now.Add(30 * time.Second), AccountID: f.account,
		Dimension: "account", ModelScope: "public", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20}
	var id int64
	require.NoError(t, integrationDB.QueryRow(`INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,created_at) VALUES($1,$2,$3,$4,'model',$5) RETURNING id`, f.user, f.key, f.account, uuid.NewString(), now).Scan(&id))
	base := fmt.Sprintf(`{"version":1,"raw_upstream":{"version":1,"status":"observed","account_id":%d,"transport":"http","credential_source":"http_transport_handoff","credential_fingerprint":"%s","route_fingerprint":"%s"},"raw_cache":{"status":"observed","source":"canonical_responses_terminal_usage","scope":"final_physical_http_responses_terminal_usage","input_tokens":100,"read_tokens":70,"write_tokens":null}}`, f.account, strings.Repeat("a", 64), strings.Repeat("b", 64))
	require.NoError(t, integrationDB.QueryRow(`INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,$3::jsonb) RETURNING usage_log_id`, id, uuid.NewString(), base).Scan(&id))
	require.Equal(t, 1, readUpstreamCache(t, f, q).Summary.ObservedRequests)
	for _, change := range []struct{ path, value string }{
		{"{raw_cache,status}", `"partial"`}, {"{raw_cache,source}", `"billing"`},
		{"{raw_cache,input_tokens}", `1e100`}, {"{raw_cache,read_tokens}", `101`},
		{"{raw_cache,read_tokens}", `null`}, {"{raw_upstream,status}", `"changed_credentials"`},
		{"{raw_upstream,account_id}", `0`}, {"{raw_upstream,credential_fingerprint}", `"secret"`},
		{"{raw_upstream,route_fingerprint}", `null`}, {"{raw_upstream,credential_source}", `"current_config"`},
		{"{raw_upstream}", `null`}, {"{version}", `2`},
	} {
		t.Run(change.path+change.value, func(t *testing.T) {
			_, err := integrationDB.Exec(`UPDATE request_timing_details SET detail=jsonb_set($1::jsonb,$2::text[],$3::jsonb) WHERE usage_log_id=$4`, base, change.path, change.value, id)
			require.NoError(t, err)
			got := readUpstreamCache(t, f, q).Summary
			require.Zero(t, got.ObservedRequests)
			require.Equal(t, 1, got.UnknownRequests)
			require.Equal(t, 0.0, *got.Coverage)
			require.Nil(t, got.TokenReadRate)
			require.Nil(t, got.RequestHitRate)
		})
	}
	_, err := integrationDB.Exec(`UPDATE request_timing_details SET detail=$1::jsonb,created_at=statement_timestamp()-interval '30 days 1 minute' WHERE usage_log_id=$2`, base, id)
	require.NoError(t, err)
	require.Zero(t, readUpstreamCache(t, f, q).Summary.ObservedRequests)
	_, err = integrationDB.Exec(`UPDATE request_timing_details SET created_at=statement_timestamp() WHERE usage_log_id=$1`, id)
	require.NoError(t, err)
	require.Equal(t, 1, readUpstreamCache(t, f, q).Summary.ObservedRequests)
	_, err = integrationDB.Exec(`UPDATE usage_logs SET created_at=statement_timestamp()-interval '30 days 1 minute' WHERE id=$1`, id)
	require.NoError(t, err)
	expired := readUpstreamCache(t, f, q).Summary
	require.Equal(t, 1, expired.Requests)
	require.Equal(t, 1, expired.UnknownRequests, "a newer diagnostic cannot repair expired unique logical attribution")
	require.Zero(t, expired.ObservedRequests)
}

func TestUpstreamRawCacheHundredScopesBoundedPage(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC()
	q := service.UpstreamOperationsFilter{From: now.Add(-time.Hour), Until: now.Add(30 * time.Second), AccountID: f.account,
		Dimension: "account", ModelScope: "public", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20}
	_, err := integrationDB.Exec(`WITH logs AS (
 INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,created_at)
 SELECT $1,$2,$3::bigint,'cache-capacity-'||($3::bigint)::text||'-'||n,'model',$4 FROM generate_series(1,10000) n RETURNING id,account_id
 ) INSERT INTO request_timing_details(usage_log_id,trace_id,detail)
 SELECT id,lpad(to_hex(id),32,'0')::uuid,jsonb_build_object('version',1,
 'raw_upstream',jsonb_build_object('version',1,'status','observed','account_id',account_id,'transport','http',
 'credential_source','http_transport_handoff','credential_fingerprint',repeat(md5((id%100)::text),2),'route_fingerprint',repeat('b',64)),
 'raw_cache',jsonb_build_object('status','observed','source','canonical_responses_terminal_usage',
 'scope','final_physical_http_responses_terminal_usage','input_tokens',100,'read_tokens',70,'write_tokens',NULL)) FROM logs`, f.user, f.key, f.account, now)
	require.NoError(t, err)
	started := time.Now()
	svc := service.NewChannelService(f.repo, nil, nil, nil, nil)
	out, err := svc.UpstreamOperations(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, "ready", out.RawCache.Status)
	var got upstreamCacheResult
	require.NoError(t, json.Unmarshal(out.RawCache.Data, &got))
	require.Equal(t, 10000, got.Summary.Requests)
	require.Equal(t, 10000, got.Summary.ObservedRequests)
	require.Equal(t, 100, got.Total)
	require.Len(t, got.Rows, 20)
	require.Equal(t, .7, *got.Summary.TokenReadRate)
	t.Logf("10000 recorded requests / 100 credential scopes / 20 returned rows: %s (service budget 25s)", time.Since(started))
}

func TestUpstreamRawCacheSectionFailureRecovery(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC()
	q := service.UpstreamOperationsFilter{From: now.Add(-time.Hour), Until: now, AccountID: f.account,
		Dimension: "account", ModelScope: "public", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20}
	// Restrict just this connection with a test-owned role; the original timing
	// table is neither renamed nor removed, and other fixture users are unaffected.
	role := "cache_reader_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err := integrationDB.Exec(`CREATE ROLE ` + role)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DROP OWNED BY ` + role)
		_, _ = integrationDB.Exec(`DROP ROLE ` + role)
	})
	_, err = integrationDB.Exec(`GRANT USAGE ON SCHEMA public TO ` + role + `; GRANT SELECT ON ALL TABLES IN SCHEMA public TO ` + role + `; REVOKE SELECT ON request_timing_details FROM ` + role)
	require.NoError(t, err)
	conn, err := integrationDB.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.ExecContext(t.Context(), `SET ROLE `+role)
	require.NoError(t, err)
	defer func() { _, _ = conn.ExecContext(t.Context(), `RESET ROLE`) }()
	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	args := []any{q.From, q.Until, "[]", "", f.account, int64(0), "", "public", 20, 0}
	failed := readUpstreamSection(t.Context(), tx, upstreamOperationsRawCache, args...)
	require.Equal(t, "query_failed", failed.Status)
	require.JSONEq(t, "null", string(failed.Data))
	require.NotContains(t, failed.Error, role)
	var alive int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT 1`).Scan(&alive), "failed cache query must not abort the remaining snapshot")
	require.Equal(t, 1, alive)
	require.NoError(t, tx.Rollback())
	_, err = integrationDB.Exec(`GRANT SELECT ON request_timing_details TO ` + role)
	require.NoError(t, err)
	tx, err = conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	recovered := readUpstreamSection(t.Context(), tx, upstreamOperationsRawCache, args...)
	require.Equal(t, "ready", recovered.Status)
	require.Empty(t, recovered.Error)
}
