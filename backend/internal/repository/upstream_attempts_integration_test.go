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

func TestUpstreamAttemptsRetainRecoveredAndUnbilledFailures(t *testing.T) {
	f := newChannelOperationsFixture(t)
	repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	now := time.Now().UTC().Truncate(time.Second)
	end := 100.0
	physical := func(number int, account int64, status int, terminal string) requesttiming.Attempt {
		return requesttiming.Attempt{Number: number, Kind: "http", AccountID: account, Status: status,
			Terminal: terminal, EndMS: &end, RouteFingerprint: strings.Repeat("b", 64),
			UpstreamCredential: &requesttiming.UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: strings.Repeat("a", 64)}}
	}
	write := func(s requesttiming.Snapshot) {
		s.Version, s.TraceID, s.StartedAt, s.TotalMS = 1, uuid.NewString(), now, 100
		t.Cleanup(func() {
			_, _ = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE trace_id=$1`, s.TraceID)
		})
		require.NoError(t, repo.writeConnectionTrace(t.Context(), service.ConnectionTrace{TraceID: s.TraceID, StartedAt: now,
			RequestID: uuid.NewString(), UserID: f.user, APIKeyID: f.key, AccountID: f.outsider, GroupID: f.group,
			Source: "application", Model: "public-model", Path: "/v1/responses", Stream: true, Detail: s}))
	}
	write(requesttiming.Snapshot{Status: 200, Outcome: "success", Terminal: "completed", FinalResponseAttempt: 2,
		Attempts: []requesttiming.Attempt{physical(1, f.account, 502, "failed"), physical(2, f.outsider, 200, "completed")}})
	write(requesttiming.Snapshot{Status: 502, Outcome: "failed", Attempts: []requesttiming.Attempt{physical(1, f.account, 503, "")}})
	q := service.UpstreamOperationsFilter{From: now.Add(-time.Minute), Until: now.Add(time.Minute), GroupID: f.group,
		AccountID: f.account, Model: "public-model", ModelScope: "public", Dimension: "account", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20}
	out, err := f.repo.ReadUpstreamOperations(t.Context(), q)
	require.NoError(t, err)
	var body struct {
		Attempts struct {
			Status string
			Data   struct {
				Summary struct {
					Attempts        int
					Failed          int
					Succeeded       int
					LogicalRequests int `json:"logical_requests"`
				}
			}
		}
	}
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Equal(t, "ready", body.Attempts.Status, string(raw))
	require.Equal(t, 2, body.Attempts.Data.Summary.Attempts)
	require.Equal(t, 2, body.Attempts.Data.Summary.Failed, "a later success must not erase the first upstream's failure")
	require.Zero(t, body.Attempts.Data.Summary.Succeeded)
	require.Equal(t, 2, body.Attempts.Data.Summary.LogicalRequests)
	var usage int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_logs WHERE api_key_id=$1`, f.key).Scan(&usage))
	require.Zero(t, usage, "unbilled failures must remain observable without creating billing rows")
	q.AccountID = 0
	out, err = f.repo.ReadUpstreamOperations(t.Context(), q)
	require.NoError(t, err)
	raw, err = json.Marshal(out)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Equal(t, 3, body.Attempts.Data.Summary.Attempts)
	require.Equal(t, 2, body.Attempts.Data.Summary.Failed)
	require.Equal(t, 1, body.Attempts.Data.Summary.Succeeded)
	require.Equal(t, 2, body.Attempts.Data.Summary.LogicalRequests)
}

func TestUpstreamAttemptsDeduplicateFallbackAndKeepUnknownCapture(t *testing.T) {
	f := newChannelOperationsFixture(t)
	repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	now := time.Now().UTC().Truncate(time.Second)
	end := 100.0
	s := requesttiming.Snapshot{Version: 1, TraceID: uuid.NewString(), StartedAt: now, TotalMS: 100, Status: 200,
		Attempts: []requesttiming.Attempt{{Kind: "http", Number: 1, AccountID: f.account, EndMS: &end, Status: 200, Terminal: "completed",
			RouteFingerprint: strings.Repeat("b", 64), UpstreamCredential: &requesttiming.UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: strings.Repeat("a", 64)}}}}
	row := service.ConnectionTrace{Source: "application", TraceID: s.TraceID, StartedAt: now, RequestID: uuid.NewString(),
		APIKeyID: f.key, UserID: f.user, AccountID: f.account, GroupID: f.group, Model: "public-model", Path: "/v1/responses", Detail: s}
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM request_timing_details WHERE trace_id=$1`, s.TraceID)
		_, _ = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE api_key_id=$1`, f.key)
	})
	require.NoError(t, repo.writeConnectionTrace(t.Context(), row))
	require.NoError(t, repo.writeConnectionTrace(t.Context(), row))
	log := &service.UsageLog{UserID: f.user, APIKeyID: f.key, AccountID: f.account, GroupID: &f.group, Model: "billing-model", RequestedModel: "public-model", RequestID: row.RequestID, CreatedAt: now}
	_, err := repo.Create(t.Context(), log)
	require.NoError(t, err)
	require.NoError(t, repo.writeTiming(t.Context(), timingWrite{requestID: row.RequestID, apiKeyID: f.key, data: s}))
	// The fallback window uses persistence time. Align this fixture's two
	// timestamps so the deduplication oracle compares the same sample time.
	_, err = integrationDB.Exec(`UPDATE request_timing_details SET created_at=$2 WHERE trace_id=$1`, s.TraceID, now)
	require.NoError(t, err)
	q := service.UpstreamOperationsFilter{From: now.Add(-time.Minute), Until: now.Add(time.Minute), AccountID: f.account, GroupID: f.group, Model: "public-model", ModelScope: "public", Dimension: "account", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20}
	read := func() map[string]any {
		t.Helper()
		out, err := f.repo.ReadUpstreamOperations(t.Context(), q)
		require.NoError(t, err)
		require.Equal(t, "ready", out.Attempts.Status)
		var data map[string]any
		require.NoError(t, json.Unmarshal(out.Attempts.Data, &data))
		return data
	}
	first := read()
	summary, ok := first["summary"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 1.0, summary["attempts"])
	_, err = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE trace_id=$1`, s.TraceID)
	require.NoError(t, err)
	fallback := read()
	require.Equal(t, first["summary"], fallback["summary"])
	var originalDetail string
	require.NoError(t, integrationDB.QueryRow(`SELECT detail::text FROM request_timing_details WHERE trace_id=$1`, s.TraceID).Scan(&originalDetail))
	// Missing versions/capture never turn into zero failures with full coverage.
	for _, patch := range []string{`{"physical_upstream":null}`, `{"physical_upstream":{"version":9,"complete":true,"attempts":[]}}`,
		`{"physical_upstream":{"version":1,"complete":true,"attempts":{}}}`, `{"physical_upstream":{"version":1,"complete":false,"attempts":[]}}`} {
		_, err = integrationDB.Exec(`UPDATE request_timing_details SET detail=detail || $2::jsonb WHERE trace_id=$1`, s.TraceID, patch)
		require.NoError(t, err)
		unknown := read()
		capture, ok := unknown["capture"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, 1.0, capture["unknown_traces"])
		require.Equal(t, 0.0, capture["complete_traces"])
		metrics, ok := unknown["summary"].(map[string]any)
		require.True(t, ok)
		require.Nil(t, metrics["confirmed_success_rate"])
	}
	// The physical model is not the final billing alias.
	_, err = integrationDB.Exec(`UPDATE request_timing_details SET detail=$2::jsonb WHERE trace_id=$1`, s.TraceID, originalDetail)
	require.NoError(t, err)
	q.ModelScope = "upstream"
	out, err := f.repo.ReadUpstreamOperations(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, "ready", out.Attempts.Status)
	var filtered map[string]any
	require.NoError(t, json.Unmarshal(out.Attempts.Data, &filtered))
	require.Equal(t, 0.0, filtered["summary"].(map[string]any)["attempts"], "unobserved historical aliases cannot match a physical model")
	require.Equal(t, 1.0, filtered["request_scope"].(map[string]any)["unknown_attempts"])
	q.ModelScope = "recorded"
	out, err = f.repo.ReadUpstreamOperations(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, "unsupported_scope", out.Attempts.Status)
	q.ModelScope = "public"
	q.Model = "does-not-exist"
	empty := read()
	metrics, ok := empty["summary"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 0.0, metrics["attempts"])
}

func TestUpstreamAttemptsRequestMetadataBucketsAndModelFilter(t *testing.T) {
	f := newChannelOperationsFixture(t)
	repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	now := time.Now().UTC().Truncate(time.Second)
	end := 100.0
	observation := func(model, tier, effort string) *requesttiming.UpstreamRequestObservation {
		return &requesttiming.UpstreamRequestObservation{Version: 1, Status: "observed", Source: "http_transport_body", Protocol: "responses", Model: model, ServiceTier: tier, ReasoningEffort: effort, Stream: true}
	}
	future, wrongSource, invalid := observation("wire-a", "priority", "high"), observation("wire-a", "priority", "high"), observation("wire-a", "future-tier", "high")
	future.Version, wrongSource.Source = 9, "websocket_generation_payload"
	for i, request := range []*requesttiming.UpstreamRequestObservation{
		observation("wire-a", "priority", "high"), observation("wire-b", "default", "low"), observation("wire-a", "default", "high"), nil, future, wrongSource, invalid,
	} {
		s := requesttiming.Snapshot{Version: 1, TraceID: uuid.NewString(), StartedAt: now, TotalMS: 100,
			Attempts: []requesttiming.Attempt{{Number: 1, Kind: "http", AccountID: f.account, EndMS: &end, Status: 502,
				UpstreamRequest: request, RouteFingerprint: strings.Repeat("b", 64),
				UpstreamCredential: &requesttiming.UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: strings.Repeat("a", 64)}}}}
		if i == 1 || i == 2 {
			s.Attempts[0].Status = 200
			s.Attempts[0].Terminal = "completed"
		}
		t.Cleanup(func() {
			_, _ = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE trace_id=$1`, s.TraceID)
		})
		require.NoError(t, repo.writeConnectionTrace(t.Context(), service.ConnectionTrace{Source: "application", TraceID: s.TraceID, StartedAt: now, RequestID: uuid.NewString(), APIKeyID: f.key, UserID: f.user, AccountID: f.account, GroupID: f.group, Model: "public-alias", Path: "/v1/responses", Detail: s}))
	}
	q := service.UpstreamOperationsFilter{From: now.Add(-time.Minute), Until: now.Add(time.Minute), GroupID: f.group, AccountID: f.account, ModelScope: "upstream", Dimension: "account", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20}
	read := func() map[string]any {
		t.Helper()
		out, err := f.repo.ReadUpstreamOperations(t.Context(), q)
		require.NoError(t, err)
		require.Equal(t, "ready", out.Attempts.Status, out.Attempts.Error)
		var data map[string]any
		require.NoError(t, json.Unmarshal(out.Attempts.Data, &data))
		return data
	}
	all := read()
	require.Equal(t, 7.0, all["summary"].(map[string]any)["attempts"])
	require.Equal(t, 3.0, all["request_scope"].(map[string]any)["observed_attempts"])
	require.Equal(t, 4.0, all["request_scope"].(map[string]any)["unknown_attempts"])
	require.Equal(t, 4.0, all["total"], "model/tier/effort and unknown metadata must not merge")
	q.Model = "wire-a"
	filtered := read()
	summary := filtered["summary"].(map[string]any)
	require.Equal(t, 2.0, summary["attempts"])
	require.Equal(t, 1.0, summary["failed"])
	require.Equal(t, 1.0, summary["succeeded"])
	require.Equal(t, 0.5, summary["confirmed_success_rate"])
	require.Equal(t, all["request_scope"], filtered["request_scope"], "unknown model attempts remain visible in the cohort coverage rather than being assigned to the selected model")
	for _, value := range filtered["rows"].([]any) {
		row := value.(map[string]any)
		require.Equal(t, "wire-a", row["upstream_model"])
		require.Equal(t, "observed", row["upstream_model_status"])
		require.Equal(t, "responses", row["upstream_protocol"])
		require.Equal(t, true, row["upstream_stream"])
		require.Equal(t, "not_observed", row["upstream_group_status"])
	}
	q.ModelScope = "public"
	q.Model = "public-alias"
	require.Equal(t, all["summary"], read()["summary"])
	q.Model = "wire-a"
	require.Equal(t, 0.0, read()["summary"].(map[string]any)["attempts"], "wire and public model scopes are distinct")
}

