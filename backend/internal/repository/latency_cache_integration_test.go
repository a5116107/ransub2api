//go:build integration

package repository

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/util/transportdiag"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type latencyCacheFixture struct {
	tx    *dbent.Tx
	repo  *usageLogRepository
	usage service.UsageLog
	trace requesttiming.Snapshot
}

func newLatencyCacheFixture(t *testing.T) latencyCacheFixture {
	t.Helper()
	tx := testEntTx(t)
	client := tx.Client()
	u := mustCreateUser(t, client, &service.User{Email: "cache-cohorts@synthetic.invalid"})
	k := mustCreateApiKey(t, client, &service.APIKey{UserID: u.ID, Key: "synthetic-cache-cohorts", Name: "synthetic"})
	a := mustCreateAccount(t, client, &service.Account{Name: "synthetic-cache-cohorts"})
	upstream, tier, effort, endpoint := "actual-model", "default", "high", "/v1/responses"
	end := 8000.0
	return latencyCacheFixture{tx: tx, repo: newUsageLogRepositoryWithSQL(client, tx),
		usage: service.UsageLog{UserID: u.ID, APIKeyID: k.ID, AccountID: a.ID, Model: "cache-fixture", UpstreamModel: &upstream,
			ServiceTier: &tier, ReasoningEffort: &effort, UpstreamEndpoint: &endpoint, Stream: true,
			InputTokens: 100, CreatedAt: time.Now().UTC().Add(-time.Minute)},
		trace: requesttiming.Snapshot{Version: 1, Status: 200, BodyBytes: 2 << 20, TotalMS: end, Attempts: []requesttiming.Attempt{
			{Kind: "http", Number: 1, AccountID: a.ID, Route: transportdiag.Route{Endpoint: "responses"}, RouteFingerprint: strings.Repeat("a", 64), ProxyID: 0, StartMS: 0,
				EndMS: &end, Status: 200, Terminal: "completed", CacheUsage: &requesttiming.CacheUsage{Status: "observed", InputTokens: 100},
				Events: map[string]float64{"headers_written": 10, "request_written": 20, "first_visible_text": 6020}},
		}},
	}
}

func TestLatencyCachePhysicalEndpointsStaySeparate(t *testing.T) {
	f := newLatencyCacheFixture(t)
	endpoints := []string{"responses", "responses_compact", "chat_completions", "messages", "gemini_generate_content", "gemini_stream_generate_content"}
	for _, endpoint := range endpoints {
		f.trace.Attempts[0].Route.Endpoint = endpoint
		f.insert(t)
	}
	for _, endpoint := range []string{"", "other", "future_endpoint", "/private/path?token=synthetic"} {
		f.trace.Attempts[0].Route.Endpoint = endpoint
		f.insert(t)
	}
	groups, err := f.repo.ReadLatencyReport(t.Context(), service.LatencyReportFilter{
		Start: f.usage.CreatedAt.Add(-time.Minute), End: time.Now().UTC(), UserID: f.usage.UserID,
	})
	require.NoError(t, err)
	require.Len(t, groups, len(endpoints)+1, "the same billed path, route, model and wire source do not prove the same physical HTTP endpoint")
	seen := make(map[string]bool)
	for _, g := range groups {
		require.Equal(t, "/v1/responses", g.UpstreamEndpoint, "retain the billed endpoint independently")
		require.False(t, seen[g.TransportEndpoint], "duplicate physical endpoint cohort")
		seen[g.TransportEndpoint] = true
		if g.TransportEndpoint == "unknown" {
			require.EqualValues(t, 4, g.Requests, "unclassified paths must not be exposed or inferred from the billed endpoint")
		} else {
			require.EqualValues(t, 1, g.Requests)
		}
	}
	for _, endpoint := range append(endpoints, "unknown") {
		require.True(t, seen[endpoint], endpoint)
	}
}

