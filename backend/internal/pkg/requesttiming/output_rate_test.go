package requesttiming

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOutputRateUsesFinalPhysicalTextWindow(t *testing.T) {
	start := time.Now()
	now := start
	c := newCollector(start, 0, func() time.Time { return now })
	req := httptest.NewRequest("POST", "/v1/responses", nil).WithContext(With(t.Context(), c))
	req, _ = StartAttempt(req, 7, 0)
	old, oldTrace := StartTransport(req)
	Content(old.Context(), []byte(`{"type":"response.output_text.delta","delta":"old"}`))
	oldTrace.Response(&http.Response{StatusCode: 503}, nil)
	req, trace := StartTransport(req)
	now = start.Add(5 * time.Second)
	Content(req.Context(), []byte(`{"type":"response.output_text.delta","delta":"first"}`))
	now = start.Add(6 * time.Second)
	Content(req.Context(), []byte(`{"type":"response.output_text.delta","delta":"last"}`))
	now = start.Add(7 * time.Second)
	Content(req.Context(), []byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":"firstlast"}]}],"usage":{"output_tokens":120,"output_tokens_details":{"reasoning_tokens":20}}}}`))
	Output(req.Context(), false, false, "completed")
	trace.Response(&http.Response{StatusCode: 200}, nil)
	c.Finish(200, false)
	c.WhenFinished(func(s Snapshot) {
		data, err := json.Marshal(DeriveContentObservation(s))
		require.NoError(t, err)
		var observation map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data, &observation))
		require.Contains(t, observation, "output_rate", "output-phase evidence must be separate from request-average TPS")
		var rate struct {
			TokensPerSecond    *float64 `json:"tokens_per_second"`
			WindowMS           *float64 `json:"window_ms"`
			OutputTokens       *int64   `json:"output_tokens"`
			ReasoningTokens    *int64   `json:"reasoning_tokens"`
			NonReasoningTokens *int64   `json:"non_reasoning_tokens"`
			Status             string   `json:"status"`
			Method             string   `json:"method"`
		}
		require.NoError(t, json.Unmarshal(observation["output_rate"], &rate))
		require.Equal(t, "estimated", rate.Status)
		require.Equal(t, "non_reasoning_tokens_over_first_text_to_terminal", rate.Method)
		require.NotNil(t, rate.TokensPerSecond)
		require.NotNil(t, rate.WindowMS)
		require.InDelta(t, 50, *rate.TokensPerSecond, 1e-9)
		require.Equal(t, 2000.0, *rate.WindowMS)
		require.EqualValues(t, 120, *rate.OutputTokens)
		require.EqualValues(t, 20, *rate.ReasoningTokens)
		require.EqualValues(t, 100, *rate.NonReasoningTokens)
	})
}

func TestOutputRateCanonicalCounters(t *testing.T) {
	for _, tc := range []struct{ name, usage, status string }{
		{"explicit_zero_reasoning", `{"output_tokens":100,"output_tokens_details":{"reasoning_tokens":0}}`, "observed"},
		{"reasoning", `{"output_tokens":120,"output_tokens_details":{"reasoning_tokens":20}}`, "observed"},
		{"zero", `{"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0}}`, "observed"},
		{"absent_total", `{"output_tokens_details":{"reasoning_tokens":0}}`, "not_reported"},
		{"absent_details", `{"output_tokens":100}`, "not_reported"},
		{"absent_reasoning", `{"output_tokens":100,"output_tokens_details":{}}`, "not_reported"},
		{"compat_alias", `{"completion_tokens":100,"completion_tokens_details":{"reasoning_tokens":0}}`, "not_reported"},
		{"null", `{"output_tokens":null,"output_tokens_details":{"reasoning_tokens":0}}`, "invalid_counters"},
		{"null_details", `{"output_tokens":100,"output_tokens_details":null}`, "invalid_counters"},
		{"null_reasoning", `{"output_tokens":100,"output_tokens_details":{"reasoning_tokens":null}}`, "invalid_counters"},
		{"string", `{"output_tokens":"100","output_tokens_details":{"reasoning_tokens":0}}`, "invalid_counters"},
		{"string_reasoning", `{"output_tokens":100,"output_tokens_details":{"reasoning_tokens":"0"}}`, "invalid_counters"},
		{"fraction", `{"output_tokens":100.5,"output_tokens_details":{"reasoning_tokens":0}}`, "invalid_counters"},
		{"fraction_reasoning", `{"output_tokens":100,"output_tokens_details":{"reasoning_tokens":0.5}}`, "invalid_counters"},
		{"negative", `{"output_tokens":-1,"output_tokens_details":{"reasoning_tokens":0}}`, "invalid_counters"},
		{"negative_reasoning", `{"output_tokens":100,"output_tokens_details":{"reasoning_tokens":-1}}`, "invalid_counters"},
		{"exceeds_total", `{"output_tokens":100,"output_tokens_details":{"reasoning_tokens":101}}`, "invalid_counters"},
		{"unsafe_integer", `{"output_tokens":9007199254740992,"output_tokens_details":{"reasoning_tokens":0}}`, "invalid_counters"},
		{"duplicate", `{"output_tokens":100,"output_tokens":200,"output_tokens_details":{"reasoning_tokens":0}}`, "invalid_counters"},
		{"duplicate_reasoning", `{"output_tokens":100,"output_tokens_details":{"reasoning_tokens":0,"reasoning_tokens":20}}`, "invalid_counters"},
		{"other_token_kind", `{"output_tokens":100,"output_tokens_details":{"reasoning_tokens":0,"audio_tokens":2}}`, "unsupported_usage"},
		{"diagnostic_limit", `{"padding":"` + strings.Repeat("x", 64*1024) + `"}`, "diagnostic_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := readOutputUsage([]byte(tc.usage))
			require.NotNil(t, raw)
			require.Equal(t, tc.status, raw.Status)
		})
	}
	require.Nil(t, readOutputUsage(nil))
	require.Nil(t, readOutputUsage([]byte(`null`)))
}