type upstreamAttemptWaitReport struct {
	Total        int
	RequestScope struct {
		Observed int `json:"observed_attempts"`
		Unknown  int `json:"unknown_attempts"`
	} `json:"request_scope"`
	Summary  struct{ Attempts, Failed, Succeeded, Interrupted, Unknown int }
	RawCache upstreamAttemptCacheMetrics `json:"raw_cache"`
	Rows     []struct {
		AccountID            int64  `json:"account_id"`
		UpstreamModel        string `json:"upstream_model"`
		Speed                string `json:"speed"`
		ThinkingMode         string `json:"thinking_mode"`
		ThinkingBudgetTokens *int64 `json:"thinking_budget_tokens"`
		Transport            string
		Attempts             int
		RawCache             upstreamAttemptCacheMetrics  `json:"raw_cache"`
		Output               upstreamAttemptOutputMetrics `json:"output"`
		Waits                struct {
			Version        int
			Source         string
			FirstByte      upstreamWaitMetrics `json:"first_byte"`
			FirstEvent     upstreamWaitMetrics `json:"first_event"`
			FirstReasoning upstreamWaitMetrics `json:"first_reasoning"`
			FirstText      upstreamWaitMetrics `json:"first_text"`
		}
	}
}

type upstreamAttemptCacheMetrics struct {
	Version  int
	Source   string
	Attempts int
	Observed int   `json:"observed_attempts"`
	Unknown  int   `json:"unknown_attempts"`
	Hits     int   `json:"hit_attempts"`
	Input    int64 `json:"input_tokens"`
	Read     int64 `json:"read_tokens"`
	Coverage *float64
	HitRate  *float64   `json:"attempt_hit_rate"`
	ReadRate *float64   `json:"token_read_rate"`
	Latest   *time.Time `json:"latest_sample_at"`
}

