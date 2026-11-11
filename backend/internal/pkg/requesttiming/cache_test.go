package requesttiming

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func protocolCacheSnapshot(t *testing.T, protocol string, events ...string) Snapshot {
	t.Helper()
	c := New(time.Now(), 0)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(With(t.Context(), c))
	req, _ = StartAttempt(req, 1, 0)
	req, tr := StartTransport(req)
	for _, event := range events {
		if protocol == "gemini" {
			GeminiContent(req.Context(), []byte(event), true)
		} else {
			require.True(t, Content(req.Context(), []byte(event)))
		}
	}
	tr.Response(&http.Response{StatusCode: http.StatusOK}, nil)
	c.Finish(http.StatusOK, false)
	var result Snapshot
	c.WhenFinished(func(s Snapshot) { result = s })
	return result
}

func TestMessagesCanonicalCacheCumulativeCounters(t *testing.T) {
	for _, tc := range []struct {
		name, initial, delta, status string
		input, read, write           int64
	}{
		{"independent_buckets", `{"input_tokens":10,"cache_read_input_tokens":30,"cache_creation_input_tokens":20}`, `{"output_tokens":120}`, "observed", 60, 30, 20},
		{"explicit_zero_replaces", `{"input_tokens":10,"cache_read_input_tokens":30,"cache_creation_input_tokens":20}`, `{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":5}`, "observed", 5, 0, 5},
		{"partial_updates", `{"input_tokens":10}`, `{"cache_read_input_tokens":90,"cache_creation_input_tokens":0}`, "observed", 100, 90, 0},
		{"missing_read", `{"input_tokens":10,"cache_creation_input_tokens":0}`, `{"output_tokens":120}`, "not_reported", 0, 0, 0},
		{"missing_write", `{"input_tokens":10,"cache_read_input_tokens":90}`, `{}`, "not_reported", 0, 0, 0},
		{"alias_only", `{"input_tokens":10,"cached_tokens":90,"cache_creation_input_tokens":0}`, `{}`, "not_reported", 0, 0, 0},
		{"ambiguous_compatibility", `{"input_tokens":100,"prompt_tokens":100,"cache_read_input_tokens":90,"cache_creation_input_tokens":0}`, `{}`, "ambiguous_counters", 0, 0, 0},
		{"negative", `{"input_tokens":10,"cache_read_input_tokens":-1,"cache_creation_input_tokens":0}`, `{}`, "invalid_counters", 0, 0, 0},
		{"string", `{"input_tokens":10,"cache_read_input_tokens":"90","cache_creation_input_tokens":0}`, `{}`, "invalid_counters", 0, 0, 0},
		{"fraction", `{"input_tokens":10,"cache_read_input_tokens":1.5,"cache_creation_input_tokens":0}`, `{}`, "invalid_counters", 0, 0, 0},
		{"null", `{"input_tokens":10,"cache_read_input_tokens":null,"cache_creation_input_tokens":0}`, `{}`, "invalid_counters", 0, 0, 0},
		{"duplicate", `{"input_tokens":10,"cache_read_input_tokens":0,"cache_read_input_tokens":90,"cache_creation_input_tokens":0}`, `{}`, "invalid_counters", 0, 0, 0},
		{"unsafe_sum", `{"input_tokens":9007199254740991,"cache_read_input_tokens":1,"cache_creation_input_tokens":0}`, `{}`, "invalid_counters", 0, 0, 0},
		{"invalid_not_healed", `{"input_tokens":10,"cache_read_input_tokens":-1,"cache_creation_input_tokens":0}`, `{"cache_read_input_tokens":0}`, "invalid_counters", 0, 0, 0},
		{"invalid_late_update", `{"input_tokens":10,"cache_read_input_tokens":90,"cache_creation_input_tokens":0}`, `{"cache_read_input_tokens":null}`, "invalid_counters", 0, 0, 0},
		{"ttl_breakdown", `{"input_tokens":10,"cache_read_input_tokens":30,"cache_creation_input_tokens":20,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":15}}`, `{}`, "observed", 60, 30, 20},
		{"ttl_mismatch", `{"input_tokens":10,"cache_read_input_tokens":30,"cache_creation_input_tokens":20,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":16}}`, `{}`, "invalid_counters", 0, 0, 0},
		{"ttl_negative", `{"input_tokens":10,"cache_read_input_tokens":30,"cache_creation_input_tokens":20,"cache_creation":{"ephemeral_5m_input_tokens":-1,"ephemeral_1h_input_tokens":21}}`, `{}`, "invalid_counters", 0, 0, 0},
		{"ttl_is_not_total_evidence", `{"input_tokens":10,"cache_read_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":15}}`, `{}`, "not_reported", 0, 0, 0},
		{"empty", `{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`, `{}`, "empty_input", 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := protocolCacheSnapshot(t, "messages", `{"type":"message_start","message":{"usage":`+tc.initial+`}}`,
				`{"type":"message_delta","usage":`+tc.delta+`}`, `{"type":"message_stop"}`)
			v := DeriveContentObservation(s).Cache
			require.Equal(t, "canonical_messages_cumulative_usage", v.Source)
			require.Equal(t, "final_physical_http_messages_cumulative_usage", v.Scope)
			require.Equal(t, tc.status, v.Status)
			require.Nil(t, s.Attempts[0].CacheUsage)
			if tc.status == "observed" {
				require.Equal(t, tc.input, *v.InputTokens)
				require.Equal(t, tc.read, *v.ReadTokens)
				require.Equal(t, tc.write, *v.WriteTokens)
				require.InDelta(t, float64(tc.read)/float64(tc.input), *v.ReadRatio, 1e-9)
			} else {
				require.Nil(t, v.ReadRatio)
			}
		})
	}
}

func TestGeminiCanonicalCacheLatestMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, initial, trailing, status string
		read                            int64
	}{
		{"terminal", `{"promptTokenCount":100,"cachedContentTokenCount":90}`, "", "observed", 90},
		{"trailing", `{"promptTokenCount":100}`, `{"promptTokenCount":100,"cachedContentTokenCount":90}`, "observed", 90},
		{"explicit_zero", `{"promptTokenCount":100,"cachedContentTokenCount":90}`, `{"promptTokenCount":100,"cachedContentTokenCount":0}`, "observed", 0},
		{"missing_read", `{"promptTokenCount":100}`, "", "not_reported", 0},
		{"latest_missing_read", `{"promptTokenCount":100,"cachedContentTokenCount":90}`, `{"promptTokenCount":100}`, "not_reported", 0},
		{"negative", `{"promptTokenCount":100,"cachedContentTokenCount":-1}`, "", "invalid_counters", 0},
		{"overlap", `{"promptTokenCount":100,"cachedContentTokenCount":101}`, "", "invalid_counters", 0},
		{"null", `{"promptTokenCount":100,"cachedContentTokenCount":null}`, "", "invalid_counters", 0},
		{"duplicate", `{"promptTokenCount":100,"cachedContentTokenCount":0,"cachedContentTokenCount":90}`, "", "invalid_counters", 0},
		{"string", `{"promptTokenCount":100,"cachedContentTokenCount":"90"}`, "", "invalid_counters", 0},
		{"invalid_not_healed", `{"promptTokenCount":100,"cachedContentTokenCount":-1}`, `{"promptTokenCount":100,"cachedContentTokenCount":90}`, "invalid_counters", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":` + tc.initial + `}}`}
			if tc.trailing != "" {
				events = append(events, `{"response":{"usageMetadata":`+tc.trailing+`}}`)
			}
			s := protocolCacheSnapshot(t, "gemini", events...)
			v := DeriveContentObservation(s).Cache
			require.Equal(t, "canonical_gemini_usage_metadata", v.Source)
			require.Equal(t, "final_physical_http_gemini_usage_metadata", v.Scope)
			require.Equal(t, tc.status, v.Status)
			require.Nil(t, s.Attempts[0].CacheUsage)
			if tc.status == "observed" {
				require.EqualValues(t, 100, *v.InputTokens)
				require.Equal(t, tc.read, *v.ReadTokens)
				require.Nil(t, v.WriteTokens, "Gemini does not report cache creation in this protocol")
			} else {
				require.Nil(t, v.ReadRatio)
			}
		})
	}
}