func TestOutputRateTerminalContentScope(t *testing.T) {
	usage := `"usage":{"output_tokens":100,"output_tokens_details":{"reasoning_tokens":0}}`
	for _, tc := range []struct{ name, response, status string }{
		{"text", `"output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}]`, "text_and_reasoning"},
		{"reasoning_and_text", `"output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":"answer"}]}]`, "text_and_reasoning"},
		{"not_reported", `"id":"fixture"`, "not_reported"},
		{"null", `"output":null`, "no_text_output"},
		{"empty", `"output":[]`, "no_text_output"},
		{"only_reasoning", `"output":[{"type":"reasoning"}]`, "no_text_output"},
		{"only_tool", `"output":[{"type":"function_call"}]`, "unsupported_output"},
		{"mixed_tool", `"output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]},{"type":"function_call"}]`, "unsupported_output"},
		{"audio", `"output":[{"type":"message","content":[{"type":"output_audio","text":"answer"}]}]`, "unsupported_output"},
		{"refusal", `"output":[{"type":"message","content":[{"type":"refusal","text":"answer"}]}]`, "unsupported_output"},
		{"duplicate_item_type", `"output":[{"type":"function_call","type":"message","content":[{"type":"output_text","text":"answer"}]}]`, "unsupported_output"},
		{"duplicate_part_type", `"output":[{"type":"message","content":[{"type":"refusal","type":"output_text","text":"answer"}]}]`, "unsupported_output"},
		{"conflicting_status", `"status":"failed","output":[]`, "invalid_terminal"},
		{"null_status", `"status":null,"output":[]`, "invalid_terminal"},
		{"unfinished_item", `"output":[{"type":"message","status":"in_progress","content":[{"type":"output_text","text":"answer"}]}]`, "invalid_terminal"},
		{"unfinished_reasoning", `"output":[{"type":"reasoning","status":"in_progress"},{"type":"message","content":[{"type":"output_text","text":"answer"}]}]`, "invalid_terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, raw := readTerminalUsage([]byte(`{"response":{` + usage + `,` + tc.response + `}}`))
			require.NotNil(t, raw)
			require.Equal(t, "observed", raw.Status)
			require.Equal(t, tc.status, raw.ContentStatus)
		})
	}
	_, raw := readTerminalUsage([]byte(`{` + usage + `,"response":{"output":[]}}`))
	require.Equal(t, "not_reported", raw.Status, "root usage must not be joined with nested text")
	_, raw = readTerminalUsage([]byte(`{"response":{"usage":{},"usage":{}}}`))
	require.Equal(t, "invalid_counters", raw.Status)
}

func outputRateTestSnapshot() Snapshot {
	s := textStreamTestSnapshot()
	s.Terminal = "completed"
	s.TotalMS = 3000
	end := 2500.0
	a := &s.Attempts[0]
	a.EndMS = &end
	a.TextStream.LastMS, a.TextStream.MaxGapMS, a.TextStream.TerminalMS = 1010, 500, 2010
	a.OutputUsage = &OutputUsage{OutputTokens: 120, ReasoningTokens: 20, Status: "observed", ContentStatus: "text_and_reasoning"}
	return s
}