func TestUpstreamAttemptsCacheIncludesUnbilledAndRecoveredScopes(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	first, second := upstreamAttemptWaitFixture(f.account), upstreamAttemptWaitFixture(f.outsider)
	first.CacheUsage = &requesttiming.CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 99}
	second.Number, second.Status, second.Terminal = 2, 200, "completed"
	second.UpstreamRequest.Model = "wire-b"
	second.CacheUsage = &requesttiming.CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 100}
	writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 100, Status: 502, DownstreamError: true, Attempts: []requesttiming.Attempt{first, second}})
	second.Number = 1
	second.CacheUsage = &requesttiming.CacheUsage{Status: "observed", InputTokens: 900, ReadTokens: 0}
	writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 100, Status: 200, Attempts: []requesttiming.Attempt{second}})
	for _, failure := range []string{"timeout", "canceled", ""} {
		a := upstreamAttemptWaitFixture(f.outsider)
		a.Status, a.Error, a.UpstreamRequest.Model = 0, failure, "wire-b"
		a.CacheUsage = &requesttiming.CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 100}
		writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 100, Status: 502, Attempts: []requesttiming.Attempt{a}})
	}
	ws := upstreamAttemptWaitFixture(f.account)
	ws.Kind, ws.Status, ws.Terminal = "ws_turn", 0, "completed"
	ws.UpstreamRequest.Model, ws.UpstreamRequest.Source, ws.UpstreamCredential.Source = "wire-ws", "websocket_generation_payload", "websocket_handshake_credentials"
	ws.CacheUsage = &requesttiming.CacheUsage{Status: "observed", InputTokens: 200, ReadTokens: 80}
	writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 100, TTFTMode: "ws_turn", Attempts: []requesttiming.Attempt{ws}})
	data := readUpstreamAttemptWaits(t, f, now)
	require.Equal(t, 7, data.Summary.Attempts)
	require.Equal(t, 1, data.RawCache.Version)
	require.Equal(t, "canonical_physical_attempt_cache_reports", data.RawCache.Source)
	require.Equal(t, 7, data.RawCache.Attempts)
	require.Equal(t, 3, data.RawCache.Observed)
	require.Equal(t, 4, data.RawCache.Unknown)
	require.Equal(t, 2, data.RawCache.Hits)
	require.EqualValues(t, 1200, data.RawCache.Input)
	require.EqualValues(t, 180, data.RawCache.Read)
	require.InDelta(t, 0.15, *data.RawCache.ReadRate, 0.000001)
	require.InDelta(t, 2.0/3, *data.RawCache.HitRate, 0.000001)
	require.InDelta(t, 3.0/7, *data.RawCache.Coverage, 0.000001)
	for _, row := range data.Rows {
		require.Equal(t, row.Attempts, row.RawCache.Attempts)
		switch row.UpstreamModel {
		case "wire-a":
			require.Zero(t, row.RawCache.Observed)
			require.Nil(t, row.RawCache.HitRate)
		case "wire-b":
			require.Equal(t, f.outsider, row.AccountID)
			require.Equal(t, 2, row.RawCache.Observed)
			require.Equal(t, 3, row.RawCache.Unknown)
			require.InDelta(t, 0.1, *row.RawCache.ReadRate, 0.000001)
			require.InDelta(t, 0.5, *row.RawCache.HitRate, 0.000001)
		case "wire-ws":
			require.Equal(t, "ws_turn", row.Transport)
			require.Equal(t, 1, row.RawCache.Observed)
		default:
			t.Fatalf("unexpected scope: %+v", row)
		}
	}
	var billed int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_logs WHERE api_key_id=$1`, f.key).Scan(&billed))
	require.Zero(t, billed, "observation must not manufacture downstream usage or billing")
}

func TestUpstreamAttemptsCacheRejectsHistoricalInvalidAndCrossScopeReports(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name, mutation string
		observed       int
	}{
		{"valid", `detail`, 1},
		{"missing_report", `detail #- '{physical_upstream,attempts,0,raw_cache}'`, 0},
		{"final_report_is_not_fallback", `jsonb_set(detail #- '{physical_upstream,attempts,0,raw_cache}','{raw_cache}',detail->'physical_upstream'->'attempts'->0->'raw_cache')`, 0},
		{"future_report", `jsonb_set(detail,'{physical_upstream,attempts,0,raw_cache,version}','9')`, 0},
		{"string_version", `jsonb_set(detail,'{physical_upstream,attempts,0,raw_cache,version}','"1"')`, 0},
		{"final_scope", `jsonb_set(detail,'{physical_upstream,attempts,0,raw_cache,scope}','"final_physical_http_responses_terminal_usage"')`, 0},
		{"foreign_source", `jsonb_set(detail,'{physical_upstream,attempts,0,raw_cache,source}','"canonical_gemini_usage_metadata"')`, 0},
		{"unknown_status", `jsonb_set(detail,'{physical_upstream,attempts,0,raw_cache,status}','"partial"')`, 0},
		{"wrong_route", `jsonb_set(detail,'{attempts,0,route_fingerprint}',to_jsonb(repeat('c',64)))`, 0},
		{"wrong_key", `jsonb_set(detail,'{attempts,0,upstream_credential,fingerprint}',to_jsonb(repeat('c',64)))`, 0},
		{"wrong_model", `jsonb_set(detail,'{attempts,0,upstream_request,model}','"wire-b"')`, 0},
		{"missing_scope", `detail #- '{physical_upstream,attempts,0,upstream_request}'`, 0},
		{"missing_identity", `jsonb_set(detail,'{physical_upstream,attempts,0,identity_known}','false')`, 0},
		{"projection_protocol", `jsonb_set(detail,'{physical_upstream,attempts,0,protocol}','"messages"')`, 0},
		{"string_account", `jsonb_set(detail,'{physical_upstream,attempts,0,account_id}',to_jsonb(detail->'physical_upstream'->'attempts'->0->>'account_id'))`, 0},
		{"physical_duplicate", `jsonb_set(detail,'{attempts}',(detail->'attempts')||(detail->'attempts'))`, 0},
		{"physical_object", `jsonb_set(detail,'{attempts}','{}')`, 0},
		{"truncated", `jsonb_set(detail,'{truncated}','true')`, 0},
		{"wrong_protocol", `jsonb_set(detail,'{attempts,0,route,endpoint}','"messages"')`, 0},
		{"raw_protocol", `jsonb_set(detail,'{attempts,0,cache_usage,protocol}','"chat_completions"')`, 0},
		{"failed_attempt", `jsonb_set(detail,'{attempts,0,terminal}','"failed"')`, 0},
		{"failed_outcome", `jsonb_set(detail,'{physical_upstream,attempts,0,outcome}','"failed"')`, 0},
		{"transport_error", `jsonb_set(detail,'{attempts,0,error}','"timeout"')`, 0},
		{"parser_gap", `jsonb_set(detail,'{attempts,0,events,typed_content_truncated}','50')`, 0},
		{"no_end", `detail #- '{attempts,0,end_ms}'`, 0},
		{"end_after_capture", `jsonb_set(detail,'{attempts,0,end_ms}','101')`, 0},
		{"string_counter", `jsonb_set(detail,'{physical_upstream,attempts,0,raw_cache,read_tokens}','"90"')`, 0},
		{"negative_counter", `jsonb_set(detail,'{physical_upstream,attempts,0,raw_cache,read_tokens}','-1')`, 0},
		{"unsafe_counter", `jsonb_set(detail,'{physical_upstream,attempts,0,raw_cache,input_tokens}','9007199254740992')`, 0},
		{"fractional_counter", `jsonb_set(detail,'{physical_upstream,attempts,0,raw_cache,read_tokens}','0.5')`, 0},
		{"unmatched_raw_counter", `jsonb_set(detail,'{attempts,0,cache_usage,read_tokens}','89')`, 0},
		{"missing_write_presence", `detail #- '{attempts,0,cache_usage,write_reported}'`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := upstreamAttemptWaitFixture(f.account)
			a.Status, a.Terminal = 200, "completed"
			a.CacheUsage = &requesttiming.CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 90}
			trace := writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 100, Status: 200, Attempts: []requesttiming.Attempt{a}})
			_, err := integrationDB.Exec(`UPDATE request_connection_traces SET detail=`+tc.mutation+` WHERE trace_id=$1`, trace)
			require.NoError(t, err)
			var before, after string
			require.NoError(t, integrationDB.QueryRow(`SELECT detail::text FROM request_connection_traces WHERE trace_id=$1`, trace).Scan(&before))
			data := readUpstreamAttemptWaits(t, f, now)
			require.Equal(t, 1, data.Summary.Attempts, "invalid cache evidence must not erase retained attempts")
			require.Equal(t, tc.observed, data.RawCache.Observed)
			require.Equal(t, 1-tc.observed, data.RawCache.Unknown)
			if tc.observed == 0 {
				require.Nil(t, data.RawCache.ReadRate)
				require.Nil(t, data.RawCache.HitRate)
				require.Nil(t, data.RawCache.Latest)
			}
			require.NoError(t, integrationDB.QueryRow(`SELECT detail::text FROM request_connection_traces WHERE trace_id=$1`, trace).Scan(&after))
			require.Equal(t, before, after, "historical JSON must not be backfilled")
			_, err = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE trace_id=$1`, trace)
			require.NoError(t, err)
		})
	}
}

func TestUpstreamAttemptsCacheUniqueFallbackIsReadOnlyAndNotDoubleCounted(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	a := upstreamAttemptWaitFixture(f.account)
	a.Status, a.Terminal = 200, "completed"
	a.CacheUsage = &requesttiming.CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 90}
	trace := writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 100, Status: 200, Attempts: []requesttiming.Attempt{a}})
	var requestID string
	var stored []byte
	require.NoError(t, integrationDB.QueryRow(`SELECT request_id,detail FROM request_connection_traces WHERE trace_id=$1`, trace).Scan(&requestID, &stored))
	var snapshot requesttiming.Snapshot
	require.NoError(t, json.Unmarshal(stored, &snapshot))
	repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	_, err := repo.Create(t.Context(), &service.UsageLog{UserID: f.user, APIKeyID: f.key, AccountID: f.account, GroupID: &f.group, Model: "bill-only", RequestedModel: "public-waits", RequestID: requestID, CreatedAt: now, CacheReadTokens: 99999})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = integrationDB.Exec(`DELETE FROM request_timing_details WHERE trace_id=$1`, trace) })
	require.NoError(t, repo.writeTiming(t.Context(), timingWrite{requestID: requestID, apiKeyID: f.key, data: snapshot}))
	_, err = integrationDB.Exec(`UPDATE request_timing_details SET created_at=$2 WHERE trace_id=$1`, trace, now)
	require.NoError(t, err)
	first := readUpstreamAttemptWaits(t, f, now)
	require.Equal(t, 1, first.RawCache.Observed)
	require.EqualValues(t, 90, first.RawCache.Read, "rewritten billing cache must not replace wire counters")
	_, err = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE trace_id=$1`, trace)
	require.NoError(t, err)
	var before, after string
	require.NoError(t, integrationDB.QueryRow(`SELECT detail::text FROM request_timing_details WHERE trace_id=$1`, trace).Scan(&before))
	fallback := readUpstreamAttemptWaits(t, f, now)
	require.Equal(t, first.RawCache, fallback.RawCache)
	require.Equal(t, 1, fallback.Summary.Attempts)
	require.NoError(t, integrationDB.QueryRow(`SELECT detail::text FROM request_timing_details WHERE trace_id=$1`, trace).Scan(&after))
	require.Equal(t, before, after)
}