func TestMessagesCanonicalCacheRequiresCompleteBusinessTerminal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, reason := range []string{"end_turn", "stop_sequence", "tool_use", "max_tokens", "pause_turn", "refusal", "model_context_window_exceeded", "future_reason", ""} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, reason), func(t *testing.T) {
				usage := `{"input_tokens":10,"cache_read_input_tokens":90,"cache_creation_input_tokens":0}`
				events := []string{`{"type":"message","stop_reason":"` + reason + `","usage":` + usage + `,"content":[{"type":"text","text":"private answer"}]}`}
				if stream {
					events = []string{`{"type":"message_start","message":{"usage":` + usage + `}}`,
						`{"type":"message_delta","delta":{"stop_reason":"` + reason + `"}}`, `{"type":"message_stop"}`}
				}
				s := protocolCacheSnapshot(t, "messages", events...)
				v := DeriveContentObservation(s)
				if reason == "end_turn" || reason == "stop_sequence" || reason == "tool_use" || (stream && reason == "") {
					require.Equal(t, "observed", v.Cache.Status)
					require.InDelta(t, 0.9, *v.Cache.ReadRatio, 1e-9)
				} else {
					require.Equal(t, "partial", v.Cache.Status)
					require.Nil(t, v.Cache.ReadRatio)
				}
				require.Nil(t, v.TextStream.Chunks, "JSON and usage metadata never invent text deltas")
				raw, err := json.Marshal(s)
				require.NoError(t, err)
				require.NotContains(t, string(raw), "private answer")
			})
		}
	}
}

func TestProtocolCacheRejectsPartialAndIsolatesStoredEvidence(t *testing.T) {
	for _, protocol := range []string{"messages", "gemini"} {
		for _, mode := range []string{"observed", "canceled", "disconnected", "downstream_error", "failed", "gap", "invalid", "truncated", "foreign_protocol", "legacy_mistag", "missing_protocol_stream"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				var s Snapshot
				if protocol == "messages" {
					s = protocolCacheSnapshot(t, protocol, `{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":90,"cache_creation_input_tokens":0}}}`, `{"type":"message_stop"}`)
				} else {
					s = protocolCacheSnapshot(t, protocol, `{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":90}}`)
				}
				a := &s.Attempts[1]
				switch mode {
				case "canceled":
					s.Canceled = true
				case "disconnected":
					s.ClientDisconnect = true
				case "downstream_error":
					s.DownstreamError = true
				case "failed":
					s.Outcome = "failed"
				case "gap":
					a.Events["typed_content_truncated"] = 1
				case "invalid":
					a.TextStream.Invalid = true
				case "truncated":
					a.TextStream.Truncated = true
				case "foreign_protocol":
					a.TextStream.Protocol = "responses"
				case "legacy_mistag":
					a.CacheUsage.Protocol = ""
				case "missing_protocol_stream":
					a.TextStream = nil
				}
				// Only persisted counters are needed to derive the same evidence.
				raw, err := json.Marshal(s)
				require.NoError(t, err)
				var saved Snapshot
				require.NoError(t, json.Unmarshal(raw, &saved))
				v := DeriveContentObservation(saved).Cache
				require.Equal(t, DeriveContentObservation(s).Cache, v)
				if mode == "observed" {
					require.Equal(t, "observed", v.Status)
					require.InDelta(t, 0.9, *v.ReadRatio, 1e-9)
				} else {
					require.NotEqual(t, "observed", v.Status)
					require.Nil(t, v.ReadRatio)
				}
			})
		}
	}
}