func TestOutputRateProjectionFailuresAndShortSamples(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Snapshot)
		status string
	}{
		{"complete", func(*Snapshot) {}, "estimated"},
		{"historical", func(s *Snapshot) { s.Attempts[0].OutputUsage = nil }, "not_collected"},
		{"missing_reasoning", func(s *Snapshot) { s.Attempts[0].OutputUsage.Status = "not_reported" }, "not_reported"},
		{"missing_text_type", func(s *Snapshot) { s.Attempts[0].OutputUsage.ContentStatus = "" }, "not_collected"},
		{"invalid_count", func(s *Snapshot) { s.Attempts[0].OutputUsage.ReasoningTokens = 121 }, "invalid_counters"},
		{"negative_count", func(s *Snapshot) { s.Attempts[0].OutputUsage.OutputTokens = -1 }, "invalid_counters"},
		{"capture_gap", func(s *Snapshot) { s.Attempts[0].Events["typed_content_truncated"] = 20 }, "incomplete_capture"},
		{"failed", func(s *Snapshot) { s.Attempts[0].Terminal = "failed" }, "partial"},
		{"error", func(s *Snapshot) { s.Attempts[0].Error = "timeout" }, "partial"},
		{"downstream_status", func(s *Snapshot) { s.Status = 502 }, "partial"},
		{"upstream_status", func(s *Snapshot) { s.Attempts[0].Status = 503 }, "partial"},
		{"global_terminal", func(s *Snapshot) { s.Terminal = "incomplete" }, "partial"},
		{"global_outcome", func(s *Snapshot) { s.Outcome = "failed" }, "partial"},
		{"cancel", func(s *Snapshot) { s.Canceled = true }, "partial"},
		{"disconnect", func(s *Snapshot) { s.ClientDisconnect = true }, "partial"},
		{"write_error", func(s *Snapshot) { s.DownstreamError = true }, "partial"},
		{"short", func(s *Snapshot) { s.Attempts[0].OutputUsage.OutputTokens = 51 }, "short_output"},
		{"no_text_tokens", func(s *Snapshot) { s.Attempts[0].OutputUsage.OutputTokens = 20 }, "short_output"},
		{"tool_tokens", func(s *Snapshot) { s.Attempts[0].OutputUsage.ContentStatus = "unsupported_output" }, "unsupported_output"},
		{"tool_delta", func(s *Snapshot) { s.Attempts[0].TextStream.NonTextOutput = true }, "unsupported_output"},
		{"terminal_only", func(s *Snapshot) { s.Attempts[0].TextStream = nil }, "insufficient_stream"},
		{"single_chunk", func(s *Snapshot) {
			s.Attempts[0].TextStream.Chunks = 1
			s.Attempts[0].TextStream.LastMS = 10
			s.Attempts[0].TextStream.MaxGapMS = 0
		}, "insufficient_stream"},
		{"buffered", func(s *Snapshot) { s.Attempts[0].TextStream.LastMS = 10; s.Attempts[0].TextStream.MaxGapMS = 0 }, "insufficient_window"},
		{"short_span", func(s *Snapshot) { s.Attempts[0].TextStream.LastMS = 250; s.Attempts[0].TextStream.MaxGapMS = 120 }, "insufficient_window"},
		{"missing_first_text", func(s *Snapshot) { delete(s.Attempts[0].Events, "first_visible_text") }, "incomplete_stream"},
		{"earlier_non_delta_text", func(s *Snapshot) { s.Attempts[0].Events["first_visible_text"] = 5 }, "incomplete_stream"},
		{"truncated_stream", func(s *Snapshot) { s.Attempts[0].TextStream.Truncated = true }, "incomplete_capture"},
		{"invalid_clock", func(s *Snapshot) { s.Attempts[0].TextStream.Invalid = true }, "invalid_boundary"},
		{"future_terminal", func(s *Snapshot) { s.Attempts[0].TextStream.TerminalMS = 4000 }, "invalid_boundary"},
		{"nan_terminal", func(s *Snapshot) { s.Attempts[0].TextStream.TerminalMS = math.NaN() }, "invalid_boundary"},
		{"missing_end", func(s *Snapshot) { s.Attempts[0].EndMS = nil }, "invalid_boundary"},
		{"new_attempt", func(s *Snapshot) { s.Attempts = append(s.Attempts, Attempt{Kind: "http"}) }, "not_collected"},
		{"ws", func(s *Snapshot) { s.Attempts[0].Kind = "websocket" }, "not_collected"},
		{"truncated_trace", func(s *Snapshot) { s.Truncated = true }, "not_collected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := outputRateTestSnapshot()
			tc.change(&s)
			v := DeriveContentObservation(s).OutputRate
			require.Equal(t, tc.status, v.Status)
			if tc.status == "estimated" {
				require.NotNil(t, v.TokensPerSecond)
				require.InDelta(t, 50, *v.TokensPerSecond, 1e-9)
			} else {
				require.Nil(t, v.TokensPerSecond)
				require.Nil(t, v.WindowMS)
			}
		})
	}
}