func TestUpstreamAttemptsWSCacheRequiresIndependentCompletedTurn(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name, mutation string
		observed       int
	}{
		{"valid", `detail`, 1},
		{"http_scope", `jsonb_set(detail,'{physical_upstream,attempts,0,raw_cache,scope}','"physical_http_responses_terminal_usage"')`, 0},
		{"http_mode", `jsonb_set(detail,'{ttft_mode}','"upstream"')`, 0},
		{"http_status", `jsonb_set(detail,'{status}','200')`, 0},
		{"parent", `jsonb_set(detail,'{attempts,0,parent}','2')`, 0},
		{"http_credential", `jsonb_set(detail,'{attempts,0,upstream_credential,source}','"http_transport_handoff"')`, 0},
		{"extra_turn", `jsonb_set(detail,'{attempts}',(detail->'attempts')||jsonb_build_array(jsonb_set(detail->'attempts'->0,'{number}','2')))`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := upstreamAttemptWaitFixture(f.account)
			a.Kind, a.Status, a.Terminal = "ws_turn", 0, "completed"
			a.UpstreamCredential.Source, a.UpstreamRequest.Source = "websocket_handshake_credentials", "websocket_generation_payload"
			a.CacheUsage = &requesttiming.CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 90}
			trace := writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 100, TTFTMode: "ws_turn", Attempts: []requesttiming.Attempt{a}})
			_, err := integrationDB.Exec(`UPDATE request_connection_traces SET detail=`+tc.mutation+` WHERE trace_id=$1`, trace)
			require.NoError(t, err)
			data := readUpstreamAttemptWaits(t, f, now)
			require.Equal(t, 1, data.Summary.Attempts)
			require.Equal(t, tc.observed, data.RawCache.Observed)
			require.Equal(t, 1-tc.observed, data.RawCache.Unknown)
			_, err = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE trace_id=$1`, trace)
			require.NoError(t, err)
		})
	}
}

func upstreamAttemptWaitFixture(account int64) requesttiming.Attempt {
	end := 100.0
	a := requesttiming.Attempt{Number: 1, Kind: "http", AccountID: account, EndMS: &end, Status: 502,
		RouteFingerprint:   strings.Repeat("b", 64),
		UpstreamCredential: &requesttiming.UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: strings.Repeat("a", 64)},
		UpstreamRequest:    &requesttiming.UpstreamRequestObservation{Version: 1, Status: "observed", Source: "http_transport_body", Protocol: "responses", Model: "wire-a", ServiceTier: "priority", ReasoningEffort: "high", Stream: true},
		Events:             map[string]float64{"headers_written": 0, "request_written": 10, "first_byte": 50, "first_sse_event_flush": 60, "first_reasoning": 70, "first_visible_text": 80}}
	a.Route.Endpoint = "responses"
	return a
}

func writeUpstreamAttemptWaits(t *testing.T, f *channelOperationsFixture, now time.Time, s requesttiming.Snapshot) string {
	t.Helper()
	s.Version, s.TraceID, s.StartedAt = 1, uuid.NewString(), now
	latency := requesttiming.DeriveLatency(s)
	s.Latency = &latency
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE trace_id=$1`, s.TraceID)
	})
	repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	require.NoError(t, repo.writeConnectionTrace(t.Context(), service.ConnectionTrace{Source: "application", TraceID: s.TraceID,
		StartedAt: now, RequestID: uuid.NewString(), APIKeyID: f.key, UserID: f.user, AccountID: f.account,
		GroupID: f.group, Model: "public-waits", Path: "/v1/responses", Stream: true, Detail: s}))
	return s.TraceID
}

func readUpstreamAttemptWaits(t *testing.T, f *channelOperationsFixture, now time.Time) upstreamAttemptWaitReport {
	t.Helper()
	out, err := f.repo.ReadUpstreamOperations(t.Context(), service.UpstreamOperationsFilter{From: now.Add(-time.Minute), Until: now.Add(time.Minute),
		GroupID: f.group, Model: "public-waits", ModelScope: "public", Dimension: "account", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20})
	require.NoError(t, err)
	require.Equal(t, "ready", out.Attempts.Status, out.Attempts.Error)
	var data upstreamAttemptWaitReport
	require.NoError(t, json.Unmarshal(out.Attempts.Data, &data))
	return data
}

