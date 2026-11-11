package requesttiming

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChatTypedContent(t *testing.T) {
	for _, tc := range []struct {
		name, data              string
		reasoning, text, broken bool
	}{
		{"text", `{"choices":[{"index":0,"delta":{"content":"private text"}}]}`, false, true, false},
		{"thinking", `{"choices":[{"index":0,"delta":{"reasoning_content":"private thinking"}}]}`, true, false, false},
		{"reasoning_alias", `{"choices":[{"index":0,"delta":{"reasoning":"private thinking"}}]}`, true, false, false},
		{"secondary", `{"choices":[{"index":1,"delta":{"content":"private text","reasoning_content":"private thinking"},"finish_reason":"stop"}]}`, false, false, false},
		{"role", `{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`, false, false, false},
		{"empty", `{"choices":[{"delta":{"content":"","reasoning_content":null}}]}`, false, false, false},
		{"tool", `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"private tool"}}]}}]}`, false, false, false},
		{"bad_content", `{"choices":[{"delta":{"content":1}}]}`, false, false, true},
		{"bad_index", `{"choices":[{"index":null,"delta":{"content":"private text"}}]}`, false, false, true},
		{"ambiguous_index", `{"choices":[{"delta":{"content":"private text"}},{"delta":{"content":"other"}}]}`, false, false, true},
		{"duplicate_primary", `{"choices":[{"index":0,"delta":{"content":"first"}},{"index":0,"delta":{"content":"other"}}]}`, false, false, true},
		{"duplicate_content", `{"choices":[{"delta":{"content":"first","content":"other"}}]}`, false, false, true},
		{"duplicate_choices", `{"choices":[],"choices":[{"delta":{"content":"private text"}}]}`, false, false, true},
		{"wrong_wire", `{"type":"response.output_text.delta","delta":"private text"}`, false, false, true},
		{"wrong_object", `{"object":"chat.completion","choices":[{"delta":{"content":"private text"}}]}`, false, false, true},
		{"wrong_payload", `{"choices":[{"message":{"content":"private text"}}]}`, false, false, true},
		{"bad_choices", `{"choices":null}`, false, false, true},
		{"missing_choices", `{"candidates":[],"usageMetadata":{"promptTokenCount":100}}`, false, false, true},
		{"bad_json", `{"choices":`, false, false, true},
		{"sentinel", `[DONE]`, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(time.Now(), 0)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(With(t.Context(), c))
			req, _ = StartAttempt(req, 1, 0)
			req, tr := StartTransport(req)
			ChatContent(req.Context(), []byte(tc.data), true)
			tr.Response(&http.Response{StatusCode: http.StatusOK}, nil)
			c.Finish(http.StatusOK, false)
			c.WhenFinished(func(s Snapshot) {
				v := DeriveContentObservation(s)
				require.Equal(t, tc.reasoning, v.ReasoningFirstMS != nil)
				require.Equal(t, tc.text, v.VisibleTextFirstMS != nil)
				_, broken := s.Attempts[1].Events["typed_content_truncated"]
				require.Equal(t, tc.broken, broken)
				require.Nil(t, v.OutputRate.TokensPerSecond)
				raw, err := json.Marshal(s)
				require.NoError(t, err)
				require.NotContains(t, string(raw), "private")
			})
		})
	}
}