func TestLatencyCachePhysicalEndpointRequiresFinalHTTPProof(t *testing.T) {
	for _, mode := range []string{"observed", "missing", "unknown", "future_version", "previous_attempt_only", "parent_only", "wrong_account", "ws_mode", "ws_kind", "ambiguous_trace", "missing_trace"} {
		t.Run(mode, func(t *testing.T) {
			f := newLatencyCacheFixture(t)
			expected := "unknown"
			switch mode {
			case "observed":
				expected = "responses"
			case "missing":
				f.trace.Attempts[0].Route.Endpoint = ""
			case "unknown":
				f.trace.Attempts[0].Route.Endpoint = "other"
			case "future_version":
				f.trace.Version = 2
			case "previous_attempt_only", "parent_only":
				previous := f.trace.Attempts[0]
				if mode == "parent_only" {
					previous.Kind = "egress"
				}
				f.trace.Attempts[0].Number, f.trace.Attempts[0].Route.Endpoint = 2, ""
				f.trace.Attempts = append([]requesttiming.Attempt{previous}, f.trace.Attempts...)
			case "wrong_account":
				f.trace.Attempts[0].AccountID++
			case "ws_mode":
				f.trace.TTFTMode = "ws_turn"
			case "ws_kind":
				f.trace.Attempts[0].Kind = "ws_turn"
			}
			id := f.insert(t)
			if mode == "ambiguous_trace" {
				raw, err := json.Marshal(f.trace)
				require.NoError(t, err)
				_, err = f.tx.ExecContext(t.Context(), `INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,$3::jsonb)`, id, uuid.NewString(), string(raw))
				require.NoError(t, err)
			}
			if mode == "missing_trace" {
				_, err := f.tx.ExecContext(t.Context(), `DELETE FROM request_timing_details WHERE usage_log_id=$1`, id)
				require.NoError(t, err)
			}
			groups, err := f.repo.ReadLatencyReport(t.Context(), service.LatencyReportFilter{
				Start: f.usage.CreatedAt.Add(-time.Minute), End: time.Now().UTC(), UserID: f.usage.UserID,
			})
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Equal(t, expected, groups[0].TransportEndpoint)
			require.Equal(t, "/v1/responses", groups[0].UpstreamEndpoint)
		})
	}
}

func TestLatencyCachePhysicalEndpointCapturedThroughHTTP(t *testing.T) {
	f := newLatencyCacheFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)
	paths := map[string]string{
		"/v1/responses": "responses", "/bridge/responses/compact": "responses_compact",
		"/bridge/chat/completions": "chat_completions", "/bridge/messages": "messages",
		"/v1beta/models/model-a:generateContent": "gemini_generate_content",
		"/v1internal:streamGenerateContent":      "gemini_stream_generate_content",
	}
	for path := range paths {
		c := requesttiming.New(time.Now(), 2)
		req, err := http.NewRequestWithContext(requesttiming.With(t.Context(), c), http.MethodPost,
			upstream.URL+path+"?token=endpoint-secret-marker", strings.NewReader("{}"))
		require.NoError(t, err)
		req, logical := requesttiming.StartAttempt(req, f.usage.AccountID, 0)
		req, physical := requesttiming.StartTransport(req)
		resp, err := upstream.Client().Do(req)
		physical.Response(resp, err)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		logical.Response(nil, nil)
		c.Finish(http.StatusOK, false)
		captured := false
		c.WhenFinished(func(s requesttiming.Snapshot) { f.trace, captured = s, true })
		require.True(t, captured)
		raw, err := json.Marshal(f.trace)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "endpoint-secret-marker")
		require.NotContains(t, string(raw), "model-a")
		f.insert(t)
	}
	groups, err := f.repo.ReadLatencyReport(t.Context(), service.LatencyReportFilter{
		Start: f.usage.CreatedAt.Add(-time.Minute), End: time.Now().UTC(), UserID: f.usage.UserID,
	})
	require.NoError(t, err)
	require.Len(t, groups, len(paths))
	seen := make(map[string]bool)
	for _, g := range groups {
		require.Equal(t, "/v1/responses", g.UpstreamEndpoint)
		require.EqualValues(t, 1, g.Requests)
		seen[g.TransportEndpoint] = true
	}
	for _, endpoint := range paths {
		require.True(t, seen[endpoint], endpoint)
	}
}