func TestUpstreamAttemptsWaitsKeepRecoveredAndUnbilledScope(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	first := upstreamAttemptWaitFixture(f.account)
	second := upstreamAttemptWaitFixture(f.outsider)
	end := 300.0
	second.Number, second.StartMS, second.EndMS, second.Status, second.Terminal = 2, 100, &end, 200, "completed"
	second.UpstreamRequest.Model = "wire-b"
	second.Events = map[string]float64{"headers_written": 100, "request_written": 110, "first_byte": 150, "first_sse_event_flush": 160, "first_reasoning": 170, "first_visible_text": 180}
	writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 300, Status: 200, Attempts: []requesttiming.Attempt{first, second}})
	timeout := upstreamAttemptWaitFixture(f.account)
	timeout.Status, timeout.Error = 0, "timeout"
	timeout.Events = map[string]float64{"headers_written": 0, "request_written": 10}
	writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 100, Status: 502, Attempts: []requesttiming.Attempt{timeout}})
	interrupted := upstreamAttemptWaitFixture(f.account)
	interrupted.Status, interrupted.Error = 0, "canceled"
	interrupted.Events = map[string]float64{"headers_written": 0, "request_written": 10}
	writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 100, Attempts: []requesttiming.Attempt{interrupted}})
	unknown := upstreamAttemptWaitFixture(f.account)
	unknown.Status, unknown.Terminal = 0, ""
	unknown.Events = map[string]float64{"headers_written": 0, "request_written": 10}
	writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 100, Attempts: []requesttiming.Attempt{unknown}})
	ws := upstreamAttemptWaitFixture(f.account)
	ws.Kind, ws.Status, ws.Terminal = "ws_turn", 0, "failed"
	ws.UpstreamCredential.Source, ws.UpstreamRequest.Source = "websocket_handshake_credentials", "websocket_generation_payload"
	ws.Events = map[string]float64{"first_upstream_event": 20, "first_reasoning": 30, "first_visible_text": 40}
	writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 100, TTFTMode: "ws_turn", Attempts: []requesttiming.Attempt{ws}})
	data := readUpstreamAttemptWaits(t, f, now)
	require.Equal(t, 6, data.Summary.Attempts)
	require.Equal(t, 1, data.Summary.Interrupted)
	require.Equal(t, 1, data.Summary.Unknown)
	require.Equal(t, 3, data.Summary.Failed)
	require.Equal(t, 1, data.Summary.Succeeded)
	require.Len(t, data.Rows, 3)
	for _, row := range data.Rows {
		require.Equal(t, 1, row.Waits.Version)
		if row.Transport == "ws_turn" {
			require.Equal(t, "canonical_physical_ws_turn_generation_start", row.Waits.Source)
			require.Equal(t, 0, row.Waits.FirstByte.Samples)
			require.Nil(t, row.Waits.FirstByte.MeanMS)
			require.Equal(t, 1, row.Waits.FirstEvent.Samples)
			require.Equal(t, 20.0, *row.Waits.FirstEvent.MeanMS)
			require.Equal(t, 40.0, *row.Waits.FirstText.MeanMS)
			continue
		}
		require.Equal(t, "canonical_physical_http_attempt", row.Waits.Source)
		require.Equal(t, 1, row.Waits.FirstByte.Samples)
		require.Equal(t, 40.0, *row.Waits.FirstByte.MeanMS)
		require.Equal(t, 70.0, *row.Waits.FirstText.MeanMS)
		if row.UpstreamModel == "wire-a" {
			require.Equal(t, f.account, row.AccountID)
			require.Equal(t, 4, row.Attempts)
			require.Equal(t, 3, row.Waits.FirstByte.Unknown)
			require.Equal(t, 0.25, *row.Waits.FirstByte.Coverage)
		} else {
			require.Equal(t, "wire-b", row.UpstreamModel)
			require.Equal(t, f.outsider, row.AccountID)
			require.Zero(t, row.Waits.FirstByte.Unknown)
		}
	}
	var billed int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM usage_logs WHERE api_key_id=$1`, f.key).Scan(&billed))
	require.Zero(t, billed)
}

func TestUpstreamAttemptsWaitsRejectAmbiguousAndHistoricalEvidence(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name, mutation  string
		byteN, contentN int
	}{
		{"missing_latency", `detail-'latency'`, 0, 0},
		{"future_latency", `jsonb_set(detail,'{latency,version}','9')`, 0, 0},
		{"truncated", `jsonb_set(detail,'{truncated}','true')`, 0, 0},
		{"physical_duplicate", `jsonb_set(detail,'{attempts}',(detail->'attempts')||(detail->'attempts'))`, 0, 0},
		{"physical_object", `jsonb_set(detail,'{attempts}','{}')`, 0, 0},
		{"physical_oversized", `jsonb_set(detail,'{attempts}',(SELECT jsonb_agg((detail->'attempts'->0)||jsonb_build_object('fixture_copy',n)) FROM generate_series(1,33) n))`, 0, 0},
		{"latency_duplicate", `jsonb_set(detail,'{latency,attempts}',(detail->'latency'->'attempts')||(detail->'latency'->'attempts'))`, 0, 0},
		{"latency_oversized", `jsonb_set(detail,'{latency,attempts}',(SELECT jsonb_agg((detail->'latency'->'attempts'->0)||jsonb_build_object('fixture_copy',n)) FROM generate_series(1,33) n))`, 0, 0},
		{"wrong_number", `jsonb_set(detail,'{latency,attempts,0,number}','2')`, 0, 0},
		{"string_number", `jsonb_set(detail,'{latency,attempts,0,number}','"1"')`, 0, 0},
		{"wrong_account", `jsonb_set(detail,'{attempts,0,account_id}','0')`, 0, 0},
		{"wrong_route", `jsonb_set(detail,'{attempts,0,route_fingerprint}',to_jsonb(repeat('c',64)))`, 0, 0},
		{"wrong_key", `jsonb_set(detail,'{attempts,0,upstream_credential,fingerprint}',to_jsonb(repeat('c',64)))`, 0, 0},
		{"wrong_model", `jsonb_set(detail,'{attempts,0,upstream_request,model}','"wire-b"')`, 0, 0},
		{"historical_scope", `detail #- '{physical_upstream,attempts,0,upstream_request}'`, 0, 0},
		{"future_scope", `jsonb_set(detail,'{physical_upstream,attempts,0,upstream_request,version}','9')`, 0, 0},
		{"missing_total", `detail-'total_ms'`, 0, 0},
		{"zero_byte", `jsonb_set(detail,'{latency,attempts,0,post_upload_first_byte_wait,ms}','0')`, 1, 1},
		{"negative_byte", `jsonb_set(detail,'{latency,attempts,0,post_upload_first_byte_wait,ms}','-1')`, 0, 1},
		{"byte_after_request", `jsonb_set(detail,'{latency,attempts,0,post_upload_first_byte_wait,ms}','101')`, 0, 1},
		{"byte_string", `jsonb_set(detail,'{latency,attempts,0,post_upload_first_byte_wait,ms}','"40"')`, 0, 1},
		{"text_truncated", `jsonb_set(detail,'{attempts,0,events,typed_content_truncated}','50')`, 1, 0},
		{"text_unknown", `jsonb_set(detail,'{latency,attempts,0,post_upload_first_text_wait,status}','"not_collected"')`, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 100, Status: 502, Attempts: []requesttiming.Attempt{upstreamAttemptWaitFixture(f.account)}})
			_, err := integrationDB.Exec(`UPDATE request_connection_traces SET detail=`+tc.mutation+` WHERE trace_id=$1`, trace)
			require.NoError(t, err)
			var before, after string
			require.NoError(t, integrationDB.QueryRow(`SELECT detail::text FROM request_connection_traces WHERE trace_id=$1`, trace).Scan(&before))
			data := readUpstreamAttemptWaits(t, f, now)
			require.Len(t, data.Rows, 1)
			require.Equal(t, 1, data.Summary.Attempts, "bad waits must not erase the independent failure denominator")
			require.Equal(t, tc.byteN, data.Rows[0].Waits.FirstByte.Samples)
			require.Equal(t, tc.contentN, data.Rows[0].Waits.FirstText.Samples)
			require.Equal(t, 1-tc.byteN, data.Rows[0].Waits.FirstByte.Unknown)
			if tc.byteN == 0 {
				require.Nil(t, data.Rows[0].Waits.FirstByte.MeanMS)
			} else if tc.name == "zero_byte" {
				require.Equal(t, 0.0, *data.Rows[0].Waits.FirstByte.MeanMS)
			}
			require.NoError(t, integrationDB.QueryRow(`SELECT detail::text FROM request_connection_traces WHERE trace_id=$1`, trace).Scan(&after))
			require.Equal(t, before, after)
			_, err = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE trace_id=$1`, trace)
			require.NoError(t, err)
		})
	}
}

func TestUpstreamAttemptsWSWaitsRejectHandshakeAndCrossTurnEvidence(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name, mutation string
		eventN, textN  int
		eventMS        float64
	}{
		{"valid", `detail`, 1, 1, 20},
		{"zero_event", `jsonb_set(detail,'{latency,ws_turn,first_event,ms}','0')`, 1, 1, 0},
		{"missing_ws_latency", `detail #- '{latency,ws_turn}'`, 0, 0, 0},
		{"future_latency", `jsonb_set(detail,'{latency,version}','9')`, 0, 0, 0},
		{"wrong_source", `jsonb_set(detail,'{latency,ws_turn,source}','"socket_handshake"')`, 0, 0, 0},
		{"wrong_number", `jsonb_set(detail,'{latency,ws_turn,number}','2')`, 0, 0, 0},
		{"wrong_mode", `jsonb_set(detail,'{ttft_mode}','"upstream"')`, 0, 0, 0},
		{"http_status", `jsonb_set(detail,'{status}','200')`, 0, 0, 0},
		{"extra_turn", `jsonb_set(detail,'{attempts}',(detail->'attempts')||jsonb_build_array(jsonb_set(detail->'attempts'->0,'{number}','2')))`, 0, 0, 0},
		{"physical_parent", `jsonb_set(detail,'{attempts,0,parent}','2')`, 0, 0, 0},
		{"http_credential", `jsonb_set(detail,'{attempts,0,upstream_credential,source}','"http_transport_handoff"')`, 0, 0, 0},
		{"wrong_model", `jsonb_set(detail,'{attempts,0,upstream_request,model}','"another-model"')`, 0, 0, 0},
		{"capture_gap", `jsonb_set(detail,'{attempts,0,events,typed_content_truncated}','50')`, 0, 0, 0},
		{"negative_text", `jsonb_set(detail,'{latency,ws_turn,first_text,ms}','-1')`, 1, 0, 20},
		{"after_request", `jsonb_set(detail,'{latency,ws_turn,first_text,ms}','101')`, 1, 0, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := upstreamAttemptWaitFixture(f.account)
			a.Kind, a.Status, a.Terminal = "ws_turn", 0, "failed"
			a.UpstreamCredential.Source, a.UpstreamRequest.Source = "websocket_handshake_credentials", "websocket_generation_payload"
			a.Events = map[string]float64{"first_upstream_event": 20, "first_reasoning": 30, "first_visible_text": 40}
			trace := writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 100, TTFTMode: "ws_turn", Attempts: []requesttiming.Attempt{a}})
			_, err := integrationDB.Exec(`UPDATE request_connection_traces SET detail=`+tc.mutation+` WHERE trace_id=$1`, trace)
			require.NoError(t, err)
			var before, after string
			require.NoError(t, integrationDB.QueryRow(`SELECT detail::text FROM request_connection_traces WHERE trace_id=$1`, trace).Scan(&before))
			data := readUpstreamAttemptWaits(t, f, now)
			require.Len(t, data.Rows, 1)
			waits := data.Rows[0].Waits
			require.Zero(t, waits.FirstByte.Samples, "socket generation has no HTTP first-byte boundary")
			require.Nil(t, waits.FirstByte.MeanMS)
			require.Equal(t, 1, waits.FirstByte.Unknown)
			require.Equal(t, tc.eventN, waits.FirstEvent.Samples)
			require.Equal(t, tc.textN, waits.FirstText.Samples)
			if tc.eventN != 0 {
				require.Equal(t, tc.eventMS, *waits.FirstEvent.MeanMS)
			}
			require.NoError(t, integrationDB.QueryRow(`SELECT detail::text FROM request_connection_traces WHERE trace_id=$1`, trace).Scan(&after))
			require.Equal(t, before, after)
			_, err = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE trace_id=$1`, trace)
			require.NoError(t, err)
		})
	}
}

func TestUpstreamAttemptsWaitsHundredCredentialScopesBoundedPage(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	snapshot := requesttiming.Snapshot{Version: 1, TotalMS: 100, Status: 502, Attempts: []requesttiming.Attempt{upstreamAttemptWaitFixture(f.account)}}
	latency := requesttiming.DeriveLatency(snapshot)
	snapshot.Latency = &latency
	physical := requesttiming.DerivePhysicalUpstream(snapshot)
	snapshot.PhysicalUpstream = &physical
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	prefix := uuid.NewString()
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE api_key_id=$1`, f.key)
	})
	_, err = integrationDB.Exec(`INSERT INTO request_connection_traces
 (trace_id,started_at,request_id,client_request_id,user_id,api_key_id,account_id,group_id,model,path,stream,detail,source)
 SELECT md5($1||n::text)::uuid,$2,$1||n::text,'',$3,$4,$5,$6,'public-waits','/v1/responses',true,
 jsonb_set(jsonb_set($7::jsonb,'{attempts,0,upstream_credential,fingerprint}',to_jsonb(repeat(md5((n%100)::text),2))),
 '{physical_upstream,attempts,0,credential_fingerprint}',to_jsonb(repeat(md5((n%100)::text),2))), 'application'
 FROM generate_series(1,1000) n`, prefix, now, f.user, f.key, f.account, f.group, raw)
	require.NoError(t, err)
	started := time.Now()
	svc := service.NewChannelService(f.repo, nil, nil, nil, nil)
	out, err := svc.UpstreamOperations(t.Context(), service.UpstreamOperationsFilter{From: now.Add(-time.Minute), Until: now.Add(time.Minute),
		GroupID: f.group, AccountID: f.account, Model: "public-waits", ModelScope: "public", Dimension: "account", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20})
	require.NoError(t, err)
	require.Equal(t, "ready", out.Attempts.Status, out.Attempts.Error)
	var data struct {
		upstreamAttemptWaitReport
		Total int
	}
	require.NoError(t, json.Unmarshal(out.Attempts.Data, &data))
	require.Equal(t, 1000, data.Summary.Attempts)
	require.Equal(t, 1000, data.Summary.Failed)
	require.Equal(t, 100, data.Total)
	require.Len(t, data.Rows, 20)
	for _, row := range data.Rows {
		require.Equal(t, 10, row.Attempts)
		require.Equal(t, 10, row.Waits.FirstByte.Samples)
		require.Zero(t, row.Waits.FirstByte.Unknown)
		require.Equal(t, 40.0, *row.Waits.FirstByte.MeanMS)
	}
	t.Logf("1000 retained failures / 100 credential scopes / 20 returned rows: %s (service budget 25s)", time.Since(started))
}

