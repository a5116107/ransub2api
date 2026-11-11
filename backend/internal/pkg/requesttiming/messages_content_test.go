package requesttiming

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMessagesTypedContent(t *testing.T) {
	for _, tc := range []struct {
		name, data, event string
		reasoning, text   bool
	}{
		{"text delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"answer"}}`, "", false, true},
		{"event type fallback", `{"delta":{"type":"text_delta","text":"answer"}}`, "content_block_delta", false, true},
		{"initial text", `{"type":"content_block_start","content_block":{"type":"text","text":"answer"}}`, "", false, true},
		{"thinking delta", `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"private thinking"}}`, "", true, false},
		{"thinking block", `{"type":"content_block_start","content_block":{"type":"thinking","thinking":""}}`, "", true, false},
		{"signature", `{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"private signature"}}`, "", false, false},
		{"tool arguments", `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{}"}}`, "", false, false},
		{"empty delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":""}}`, "", false, false},
		{"heartbeat", `{"type":"ping"}`, "", false, false},
		{"message start", `{"type":"message_start","message":{"content":[],"usage":{"output_tokens":1}}}`, "", false, false},
		{"wrong delta shape", `{"type":"content_block_delta","delta":"answer"}`, "", false, false},
		{"payload type wins", `{"type":"ping","delta":{"type":"text_delta","text":"answer"}}`, "content_block_delta", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(time.Now(), 0)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(With(t.Context(), c))
			req, _ = StartAttempt(req, 1, 0)
			req, tr := StartTransport(req)
			Content(req.Context(), []byte(tc.data), tc.event)
			tr.Response(&http.Response{StatusCode: http.StatusOK}, nil)
			c.Finish(http.StatusOK, false)
			c.WhenFinished(func(s Snapshot) {
				v := DeriveContentObservation(s)
				require.Equal(t, tc.reasoning, v.ReasoningFirstMS != nil)
				require.Equal(t, tc.text, v.VisibleTextFirstMS != nil)
				require.Equal(t, "unknown", v.ReasoningStreamingCapability)
				require.Nil(t, v.OutputRate.TokensPerSecond, "Messages usage does not separate reasoning tokens")
				raw, err := json.Marshal(s)
				require.NoError(t, err)
				require.NotContains(t, string(raw), "private thinking")
				require.NotContains(t, string(raw), "private signature")
				require.NotContains(t, string(raw), "answer")
			})
		})
	}
}

func TestMessagesTextStreamBoundaries(t *testing.T) {
	for _, ending := range []string{"completed", "error", "eof", "done_marker", "disconnected", "write_error", "failed_outcome", "capture_gap"} {
		t.Run(ending, func(t *testing.T) {
			start := time.Now()
			now := start
			c := newCollector(start, 0, func() time.Time { return now })
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(With(t.Context(), c))
			req, _ = StartAttempt(req, 7, 0)
			req, tr := StartTransport(req)
			for _, event := range []struct {
				ms   int
				data string
			}{
				{10, `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"private thinking"}}`},
				{20, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"first"}}`},
				{100, `{"type":"ping"}`},
				{130, `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"more thinking"}}`},
				{150, `{"type":"content_block_delta","delta":{"type":"text_delta","text":""}}`},
				{320, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"last"}}`},
				{350, `{"type":"message_delta","usage":{"output_tokens":200}}`},
			} {
				now = start.Add(time.Duration(event.ms) * time.Millisecond)
				Content(req.Context(), []byte(event.data))
			}
			now = start.Add(400 * time.Millisecond)
			switch ending {
			case "error":
				Content(req.Context(), []byte(`{"type":"error","error":{"type":"overloaded_error"}}`))
			case "eof":
			case "done_marker":
				Content(req.Context(), []byte(`[DONE]`))
			default:
				Content(req.Context(), []byte(`{"type":"message_stop"}`))
			}
			if ending == "capture_gap" {
				Stage(req.Context(), "typed_content_truncated")
			}
			tr.Response(&http.Response{StatusCode: http.StatusOK}, nil)
			if ending == "disconnected" {
				Outcome(req.Context(), "canceled", true)
			}
			if ending == "write_error" {
				c.data.DownstreamError = true
			}
			if ending == "failed_outcome" {
				Outcome(req.Context(), "error", false)
			}
			now = start.Add(450 * time.Millisecond)
			c.Finish(http.StatusOK, false)
			c.WhenFinished(func(s Snapshot) {
				v := DeriveContentObservation(s)
				require.Equal(t, "typed_messages_sse", v.TextStream.Source)
				require.Equal(t, "final_physical_http_messages_text_deltas_at_parser", v.TextStream.Scope)
				if ending == "capture_gap" {
					require.Equal(t, "incomplete_capture", v.TextStream.Status)
					require.Nil(t, v.TextStream.MaxGapMS)
					require.Nil(t, v.ReasoningFirstMS)
					require.Nil(t, v.VisibleTextFirstMS)
					return
				}
				require.Equal(t, 10.0, *v.ReasoningFirstMS)
				require.Equal(t, 20.0, *v.VisibleTextFirstMS)
				require.Equal(t, 2, *v.TextStream.Chunks)
				require.Equal(t, 300.0, *v.TextStream.MaxGapMS)
				if ending == "completed" {
					require.Equal(t, "observed", v.TextStream.Status)
					require.Equal(t, 80.0, *v.TextStream.TailWaitMS)
				} else {
					require.Equal(t, "partial", v.TextStream.Status)
				}
				require.Nil(t, v.Cache.ReadRatio, "Responses counters are not inferred from Messages usage")
				require.Nil(t, v.OutputRate.TokensPerSecond)
			})
		})
	}
}

func TestMessagesTextStreamFinalAttemptIsolation(t *testing.T) {
	for _, finalText := range []bool{false, true} {
		name := "terminal_only"
		if finalText {
			name = "text"
		}
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			now := start
			c := newCollector(start, 0, func() time.Time { return now })
			root := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(With(t.Context(), c))
			first, _ := StartAttempt(root, 7, 0)
			first, trace := StartTransport(first)
			Content(first.Context(), []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"old private text"}}`))
			Content(first.Context(), []byte(`{"type":"error","error":{"type":"overloaded_error"}}`))
			trace.Response(&http.Response{StatusCode: http.StatusServiceUnavailable}, nil)
			now = start.Add(100 * time.Millisecond)
			last, _ := StartAttempt(root, 9, 0)
			last, trace = StartTransport(last)
			if finalText {
				Content(last.Context(), []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"new private text"}}`))
			}
			now = start.Add(300 * time.Millisecond)
			Content(last.Context(), []byte(`{"type":"message_stop"}`))
			// Later events cannot rewrite the first terminal or its counters,
			// but contradictory output/error invalidates the healthy sample.
			Content(last.Context(), []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"late private text"}}`))
			Content(last.Context(), []byte(`{"type":"error","error":{"type":"overloaded_error"}}`))
			trace.Response(&http.Response{StatusCode: http.StatusOK}, nil)
			c.Finish(http.StatusOK, false)
			c.WhenFinished(func(s Snapshot) {
				require.Len(t, s.Attempts, 4)
				require.Equal(t, "failed", s.Attempts[1].TextStream.Terminal)
				require.Equal(t, "completed", s.Attempts[3].TextStream.Terminal)
				require.Equal(t, "completed", s.Terminal)
				v := DeriveContentObservation(s)
				require.Equal(t, finalText, v.VisibleTextFirstMS != nil)
				if finalText {
					require.Equal(t, 100.0, *v.VisibleTextFirstMS)
					require.Equal(t, 1, s.Attempts[3].TextStream.Chunks)
					require.Equal(t, "invalid_boundary", v.TextStream.Status)
					require.Nil(t, v.TextStream.Chunks)
					require.Nil(t, v.TextStream.MaxGapMS)
				} else {
					require.Equal(t, "not_collected", v.TextStream.Status)
					require.Nil(t, v.TextStream.Chunks)
				}
			})
		})
	}
}