func TestLatencyCachePhysicalEndpointHistoricalComparisonAndRecovery(t *testing.T) {
	for _, endpoint := range []string{"messages", "unknown", "responses"} {
		t.Run(endpoint, func(t *testing.T) {
			f := newLatencyCacheFixture(t)
			f.trace.Terminal, f.trace.Outcome = "completed", "success"
			var warmIDs []int64
			for _, read := range []int{0, 95} {
				for range 5 {
					f.usage.InputTokens, f.usage.CacheReadTokens = 100-read, read
					f.trace.Attempts[0].CacheUsage.ReadTokens = int64(read)
					if read > 0 {
						f.trace.Attempts[0].Route.Endpoint = endpoint
					}
					id := f.insert(t)
					if read > 0 {
						warmIDs = append(warmIDs, id)
					}
				}
			}
			s := service.NewUsageService(f.repo, nil, nil, nil)
			filter := service.LatencyReportFilter{Start: f.usage.CreatedAt.Add(-time.Minute), End: time.Now().UTC(), UserID: f.usage.UserID}
			report, err := s.LatencyReport(t.Context(), filter)
			require.NoError(t, err)
			require.Len(t, report.Groups, 2)
			for _, g := range report.Groups {
				expected := "insufficient_samples"
				if endpoint == "responses" {
					expected = "historical_association"
				} else if g.TransportEndpoint == "unknown" {
					expected = "unknown_scope"
				}
				require.Equal(t, expected, g.ColdStart.Status)
				if endpoint != "responses" {
					require.Nil(t, g.ColdStart.DeltaMS)
				}
			}
			for _, id := range warmIDs {
				_, err := f.tx.ExecContext(t.Context(), `UPDATE request_timing_details SET detail=jsonb_set(detail,'{attempts,0,route,endpoint}','"responses"'::jsonb) WHERE usage_log_id=$1`, id)
				require.NoError(t, err)
			}
			report, err = s.LatencyReport(t.Context(), filter)
			require.NoError(t, err)
			require.Len(t, report.Groups, 2)
			for _, g := range report.Groups {
				require.Equal(t, "responses", g.TransportEndpoint)
				require.Equal(t, "historical_association", g.ColdStart.Status)
				require.EqualValues(t, 5, g.ColdStart.ColdSamples)
				require.EqualValues(t, 5, g.ColdStart.WarmSamples)
				require.NotNil(t, g.ColdStart.DeltaMS)
				require.Zero(t, *g.ColdStart.DeltaMS)
			}
		})
	}
}