type upstreamAttemptOutputDistribution struct {
	Samples    int
	Unknown    int      `json:"unknown_samples"`
	Partial    int      `json:"partial_samples"`
	Mean       *float64 `json:"mean_ms"`
	P95        *float64 `json:"p95_ms"`
	Max        *float64 `json:"max_ms"`
	PartialMax *float64 `json:"partial_max_ms"`
	Coverage   *float64
	Latest     *time.Time `json:"latest_sample_at"`
}
type upstreamAttemptOutputMetrics struct {
	Version  int
	Source   string
	Attempts int
	Gap      upstreamAttemptOutputDistribution `json:"max_gap"`
	Tail     upstreamAttemptOutputDistribution `json:"tail_wait"`
	Rate     struct {
		Samples  int
		Unknown  int      `json:"unknown_samples"`
		TPS      *float64 `json:"tokens_per_second"`
		Tokens   *int64   `json:"non_reasoning_tokens"`
		Window   *float64 `json:"window_ms"`
		Coverage *float64
		Latest   *time.Time `json:"latest_sample_at"`
	}
}

func upstreamAttemptOutputFixture(account int64) requesttiming.Attempt {
	a := upstreamAttemptWaitFixture(account)
	end := 450.0
	a.Status, a.Terminal, a.EndMS = 200, "completed", &end
	a.Events["first_visible_text"] = 10
	a.TextStream = &requesttiming.TextStreamTiming{Chunks: 3, FirstMS: 10, LastMS: 310, MaxGapMS: 200, Terminal: "completed", TerminalMS: 410}
	a.OutputUsage = &requesttiming.OutputUsage{Status: "observed", ContentStatus: "text_and_reasoning", OutputTokens: 120, ReasoningTokens: 20}
	return a
}

func TestUpstreamAttemptsMessagesScopeBucketsAndMetrics(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, variant := range []string{"base", "fast", "standard", "budget4096", "budget8192", "adaptive", "disabled", "effort", "tier", "other_model"} {
		a := upstreamAttemptMessagesFixture(f.account)
		switch variant {
		case "fast":
			a.UpstreamRequest.Speed = "fast"
		case "standard":
			a.UpstreamRequest.Speed = "standard"
		case "budget4096", "budget8192":
			a.UpstreamRequest.ThinkingMode, a.UpstreamRequest.ThinkingBudgetTokens = "enabled", 4096
			if variant == "budget8192" {
				a.UpstreamRequest.ThinkingBudgetTokens = 8192
			}
		case "adaptive", "disabled":
			a.UpstreamRequest.ThinkingMode = variant
		case "effort":
			a.UpstreamRequest.ReasoningEffort = "max"
		case "tier":
			a.UpstreamRequest.ServiceTier = "standard_only"
		case "other_model":
			a.UpstreamRequest.Model = "wire-b"
		}
		writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 500, Status: 200, Attempts: []requesttiming.Attempt{a}})
	}
	data := readUpstreamAttemptWaits(t, f, now)
	require.Equal(t, 10, data.Summary.Attempts)
	require.Equal(t, 10, data.RequestScope.Observed)
	require.Zero(t, data.RequestScope.Unknown)
	require.Len(t, data.Rows, 10, "speed, mode and exact budget must not collapse distinct performance/cache scopes")
	require.Equal(t, 10, data.RawCache.Observed)
	require.EqualValues(t, 1000, data.RawCache.Input)
	require.EqualValues(t, 750, data.RawCache.Read)
	require.InDelta(t, 0.75, *data.RawCache.ReadRate, 0.000001)
	seen := map[string]bool{}
	for _, row := range data.Rows {
		budget := int64(0)
		if row.ThinkingBudgetTokens != nil {
			budget = *row.ThinkingBudgetTokens
		}
		seen[fmt.Sprintf("%s:%s:%s:%d", row.UpstreamModel, row.Speed, row.ThinkingMode, budget)] = true
		require.Equal(t, 1, row.RawCache.Observed)
		require.Equal(t, 1, row.Waits.FirstByte.Samples)
		require.Equal(t, 1, row.Waits.FirstText.Samples)
		require.Equal(t, 1, row.Output.Gap.Samples)
		require.Equal(t, 1, row.Output.Rate.Samples)
		require.InDelta(t, 250.0, *row.Output.Rate.TPS, 0.000001)
	}
	require.True(t, seen["wire-a:unspecified:enabled:4096"])
	require.True(t, seen["wire-a:unspecified:enabled:8192"])
	require.True(t, seen["wire-a:fast:unspecified:0"])
}

func upstreamAttemptMessagesFixture(account int64) requesttiming.Attempt {
	a := upstreamAttemptOutputFixture(account)
	a.Route.Endpoint, a.TextStream.Protocol = "messages", "messages"
	a.UpstreamRequest = &requesttiming.UpstreamRequestObservation{Version: 1, Status: "observed", Source: "http_transport_body", Protocol: "messages", Model: "wire-a", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Speed: "unspecified", ThinkingMode: "unspecified", Stream: true}
	a.OutputUsage.Protocol, a.OutputUsage.ContentStatus, a.OutputUsage.Final = "messages", "messages_text_and_reasoning", true
	a.CacheUsage = &requesttiming.CacheUsage{Protocol: "messages", Status: "observed", InputTokens: 100, ReadTokens: 75, WriteTokens: 5, UncachedTokens: 20, InputReported: true, ReadReported: true, WriteReported: true}
	return a
}