func TestChatCanonicalCacheUsage(t *testing.T) {
	for _, tc := range []struct{ name, raw, status string }{
		{"hit", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":90}}`, "observed"},
		{"miss", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":0}}`, "observed"},
		{"empty_input", `{"prompt_tokens":0,"prompt_tokens_details":{"cached_tokens":0}}`, "observed"},
		{"missing_read", `{"prompt_tokens":100,"prompt_tokens_details":{}}`, "not_reported"},
		{"missing_details", `{"prompt_tokens":100}`, "not_reported"},
		{"missing_input", `{"prompt_tokens_details":{"cached_tokens":90}}`, "not_reported"},
		{"response_alias", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":90}}`, "ambiguous_counters"},
		{"vendor_alias", `{"prompt_tokens":100,"prompt_cache_hit_tokens":90,"prompt_cache_miss_tokens":10}`, "ambiguous_counters"},
		{"null_details", `{"prompt_tokens":100,"prompt_tokens_details":null}`, "invalid_counters"},
		{"null_read", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":null}}`, "invalid_counters"},
		{"string_read", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":"90"}}`, "invalid_counters"},
		{"negative_read", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":-1}}`, "invalid_counters"},
		{"fractional_read", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":1.5}}`, "invalid_counters"},
		{"excess_read", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":101}}`, "invalid_counters"},
		{"write_overlap", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":90,"cache_write_tokens":11}}`, "invalid_counters"},
		{"explicit_write", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":90,"cache_write_tokens":5}}`, "observed"},
		{"duplicate_read", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":0,"cached_tokens":90}}`, "invalid_counters"},
		{"duplicate_input", `{"prompt_tokens":100,"prompt_tokens":200,"prompt_tokens_details":{"cached_tokens":90}}`, "invalid_counters"},
		{"unsafe_input", `{"prompt_tokens":9007199254740992,"prompt_tokens_details":{"cached_tokens":0}}`, "invalid_counters"},
		{"diagnostic_limit", `{"padding":"` + strings.Repeat("x", 64*1024) + `"}`, "diagnostic_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := readCacheUsage([]byte(tc.raw), "chat_completions")
			require.NotNil(t, v)
			require.Equal(t, "chat_completions", v.Protocol)
			require.Equal(t, tc.status, v.Status)
		})
	}
}

func TestChatFinalAttemptIsolation(t *testing.T) {
	c := New(time.Now(), 0)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(With(t.Context(), c))
	for _, id := range []int64{1, 2} {
		attempt, _ := StartAttempt(req, id, 0)
		attempt, tr := StartTransport(attempt)
		if id == 1 {
			ChatContent(attempt.Context(), []byte(`{"choices":[{"delta":{"content":"private text"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":90}}}`), true)
		}
		tr.Response(&http.Response{StatusCode: http.StatusOK}, nil)
	}
	Outcome(req.Context(), "success", false)
	c.Finish(http.StatusOK, false)
	c.WhenFinished(func(s Snapshot) {
		require.Len(t, s.Attempts, 4)
		require.NotNil(t, s.Attempts[1].CacheUsage)
		require.Nil(t, s.Attempts[3].CacheUsage)
		v := DeriveContentObservation(s)
		require.Equal(t, "not_collected", v.Cache.Status)
		require.Nil(t, v.Cache.ReadRatio)
		require.Nil(t, v.VisibleTextFirstMS)
		require.Nil(t, v.TextStream.Chunks)
	})
}

func TestChatTextStreamBoundaries(t *testing.T) {
	for _, ending := range []string{"complete", "json", "eof", "sentinel", "length", "late_error", "late_text", "bad_usage_then_good", "usage_missing_then_zero", "canceled", "disconnect", "downstream_error", "capture_gap", "foreign_protocol", "empty_input"} {
		t.Run(ending, func(t *testing.T) {
			start := time.Now()
			now := start
			c := newCollector(start, 0, func() time.Time { return now })
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(With(t.Context(), c))
			req, _ = StartAttempt(req, 1, 0)
			req, tr := StartTransport(req)
			ctx := req.Context()
			if ending != "json" {
				now = start.Add(10 * time.Millisecond)
				ChatContent(ctx, []byte(`{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`), true)
				now = start.Add(20 * time.Millisecond)
				ChatContent(ctx, []byte(`{"choices":[{"delta":{"content":"first"}}]}`), true)
				now = start.Add(100 * time.Millisecond)
				ChatContent(ctx, []byte(`{"choices":[{"delta":{"role":"assistant"}}]}`), true)
				now = start.Add(320 * time.Millisecond)
				ChatContent(ctx, []byte(`{"choices":[{"delta":{"content":"last"}}]}`), true)
			}
			now = start.Add(400 * time.Millisecond)
			switch ending {
			case "json":
				ChatContent(ctx, []byte(`{"choices":[{"message":{"content":"full text"},"finish_reason":"stop"}]}`), false)
			case "eof", "sentinel":
				ChatContent(ctx, []byte(`[DONE]`), true)
			case "length":
				ChatContent(ctx, []byte(`{"choices":[{"delta":{},"finish_reason":"length"}]}`), true)
			default:
				ChatContent(ctx, []byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`), true)
			}
			now = start.Add(410 * time.Millisecond)
			if ending == "bad_usage_then_good" {
				ChatContent(ctx, []byte(`{"choices":[],"usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":-1}}}`), true)
			}
			ChatContent(ctx, []byte(`{"choices":[],"usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":90}}}`), true)
			switch ending {
			case "late_error":
				ChatContent(ctx, []byte(`{"error":{"message":"synthetic"}}`), true)
			case "late_text":
				ChatContent(ctx, []byte(`{"choices":[{"delta":{"content":"late"}}]}`), true)
			case "usage_missing_then_zero":
				ChatContent(ctx, []byte(`{"choices":[],"usage":{"prompt_tokens":100}}`), true)
				ChatContent(ctx, []byte(`{"choices":[],"usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":0}}}`), true)
			case "empty_input":
				ChatContent(ctx, []byte(`{"choices":[],"usage":{"prompt_tokens":0,"prompt_tokens_details":{"cached_tokens":0}}}`), true)
			case "downstream_error":
				c.mu.Lock()
				c.data.DownstreamError = true
				c.mu.Unlock()
			case "capture_gap":
				Stage(ctx, "typed_content_truncated")
			case "foreign_protocol":
				Content(ctx, []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"other"}}`))
			}
			Outcome(ctx, "success", ending == "disconnect")
			tr.Response(&http.Response{StatusCode: http.StatusOK}, nil)
			now = start.Add(500 * time.Millisecond)
			c.Finish(http.StatusOK, ending == "canceled")
			c.WhenFinished(func(s Snapshot) {
				v := DeriveContentObservation(s)
				wantHealthy := ending == "complete" || ending == "json" || ending == "usage_missing_then_zero"
				require.Equal(t, wantHealthy, v.Cache.Status == "observed", v.Cache.Status)
				require.Equal(t, "canonical_chat_completions_usage", v.Cache.Source)
				if wantHealthy {
					wantRatio := 0.9
					if ending == "usage_missing_then_zero" {
						wantRatio = 0
					}
					require.InDelta(t, wantRatio, *v.Cache.ReadRatio, 1e-9)
				} else {
					require.Nil(t, v.Cache.ReadRatio)
				}
				if ending == "complete" {
					require.Equal(t, "observed", v.TextStream.Status)
					require.Equal(t, 2, *v.TextStream.Chunks)
					require.Equal(t, 300.0, *v.TextStream.MaxGapMS)
					require.Equal(t, 80.0, *v.TextStream.TailWaitMS, "usage-only tail must not move the generation terminal")
				}
				if ending == "json" {
					require.Nil(t, v.TextStream.Chunks)
				}
				require.Nil(t, v.OutputRate.TokensPerSecond)
			})
		})
	}
}