func TestLatencyOutputRatePersistsAndRejectsAmbiguousTraces(t *testing.T) {
	for _, mode := range []string{"complete", "chat_completions", "messages", "gemini", "wrong_protocol", "not_final", "multiple_candidates", "missing_usage", "downstream_error", "retry_without_usage", "duplicate_trace"} {
		t.Run(mode, func(t *testing.T) {
			f := newLatencyCacheFixture(t)
			f.usage.OutputTokens = 120
			f.trace.Terminal = "completed"
			f.trace.Attempts[0].OutputUsage = &requesttiming.OutputUsage{
				OutputTokens: 120, ReasoningTokens: 20, Status: "observed", ContentStatus: "text_and_reasoning",
			}
			f.trace.Attempts[0].TextStream = &requesttiming.TextStreamTiming{
				Chunks: 3, FirstMS: 6020, LastMS: 7020, MaxGapMS: 500, Terminal: "completed", TerminalMS: 7520,
			}
			switch mode {
			case "chat_completions", "messages", "gemini":
				f.trace.Attempts[0].OutputUsage.Protocol, f.trace.Attempts[0].OutputUsage.Final = mode, true
				f.trace.Attempts[0].OutputUsage.ContentStatus = mode + "_text_and_reasoning"
				f.trace.Attempts[0].TextStream.Protocol = mode
			case "wrong_protocol":
				f.trace.Attempts[0].OutputUsage.Protocol = "gemini"
			case "not_final":
				f.trace.Attempts[0].OutputUsage.Protocol = "messages"
				f.trace.Attempts[0].TextStream.Protocol = "messages"
			case "multiple_candidates":
				f.trace.Attempts[0].Events["typed_output_multiple_candidates"] = 1
			case "missing_usage":
				f.trace.Attempts[0].OutputUsage = nil
			case "downstream_error":
				f.trace.DownstreamError = true
			case "retry_without_usage":
				f.trace.Attempts = append(f.trace.Attempts, requesttiming.Attempt{Kind: "http", AccountID: f.usage.AccountID})
			}
			rate := requesttiming.DeriveContentObservation(f.trace).OutputRate
			f.trace.OutputRate = &rate
			id := f.insert(t)
			if mode == "duplicate_trace" {
				_, err := f.tx.ExecContext(t.Context(), `INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,'{}'::jsonb)`, id, uuid.NewString())
				require.NoError(t, err)
			}
			items, err := f.repo.ReadUsageContentTimings(t.Context(), []int64{id})
			require.NoError(t, err)
			observed := items[id].OutputRate
			switch mode {
			case "complete", "chat_completions", "messages", "gemini":
				traces, err := f.repo.RequestTimings(t.Context(), id)
				require.NoError(t, err)
				require.Len(t, traces, 1)
				var saved requesttiming.Snapshot
				require.NoError(t, json.Unmarshal(traces[0], &saved))
				require.NotNil(t, saved.OutputRate)
				require.Equal(t, observed, *saved.OutputRate)
				require.Equal(t, "estimated", observed.Status)
				require.EqualValues(t, 100, *observed.NonReasoningTokens)
				require.InDelta(t, 100/1.5, *observed.TokensPerSecond, 1e-9)
				if mode != "complete" {
					require.Equal(t, "canonical_"+mode+"_usage_and_text_events", observed.Source)
					require.Equal(t, "final_physical_http_"+mode+"_text_at_parser", observed.Scope)
				}
			case "wrong_protocol":
				require.Equal(t, "invalid_protocol", observed.Status)
				require.Nil(t, observed.TokensPerSecond)
			case "not_final":
				require.Equal(t, "not_reported", observed.Status)
				require.Nil(t, observed.TokensPerSecond)
			case "multiple_candidates":
				require.Equal(t, "unsupported_output", observed.Status)
				require.Nil(t, observed.TokensPerSecond)
			case "downstream_error":
				require.Equal(t, "partial", observed.Status)
				require.Nil(t, observed.TokensPerSecond)
			default:
				require.Equal(t, "not_collected", observed.Status)
				require.Nil(t, observed.TokensPerSecond)
			}
		})
	}
}

func (f latencyCacheFixture) insert(t *testing.T) int64 {
	t.Helper()
	_, err := f.repo.Create(t.Context(), &f.usage)
	require.NoError(t, err)
	raw, err := json.Marshal(f.trace)
	require.NoError(t, err)
	_, err = f.tx.ExecContext(t.Context(), `INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,$3::jsonb)`, f.usage.ID, uuid.NewString(), string(raw))
	require.NoError(t, err)
	return f.usage.ID
}