func TestUpstreamAttemptsMessagesRejectInvalidScopeAndCounters(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name, patch          string
		scope, cache, output int
	}{
		{"valid", `detail`, 1, 1, 1},
		{"missing_speed", `detail #- '{physical_upstream,attempts,0,upstream_request,speed}'`, 0, 0, 0},
		{"bad_tier", `jsonb_set(detail,'{physical_upstream,attempts,0,upstream_request,service_tier}','"priority"')`, 0, 0, 0},
		{"bad_effort", `jsonb_set(detail,'{physical_upstream,attempts,0,upstream_request,reasoning_effort}','"turbo"')`, 0, 0, 0},
		{"unknown_mode", `jsonb_set(detail,'{physical_upstream,attempts,0,upstream_request,thinking_mode}','"future"')`, 0, 0, 0},
		{"unexpected_budget", `jsonb_set(detail,'{physical_upstream,attempts,0,upstream_request,thinking_budget_tokens}','4096')`, 0, 0, 0},
		{"null_budget", `jsonb_set(detail,'{physical_upstream,attempts,0,upstream_request,thinking_budget_tokens}','null')`, 0, 0, 0},
		{"missing_enabled_budget", `jsonb_set(detail,'{physical_upstream,attempts,0,upstream_request,thinking_mode}','"enabled"')`, 0, 0, 0},
		{"fraction_budget", `jsonb_set(jsonb_set(detail,'{physical_upstream,attempts,0,upstream_request,thinking_mode}','"enabled"'),'{physical_upstream,attempts,0,upstream_request,thinking_budget_tokens}','1024.5')`, 0, 0, 0},
		{"unsafe_budget", `jsonb_set(jsonb_set(detail,'{physical_upstream,attempts,0,upstream_request,thinking_mode}','"enabled"'),'{physical_upstream,attempts,0,upstream_request,thinking_budget_tokens}','9007199254740992')`, 0, 0, 0},
		{"string_budget", `jsonb_set(jsonb_set(detail,'{physical_upstream,attempts,0,upstream_request,thinking_mode}','"enabled"'),'{physical_upstream,attempts,0,upstream_request,thinking_budget_tokens}','"4096"')`, 0, 0, 0},
		{"bad_uncached", `jsonb_set(detail,'{attempts,0,cache_usage,uncached_tokens}','21')`, 1, 0, 1},
		{"missing_read_presence", `detail #- '{attempts,0,cache_usage,read_reported}'`, 1, 0, 1},
		{"wrong_cache_protocol", `jsonb_set(detail,'{attempts,0,cache_usage,protocol}','"responses"')`, 1, 0, 1},
		{"wrong_output_protocol", `jsonb_set(detail,'{attempts,0,output_usage,protocol}','"responses"')`, 1, 1, 0},
		{"zero_uncached_omitted", `detail`, 1, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := upstreamAttemptMessagesFixture(f.account)
			if tc.name == "zero_uncached_omitted" {
				a.CacheUsage.UncachedTokens, a.CacheUsage.ReadTokens = 0, 95
			}
			trace := writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 500, Status: 200, Attempts: []requesttiming.Attempt{a}})
			_, err := integrationDB.Exec(`UPDATE request_connection_traces SET detail=`+tc.patch+` WHERE trace_id=$1`, trace)
			require.NoError(t, err)
			var before, after string
			require.NoError(t, integrationDB.QueryRow(`SELECT detail::text FROM request_connection_traces WHERE trace_id=$1`, trace).Scan(&before))
			data := readUpstreamAttemptWaits(t, f, now)
			require.Equal(t, 1, data.Summary.Attempts)
			require.Equal(t, tc.scope, data.RequestScope.Observed)
			require.Equal(t, tc.cache, data.RawCache.Observed)
			require.Len(t, data.Rows, 1)
			require.Equal(t, tc.output, data.Rows[0].Output.Rate.Samples)
			require.NoError(t, integrationDB.QueryRow(`SELECT detail::text FROM request_connection_traces WHERE trace_id=$1`, trace).Scan(&after))
			require.Equal(t, before, after, "observation must not rewrite old evidence")
			_, err = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE trace_id=$1`, trace)
			require.NoError(t, err)
		})
	}
}

func TestUpstreamAttemptsOutputPerScopeCompletePartialAndWeightedRate(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, kind := range []string{"first", "second", "partial", "missing", "other_model", "ws"} {
		a := upstreamAttemptOutputFixture(f.account)
		s := requesttiming.Snapshot{TotalMS: 1500, Attempts: []requesttiming.Attempt{a}}
		switch kind {
		case "second":
			a.TextStream.LastMS, a.TextStream.MaxGapMS, a.TextStream.TerminalMS = 610, 400, 810
			end := 850.0
			a.EndMS = &end
		case "partial":
			a.Terminal, a.Error = "failed", "timeout"
			a.TextStream.LastMS, a.TextStream.MaxGapMS, a.TextStream.TerminalMS, a.TextStream.Terminal = 910, 600, 1010, "failed"
			end := 1100.0
			a.EndMS = &end
		case "missing":
			a.TextStream, a.OutputUsage = nil, nil
		case "other_model":
			a.UpstreamRequest.Model = "wire-b"
		case "ws":
			a.Kind, a.Status, a.UpstreamCredential.Source = "ws_turn", 0, "websocket_handshake_credentials"
			a.UpstreamRequest.Source = "websocket_generation_payload"
			s.TTFTMode = "ws_turn"
		}
		s.Attempts[0] = a
		writeUpstreamAttemptWaits(t, f, now, s)
	}
	data := readUpstreamAttemptWaits(t, f, now)
	require.Equal(t, 6, data.Summary.Attempts)
	require.Len(t, data.Rows, 3)
	for _, row := range data.Rows {
		m := row.Output
		require.Equal(t, 1, m.Version)
		require.Equal(t, "canonical_physical_attempt_output", m.Source)
		require.Equal(t, row.Attempts, m.Attempts)
		if row.Transport == "ws_turn" || row.UpstreamModel == "wire-b" {
			require.Equal(t, 1, m.Gap.Samples)
			require.Zero(t, m.Gap.Unknown)
			require.Equal(t, 250.0, *m.Rate.TPS)
			continue
		}
		require.Equal(t, 2, m.Gap.Samples)
		require.Equal(t, 2, m.Gap.Unknown)
		require.Equal(t, 1, m.Gap.Partial)
		require.Equal(t, 300.0, *m.Gap.Mean)
		require.InDelta(t, 390, *m.Gap.P95, 0.000001)
		require.Equal(t, 400.0, *m.Gap.Max)
		require.Equal(t, 600.0, *m.Gap.PartialMax)
		require.Equal(t, 0.5, *m.Gap.Coverage)
		require.NotNil(t, m.Gap.Latest)
		require.Equal(t, 150.0, *m.Tail.Mean)
		require.InDelta(t, 195, *m.Tail.P95, 0.000001)
		require.Equal(t, 2, m.Rate.Samples)
		require.Equal(t, 2, m.Rate.Unknown)
		require.EqualValues(t, 200, *m.Rate.Tokens)
		require.Equal(t, 1200.0, *m.Rate.Window)
		require.InDelta(t, 200.0/1.2, *m.Rate.TPS, 0.000001)
	}
}

func TestUpstreamAttemptsOutputRejectsHistoricalCrossScopeAndMalformedReports(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name, mutation     string
		gap, partial, rate int
	}{
		{"valid", `detail`, 1, 0, 1},
		{"missing", `detail #- '{physical_upstream,attempts,0,output}'`, 0, 0, 0},
		{"final_is_not_fallback", `jsonb_set(detail #- '{physical_upstream,attempts,0,output}','{output_rate}',detail->'physical_upstream'->'attempts'->0->'output')`, 0, 0, 0},
		{"future", `jsonb_set(detail,'{physical_upstream,attempts,0,output,version}','9')`, 0, 0, 0},
		{"string_version", `jsonb_set(detail,'{physical_upstream,attempts,0,output,version}','"1"')`, 0, 0, 0},
		{"foreign_source", `jsonb_set(detail,'{physical_upstream,attempts,0,output,text_stream,source}','"typed_responses_ws"')`, 0, 0, 0},
		{"final_scope", `jsonb_set(detail,'{physical_upstream,attempts,0,output,text_stream,scope}','"final_physical_http_responses_text_deltas_at_parser"')`, 0, 0, 0},
		{"wrong_route", `jsonb_set(detail,'{attempts,0,route_fingerprint}',to_jsonb(repeat('c',64)))`, 0, 0, 0},
		{"wrong_key", `jsonb_set(detail,'{attempts,0,upstream_credential,fingerprint}',to_jsonb(repeat('c',64)))`, 0, 0, 0},
		{"wrong_model", `jsonb_set(detail,'{attempts,0,upstream_request,model}','"wire-b"')`, 0, 0, 0},
		{"wrong_protocol", `jsonb_set(detail,'{attempts,0,route,endpoint}','"messages"')`, 0, 0, 0},
		{"raw_protocol", `jsonb_set(detail,'{attempts,0,text_stream,protocol}','"chat_completions"')`, 0, 0, 0},
		{"missing_identity", `jsonb_set(detail,'{physical_upstream,attempts,0,identity_known}','false')`, 0, 0, 0},
		{"missing_scope", `detail #- '{physical_upstream,attempts,0,upstream_request}'`, 0, 0, 0},
		{"physical_duplicate", `jsonb_set(detail,'{attempts}',(detail->'attempts')||(detail->'attempts'))`, 0, 0, 0},
		{"physical_object", `jsonb_set(detail,'{attempts}','{}')`, 0, 0, 0},
		{"capture_truncated", `jsonb_set(detail,'{truncated}','true')`, 0, 0, 0},
		{"parser_gap", `jsonb_set(detail,'{attempts,0,events,typed_content_truncated}','100')`, 0, 0, 0},
		{"invalid", `jsonb_set(detail,'{attempts,0,text_stream,invalid}','true')`, 0, 0, 0},
		{"truncated_cannot_complete", `jsonb_set(detail,'{attempts,0,text_stream,truncated}','true')`, 0, 0, 0},
		{"failed_cannot_complete", `jsonb_set(detail,'{attempts,0,terminal}','"failed"')`, 0, 0, 0},
		{"no_end", `detail #- '{attempts,0,end_ms}'`, 0, 0, 0},
		{"end_after_capture", `jsonb_set(detail,'{attempts,0,end_ms}','1501')`, 0, 0, 0},
		{"string_chunks", `jsonb_set(detail,'{attempts,0,text_stream,chunks}','"3"')`, 0, 0, 0},
		{"fractional_chunks", `jsonb_set(detail,'{attempts,0,text_stream,chunks}','3.5')`, 0, 0, 0},
		{"negative_gap", `jsonb_set(detail,'{attempts,0,text_stream,max_gap_ms}','-1')`, 0, 0, 0},
		{"impossible_gap", `jsonb_set(detail,'{attempts,0,text_stream,max_gap_ms}','10')`, 0, 0, 0},
		{"unmatched_gap", `jsonb_set(detail,'{physical_upstream,attempts,0,output,text_stream,max_gap_ms}','201')`, 0, 0, 0},
		{"unmatched_tail", `jsonb_set(detail,'{physical_upstream,attempts,0,output,text_stream,tail_wait_ms}','101')`, 0, 0, 0},
		{"unmatched_span", `jsonb_set(detail,'{physical_upstream,attempts,0,output,text_stream,span_ms}','301')`, 0, 0, 0},
		{"terminal_after_capture", `jsonb_set(detail,'{attempts,0,text_stream,terminal_ms}','1501')`, 0, 0, 0},
		{"cross_next_send", `jsonb_set(detail,'{attempts}',(detail->'attempts')||'[{"number":2,"kind":"http","start_ms":300}]')`, 0, 0, 0},
		{"next_missing_start", `jsonb_set(detail,'{attempts}',(detail->'attempts')||'[{"number":2,"kind":"http"}]')`, 0, 0, 0},
		{"next_null_start", `jsonb_set(detail,'{attempts}',(detail->'attempts')||'[{"number":2,"kind":"http","start_ms":null}]')`, 0, 0, 0},
		{"next_bad_number", `jsonb_set(detail,'{attempts}',(detail->'attempts')||'[{"number":"2","kind":"http","start_ms":300}]')`, 0, 0, 0},
		{"past_span", `jsonb_set(jsonb_set(detail,'{attempts,0,text_stream,max_gap_ms}','301'),'{physical_upstream,attempts,0,output,text_stream,max_gap_ms}','301')`, 0, 0, 0},
		{"rate_source", `jsonb_set(detail,'{physical_upstream,attempts,0,output,rate,source}','"legacy_tps"')`, 1, 0, 0},
		{"rate_scope", `jsonb_set(detail,'{physical_upstream,attempts,0,output,rate,scope}','"final_physical_http_responses_text_at_parser"')`, 1, 0, 0},
		{"rate_method", `jsonb_set(detail,'{physical_upstream,attempts,0,output,rate,method}','"billable_tokens"')`, 1, 0, 0},
		{"rate_zero_window", `jsonb_set(detail,'{physical_upstream,attempts,0,output,rate,window_ms}','0')`, 1, 0, 0},
		{"rate_string_window", `jsonb_set(detail,'{physical_upstream,attempts,0,output,rate,window_ms}','"400"')`, 1, 0, 0},
		{"rate_mismatch", `jsonb_set(detail,'{physical_upstream,attempts,0,output,rate,tokens_per_second}','251')`, 1, 0, 0},
		{"rate_tokens", `jsonb_set(detail,'{attempts,0,output_usage,output_tokens}','121')`, 1, 0, 0},
		{"rate_unsafe", `jsonb_set(detail,'{attempts,0,output_usage,output_tokens}','9007199254740992')`, 1, 0, 0},
		{"rate_tool", `jsonb_set(detail,'{attempts,0,text_stream,non_text_output}','true')`, 1, 0, 0},
		{"rate_multiple", `jsonb_set(detail,'{attempts,0,events,typed_output_multiple_candidates}','1')`, 1, 0, 0},
		{"rate_no_usage", `detail #- '{attempts,0,output_usage}'`, 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := upstreamAttemptOutputFixture(f.account)
			trace := writeUpstreamAttemptWaits(t, f, now, requesttiming.Snapshot{TotalMS: 1500, Attempts: []requesttiming.Attempt{a}})
			_, err := integrationDB.Exec(`UPDATE request_connection_traces SET detail=`+tc.mutation+` WHERE trace_id=$1`, trace)
			require.NoError(t, err)
			var before, after string
			require.NoError(t, integrationDB.QueryRow(`SELECT detail::text FROM request_connection_traces WHERE trace_id=$1`, trace).Scan(&before))
			data := readUpstreamAttemptWaits(t, f, now)
			require.Equal(t, 1, data.Summary.Attempts)
			require.Len(t, data.Rows, 1)
			m := data.Rows[0].Output
			require.Equal(t, tc.gap, m.Gap.Samples)
			require.Equal(t, tc.partial, m.Gap.Partial)
			require.Equal(t, tc.rate, m.Rate.Samples)
			require.Equal(t, 1-tc.gap, m.Gap.Unknown)
			require.Equal(t, 1-tc.rate, m.Rate.Unknown)
			if tc.gap == 0 {
				require.Nil(t, m.Gap.Mean)
				require.Nil(t, m.Gap.P95)
				require.Nil(t, m.Gap.Latest)
			}
			if tc.rate == 0 {
				require.Nil(t, m.Rate.TPS)
				require.Nil(t, m.Rate.Latest)
			}
			require.NoError(t, integrationDB.QueryRow(`SELECT detail::text FROM request_connection_traces WHERE trace_id=$1`, trace).Scan(&after))
			require.Equal(t, before, after)
			_, err = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE trace_id=$1`, trace)
			require.NoError(t, err)
		})
	}
}

func TestUpstreamAttemptsOutputZeroSinglePartialAndIndependentWS(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, mode := range []string{"zero", "single", "partial", "truncated", "no_terminal", "short", "no_reasoning", "buffered", "fractional_clock_negative", "fractional_clock_positive", "chat", "chat_missing_final", "messages_unattributed", "ws", "ws_handshake", "ws_other_turn"} {
		t.Run(mode, func(t *testing.T) {
			a := upstreamAttemptOutputFixture(f.account)
			s := requesttiming.Snapshot{TotalMS: 1500}
			gap, partial, tail, rate := 1, 0, 1, 1
			switch mode {
			case "zero":
				a.TextStream.LastMS, a.TextStream.MaxGapMS = 10, 0
				rate = 0
			case "single":
				a.TextStream.Chunks, a.TextStream.LastMS, a.TextStream.MaxGapMS = 1, 10, 0
				gap, rate = 0, 0
			case "partial":
				a.Terminal, a.Error, a.TextStream.Terminal = "failed", "timeout", "failed"
				gap, partial, tail, rate = 0, 1, 0, 0
			case "truncated":
				a.TextStream.Truncated = true
				gap, partial, tail, rate = 0, 1, 0, 0
			case "no_terminal":
				a.Terminal, a.TextStream.Terminal, a.TextStream.TerminalMS = "", "", 0
				gap, partial, tail, rate = 0, 1, 0, 0
			case "short":
				a.OutputUsage.OutputTokens = 30
				rate = 0
			case "no_reasoning":
				a.OutputUsage.Status = "not_reported"
				rate = 0
			case "fractional_clock_negative", "fractional_clock_positive":
				a.TextStream.Chunks, a.TextStream.FirstMS, a.TextStream.LastMS = 2, 0.1, 300.2
				if mode == "fractional_clock_positive" {
					a.TextStream.FirstMS, a.TextStream.LastMS = 0.0001, 300.1
				}
				a.Events["first_visible_text"] = a.TextStream.FirstMS
				a.TextStream.MaxGapMS = a.TextStream.LastMS - a.TextStream.FirstMS

			case "buffered":
				end := 20.0
				a.EndMS = &end
			}
			if mode == "chat" || mode == "chat_missing_final" {
				a.Route.Endpoint, a.UpstreamRequest.Protocol, a.TextStream.Protocol = "chat_completions", "chat_completions", "chat_completions"
				a.OutputUsage.Protocol, a.OutputUsage.ContentStatus, a.OutputUsage.Final = "chat_completions", "chat_completions_text_and_reasoning", mode == "chat"
				if mode == "chat_missing_final" {
					rate = 0
				}
			}
			if mode == "messages_unattributed" {
				a.Route.Endpoint, a.UpstreamRequest.Protocol, a.TextStream.Protocol = "messages", "messages", "messages"
				a.OutputUsage.Protocol, a.OutputUsage.ContentStatus, a.OutputUsage.Final = "messages", "messages_text_and_reasoning", true
				gap, partial, tail, rate = 0, 0, 0, 0
			}
			if strings.HasPrefix(mode, "ws") {
				a.Kind, a.Status, a.UpstreamCredential.Source = "ws_turn", 0, "websocket_handshake_credentials"
				a.UpstreamRequest.Source = "websocket_generation_payload"
				s.TTFTMode = "ws_turn"
			}
			s.Attempts = []requesttiming.Attempt{a}
			trace := writeUpstreamAttemptWaits(t, f, now, s)
			if mode == "ws_handshake" || mode == "ws_other_turn" {
				mutation := `jsonb_set(detail,'{attempts,0,status}','101')`
				if mode == "ws_other_turn" {
					mutation = `jsonb_set(detail,'{attempts}',(detail->'attempts')||'[{"number":2,"kind":"http","start_ms":1000}]')`
				}
				_, err := integrationDB.Exec(`UPDATE request_connection_traces SET detail=`+mutation+` WHERE trace_id=$1`, trace)
				require.NoError(t, err)
				gap, partial, tail, rate = 0, 0, 0, 0
			}
			data := readUpstreamAttemptWaits(t, f, now)
			require.Len(t, data.Rows, 1)
			m := data.Rows[0].Output
			require.Equal(t, gap, m.Gap.Samples)
			require.Equal(t, partial, m.Gap.Partial)
			require.Equal(t, tail, m.Tail.Samples)
			require.Equal(t, rate, m.Rate.Samples)
			if mode == "zero" {
				require.NotNil(t, m.Gap.Mean)
				require.Zero(t, *m.Gap.Mean)
			}
			if partial > 0 {
				require.Equal(t, 200.0, *m.Gap.PartialMax)
				require.Nil(t, m.Gap.Mean)
			}
			_, err := integrationDB.Exec(`DELETE FROM request_connection_traces WHERE trace_id=$1`, trace)
			require.NoError(t, err)
		})
	}
}
