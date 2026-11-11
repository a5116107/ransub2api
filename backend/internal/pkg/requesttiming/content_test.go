package requesttiming

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIssue605TypedContentDoesNotInferReasoning(t *testing.T) {
	for _, tc := range []struct {
		data, event  string
		reason, text bool
	}{
		{`{"type":"response.output_item.added","item":{"type":"message"}}`, "", false, false},
		{`{"type":"response.output_item.added","item":{"type":"function_call"}}`, "", false, false},
		{`{"type":"response.output_item.done","item":{"type":"reasoning"}}`, "", true, false},
		{`{"type":"response.reasoning_summary_text.delta","delta":"summary"}`, "", true, false},
		{`{"type":"response.output_text.delta","delta":"visible"}`, "", false, true},
		{`{"delta":"visible"}`, "response.output_text.delta", false, true},
		{`{"delta":"summary"}`, "response.reasoning_summary_text.delta", true, false},
		{`{"type":"response.created"}`, "response.output_text.delta", false, false},
	} {
		t.Run(tc.data+tc.event, func(t *testing.T) {
			c := New(time.Now(), 0)
			r := httptest.NewRequest("POST", "/", nil).WithContext(With(context.Background(), c))
			r, _ = StartAttempt(r, 1, 0)
			r, tr := StartTransport(r)
			Content(r.Context(), []byte(tc.data), tc.event)
			tr.Response(nil, nil)
			c.Finish(200, false)
			c.WhenFinished(func(s Snapshot) {
				_, reason := s.Attempts[1].Events["first_reasoning"]
				_, text := s.Attempts[1].Events["first_visible_text"]
				require.Equal(t, tc.reason, reason)
				require.Equal(t, tc.text, text)
				v := DeriveContentObservation(s)
				require.Equal(t, tc.reason, v.ReasoningFirstMS != nil)
				require.Equal(t, tc.text, v.VisibleTextFirstMS != nil)
				require.Equal(t, "unknown", v.ReasoningStreamingCapability)
				require.Equal(t, "none", v.CapabilitySource)
				require.Nil(t, v.ClientSocket.MinRTTMS)
				require.Nil(t, v.UpstreamSocket.Retransmissions)
			})
		})
	}
}

func TestIssue605HistoricalAndProxySocketEvidenceStaysUnknown(t *testing.T) {
	v := DeriveContentObservation(Snapshot{Events: map[string]float64{"first_semantic": 10, "first_visible": 20}})
	require.Nil(t, v.ReasoningFirstMS)
	require.Nil(t, v.VisibleTextFirstMS)
	end := 100.0
	v = DeriveContentObservation(Snapshot{Attempts: []Attempt{{Kind: "http", StartMS: 0, EndMS: &end, ProxyID: 8, Events: map[string]float64{"first_byte": 5}}}})
	require.Equal(t, 5.0, *v.ResponseByteFirstMS)
	require.Equal(t, "proxy_obscured", v.UpstreamSocket.Status)
	require.Nil(t, v.UpstreamSocket.MinRTTMS)
}

func TestIssue605ContentAfterTransportEOFKeepsParserObservation(t *testing.T) {
	end := 100.0
	s := Snapshot{TotalMS: 300, Attempts: []Attempt{{Kind: "http", StartMS: 0, EndMS: &end, Events: map[string]float64{"headers_written": 10, "request_written": 20, "first_byte": 30, "first_reasoning": 150, "first_visible_text": 200, "first_semantic": 150, "first_visible": 200}}}}
	v := DeriveContentObservation(s)
	require.NotNil(t, v.ReasoningFirstMS, "buffered SSE may be parsed after transport EOF")
	require.Equal(t, 150.0, *v.ReasoningFirstMS, "buffered SSE may be parsed after transport EOF")
	require.Equal(t, 200.0, *v.VisibleTextFirstMS)
	require.Equal(t, 130.0, *DeriveLatency(s).Attempts[0].SemanticWait.MS)
	s.Attempts[0].Events["first_reasoning"] = 400
	require.Nil(t, DeriveContentObservation(s).ReasoningFirstMS, "request completion still bounds parser events")
}

