//go:build integration

package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

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
