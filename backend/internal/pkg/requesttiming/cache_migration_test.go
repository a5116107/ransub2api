package requesttiming

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func cacheMigrationSnapshot() Snapshot {
	first := latencyAttempt(1, 7, 0, 200)
	first.RouteFingerprint = strings.Repeat("a", 64)
	first.Status, first.Error = 502, "transport_error"
	last := latencyAttempt(2, 8, 220, 1000)
	last.RouteFingerprint = strings.Repeat("b", 64)
	last.Status, last.Terminal = 200, "completed"
	last.CacheUsage = &CacheUsage{InputTokens: 1000, ReadTokens: 900, WriteTokens: 20, WriteReported: true, Status: "observed"}
	return Snapshot{Version: 1, TotalMS: 1000, Status: 200, Terminal: "completed", Outcome: "success", Attempts: []Attempt{first, last}}
}

func TestCacheMigrationScopeAndFailureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		edit         func(*Snapshot)
	}{
		{"account", "bounded_attribution_unknown", func(*Snapshot) {}},
		{"same_account_credentials", "bounded_attribution_unknown", func(s *Snapshot) { s.Attempts[1].AccountID = 7 }},
		{"same_account_proxy", "bounded_attribution_unknown", func(s *Snapshot) {
			s.Attempts[1].AccountID, s.Attempts[1].RouteFingerprint, s.Attempts[1].ProxyID = 7, s.Attempts[0].RouteFingerprint, 3
		}},
		{"same_route", "no_observed_route_change", func(s *Snapshot) {
			s.Attempts[1].AccountID, s.Attempts[1].RouteFingerprint = 7, s.Attempts[0].RouteFingerprint
		}},
		{"one_attempt_is_not_a_session", "no_observed_route_change", func(s *Snapshot) { s.Attempts = s.Attempts[1:] }},
		{"unknown_old_route", "unknown_route", func(s *Snapshot) { s.Attempts[0].RouteFingerprint = "" }},
		{"unknown_final_route", "unknown_route", func(s *Snapshot) { s.Attempts[1].RouteFingerprint = "" }},
		{"zero_digest", "unknown_route", func(s *Snapshot) { s.Attempts[0].RouteFingerprint = strings.Repeat("0", 64) }},
		{"invalid_digest", "unknown_route", func(s *Snapshot) { s.Attempts[0].RouteFingerprint = "unverified" }},
		{"noncanonical_digest", "unknown_route", func(s *Snapshot) { s.Attempts[0].RouteFingerprint = strings.Repeat("A", 64) }},
		{"negative_proxy", "unknown_route", func(s *Snapshot) { s.Attempts[0].ProxyID = -1 }},
		{"unknown_account", "partial", func(s *Snapshot) { s.Attempts[0].AccountID = 0 }},
		{"truncated", "partial", func(s *Snapshot) { s.Truncated = true }},
		{"missing_physical_send", "partial", func(s *Snapshot) {
			s.Attempts = append([]Attempt{{Kind: "egress", Number: 10, AccountID: 3}}, s.Attempts...)
		}},
		{"unobserved_final_send", "not_collected", func(s *Snapshot) { s.Attempts = append(s.Attempts, Attempt{Kind: "egress"}) }},
		{"missing_usage", "cache_usage_unavailable", func(s *Snapshot) { s.Attempts[1].CacheUsage = nil }},
		{"missing_read", "cache_usage_unavailable", func(s *Snapshot) { s.Attempts[1].CacheUsage.Status = "not_reported" }},
		{"invalid_read", "cache_usage_unavailable", func(s *Snapshot) { s.Attempts[1].CacheUsage.ReadTokens = 1001 }},
		{"unsafe_input", "cache_usage_unavailable", func(s *Snapshot) { s.Attempts[1].CacheUsage.InputTokens = 1 << 53 }},
		{"write_failure", "cache_usage_unavailable", func(s *Snapshot) { s.DownstreamError = true }},
		{"cancelled", "cache_usage_unavailable", func(s *Snapshot) { s.Canceled = true }},
		{"disconnect", "cache_usage_unavailable", func(s *Snapshot) { s.ClientDisconnect = true }},
		{"business_failure", "cache_usage_unavailable", func(s *Snapshot) { s.Outcome = "failed" }},
		{"cache_capture_gap", "cache_usage_unavailable", func(s *Snapshot) { s.Attempts[1].Events["typed_content_truncated"] = 0 }},
		{"wrong_protocol", "cache_usage_unavailable", func(s *Snapshot) { s.Attempts[1].CacheUsage.Protocol = "gemini" }},
		{"overlap", "partial", func(s *Snapshot) { s.Attempts[1].StartMS = 100 }},
		{"missing_end", "invalid_boundary", func(s *Snapshot) { s.Attempts[1].EndMS = nil }},
		{"negative_start", "invalid_boundary", func(s *Snapshot) { s.Attempts[0].StartMS = -1 }},
		{"nan_start", "invalid_boundary", func(s *Snapshot) { s.Attempts[0].StartMS = math.NaN() }},
		{"nan_total", "invalid_boundary", func(s *Snapshot) { s.TotalMS = math.NaN() }},
		{"after_request", "invalid_boundary", func(s *Snapshot) { s.TotalMS = 500 }},
		{"duplicate_number", "invalid_boundary", func(s *Snapshot) { s.Attempts[1].Number = 1 }},
		{"missing_number", "invalid_boundary", func(s *Snapshot) { s.Attempts[0].Number = 0 }},
		{"websocket", "unsupported_transport", func(s *Snapshot) { s.TTFTMode = "ws_turn" }},
		{"mixed_transport", "unsupported_transport", func(s *Snapshot) {
			s.Attempts = append([]Attempt{{Kind: "ws_turn", Number: 3}}, s.Attempts...)
		}},
		{"unknown_version", "not_collected", func(s *Snapshot) { s.Version = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := cacheMigrationSnapshot()
			tc.edit(&s)
			v := DeriveLatency(s).CacheMigration
			require.Equal(t, tc.status, v.Status)
			require.Equal(t, "final_physical_http_input_after_request_route_transitions", v.Scope)
			require.Equal(t, "zero_to_final_non_read_input_not_causal", v.Method)
			if tc.status == "bounded_attribution_unknown" {
				require.Equal(t, 1, v.RouteSwitches)
				require.EqualValues(t, 0, *v.ExtraColdInputMinTokens)
				require.EqualValues(t, 100, *v.ExtraColdInputMaxTokens)
			} else {
				require.Nil(t, v.ExtraColdInputMinTokens)
				require.Nil(t, v.ExtraColdInputMaxTokens, "unknown must not become a zero-loss promise")
			}
		})
	}
}