func TestContentCompletedTextIsVisible(t *testing.T) {
	for _, event := range []string{"response.completed", "response.done"} {
		c := New(time.Now(), 0)
		r := httptest.NewRequest("POST", "/", nil).WithContext(With(t.Context(), c))
		r, _ = StartAttempt(r, 1, 0)
		r, tr := StartTransport(r)
		Content(r.Context(), []byte(`{"type":"`+event+`","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"complete answer"}]}]}}`))
		tr.Response(nil, nil)
		c.Finish(200, false)
		c.WhenFinished(func(s Snapshot) {
			require.NotNil(t, DeriveContentObservation(s).VisibleTextFirstMS, "terminal-only text is visible even without a preceding delta")
		})
	}
}

func TestTextStreamCountsContentNotHeartbeats(t *testing.T) {
	start := time.Now()
	now := start
	c := newCollector(start, 0, func() time.Time { return now })
	r := httptest.NewRequest("POST", "/", nil).WithContext(With(t.Context(), c))
	r, _ = StartAttempt(r, 1, 0)
	r, tr := StartTransport(r)
	for _, event := range []struct {
		ms   int
		data string
	}{
		{10, `{"type":"response.output_text.delta","delta":"first"}`},
		{80, `{"type":"keepalive"}`},
		{100, `{"type":"response.reasoning_summary_text.delta","delta":"reasoning"}`},
		{150, `{"type":"response.function_call_arguments.delta","delta":"{}"}`},
		{210, `{"type":"response.output_text.delta","delta":"second"}`},
		{250, `{"type":"response.output_text.done","text":"firstsecond"}`},
		{300, `{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"firstsecond"}]}]}}`},
	} {
		now = start.Add(time.Duration(event.ms) * time.Millisecond)
		Content(r.Context(), []byte(event.data))
	}
	Output(r.Context(), false, false, "completed")
	tr.Response(&http.Response{StatusCode: http.StatusOK}, nil)
	c.Finish(200, false)
	c.WhenFinished(func(s Snapshot) {
		payload, err := json.Marshal(DeriveContentObservation(s))
		require.NoError(t, err)
		var projection map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(payload, &projection))
		require.Contains(t, projection, "text_stream", "text gaps need their own bounded observation, not the first-token timestamp")
		var stream struct {
			Chunks     int     `json:"chunks"`
			GapSamples int     `json:"gap_samples"`
			SpanMS     float64 `json:"span_ms"`
			MaxGapMS   float64 `json:"max_gap_ms"`
			Status     string  `json:"status"`
		}
		require.NoError(t, json.Unmarshal(projection["text_stream"], &stream))
		require.Equal(t, 2, stream.Chunks)
		require.Equal(t, 1, stream.GapSamples)
		require.Equal(t, 200.0, stream.SpanMS)
		require.Equal(t, 200.0, stream.MaxGapMS)
		require.Equal(t, "observed", stream.Status)
	})
}

func textStreamTestSnapshot() Snapshot {
	end := 400.0
	return Snapshot{Status: http.StatusOK, TotalMS: 500, Attempts: []Attempt{{Kind: "http", StartMS: 0, EndMS: &end, Status: http.StatusOK, Terminal: "completed",
		Events:     map[string]float64{"first_visible_text": 10},
		TextStream: &TextStreamTiming{Chunks: 3, FirstMS: 10, LastMS: 210, MaxGapMS: 150, Terminal: "completed", TerminalMS: 300},
	}}}
}

func TestTextStreamProjectionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Snapshot)
		status string
	}{
		{"complete", func(*Snapshot) {}, "observed"},
		{"historical", func(s *Snapshot) { s.Attempts[0].TextStream = nil }, "not_collected"},
		{"diagnostic capture discarded", func(s *Snapshot) { s.Attempts[0].Events["typed_content_truncated"] = 20 }, "incomplete_capture"},
		{"missing transport end", func(s *Snapshot) { s.Attempts[0].EndMS = nil }, "invalid_boundary"},
		{"unsupported transport", func(s *Snapshot) { s.Attempts[0].Kind = "ws" }, "not_collected"},
		{"trace truncated", func(s *Snapshot) { s.Truncated = true }, "not_collected"},
		{"aggregate truncated", func(s *Snapshot) { s.Attempts[0].TextStream.Truncated = true }, "partial"},
		{"invalid clock", func(s *Snapshot) { s.Attempts[0].TextStream.Invalid = true }, "invalid_boundary"},
		{"future delta", func(s *Snapshot) { s.Attempts[0].TextStream.LastMS = 501 }, "invalid_boundary"},
		{"delta before attempt", func(s *Snapshot) { s.Attempts[0].TextStream.FirstMS = -1 }, "invalid_boundary"},
		{"negative span", func(s *Snapshot) { s.Attempts[0].TextStream.LastMS = 1 }, "invalid_boundary"},
		{"gap exceeds span", func(s *Snapshot) { s.Attempts[0].TextStream.MaxGapMS = 201 }, "invalid_boundary"},
		{"gap below mean", func(s *Snapshot) { s.Attempts[0].TextStream.MaxGapMS = 99 }, "invalid_boundary"},
		{"negative gap", func(s *Snapshot) { s.Attempts[0].TextStream.MaxGapMS = -1 }, "invalid_boundary"},
		{"infinite gap", func(s *Snapshot) { s.Attempts[0].TextStream.MaxGapMS = math.Inf(1) }, "invalid_boundary"},
		{"NaN first", func(s *Snapshot) { s.Attempts[0].TextStream.FirstMS = math.NaN() }, "invalid_boundary"},
		{"counter overflow", func(s *Snapshot) { s.Attempts[0].TextStream.Chunks = maxTextStreamChunks + 1 }, "invalid_boundary"},
		{"terminal before last", func(s *Snapshot) { s.Attempts[0].TextStream.TerminalMS = 209 }, "invalid_boundary"},
		{"future terminal", func(s *Snapshot) { s.Attempts[0].TextStream.TerminalMS = 501 }, "invalid_boundary"},
		{"unknown terminal", func(s *Snapshot) { s.Attempts[0].TextStream.Terminal = "unknown" }, "invalid_boundary"},
		{"EOF without terminal", func(s *Snapshot) {
			s.Attempts[0].TextStream.Terminal = ""
			s.Attempts[0].TextStream.TerminalMS = 0
			s.Attempts[0].Terminal = ""
		}, "partial"},
		{"failed terminal", func(s *Snapshot) { s.Attempts[0].TextStream.Terminal = "failed"; s.Attempts[0].Terminal = "failed" }, "partial"},
		{"incomplete terminal", func(s *Snapshot) {
			s.Attempts[0].TextStream.Terminal = "incomplete"
			s.Attempts[0].Terminal = "incomplete"
		}, "partial"},
		{"missing parser acceptance", func(s *Snapshot) { s.Attempts[0].Terminal = "" }, "partial"},
		{"upstream transport error", func(s *Snapshot) { s.Attempts[0].Error = "transport_error" }, "partial"},
		{"upstream failure", func(s *Snapshot) { s.Attempts[0].Status = 502 }, "partial"},
		{"unobserved upstream status", func(s *Snapshot) { s.Attempts[0].Status = 0 }, "partial"},
		{"request canceled", func(s *Snapshot) { s.Canceled = true }, "partial"},
		{"client disconnected", func(s *Snapshot) { s.ClientDisconnect = true }, "partial"},
		{"downstream write failure", func(s *Snapshot) { s.DownstreamError = true }, "partial"},
		{"failed business outcome", func(s *Snapshot) { s.Outcome = "error" }, "partial"},
		{"downstream failure", func(s *Snapshot) { s.Status = 502 }, "partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := textStreamTestSnapshot()
			tc.mutate(&s)
			v := DeriveContentObservation(s).TextStream
			require.Equal(t, tc.status, v.Status)
			if tc.status == "not_collected" || tc.status == "invalid_boundary" || tc.status == "incomplete_capture" {
				require.Nil(t, v.Chunks)
				require.Nil(t, v.GapSamples)
				require.Nil(t, v.SpanMS)
				require.Nil(t, v.MaxGapMS)
				require.Nil(t, v.TailWaitMS)
			} else {
				require.Equal(t, 3, *v.Chunks)
				require.Equal(t, 2, *v.GapSamples)
				require.Equal(t, 200.0, *v.SpanMS)
				require.Equal(t, 150.0, *v.MaxGapMS)
			}
		})
	}
}

