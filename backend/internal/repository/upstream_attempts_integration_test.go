//go:build integration

package repository

import (
	"encoding/json"
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
	Summary struct{ Attempts, Failed, Succeeded, Interrupted, Unknown int }
	Rows    []struct {
		AccountID     int64  `json:"account_id"`
		UpstreamModel string `json:"upstream_model"`
		Transport     string
		Attempts      int
		Waits         struct {
			Version        int
			Source         string
			FirstByte      upstreamWaitMetrics `json:"first_byte"`
			FirstEvent     upstreamWaitMetrics `json:"first_event"`
			FirstReasoning upstreamWaitMetrics `json:"first_reasoning"`
			FirstText      upstreamWaitMetrics `json:"first_text"`
		}
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
