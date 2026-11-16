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

func TestUpstreamRawWaitsWSGenerationKeepsScopeAndUnknowns(t *testing.T) {
	f := newChannelOperationsFixture(t)
	repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	now := time.Now().UTC()
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	write := func(model, credential, route string, text float64) *service.UsageLog {
		log := &service.UsageLog{UserID: f.user, APIKeyID: f.key, AccountID: f.account,
			Model: "ws-fixture", RequestedModel: model, RequestID: uuid.NewString(), CreatedAt: now, Stream: true}
		_, err := repo.Create(t.Context(), log)
		require.NoError(t, err)
		end := 500.0
		s := requesttiming.Snapshot{Version: 1, TraceID: uuid.NewString(), TTFTMode: "ws_turn", TotalMS: 600,
			Terminal: "completed", Outcome: "success", Attempts: []requesttiming.Attempt{
				{Number: 1, Kind: "ws_turn", AccountID: f.account, EndMS: &end, Terminal: "completed",
					RouteFingerprint: route, UpstreamCredential: &requesttiming.UpstreamCredentialObservation{Status: "observed", Source: "websocket_handshake_credentials", Fingerprint: credential},
					CacheUsage: &requesttiming.CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 50},
					Events:     map[string]float64{"first_upstream_event": 20, "first_reasoning": 40, "first_visible_text": text}},
			}}
		require.NoError(t, repo.writeTiming(t.Context(), timingWrite{requestID: log.RequestID, apiKeyID: f.key, data: s}))
		return log
	}
	q := service.UpstreamOperationsFilter{From: now.Add(-time.Hour), Until: now.Add(time.Minute), AccountID: f.account,
		ModelScope: "public", Model: "same-scope", Dimension: "model", Sort: "requests", Direction: "desc", Page: 1, PageSize: 100}
	write(q.Model, a, b, 100)
	write(q.Model, a, b, 0)
	missing := write(q.Model, a, b, 200)
	_, err := integrationDB.ExecContext(t.Context(), `UPDATE request_timing_details SET detail=detail #- '{latency,ws_turn}' WHERE usage_log_id=$1`, missing.ID)
	require.NoError(t, err)
	write(q.Model, strings.Repeat("c", 64), b, 300)
	write(q.Model, a, strings.Repeat("d", 64), 400)
	got := readUpstreamCache(t, f, q)
	require.Equal(t, 5, got.Summary.Requests)
	require.Len(t, got.Rows, 3)
	waits := got.Rows[0].Waits
	require.Equal(t, "ws_turn", got.Rows[0].Transport)
	require.Equal(t, "canonical_physical_ws_turn_generation_start", waits.Source)
	require.Equal(t, 0, waits.FirstByte.Samples, "frames do not establish socket first-byte timing")
	require.Nil(t, waits.FirstByte.MeanMS)
	require.Equal(t, 2, waits.FirstText.Samples)
	require.Equal(t, 1, waits.FirstText.Unknown)
	require.Equal(t, 50.0, *waits.FirstText.MeanMS)
	require.InDelta(t, 2.0/3.0, *waits.FirstText.Coverage, 1e-12)
	require.NotNil(t, waits.FirstText.LatestSampleAt)
	for _, value := range []upstreamWaitMetrics{waits.FirstEvent, waits.FirstReasoning} {
		require.Equal(t, 2, value.Samples)
		require.Equal(t, 1, value.Unknown)
	}
	httpLog := write(q.Model, a, b, 100)
	var raw []byte
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT detail FROM request_timing_details WHERE usage_log_id=$1`, httpLog.ID).Scan(&raw))
	var httpSample requesttiming.Snapshot
	require.NoError(t, json.Unmarshal(raw, &httpSample))
	httpSample.TTFTMode, httpSample.Status = "", 200
	httpSample.Attempts[0].Kind, httpSample.Attempts[0].Status = "http", 200
	httpSample.Attempts[0].UpstreamCredential.Source = "http_transport_handoff"
	httpSample.Attempts[0].Events = map[string]float64{"headers_written": 10, "request_written": 20, "first_byte": 25, "first_sse_event_flush": 30, "first_visible_text": 50}
	require.NoError(t, repo.writeTiming(t.Context(), timingWrite{requestID: httpLog.RequestID, apiKeyID: f.key, data: httpSample}))
	mixed := readUpstreamCache(t, f, q)
	require.Equal(t, 6, mixed.Summary.Requests)
	require.Len(t, mixed.Rows, 4, "HTTP and WS origins remain separate even for the same credential, route and model")
	for _, row := range mixed.Rows {
		if row.Transport == "http" {
			require.Equal(t, "canonical_final_physical_http_attempt", row.Waits.Source)
			require.Equal(t, 5.0, *row.Waits.FirstByte.MeanMS)
			require.Equal(t, 30.0, *row.Waits.FirstText.MeanMS)
		}
	}
	for _, tc := range []struct {
		name, mutation            string
		eventSamples, textSamples int
	}{
		{"missing_ws", `detail #- '{latency,ws_turn}'`, 0, 0},
		{"future_version", `jsonb_set(detail,'{latency,version}','9')`, 0, 0},
		{"wrong_origin", `jsonb_set(detail,'{latency,ws_turn,source}','"http_upload"')`, 0, 0},
		{"no_mode", `detail-'ttft_mode'`, 0, 0},
		{"http_status", `jsonb_set(detail,'{status}','200')`, 0, 0},
		{"handshake_status", `jsonb_set(detail,'{attempts,0,status}','101')`, 0, 0},
		{"wrong_final", `jsonb_set(detail,'{final_response_attempt}','2')`, 0, 0},
		{"wrong_number", `jsonb_set(detail,'{latency,ws_turn,number}','2')`, 0, 0},
		{"string_number", `jsonb_set(detail,'{latency,ws_turn,number}','"1"')`, 0, 0},
		{"missing_number", `detail #- '{raw_upstream,attempt_number}'`, 0, 0},
		{"different_route", `jsonb_set(detail,'{attempts,0,route_fingerprint}',to_jsonb(repeat('e',64)))`, 0, 0},
		{"different_key", `jsonb_set(detail,'{attempts,0,upstream_credential,fingerprint}',to_jsonb(repeat('e',64)))`, 0, 0},
		{"different_account", `jsonb_set(detail,'{attempts,0,account_id}','0')`, 0, 0},
		{"wrong_kind", `jsonb_set(detail,'{attempts,0,kind}','"http"')`, 0, 0},
		{"cross_transport", `jsonb_set(jsonb_set(jsonb_set(detail,'{attempts,0,kind}','"http"'),'{attempts,0,upstream_credential,source}','"http_transport_handoff"'),'{latency,attempts}','[{"number":1,"post_upload_first_byte_wait":{"status":"measured","ms":5}}]')`, 0, 0},
		{"wrong_source", `jsonb_set(detail,'{attempts,0,upstream_credential,source}','"http_transport_handoff"')`, 0, 0},
		{"wrong_parent", `jsonb_set(detail,'{attempts,0,parent}','1')`, 0, 0},
		{"two_turns", `jsonb_set(detail,'{attempts}',(detail->'attempts')||(detail->'attempts'))`, 0, 0},
		{"truncated_trace", `jsonb_set(detail,'{truncated}','true')`, 0, 0},
		{"lost_event", `jsonb_set(detail,'{attempts,0,events,typed_content_truncated}','40')`, 0, 0},
		{"negative_text", `jsonb_set(detail,'{latency,ws_turn,first_text,ms}','-1')`, 1, 0},
		{"string_text", `jsonb_set(detail,'{latency,ws_turn,first_text,ms}','"10"')`, 1, 0},
		{"after_request", `jsonb_set(detail,'{latency,ws_turn,first_text,ms}','601')`, 1, 0},
		{"invalid_text", `jsonb_set(detail,'{latency,ws_turn,first_text,status}','"invalid_boundary"')`, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filter := q
			filter.Model = fmt.Sprintf("ws-%s", tc.name)
			log := write(filter.Model, a, b, 100)
			_, err := integrationDB.ExecContext(t.Context(), `UPDATE request_timing_details SET detail=`+tc.mutation+` WHERE usage_log_id=$1`, log.ID)
			require.NoError(t, err)
			var before, after string
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT detail::text FROM request_timing_details WHERE usage_log_id=$1`, log.ID).Scan(&before))
			result := readUpstreamCache(t, f, filter)
			require.Len(t, result.Rows, 1)
			require.Equal(t, 1, result.Summary.ObservedRequests, "missing waits never erases independent canonical cache counts")
			require.Equal(t, tc.eventSamples, result.Rows[0].Waits.FirstEvent.Samples)
			require.Equal(t, tc.textSamples, result.Rows[0].Waits.FirstText.Samples)
			require.Zero(t, result.Rows[0].Waits.FirstByte.Samples, "WS never borrows HTTP latency even from contradictory stored projections")
			require.Nil(t, result.Rows[0].Waits.FirstByte.MeanMS)
			require.Nil(t, result.Rows[0].Waits.FirstText.MeanMS)
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT detail::text FROM request_timing_details WHERE usage_log_id=$1`, log.ID).Scan(&after))
			require.Equal(t, before, after, "aggregates cannot backfill historical evidence")
		})
	}
}

