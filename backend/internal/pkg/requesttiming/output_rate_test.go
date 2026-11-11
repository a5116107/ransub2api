package requesttiming

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func observeOutputRateFixture(ctx context.Context, protocol, phase, usage string) {
	extra := ""
	if usage != "" {
		key := "usage"
		if protocol == "gemini" {
			key = "usageMetadata"
		}
		extra = `,"` + key + `":` + usage
	}
	switch protocol {
	case "chat_completions":
		choices := `[{"index":0,"delta":{"content":"private text"}}]`
		switch phase {
		case "terminal":
			choices = `[{"index":0,"delta":{},"finish_reason":"stop"}]`
		case "usage":
			choices = `[]`
		case "secondary":
			choices = `[{"index":1,"delta":{"content":"private secondary"}}]`
		case "tool":
			choices = `[{"index":0,"delta":{"tool_calls":[{"index":0}]}}]`
		}
		ChatContent(ctx, []byte(`{"choices":`+choices+extra+`}`), true)
	case "gemini":
		candidates := `[{"index":0,"content":{"parts":[{"text":"private text"}]}}]`
		switch phase {
		case "terminal":
			candidates = `[{"index":0,"finishReason":"STOP"}]`
		case "usage":
			candidates = `[]`
		case "secondary":
			candidates = `[{"index":1,"content":{"parts":[{"text":"private secondary"}]}}]`
		case "tool":
			candidates = `[{"index":0,"content":{"parts":[{"functionCall":{"name":"fixture"}}]}}]`
		}
		GeminiContent(ctx, []byte(`{"response":{"candidates":`+candidates+extra+`}}`), true)
	case "messages":
		switch phase {
		case "terminal":
			Content(ctx, []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"}`+extra+`}`))
			Content(ctx, []byte(`{"type":"message_stop"}`))
		case "usage":
			Content(ctx, []byte(`{"type":"message_delta","delta":{}`+extra+`}`))
		case "tool":
			Content(ctx, []byte(`{"type":"content_block_start","content_block":{"type":"tool_use","name":"fixture"}}`))
		default:
			Content(ctx, []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"private text"}}`))
		}
	}
}

func TestOutputRateOriginalWireProtocols(t *testing.T) {
	for _, protocol := range []string{"chat_completions", "messages", "gemini"} {
		for _, mode := range []string{"complete", "explicit_zero", "missing_reasoning", "invalid_reasoning", "duplicate", "unsafe", "before_only", "late_usage", "late_missing", "tool", "secondary", "capture_gap", "write_error", "missing_terminal"} {
			if mode == "secondary" && protocol == "messages" {
				continue
			}
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				usage := `{"output_tokens":120,"output_tokens_details":{"thinking_tokens":20}}`
				switch protocol {
				case "chat_completions":
					usage = `{"completion_tokens":120,"completion_tokens_details":{"reasoning_tokens":20}}`
				case "gemini":
					usage = `{"candidatesTokenCount":100,"thoughtsTokenCount":20}`
				}
				expected := "estimated"
				switch mode {
				case "explicit_zero":
					usage = strings.ReplaceAll(strings.ReplaceAll(usage, ":120", ":100"), ":20", ":0")
				case "missing_reasoning":
					usage = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(usage, `"thinking_tokens":20`, ""), `"reasoning_tokens":20`, ""), `,"thoughtsTokenCount":20`, "")
					expected = "not_reported"
				case "invalid_reasoning":
					usage = strings.ReplaceAll(usage, ":20", ":-1")
					expected = "invalid_counters"
				case "duplicate":
					key := "output_tokens"
					switch protocol {
					case "chat_completions":
						key = "completion_tokens"
					case "gemini":
						key = "candidatesTokenCount"
					}
					usage = strings.TrimSuffix(usage, "}") + `,"` + key + `":121}`
					expected = "invalid_counters"
				case "unsafe":
					usage = strings.ReplaceAll(strings.ReplaceAll(usage, ":120", ":9007199254740992"), ":100", ":9007199254740992")
					expected = "invalid_counters"
				case "before_only", "late_missing":
					expected = "not_reported"
				case "tool", "secondary":
					expected = "unsupported_output"
				case "capture_gap":
					expected = "incomplete_capture"
				case "write_error", "missing_terminal":
					expected = "partial"
				}
				if protocol == "messages" && mode == "late_usage" {
					expected = "invalid_boundary"
				}
				start := time.Now()
				now := start
				c := newCollector(start, 0, func() time.Time { return now })
				req := httptest.NewRequest(http.MethodPost, "/fixture", nil).WithContext(With(t.Context(), c))
				req, _ = StartAttempt(req, 7, 0)
				req, tr := StartTransport(req)
				for _, ms := range []int{10, 1010} {
					now = start.Add(time.Duration(ms) * time.Millisecond)
					observeOutputRateFixture(req.Context(), protocol, "text", "")
				}
				if mode == "tool" || mode == "secondary" {
					observeOutputRateFixture(req.Context(), protocol, mode, "")
				}
				if mode == "before_only" || mode == "missing_terminal" {
					observeOutputRateFixture(req.Context(), protocol, "usage", usage)
				}
				now = start.Add(2010 * time.Millisecond)
				if mode != "missing_terminal" {
					terminalUsage := usage
					if mode == "before_only" || mode == "late_usage" {
						terminalUsage = ""
					}
					observeOutputRateFixture(req.Context(), protocol, "terminal", terminalUsage)
				}
				now = start.Add(3 * time.Second)
				switch mode {
				case "late_usage":
					observeOutputRateFixture(req.Context(), protocol, "usage", usage)
				case "late_missing":
					observeOutputRateFixture(req.Context(), protocol, "usage", `{}`)
				}
				if mode == "capture_gap" {
					Stage(req.Context(), "typed_content_truncated")
				}
				if mode == "write_error" {
					c.Written(now, 0, io.ErrClosedPipe, false)
				}
				tr.Response(&http.Response{StatusCode: http.StatusOK}, nil)
				c.Finish(http.StatusOK, false)
				c.WhenFinished(func(s Snapshot) {
					if protocol == "messages" && (mode == "late_usage" || mode == "late_missing") {
						require.True(t, s.Attempts[1].TextStream.Invalid, "usage after message_stop contradicts the completed wire")
					}
					rate := DeriveContentObservation(s).OutputRate
					require.Equal(t, expected, rate.Status)
					require.Equal(t, "canonical_"+protocol+"_usage_and_text_events", rate.Source)
					require.Equal(t, "final_physical_http_"+protocol+"_text_at_parser", rate.Scope)
					if expected == "estimated" {
						require.NotNil(t, rate.TokensPerSecond)
						require.InDelta(t, 50, *rate.TokensPerSecond, 1e-9)
						require.Equal(t, 2000.0, *rate.WindowMS, "late usage must not move completion")
						require.EqualValues(t, 100, *rate.NonReasoningTokens)
					} else {
						require.Nil(t, rate.TokensPerSecond)
					}
					raw, err := json.Marshal(s)
					require.NoError(t, err)
					require.NotContains(t, string(raw), "private")
				})
			})
		}
	}
}

func TestOutputRateProtocolCountersAndFinalEvidence(t *testing.T) {
	for _, tc := range []struct{ protocol, name, usage, status string }{
		{"chat_completions", "zero_optional", `{"completion_tokens":120,"completion_tokens_details":{"reasoning_tokens":20,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}`, "observed"},
		{"chat_completions", "audio", `{"completion_tokens":120,"completion_tokens_details":{"reasoning_tokens":20,"audio_tokens":1}}`, "unsupported_usage"},
		{"chat_completions", "prediction", `{"completion_tokens":120,"completion_tokens_details":{"reasoning_tokens":20,"accepted_prediction_tokens":1}}`, "unsupported_usage"},
		{"chat_completions", "rejected_prediction", `{"completion_tokens":120,"completion_tokens_details":{"reasoning_tokens":20,"rejected_prediction_tokens":1}}`, "unsupported_usage"},
		{"chat_completions", "unknown_detail", `{"completion_tokens":120,"completion_tokens_details":{"reasoning_tokens":20,"future":0}}`, "unsupported_usage"},
		{"chat_completions", "alias", `{"completion_tokens":120,"completion_tokens_details":{"reasoning_tokens":20},"output_tokens":120}`, "unsupported_usage"},
		{"chat_completions", "invalid_optional", `{"completion_tokens":120,"completion_tokens_details":{"reasoning_tokens":20,"audio_tokens":null}}`, "invalid_counters"},
		{"messages", "wrong_detail", `{"output_tokens":120,"output_tokens_details":{"reasoning_tokens":20}}`, "not_reported"},
		{"messages", "alias", `{"output_tokens":120,"output_tokens_details":{"thinking_tokens":20},"completion_tokens":120}`, "unsupported_usage"},
		{"messages", "other_detail", `{"output_tokens":120,"output_tokens_details":{"thinking_tokens":20,"future":0}}`, "unsupported_usage"},
		{"messages", "string", `{"output_tokens":120,"output_tokens_details":{"thinking_tokens":"20"}}`, "invalid_counters"},
		{"messages", "excess", `{"output_tokens":120,"output_tokens_details":{"thinking_tokens":121}}`, "invalid_counters"},
		{"gemini", "text_modality", `{"candidatesTokenCount":100,"thoughtsTokenCount":20,"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":100}]}`, "observed"},
		{"gemini", "audio_modality", `{"candidatesTokenCount":100,"thoughtsTokenCount":20,"candidatesTokensDetails":[{"modality":"AUDIO","tokenCount":100}]}`, "unsupported_usage"},
		{"gemini", "modality_total", `{"candidatesTokenCount":100,"thoughtsTokenCount":20,"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":99}]}`, "invalid_counters"},
		{"gemini", "empty_modality", `{"candidatesTokenCount":100,"thoughtsTokenCount":20,"candidatesTokensDetails":[]}`, "invalid_counters"},
		{"gemini", "null_modality", `{"candidatesTokenCount":100,"thoughtsTokenCount":20,"candidatesTokensDetails":null}`, "invalid_counters"},
		{"gemini", "duplicate_modality", `{"candidatesTokenCount":100,"thoughtsTokenCount":20,"candidatesTokensDetails":[{"modality":"TEXT","modality":"AUDIO","tokenCount":100}]}`, "invalid_counters"},
		{"gemini", "sum_overflow", `{"candidatesTokenCount":9007199254740991,"thoughtsTokenCount":1}`, "invalid_counters"},
		{"gemini", "null_thoughts", `{"candidatesTokenCount":100,"thoughtsTokenCount":null}`, "invalid_counters"},
		{"gemini", "fractional", `{"candidatesTokenCount":100.5,"thoughtsTokenCount":20}`, "invalid_counters"},
		{"gemini", "alias", `{"candidatesTokenCount":100,"thoughtsTokenCount":20,"completion_tokens":120}`, "unsupported_usage"},
	} {
		t.Run(tc.protocol+"/"+tc.name, func(t *testing.T) {
			raw := readOutputUsage([]byte(tc.usage), tc.protocol)
			require.NotNil(t, raw)
			require.Equal(t, tc.status, raw.Status)
			require.False(t, raw.Final, "count decoding alone does not prove a final report")
			if raw.Status == "observed" {
				var legacy struct {
					ContentStatus string `json:"content_status"`
				}
				data, err := json.Marshal(raw)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(data, &legacy))
				require.NotEqual(t, "text_and_reasoning", legacy.ContentStatus, "the original reader's content gate must reject new protocols even after unknown JSON fields are discarded")
			}
		})
	}
	for _, protocol := range []string{"chat_completions", "messages", "gemini"} {
		t.Run(protocol+"/sticky_invalid", func(t *testing.T) {
			a := Attempt{}
			updateProtocolOutput(&a, &OutputUsage{Protocol: protocol, Status: "invalid_counters"}, false)
			updateProtocolOutput(&a, &OutputUsage{Protocol: protocol, Status: "observed"}, true)
			require.Equal(t, "invalid_counters", a.OutputUsage.Status)
		})
		t.Run(protocol+"/cumulative_decrease", func(t *testing.T) {
			a := Attempt{}
			updateProtocolOutput(&a, &OutputUsage{Protocol: protocol, Status: "observed", OutputTokens: 120, ReasoningTokens: 20}, false)
			updateProtocolOutput(&a, &OutputUsage{Protocol: protocol, Status: "observed", OutputTokens: 100, ReasoningTokens: 10}, true)
			require.Equal(t, "invalid_counters", a.OutputUsage.Status)
		})
		for _, mode := range []string{"wrong_protocol", "not_final", "ws", "new_attempt"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				s := outputRateTestSnapshot()
				s.Attempts[0].OutputUsage.Protocol, s.Attempts[0].OutputUsage.Final = protocol, true
				s.Attempts[0].TextStream.Protocol = protocol
				expected := "invalid_protocol"
				switch mode {
				case "wrong_protocol":
					s.Attempts[0].TextStream.Protocol = "responses"
				case "not_final":
					s.Attempts[0].OutputUsage.Final = false
					expected = "not_reported"
				case "ws":
					s.TTFTMode, s.Attempts[0].Kind = "ws_turn", "ws_turn"
				case "new_attempt":
					s.Attempts = append(s.Attempts, Attempt{Kind: "http"})
					expected = "not_collected"
				}
				rate := DeriveContentObservation(s).OutputRate
				require.Equal(t, expected, rate.Status)
				require.Nil(t, rate.TokensPerSecond)
			})
		}
	}
}

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