func TestCacheMigrationCanonicalProtocolsAndMeasuredZero(t *testing.T) {
	for _, protocol := range []string{"responses", "chat_completions", "messages", "gemini"} {
		for _, read := range []int64{0, 900, 1000} {
			t.Run(protocol+"/read_"+strconv.FormatInt(read, 10), func(t *testing.T) {
				s := cacheMigrationSnapshot()
				a := &s.Attempts[1]
				a.CacheUsage = &CacheUsage{Status: "observed", InputTokens: 1000, ReadTokens: read}
				if protocol != "responses" {
					a.CacheUsage.Protocol = protocol
					a.TextStream = &TextStreamTiming{Protocol: protocol}
				}
				if protocol == "messages" {
					a.CacheUsage.InputReported, a.CacheUsage.ReadReported, a.CacheUsage.WriteReported = true, true, true
					a.CacheUsage.UncachedTokens = 1000 - read
				}
				v := DeriveLatency(s).CacheMigration
				require.Equal(t, "bounded_attribution_unknown", v.Status)
				require.Equal(t, "observed", v.CacheStatus)
				require.EqualValues(t, 1000-read, *v.NonReadInputTokens)
				require.EqualValues(t, 1000-read, *v.ExtraColdInputMaxTokens)
				*v.NonReadInputTokens = 123
				require.EqualValues(t, 1000-read, *DeriveLatency(s).CacheMigration.NonReadInputTokens, "a caller cannot modify raw counters through the projection")
			})
		}
	}
}

func TestCacheMigrationCollectorPublishesAttributionBounds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/first" {
			w.WriteHeader(http.StatusBadGateway)
			_, err := io.WriteString(w, `{"error":"synthetic failure"}`)
			require.NoError(t, err)
			return
		}
		_, err := io.WriteString(w, `{"type":"response.completed","response":{"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":900,"cache_write_tokens":20}}}}`)
		require.NoError(t, err)
	}))
	defer server.Close()
	c := New(time.Now(), 0)
	for i, path := range []string{"/first", "/last"} {
		req, err := http.NewRequestWithContext(With(t.Context(), c), http.MethodPost, server.URL+path, nil)
		require.NoError(t, err)
		req, logical := StartAttempt(req, int64(7+i), 0)
		RecordAccountRoute(req.Context(), sha256.Sum256([]byte(path)))
		req, physical := StartTransport(req)
		resp, err := server.Client().Do(req)
		physical.Response(resp, err)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		if i == 1 {
			require.True(t, Content(req.Context(), body))
			Output(req.Context(), false, false, "completed")
			Outcome(req.Context(), "success", false)
		}
		logical.Response(resp, nil)
	}
	c.Finish(http.StatusOK, false)
	called := false
	c.WhenFinished(func(s Snapshot) {
		called = true
		require.Len(t, s.Attempts, 4)
		require.Equal(t, http.StatusBadGateway, s.Attempts[1].Status)
		require.True(t, s.Attempts[1].BodyEOF)
		require.True(t, s.Attempts[3].BodyEOF)
		require.NotNil(t, s.Latency)
		require.Equal(t, "bounded_attribution_unknown", s.Latency.CacheMigration.Status, "%+v", s.Latency.CacheMigration)
		require.Equal(t, 1, s.Latency.CacheMigration.RouteSwitches)
		raw, err := json.Marshal(s)
		require.NoError(t, err)
		var doc struct {
			Latency struct {
				CacheMigration struct {
					Status        string `json:"status"`
					NonReadTokens *int64 `json:"non_read_input_tokens"`
					Lower         *int64 `json:"extra_cold_input_min_tokens"`
					Upper         *int64 `json:"extra_cold_input_max_tokens"`
				} `json:"cache_migration"`
			} `json:"latency"`
		}
		require.NoError(t, json.Unmarshal(raw, &doc))
		v := doc.Latency.CacheMigration
		require.Equal(t, "bounded_attribution_unknown", v.Status)
		require.NotNil(t, v.NonReadTokens)
		require.NotNil(t, v.Lower)
		require.NotNil(t, v.Upper)
		require.EqualValues(t, 100, *v.NonReadTokens, "cache writes remain non-read input, not a second deduction")
		require.EqualValues(t, 0, *v.Lower)
		require.EqualValues(t, 100, *v.Upper)
	})
	require.True(t, called, "the finished collector must publish its own derived observation")
}
