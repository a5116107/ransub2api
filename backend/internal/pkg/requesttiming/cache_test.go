package requesttiming

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCacheUsageRequiresExplicitCanonicalCounters(t *testing.T) {
	for _, tc := range []struct {
		name, usage, status string
	}{
		{"explicit_miss", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":0}}`, "observed"},
		{"hit", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":90,"cache_write_tokens":5}}`, "observed"},
		{"absent_read", `{"input_tokens":100,"input_tokens_details":{}}`, "not_reported"},
		{"absent_details", `{"input_tokens":100}`, "not_reported"},
		{"compat_alias_not_canonical", `{"prompt_tokens":100,"cached_tokens":90}`, "not_reported"},
		{"null_read", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":null}}`, "invalid_counters"},
		{"string_read", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":"90"}}`, "invalid_counters"},
		{"fractional_read", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":1.5}}`, "invalid_counters"},
		{"negative_read", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":-1}}`, "invalid_counters"},
		{"exceeds_input", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":101}}`, "invalid_counters"},
		{"write_overlap", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":90,"cache_write_tokens":11}}`, "invalid_counters"},
		{"duplicate_input", `{"input_tokens":100,"input_tokens":200,"input_tokens_details":{"cached_tokens":90}}`, "invalid_counters"},
		{"duplicate_details", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":0},"input_tokens_details":{"cached_tokens":90}}`, "invalid_counters"},
		{"negative_input", `{"input_tokens":-100,"input_tokens_details":{"cached_tokens":0}}`, "invalid_counters"},
		{"unsafe_integer", `{"input_tokens":9007199254740992,"input_tokens_details":{"cached_tokens":0}}`, "invalid_counters"},
		{"diagnostic_limit", `{"padding":"` + strings.Repeat("x", 64*1024) + `"}`, "diagnostic_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := readCacheUsage([]byte(tc.usage))
			require.NotNil(t, v)
			require.Equal(t, tc.status, v.Status)
		})
	}
	for _, envelope := range []string{
		`{"response":{"usage":{},"usage":{}}}`,
		`{"response":{},"response":{"usage":{}}}`,
		`{"usage":{},"usage":{}}`,
	} {
		cache, _ := readTerminalUsage([]byte(envelope))
		require.Equal(t, "invalid_counters", cache.Status)
	}
}

func TestCacheObservationTerminalRouteAndSnapshotIsolation(t *testing.T) {
	for _, mode := range []string{"hit", "miss", "missing", "empty", "failed", "capture_gap", "canceled", "disconnect", "downstream_error", "transport_error"} {
		t.Run(mode, func(t *testing.T) {
			c := New(time.Now(), 0)
			req := httptest.NewRequest("POST", "/v1/responses", nil).WithContext(With(t.Context(), c))
			req, _ = StartAttempt(req, 1, 0)
			fingerprint := sha256.Sum256([]byte("synthetic-route"))
			RecordAccountRoute(req.Context(), fingerprint)
			RecordAccountRoute(req.Context(), sha256.Sum256([]byte("must-not-relabel")))
			req, tr := StartTransport(req)
			read, input, terminal := 90, 100, "completed"
			if mode == "miss" || mode == "empty" {
				read = 0
			}
			if mode == "empty" {
				input = 0
			}
			if mode == "failed" {
				terminal = "failed"
			}
			usage := fmt.Sprintf(`{"input_tokens":%d,"input_tokens_details":{"cached_tokens":%d}}`, input, read)
			if mode == "missing" {
				usage = `{"input_tokens":100}`
			}
			Content(req.Context(), []byte(`{"type":"response.`+terminal+`","response":{"usage":`+usage+`}}`))
			Content(req.Context(), []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":1}}}}`))
			if mode == "capture_gap" {
				Stage(req.Context(), "typed_content_truncated")
			}
			if mode == "transport_error" {
				tr.update(func(a *Attempt) { a.Error = "timeout" })
			}
			Output(req.Context(), false, false, terminal)
			Outcome(req.Context(), "success", mode == "disconnect")
			if mode == "downstream_error" {
				c.data.DownstreamError = true
			}
			tr.Response(&http.Response{StatusCode: 200}, nil)
			c.Finish(200, mode == "canceled")
			c.WhenFinished(func(s Snapshot) {
				require.Nil(t, s.Attempts[0].CacheUsage, "only the physical attempt owns counters")
				require.Equal(t, hex.EncodeToString(fingerprint[:]), s.Attempts[1].RouteFingerprint)
				v := DeriveContentObservation(s).Cache
				switch mode {
				case "hit", "miss":
					require.Equal(t, "observed", v.Status)
					require.EqualValues(t, read, *v.ReadTokens)
					require.Equal(t, mode == "hit", *v.Hit)
					require.InDelta(t, float64(read)/100, *v.ReadRatio, 1e-9)
					require.Nil(t, v.WriteTokens, "an unreported write count stays unknown")
				case "missing":
					require.Equal(t, "not_reported", v.Status)
				case "empty":
					require.Equal(t, "empty_input", v.Status)
				case "capture_gap":
					require.Equal(t, "incomplete_capture", v.Status)
				default:
					require.Equal(t, "partial", v.Status)
				}
				if mode != "hit" && mode != "miss" {
					require.Nil(t, v.ReadRatio)
					require.Nil(t, v.Hit)
				}
				s.Attempts[1].CacheUsage.ReadTokens = 999
			})
			c.WhenFinished(func(s Snapshot) {
				if mode == "missing" {
					require.Equal(t, "not_reported", s.Attempts[1].CacheUsage.Status)
					require.Zero(t, s.Attempts[1].CacheUsage.ReadTokens)
				} else {
					require.EqualValues(t, read, s.Attempts[1].CacheUsage.ReadTokens)
				}
			})
		})
	}
	require.Equal(t, "not_collected", DeriveContentObservation(Snapshot{}).Cache.Status)
}