func TestProtocolCacheNeverMasksContradictoryOrMalformedFollowups(t *testing.T) {
	for _, tc := range []struct{ name, protocol, followup string }{
		{"messages_malformed_delta", "messages", `{"type":"content_block_delta","delta":"private lost text"}`},
		{"messages_duplicate_stop_reason", "messages", `{"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_reason":"end_turn"}}`},
		{"messages_duplicate_usage", "messages", `{"type":"message_delta","usage":{},"usage":{}}`},
		{"messages_late_usage", "messages", `{"type":"message_delta","usage":{"cache_read_input_tokens":0}}`},
		{"messages_late_error", "messages", `{"type":"error","error":{"type":"overloaded_error"}}`},
		{"gemini_duplicate_metadata", "gemini", `{"usageMetadata":{},"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":90}}`},
		{"gemini_late_text", "gemini", `{"candidates":[{"content":{"parts":[{"text":"late text"}]}}]}`},
		{"gemini_late_error", "gemini", `{"error":{"code":503}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(time.Now(), 0)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(With(t.Context(), c))
			req, _ = StartAttempt(req, 1, 0)
			req, tr := StartTransport(req)
			if tc.protocol == "messages" {
				Content(req.Context(), []byte(`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":90,"cache_creation_input_tokens":0}}}`))
				if tc.name != "messages_duplicate_stop_reason" && tc.name != "messages_malformed_delta" {
					Content(req.Context(), []byte(`{"type":"message_stop"}`))
				}
				Content(req.Context(), []byte(tc.followup))
				Content(req.Context(), []byte(`{"type":"message_stop"}`))
			} else {
				GeminiContent(req.Context(), []byte(`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":90}}`), true)
				GeminiContent(req.Context(), []byte(tc.followup), true)
			}
			tr.Response(&http.Response{StatusCode: http.StatusOK}, nil)
			c.Finish(http.StatusOK, false)
			c.WhenFinished(func(s Snapshot) {
				v := DeriveContentObservation(s).Cache
				require.NotEqual(t, "observed", v.Status)
				require.Nil(t, v.ReadRatio)
			})
		})
	}
}

func TestProtocolCacheDoesNotBorrowFromPreviousAttempt(t *testing.T) {
	for _, previous := range []string{"messages", "gemini"} {
		for _, current := range []string{"messages", "gemini"} {
			t.Run(previous+"/to/"+current, func(t *testing.T) {
				c := New(time.Now(), 0)
				root := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(With(t.Context(), c))
				req, _ := StartAttempt(root, 1, 0)
				req, tr := StartTransport(req)
				if previous == "messages" {
					Content(req.Context(), []byte(`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":90,"cache_creation_input_tokens":0}}}`))
					Content(req.Context(), []byte(`{"type":"message_stop"}`))
				} else {
					GeminiContent(req.Context(), []byte(`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":90}}`), true)
				}
				tr.Response(&http.Response{StatusCode: http.StatusOK}, nil)
				req, _ = StartAttempt(root, 2, 0)
				req, tr = StartTransport(req)
				if current == "messages" {
					Content(req.Context(), []byte(`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`))
					Content(req.Context(), []byte(`{"type":"message_stop"}`))
				} else {
					GeminiContent(req.Context(), []byte(`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100}}`), true)
				}
				tr.Response(&http.Response{StatusCode: http.StatusOK}, nil)
				c.Finish(http.StatusOK, false)
				c.WhenFinished(func(s Snapshot) {
					require.Len(t, s.Attempts, 4)
					require.Nil(t, s.Attempts[0].CacheUsage)
					require.Nil(t, s.Attempts[2].CacheUsage)
					require.EqualValues(t, 90, s.Attempts[1].CacheUsage.ReadTokens)
					require.EqualValues(t, 2, s.Attempts[3].AccountID)
					v := DeriveContentObservation(s).Cache
					require.Equal(t, "not_reported", v.Status)
					require.Nil(t, v.ReadRatio)
					// Snapshot observers own their copies even for cumulative buckets.
					s.Attempts[1].CacheUsage.ReadTokens = 999
				})
				c.WhenFinished(func(s Snapshot) { require.EqualValues(t, 90, s.Attempts[1].CacheUsage.ReadTokens) })
			})
		}
	}
}