func TestOutputRateMinimumAndSnapshotIsolation(t *testing.T) {
	s := outputRateTestSnapshot()
	a := &s.Attempts[0]
	a.TextStream.Chunks = 2
	a.TextStream.LastMS, a.TextStream.TerminalMS, a.TextStream.MaxGapMS = 260, 260, 250
	a.OutputUsage.OutputTokens, a.OutputUsage.ReasoningTokens = 32, 0
	v := DeriveContentObservation(s).OutputRate
	require.Equal(t, "estimated", v.Status)
	require.Equal(t, 128.0, *v.TokensPerSecond)
	c := New(time.Now(), 0)
	c.data = s
	c.finished = true
	c.WhenFinished(func(snapshot Snapshot) {
		require.Equal(t, "estimated", snapshot.OutputRate.Status)
		snapshot.Attempts[0].OutputUsage.OutputTokens = 999
		*snapshot.OutputRate.OutputTokens = 999
	})
	c.WhenFinished(func(snapshot Snapshot) {
		require.EqualValues(t, 32, snapshot.Attempts[0].OutputUsage.OutputTokens)
		require.EqualValues(t, 32, *snapshot.OutputRate.OutputTokens)
	})
}

func TestOutputRateRecognizesMixedWireEvents(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		mixed       bool
	}{
		{"tool_item", `{"type":"response.output_item.added","item":{"type":"function_call"}}`, true},
		{"tool_delta", `{"type":"response.function_call_arguments.delta","delta":"{}"}`, true},
		{"custom_tool_delta", `{"type":"response.custom_tool_call_input.delta","delta":"args"}`, true},
		{"audio", `{"type":"response.audio.delta","delta":"audio"}`, true},
		{"unknown_delta", `{"type":"response.future.delta","delta":"unknown"}`, true},
		{"heartbeat", `{"type":"keepalive"}`, false},
		{"empty_text", `{"type":"response.output_text.delta","delta":""}`, false},
		{"empty_reasoning", `{"type":"response.reasoning_summary_text.delta","delta":""}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			now := start
			c := newCollector(start, 0, func() time.Time { return now })
			req := httptest.NewRequest("POST", "/", nil).WithContext(With(t.Context(), c))
			req, _ = StartAttempt(req, 1, 0)
			req, trace := StartTransport(req)
			now = start.Add(10 * time.Millisecond)
			Content(req.Context(), []byte(`{"type":"response.output_text.delta","delta":"first"}`))
			now = start.Add(500 * time.Millisecond)
			Content(req.Context(), []byte(tc.event))
			now = start.Add(time.Second)
			Content(req.Context(), []byte(`{"type":"response.output_text.delta","delta":"second"}`))
			now = start.Add(2 * time.Second)
			Content(req.Context(), []byte(`{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"firstsecond"}]}],"usage":{"output_tokens":100,"output_tokens_details":{"reasoning_tokens":0}}}}`))
			Content(req.Context(), []byte(`{"type":"response.future.delta","delta":"after terminal is ignored"}`))
			Output(req.Context(), false, false, "completed")
			trace.Response(&http.Response{StatusCode: 200}, nil)
			c.Finish(200, false)
			c.WhenFinished(func(s Snapshot) {
				rate := DeriveContentObservation(s).OutputRate
				if tc.mixed {
					require.Equal(t, "unsupported_output", rate.Status)
					require.Nil(t, rate.TokensPerSecond)
				} else {
					require.Equal(t, "estimated", rate.Status)
					require.NotNil(t, rate.TokensPerSecond)
				}
				require.Equal(t, 2, s.Attempts[1].TextStream.Chunks)
			})
		})
	}
}