func TestCacheMigrationPersistsWithoutInventingSessionLoss(t *testing.T) {
	for _, mode := range []string{"changed", "same_route", "same_account_new_route", "unknown_old_route", "downstream_failed", "two_traces"} {
		t.Run(mode, func(t *testing.T) {
			f := newLatencyCacheFixture(t)
			old := mustCreateAccount(t, f.tx.Client(), &service.Account{Name: "previous-cache-route"})
			f.usage.InputTokens, f.usage.CacheReadTokens, f.usage.CacheCreationTokens = 80, 900, 20
			f.trace.Terminal, f.trace.Outcome = "completed", "success"
			last := f.trace.Attempts[0]
			last.Number, last.StartMS, last.RouteFingerprint = 2, 1000, strings.Repeat("b", 64)
			last.CacheUsage = &requesttiming.CacheUsage{Status: "observed", InputTokens: 1000, ReadTokens: 900, WriteTokens: 20, WriteReported: true}
			firstEnd := 500.0
			first := requesttiming.Attempt{Kind: "http", Number: 1, AccountID: old.ID, RouteFingerprint: strings.Repeat("a", 64), EndMS: &firstEnd, Status: 502, Error: "transport_error"}
			expected := "bounded_attribution_unknown"
			switch mode {
			case "same_route":
				first.AccountID, first.RouteFingerprint = last.AccountID, last.RouteFingerprint
				expected = "no_observed_route_change"
			case "same_account_new_route":
				first.AccountID = last.AccountID
			case "unknown_old_route":
				first.RouteFingerprint = ""
				expected = "unknown_route"
			case "downstream_failed":
				f.trace.DownstreamError = true
				expected = "cache_usage_unavailable"
			}
			f.trace.Attempts = []requesttiming.Attempt{first, last}
			derived := requesttiming.DeriveLatency(f.trace)
			f.trace.Latency = &derived
			id := f.insert(t)
			if mode == "two_traces" {
				_, err := f.tx.ExecContext(t.Context(), `INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,'{}'::jsonb)`, id, uuid.NewString())
				require.NoError(t, err)
			}
			traces, err := f.repo.RequestTimings(t.Context(), id)
			require.NoError(t, err)
			observed, missing := 0, 0
			for _, raw := range traces {
				var saved requesttiming.Snapshot
				require.NoError(t, json.Unmarshal(raw, &saved))
				if saved.Latency == nil {
					missing++
					continue
				}
				observed++
				v := saved.Latency.CacheMigration
				require.Equal(t, expected, v.Status)
				if expected == "bounded_attribution_unknown" {
					require.EqualValues(t, 100, *v.NonReadInputTokens)
					require.EqualValues(t, 0, *v.ExtraColdInputMinTokens)
					require.EqualValues(t, 100, *v.ExtraColdInputMaxTokens)
				} else {
					require.Nil(t, v.ExtraColdInputMaxTokens)
				}
			}
			require.Equal(t, 1, observed)
			if mode == "two_traces" {
				require.Equal(t, 1, missing, "an old trace must not borrow another trace's observation")
			}
		})
	}
}

func TestLatencyCacheCanonicalCountersAndBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, bucket, source string
		edit                 func(*latencyCacheFixture)
	}{
		{"explicit_miss", "0%", "canonical_responses_terminal_usage", func(*latencyCacheFixture) {}},
		{"explicit_hit", "95%+", "canonical_responses_terminal_usage", func(f *latencyCacheFixture) {
			f.usage.InputTokens, f.usage.CacheReadTokens, f.trace.Attempts[0].CacheUsage.ReadTokens = 5, 95, 95
		}},
		{"writes_in_denominator", "<80%", "canonical_responses_terminal_usage", func(f *latencyCacheFixture) {
			f.usage.InputTokens, f.usage.CacheReadTokens, f.usage.CacheCreationTokens = 1, 99, 1000
			f.trace.Attempts[0].CacheUsage = &requesttiming.CacheUsage{Status: "observed", InputTokens: 1100, ReadTokens: 99, WriteTokens: 1000, WriteReported: true}
		}},
		{"missing_read", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].CacheUsage.Status = "not_reported" }},
		{"legacy_zero_is_unknown", "unknown", "none", func(f *latencyCacheFixture) { f.trace.Attempts[0].CacheUsage = nil }},
		{"ws_legacy_positive_is_unknown", "unknown", "none", func(f *latencyCacheFixture) {
			f.usage.InputTokens, f.usage.CacheReadTokens = 5, 95
			f.trace.TTFTMode = "ws_turn"
			f.trace.Attempts[0].CacheUsage = nil
		}},
		{"ws_kind_legacy_positive_is_unknown", "unknown", "none", func(f *latencyCacheFixture) {
			f.usage.InputTokens, f.usage.CacheReadTokens = 5, 95
			f.trace.Attempts[0].Kind, f.trace.Attempts[0].CacheUsage = "ws_turn", nil
		}},
		{"counter_mismatch", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.usage.InputTokens++ }},
		{"negative_counter", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].CacheUsage.ReadTokens = -1 }},
		{"excess_read", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].CacheUsage.ReadTokens = 101 }},
		{"negative_write", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) {
			f.trace.Attempts[0].CacheUsage.WriteReported = true
			f.trace.Attempts[0].CacheUsage.WriteTokens = -1
		}},
		{"wrong_account", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].AccountID++ }},
		{"wrong_attempt_kind", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].Kind = "egress" }},
		{"failed_terminal", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].Terminal = "failed" }},
		{"http_error", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].Status = 502 }},
		{"request_error", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Status = 502 }},
		{"failed_outcome", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Outcome = "failed" }},
		{"failed_request_terminal", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Terminal = "incomplete" }},
		{"ws_mode_with_http_status", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.TTFTMode = "ws_turn" }},
		{"invalid_stream", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) {
			f.trace.Attempts[0].TextStream = &requesttiming.TextStreamTiming{Invalid: true}
		}},
		{"truncated_stream", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) {
			f.trace.Attempts[0].TextStream = &requesttiming.TextStreamTiming{Truncated: true}
		}},
		{"canceled", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Canceled = true }},
		{"disconnected", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.ClientDisconnect = true }},
		{"downstream_error", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.DownstreamError = true }},
		{"capture_gap", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].Events["typed_content_truncated"] = 1 }},
		{"truncated", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Truncated = true }},
		{"transport_error", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].Error = "timeout" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLatencyCacheFixture(t)
			tc.edit(&f)
			id := f.insert(t)
			groups, err := f.repo.ReadLatencyReport(t.Context(), service.LatencyReportFilter{Start: f.usage.CreatedAt.Add(-time.Minute), End: time.Now().UTC(), UserID: f.usage.UserID})
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Equal(t, tc.bucket, groups[0].CacheBucket)
			require.Equal(t, tc.source, groups[0].CacheSource)
			switch tc.name {
			case "failed_outcome", "failed_request_terminal", "ws_mode_with_http_status", "invalid_stream", "truncated_stream":
				items, err := f.repo.ReadUsageContentTimings(t.Context(), []int64{id})
				require.NoError(t, err)
				require.NotEqual(t, "observed", items[id].Cache.Status, "the detail projection and HTTP cohort must both exclude unhealthy evidence")
				require.Nil(t, items[id].Cache.ReadRatio)
			}
		})
	}
}

func TestLatencyCacheRecentWindowDoesNotBorrowOldSamples(t *testing.T) {
	f := newLatencyCacheFixture(t)
	now := time.Now().UTC()
	for _, read := range []int{0, 95} {
		for i := range 6 {
			f.usage.InputTokens, f.usage.CacheReadTokens = 100-read, read
			f.trace.Attempts[0].CacheUsage.ReadTokens = int64(read)
			f.usage.CreatedAt = now.Add(-time.Minute)
			if i < 5 {
				f.usage.CreatedAt = now.Add(-30 * time.Minute)
			}
			f.insert(t)
		}
	}
	groups, err := f.repo.ReadLatencyReport(t.Context(), service.LatencyReportFilter{Start: now.Add(-time.Hour), End: now, UserID: f.usage.UserID})
	require.NoError(t, err)
	require.Len(t, groups, 2)
	for _, g := range groups {
		require.EqualValues(t, 6, g.Requests, "the report retains the requested historical population")
		require.EqualValues(t, 1, g.TextWaitSamples, "one recent sample must not make five old samples look fresh")
		require.Equal(t, now.Add(-time.Minute).Unix(), g.TextWaitLatest.Unix())
	}
}