func TestContentErrorBeforeTextIsNotRecoveredByLaterTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, delta, terminal string
	}{
		{"messages", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"late text"}}`, `{"type":"message_stop"}`},
		{"responses", `{"type":"response.output_text.delta","delta":"late text"}`, `{"type":"response.completed"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(time.Now(), 0)
			req := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(With(t.Context(), c))
			req, _ = StartAttempt(req, 1, 0)
			req, trace := StartTransport(req)
			Content(req.Context(), []byte(`{"type":"error","error":{"type":"overloaded_error"}}`))
			Content(req.Context(), []byte(tc.delta))
			Content(req.Context(), []byte(tc.terminal))
			trace.Response(&http.Response{StatusCode: http.StatusOK}, nil)
			c.Finish(http.StatusOK, false)
			c.WhenFinished(func(s Snapshot) {
				require.Equal(t, "failed", s.Attempts[1].Terminal)
				require.Equal(t, "failed", s.Terminal)
				v := DeriveContentObservation(s)
				require.Nil(t, v.VisibleTextFirstMS)
				require.Nil(t, v.TextStream.Chunks)
				require.Nil(t, v.OutputRate.TokensPerSecond)
			})
		})
	}
}

func TestMessagesCaptureGapBeforeContentKeepsProtocol(t *testing.T) {
	c := New(time.Now(), 0)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(With(t.Context(), c))
	req, _ = StartAttempt(req, 1, 0)
	req, trace := StartTransport(req)
	Stage(req.Context(), "typed_content_truncated")
	trace.Response(&http.Response{StatusCode: http.StatusOK}, nil)
	c.Finish(http.StatusOK, false)
	c.WhenFinished(func(s Snapshot) {
		v := DeriveContentObservation(s)
		require.Equal(t, "incomplete_capture", v.TextStream.Status)
		require.Equal(t, "typed_messages_sse", v.TextStream.Source)
		require.Equal(t, "final_physical_http_messages_text_deltas_at_parser", v.TextStream.Scope)
		require.Nil(t, v.VisibleTextFirstMS)
		require.Nil(t, v.TextStream.Chunks)
	})
}