type upstreamWaitMetrics struct {
	Samples        int
	Unknown        int      `json:"unknown_samples"`
	MeanMS         *float64 `json:"mean_ms"`
	Coverage       *float64
	LatestSampleAt *time.Time `json:"latest_sample_at"`
}

func TestUpstreamRawWaitsFinalScopeWeightedSamplesAndUnknowns(t *testing.T) {
	f := newChannelOperationsFixture(t)
	repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	now := time.Now().UTC()
	write := func(model, credential, route string, text float64) *service.UsageLog {
		log := &service.UsageLog{UserID: f.user, APIKeyID: f.key, AccountID: f.account,
			Model: "fixture", RequestedModel: model, RequestID: uuid.NewString(), CreatedAt: now}
		_, err := repo.Create(t.Context(), log)
		require.NoError(t, err)
		failedEnd, end := 90.0, 500.0
		s := requesttiming.Snapshot{Version: 1, TraceID: uuid.NewString(), TotalMS: 600, FinalResponseAttempt: 2,
			Status: 200, Terminal: "completed", Outcome: "success", Attempts: []requesttiming.Attempt{
				{Number: 1, Kind: "http", AccountID: f.outsider, Status: 502, EndMS: &failedEnd,
					RouteFingerprint: strings.Repeat("e", 64), UpstreamCredential: &requesttiming.UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: strings.Repeat("f", 64)}},
				{Number: 2, Kind: "http", AccountID: f.account, StartMS: 100, EndMS: &end, Status: 200, Terminal: "completed",
					RouteFingerprint: route, UpstreamCredential: &requesttiming.UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: credential},
					CacheUsage: &requesttiming.CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 50},
					Events:     map[string]float64{"headers_written": 120, "request_written": 150, "first_byte": 175, "first_sse_event_flush": 200, "first_reasoning": 250, "first_visible_text": text}},
			}}
		require.NoError(t, repo.writeTiming(t.Context(), timingWrite{requestID: log.RequestID, apiKeyID: f.key, data: s}))
		return log
	}
	q := service.UpstreamOperationsFilter{From: now.Add(-time.Hour), Until: now.Add(time.Minute), AccountID: f.account,
		ModelScope: "public", Dimension: "model", Sort: "requests", Direction: "desc", Page: 1, PageSize: 100}
	a, b, c, d := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
	write("shared", a, b, 350)
	write("shared", a, b, 150)
	missing := write("shared", a, b, 350)
	_, err := integrationDB.ExecContext(t.Context(), `UPDATE request_timing_details SET detail=detail #- '{raw_upstream,attempt_number}' WHERE usage_log_id=$1`, missing.ID)
	require.NoError(t, err)
	write("shared", c, b, 450)
	write("shared", a, d, 450)
	write("another-model", a, b, 450)
	q.Model = "shared"
	got := readUpstreamCache(t, f, q)
	require.Equal(t, 5, got.Summary.Requests, "physical retries do not inflate logical cache counts")
	require.Len(t, got.Rows, 3, "key and route changes remain separate")
	require.Equal(t, a, got.Rows[0].Credential)
	require.Equal(t, b, got.Rows[0].Route)
	waits := got.Rows[0].Waits
	require.Equal(t, 1, waits.Version)
	require.Equal(t, "canonical_final_physical_http_attempt", waits.Source)
	require.Equal(t, 2, waits.FirstText.Samples)
	require.Equal(t, 1, waits.FirstText.Unknown, "legacy identities are not retroactively guessed")
	require.NotNil(t, waits.FirstText.MeanMS)
	require.Equal(t, 100.0, *waits.FirstText.MeanMS, "means use samples, including measured zero")
	require.InDelta(t, 2.0/3.0, *waits.FirstText.Coverage, 1e-12)
	require.NotNil(t, waits.FirstText.LatestSampleAt)
	for _, v := range []upstreamWaitMetrics{waits.FirstByte, waits.FirstEvent, waits.FirstReasoning} {
		require.Equal(t, 2, v.Samples)
		require.Equal(t, 1, v.Unknown)
	}
	for _, tc := range []struct {
		name, mutation           string
		byteSamples, textSamples int
	}{
		{"missing_latency", `detail-'latency'`, 0, 0},
		{"future_latency", `jsonb_set(detail,'{latency,version}','9')`, 0, 0},
		{"missing_number", `detail #- '{raw_upstream,attempt_number}'`, 0, 0},
		{"string_number", `jsonb_set(detail,'{raw_upstream,attempt_number}','"2"')`, 0, 0},
		{"invalid_number", `jsonb_set(detail,'{raw_upstream,attempt_number}','33')`, 0, 0},
		{"truncated_trace", `jsonb_set(detail,'{truncated}','true')`, 0, 0},
		{"missing_attempts", `detail-'attempts'`, 0, 0},
		{"object_attempts", `jsonb_set(detail,'{attempts}','{}')`, 0, 0},
		{"oversized_attempts", `jsonb_set(detail,'{attempts}',(SELECT jsonb_agg(CASE WHEN seq.n>0 THEN detail->'attempts'->1 END) FROM generate_series(1,33) AS seq(n)))`, 0, 0},
		{"null_latency_attempts", `jsonb_set(detail,'{latency,attempts}','null')`, 0, 0},
		{"oversized_latency_attempts", `jsonb_set(detail,'{latency,attempts}',(SELECT jsonb_agg(CASE WHEN seq.n>0 THEN detail->'latency'->'attempts'->1 END) FROM generate_series(1,33) AS seq(n)))`, 0, 0},
		{"duplicate_physical", `jsonb_set(detail,'{attempts}',(detail->'attempts')||jsonb_build_array(detail->'attempts'->1))`, 0, 0},
		{"duplicate_latency", `jsonb_set(detail,'{latency,attempts}',(detail->'latency'->'attempts')||jsonb_build_array(detail->'latency'->'attempts'->1))`, 0, 0},
		{"different_route", `jsonb_set(detail,'{attempts,1,route_fingerprint}',to_jsonb(repeat('c',64)))`, 0, 0},
		{"different_credential", `jsonb_set(detail,'{attempts,1,upstream_credential,fingerprint}',to_jsonb(repeat('c',64)))`, 0, 0},
		{"different_account", `jsonb_set(detail,'{attempts,1,account_id}','0')`, 0, 0},
		{"different_kind", `jsonb_set(detail,'{attempts,1,kind}','"egress"')`, 0, 0},
		{"unconfirmed_credential", `jsonb_set(detail,'{attempts,1,upstream_credential,status}','"changed_credentials"')`, 0, 0},
		{"different_source", `jsonb_set(detail,'{attempts,1,upstream_credential,source}','"websocket_handshake_credentials"')`, 0, 0},
		{"different_number", `jsonb_set(detail,'{latency,attempts,1,number}','3')`, 0, 0},
		{"missing_total", `detail-'total_ms'`, 0, 0},
		{"negative_wait", `jsonb_set(detail,'{latency,attempts,1,post_upload_first_text_wait,ms}','-1')`, 1, 0},
		{"string_wait", `jsonb_set(detail,'{latency,attempts,1,post_upload_first_text_wait,ms}','"200"')`, 1, 0},
		{"after_request", `jsonb_set(detail,'{latency,attempts,1,post_upload_first_text_wait,ms}','601')`, 1, 0},
		{"invalid_status", `jsonb_set(detail,'{latency,attempts,1,post_upload_first_text_wait,status}','"invalid_boundary"')`, 1, 0},
		{"capture_incomplete", `jsonb_set(detail,'{attempts,1,events,typed_content_truncated}','400')`, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := write(tc.name, a, b, 350)
			_, err := integrationDB.ExecContext(t.Context(), `UPDATE request_timing_details SET detail=`+tc.mutation+` WHERE usage_log_id=$1`, log.ID)
			require.NoError(t, err)
			var before, after string
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT detail::text FROM request_timing_details WHERE usage_log_id=$1`, log.ID).Scan(&before))
			filter := q
			filter.Model = tc.name
			result := readUpstreamCache(t, f, filter)
			require.Len(t, result.Rows, 1)
			require.Equal(t, 1, result.Summary.ObservedRequests, "missing timing does not erase independent raw cache reports")
			v := result.Rows[0].Waits
			require.Equal(t, tc.byteSamples, v.FirstByte.Samples)
			require.Equal(t, tc.textSamples, v.FirstText.Samples)
			require.Equal(t, 1, v.FirstText.Unknown)
			require.Nil(t, v.FirstText.MeanMS, "unknown never becomes a zero wait")
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT detail::text FROM request_timing_details WHERE usage_log_id=$1`, log.ID).Scan(&after))
			require.Equal(t, before, after, "aggregation never backfills historical diagnostics")
		})
	}
}