func TestLatencyCacheNativeProtocolsStaySeparateAndConserveBuckets(t *testing.T) {
	for _, protocol := range []string{"messages", "gemini", "chat_completions"} {
		for _, mode := range []string{"observed", "billing_mismatch", "wrong_protocol", "missing_presence", "forged_write", "bad_uncached", "failed", "capture_gap", "ambiguous_trace"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				f := newLatencyCacheFixture(t)
				f.usage.InputTokens, f.usage.CacheReadTokens = 5, 95
				a := &f.trace.Attempts[0]
				a.CacheUsage = &requesttiming.CacheUsage{Protocol: protocol, Status: "observed", InputTokens: 100, ReadTokens: 95}
				a.TextStream = &requesttiming.TextStreamTiming{Protocol: protocol, Terminal: "completed"}
				source := "canonical_gemini_usage_metadata"
				if protocol == "messages" {
					source = "canonical_messages_cumulative_usage"
					a.CacheUsage.UncachedTokens, a.CacheUsage.InputReported, a.CacheUsage.ReadReported, a.CacheUsage.WriteReported = 5, true, true, true
				}
				if protocol == "chat_completions" {
					source = "canonical_chat_completions_usage"
				}
				switch mode {
				case "billing_mismatch":
					f.usage.CacheReadTokens--
				case "wrong_protocol":
					a.TextStream.Protocol = "responses"
				case "missing_presence":
					if protocol != "messages" {
						a.CacheUsage.Status = "not_reported"
					} else {
						a.CacheUsage.ReadReported = false
					}
				case "forged_write":
					a.CacheUsage.WriteTokens, a.CacheUsage.WriteReported = 1, true
					if protocol == "chat_completions" {
						a.CacheUsage.WriteTokens = 6 // Read + write must not exceed total input.
					}
				case "bad_uncached":
					if protocol != "messages" {
						a.CacheUsage.Protocol = "future_protocol"
					} else {
						a.CacheUsage.UncachedTokens++
					}
				case "failed":
					f.trace.Outcome = "error"
				case "capture_gap":
					a.Events["typed_content_truncated"] = 1
				}
				id := f.insert(t)
				if mode == "ambiguous_trace" {
					_, err := f.tx.ExecContext(t.Context(), `INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,'{}'::jsonb)`, id, uuid.NewString())
					require.NoError(t, err)
				}
				groups, err := f.repo.ReadLatencyReport(t.Context(), service.LatencyReportFilter{Start: f.usage.CreatedAt.Add(-time.Minute), End: time.Now().UTC(), UserID: f.usage.UserID})
				require.NoError(t, err)
				items, err := f.repo.ReadUsageContentTimings(t.Context(), []int64{id})
				require.NoError(t, err)
				if mode == "observed" {
					require.Len(t, groups, 1)
					require.Equal(t, source, groups[0].CacheSource)
					require.Equal(t, "95%+", groups[0].CacheBucket)
					require.Equal(t, "observed", items[id].Cache.Status)
					require.Equal(t, source, items[id].Cache.Source)
					require.InDelta(t, 0.95, *items[id].Cache.ReadRatio, 1e-9)
				} else {
					for _, g := range groups {
						require.Equal(t, "unknown", g.CacheBucket)
						require.NotEqual(t, source, g.CacheSource)
					}
					if mode != "billing_mismatch" {
						require.NotEqual(t, "observed", items[id].Cache.Status)
						require.Nil(t, items[id].Cache.ReadRatio)
					}
				}
			})
		}
	}
	f := newLatencyCacheFixture(t)
	for _, protocol := range []string{"responses", "messages", "gemini"} {
		a := &f.trace.Attempts[0]
		a.CacheUsage = &requesttiming.CacheUsage{Protocol: protocol, Status: "observed", InputTokens: 100, ReadTokens: 95}
		a.TextStream = &requesttiming.TextStreamTiming{Protocol: protocol, Terminal: "completed"}
		if protocol == "messages" {
			a.CacheUsage.UncachedTokens, a.CacheUsage.InputReported, a.CacheUsage.ReadReported, a.CacheUsage.WriteReported = 5, true, true, true
		}
		f.usage.InputTokens, f.usage.CacheReadTokens = 5, 95
		f.insert(t)
	}
	groups, err := f.repo.ReadLatencyReport(t.Context(), service.LatencyReportFilter{Start: f.usage.CreatedAt.Add(-time.Minute), End: time.Now().UTC(), UserID: f.usage.UserID})
	require.NoError(t, err)
	require.Len(t, groups, 3, "protocol sources cannot share a distribution even with identical endpoint/model/route")
	for _, g := range groups {
		require.EqualValues(t, 1, g.Requests)
	}
}