func TestTextStreamSingleAndMeasuredZero(t *testing.T) {
	s := textStreamTestSnapshot()
	s.Attempts[0].TextStream.LastMS = 10
	s.Attempts[0].TextStream.MaxGapMS = 0
	s.Attempts[0].TextStream.Chunks = 1
	v := DeriveContentObservation(s).TextStream
	require.Equal(t, "observed", v.Status)
	require.Equal(t, 0, *v.GapSamples)
	require.Nil(t, v.SpanMS)
	require.Nil(t, v.MaxGapMS, "a single delta does not measure inter-delta latency")
	require.Equal(t, 290.0, *v.TailWaitMS)
	s.Attempts[0].TextStream.Chunks = 2
	v = DeriveContentObservation(s).TextStream
	require.Equal(t, "observed", v.Status)
	require.Equal(t, 0.0, *v.MaxGapMS, "two same-clock deltas are a measured zero, unlike a single delta")
	require.Equal(t, 0.0, *v.SpanMS)
	// Buffered content is bounded by request completion, not network EOF.
	end := 5.0
	s.Attempts[0].EndMS = &end
	require.Equal(t, "observed", DeriveContentObservation(s).TextStream.Status)
	// A later retry/egress parent must not borrow the old attempt's stream.
	s.Attempts = append(s.Attempts, Attempt{Kind: "egress"})
	require.Equal(t, "not_collected", DeriveContentObservation(s).TextStream.Status)
}

func TestTextStreamCollectorBoundsAndSnapshotIsolation(t *testing.T) {
	start := time.Now()
	now := start.Add(time.Millisecond)
	c := newCollector(start, 0, func() time.Time { return now })
	r := httptest.NewRequest("POST", "/", nil).WithContext(With(t.Context(), c))
	r, _ = StartAttempt(r, 1, 0)
	r, tr := StartTransport(r)
	delta := []byte(`{"type":"response.output_text.delta","delta":"private generated text"}`)
	Content(r.Context(), delta)
	c.data.Attempts[tr.index].TextStream.Chunks = maxTextStreamChunks
	Content(r.Context(), delta)
	require.True(t, c.data.Attempts[tr.index].TextStream.Truncated)
	require.Equal(t, maxTextStreamChunks, c.data.Attempts[tr.index].TextStream.Chunks)
	Content(r.Context(), []byte(`{"type":"response.completed"}`))
	Content(r.Context(), delta)
	tr.Response(&http.Response{StatusCode: http.StatusOK}, nil)
	c.Finish(200, false)
	c.WhenFinished(func(s Snapshot) {
		v := DeriveContentObservation(s).TextStream
		require.Equal(t, "partial", v.Status)
		require.Nil(t, v.TailWaitMS, "a saturated prefix has no observed last text boundary")
		s.Attempts[tr.index].TextStream.Chunks = 7
		payload, err := json.Marshal(s)
		require.NoError(t, err)
		require.NotContains(t, string(payload), "private generated text")
	})
	c.WhenFinished(func(s Snapshot) { require.Equal(t, maxTextStreamChunks, s.Attempts[tr.index].TextStream.Chunks) })
	Content(r.Context(), delta)
	require.Equal(t, maxTextStreamChunks, c.data.Attempts[tr.index].TextStream.Chunks, "finished collectors never change")

	// Non-monotonic observation clocks invalidate the aggregate, not the request.
	c = newCollector(start, 0, func() time.Time { return now })
	r = httptest.NewRequest("POST", "/", nil).WithContext(With(t.Context(), c))
	r, _ = StartAttempt(r, 1, 0)
	r, tr = StartTransport(r)
	Content(r.Context(), delta)
	now = start
	Content(r.Context(), delta)
	require.True(t, c.data.Attempts[tr.index].TextStream.Invalid)
}