func TestLatencyCacheCohortsKeepRouteModelTierAndProtocolSeparate(t *testing.T) {
	f := newLatencyCacheFixture(t)
	now := time.Now().UTC()
	f.insert(t)
	for _, edit := range []func(*latencyCacheFixture){
		func(f *latencyCacheFixture) { v := "another-model"; f.usage.UpstreamModel = &v },
		func(f *latencyCacheFixture) { v := "priority"; f.usage.ServiceTier = &v },
		func(f *latencyCacheFixture) { v := "/v1/messages"; f.usage.UpstreamEndpoint = &v },
		func(f *latencyCacheFixture) { f.trace.Attempts[0].RouteFingerprint = strings.Repeat("b", 64) },
		func(f *latencyCacheFixture) { f.trace.Attempts[0].ProxyID = 9 },
		func(f *latencyCacheFixture) { f.usage.Stream = false },
	} {
		copy := f
		copy.trace.Attempts = append([]requesttiming.Attempt(nil), f.trace.Attempts...)
		edit(&copy)
		copy.insert(t)
	}
	groups, err := f.repo.ReadLatencyReport(t.Context(), service.LatencyReportFilter{Start: now.Add(-time.Hour), End: now, UserID: f.usage.UserID})
	require.NoError(t, err)
	require.Len(t, groups, 7)
	for _, g := range groups {
		require.EqualValues(t, 1, g.Requests)
	}
}

func TestLatencyCacheOnlyFinalAttemptCanOwnCacheEvidence(t *testing.T) {
	f := newLatencyCacheFixture(t)
	previous := f.trace.Attempts[0]
	previous.AccountID++
	previous.CacheUsage = &requesttiming.CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 95}
	f.trace.Attempts = append([]requesttiming.Attempt{previous}, f.trace.Attempts...)
	f.trace.Attempts[1].Number = 2
	id := f.insert(t)
	filter := service.LatencyReportFilter{Start: f.usage.CreatedAt.Add(-time.Minute), End: time.Now().UTC(), UserID: f.usage.UserID}
	groups, err := f.repo.ReadLatencyReport(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, "0%", groups[0].CacheBucket, "a previous account's hit must not be assigned to the final request")
	require.EqualValues(t, 1, groups[0].TextWaitSamples)
	raw, err := json.Marshal(f.trace)
	require.NoError(t, err)
	_, err = f.tx.ExecContext(t.Context(), `INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,$3::jsonb)`, id, uuid.NewString(), string(raw))
	require.NoError(t, err)
	groups, err = f.repo.ReadLatencyReport(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.EqualValues(t, 1, groups[0].Requests)
	require.EqualValues(t, 2, groups[0].Traces)
	require.Equal(t, "unknown", groups[0].CacheBucket, "multiple traces cannot independently claim one billed usage record")
	require.Equal(t, "unverified_terminal_usage", groups[0].CacheSource)
}