func TestTextStreamConcurrentCapture(t *testing.T) {
	c := New(time.Now(), 0)
	r := httptest.NewRequest("POST", "/", nil).WithContext(With(t.Context(), c))
	r, _ = StartAttempt(r, 1, 0)
	r, tr := StartTransport(r)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				Content(r.Context(), []byte(`{"type":"response.output_text.delta","delta":"fragment"}`))
			}
		})
	}
	wg.Wait()
	Content(r.Context(), []byte(`{"type":"response.completed"}`))
	Output(r.Context(), false, false, "completed")
	tr.Response(&http.Response{StatusCode: http.StatusOK}, nil)
	c.Finish(200, false)
	c.WhenFinished(func(s Snapshot) {
		v := DeriveContentObservation(s).TextStream
		require.Equal(t, "observed", v.Status)
		require.Equal(t, 800, *v.Chunks)
		require.Equal(t, 799, *v.GapSamples)
		require.Nil(t, s.Attempts[0].TextStream, "physical counts must not be duplicated on the enclosing egress")
	})
}

func TestTextStreamAttemptAndTerminalIsolation(t *testing.T) {
	start := time.Now()
	now := start
	c := newCollector(start, 0, func() time.Time { return now })
	r := httptest.NewRequest("POST", "/", nil).WithContext(With(t.Context(), c))
	r, _ = StartAttempt(r, 1, 0)
	r, first := StartTransport(r)
	now = start.Add(10 * time.Millisecond)
	Content(r.Context(), []byte(`{"type":"response.output_text.delta","delta":"old route"}`))
	Content(r.Context(), []byte(`{"type":"response.failed"}`))
	now = start.Add(20 * time.Millisecond)
	Content(r.Context(), []byte(`{"type":"response.output_text.delta","delta":"after terminal"}`))
	require.Equal(t, 1, c.data.Attempts[first.index].TextStream.Chunks)
	require.Equal(t, 10.0, c.data.Attempts[first.index].TextStream.LastMS)
	first.Response(&http.Response{StatusCode: http.StatusOK}, nil)
	r = httptest.NewRequest("POST", "/", nil).WithContext(With(t.Context(), c))
	r, _ = StartAttempt(r, 2, 0)
	r, last := StartTransport(r)
	Content(r.Context(), []byte(`{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"new route"}]}]}}`))
	Content(r.Context(), []byte(`{"type":"response.output_text.delta","delta":"after completed"}`))
	last.Response(&http.Response{StatusCode: http.StatusOK}, nil)
	c.Finish(200, false)
	c.WhenFinished(func(s Snapshot) {
		v := DeriveContentObservation(s)
		require.Equal(t, 20.0, *v.VisibleTextFirstMS)
		require.Equal(t, "not_collected", v.TextStream.Status, "the final route must not inherit either an earlier route or post-terminal chunks")
		require.Nil(t, v.TextStream.Chunks)
		require.Equal(t, 1, s.Attempts[first.index].TextStream.Chunks)
	})
}
