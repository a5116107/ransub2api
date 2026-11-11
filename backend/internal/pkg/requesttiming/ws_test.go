package requesttiming

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWSTurnTimingIndependentGenerationAndTypedContent(t *testing.T) {
	start := time.Now()
	for i := range 140 {
		w := NewWSTurn(start.Add(time.Duration(i)*time.Hour), 17, 3, [32]byte{1})
		now := w.start
		w.now = func() time.Time { return now }
		for _, e := range []struct {
			ms  int
			raw string
		}{
			{5, `{"type":"response.created"}`},
			{10, `{"type":"response.reasoning_summary_text.delta","delta":"private-thought"}`},
			{20, `{"type":"response.output_text.delta","delta":"private-answer"}`},
			{200, `{"type":"ping"}`},
			{320, `{"type":"response.output_text.delta","delta":"private-answer"}`},
			{400, `{"type":"response.completed","response":{"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":80}}}}`},
		} {
			now = w.start.Add(time.Duration(e.ms) * time.Millisecond)
			w.Content([]byte(e.raw))
		}
		now = w.start.Add(450 * time.Millisecond)
		w.Finish(nil, false, false)
		w.WhenFinished(func(s Snapshot) {
			require.Equal(t, "ws_turn", s.TTFTMode)
			require.Zero(t, s.Status)
			require.Len(t, s.Attempts, 1)
			require.Equal(t, "ws_turn", s.Attempts[0].Kind)
			require.EqualValues(t, 17, s.Attempts[0].AccountID)
			require.Len(t, s.Attempts[0].RouteFingerprint, 64)
			require.Equal(t, "not_collected", s.Latency.Status)
			v := DeriveContentObservation(s)
			require.Equal(t, "typed_responses_ws", v.Source)
			require.Equal(t, 10.0, *v.ReasoningFirstMS)
			require.Equal(t, 20.0, *v.VisibleTextFirstMS)
			require.Nil(t, v.ResponseByteFirstMS)
			require.Equal(t, "observed", v.TextStream.Status)
			require.Equal(t, 2, *v.TextStream.Chunks)
			require.Equal(t, 300.0, *v.TextStream.MaxGapMS)
			require.Equal(t, 80.0, *v.TextStream.TailWaitMS)
			require.Equal(t, "observed", v.Cache.Status)
			require.InDelta(t, 0.8, *v.Cache.ReadRatio, 1e-9)
			require.Equal(t, "physical_websocket_turn_responses_terminal_usage", v.Cache.Scope)
			require.Nil(t, v.OutputRate.TokensPerSecond)
			raw, err := json.Marshal(s)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "private-")
			require.Less(t, len(raw), 4096)
		})
	}
}

func TestWSTurnCanonicalCacheAndOutputRateStayWithinGeneration(t *testing.T) {
	for turn := range 3 {
		w := NewWSTurn(time.Now().Add(time.Duration(turn)*time.Hour), 17, 3, [32]byte{1})
		now := w.start
		w.now = func() time.Time { return now }
		usage := `"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":900,"cache_write_tokens":50},"output_tokens":120,"output_tokens_details":{"reasoning_tokens":20}},`
		switch turn {
		case 1:
			usage = ""
		case 2:
			usage = `"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":0},"output_tokens":120,"output_tokens_details":{"reasoning_tokens":0}},`
		}
		for _, e := range []struct {
			ms  int
			raw string
		}{
			{10, `{"type":"response.reasoning_summary_text.delta","delta":"private-thought"}`},
			{20, `{"type":"response.output_text.delta","delta":"private-answer"}`},
			{200, `{"type":"ping"}`},
			{320, `{"type":"response.output_text.delta","delta":"private-answer"}`},
			{420, `{"type":"response.completed","response":{` + usage + `"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"private-answer"}]}]}}`},
		} {
			now = w.start.Add(time.Duration(e.ms) * time.Millisecond)
			w.Content([]byte(e.raw))
		}
		now = w.start.Add(450 * time.Millisecond)
		w.Finish(nil, false, false)
		w.WhenFinished(func(s Snapshot) {
			v := DeriveContentObservation(s)
			require.Equal(t, "physical_websocket_turn_responses_terminal_usage", v.Cache.Scope)
			require.Equal(t, "physical_websocket_turn_responses_text_at_parser", v.OutputRate.Scope)
			require.Nil(t, v.ResponseByteFirstMS)
			require.Equal(t, "not_collected", s.Latency.Status)
			if turn == 1 {
				require.Equal(t, "not_collected", v.Cache.Status)
				require.Equal(t, "not_reported", v.OutputRate.Status)
				require.Nil(t, v.Cache.ReadTokens)
				require.Nil(t, v.OutputRate.TokensPerSecond, "never borrow prior-turn usage")
				return
			}
			require.Equal(t, "observed", v.Cache.Status)
			require.Equal(t, "canonical_responses_terminal_usage", v.Cache.Source)
			require.Equal(t, turn == 0, *v.Cache.Hit)
			if turn == 0 {
				require.EqualValues(t, 900, *v.Cache.ReadTokens)
				require.EqualValues(t, 50, *v.Cache.WriteTokens)
			} else {
				require.Zero(t, *v.Cache.ReadTokens, "explicit miss is different from absent counters")
				require.Nil(t, v.Cache.WriteTokens)
			}
			require.Equal(t, "estimated", v.OutputRate.Status)
			require.Equal(t, 400.0, *v.OutputRate.WindowMS)
			require.InDelta(t, float64(*v.OutputRate.NonReasoningTokens)/0.4, *v.OutputRate.TokensPerSecond, 1e-9)
			require.Equal(t, v.OutputRate, *s.OutputRate)
			raw, err := json.Marshal(s)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "private-")
			require.Less(t, len(raw), 4096)
			s.Attempts[0].CacheUsage.ReadTokens = 99999
			s.Attempts[0].OutputUsage.OutputTokens = 99999
		})
		if turn != 1 {
			w.WhenFinished(func(s Snapshot) {
				require.EqualValues(t, 1000, s.Attempts[0].CacheUsage.InputTokens)
				require.EqualValues(t, 120, s.Attempts[0].OutputUsage.OutputTokens)
				require.Less(t, s.Attempts[0].CacheUsage.ReadTokens, int64(1000), "snapshots must not share mutable counters")
			})
		}
	}
}

func TestWSTurnCanonicalEvidenceRejectsFailedAndAmbiguousSamples(t *testing.T) {
	for _, tc := range []struct {
		name        string
		change      func(*Snapshot)
		cache, rate string
	}{
		{"complete", func(*Snapshot) {}, "observed", "estimated"},
		{"missing_counters", func(s *Snapshot) { s.Attempts[0].CacheUsage, s.Attempts[0].OutputUsage = nil, nil }, "not_collected", "not_collected"},
		{"failed_turn", func(s *Snapshot) { s.Outcome = "failed" }, "partial", "partial"},
		{"unknown_outcome", func(s *Snapshot) { s.Outcome = "" }, "partial", "partial"},
		{"canceled", func(s *Snapshot) { s.Canceled = true }, "partial", "partial"},
		{"disconnected", func(s *Snapshot) { s.ClientDisconnect = true }, "partial", "partial"},
		{"downstream_write", func(s *Snapshot) { s.DownstreamError = true }, "partial", "partial"},
		{"read_error", func(s *Snapshot) { s.Attempts[0].Error = "timeout" }, "partial", "partial"},
		{"failed_terminal", func(s *Snapshot) { s.Attempts[0].Terminal = "failed" }, "partial", "partial"},
		{"incomplete_terminal", func(s *Snapshot) { s.Terminal = "incomplete" }, "partial", "partial"},
		{"capture_gap", func(s *Snapshot) { s.Attempts[0].Events["typed_content_truncated"] = 1 }, "incomplete_capture", "incomplete_capture"},
		{"output_after_terminal", func(s *Snapshot) { s.Attempts[0].TextStream.Invalid = true }, "invalid_boundary", "invalid_boundary"},
		{"chunk_limit", func(s *Snapshot) { s.Attempts[0].TextStream.Truncated = true }, "incomplete_capture", "incomplete_capture"},
		{"http_status_is_not_turn_success", func(s *Snapshot) { s.Status = 200 }, "partial", "partial"},
		{"handshake_status_is_not_turn_success", func(s *Snapshot) { s.Attempts[0].Status = 101 }, "partial", "partial"},
		{"invalid_read", func(s *Snapshot) { s.Attempts[0].CacheUsage.ReadTokens = 101 }, "invalid_counters", "estimated"},
		{"unknown_reasoning", func(s *Snapshot) { s.Attempts[0].OutputUsage.Status = "not_reported" }, "observed", "not_reported"},
		{"tools", func(s *Snapshot) { s.Attempts[0].OutputUsage.ContentStatus = "unsupported_output" }, "observed", "unsupported_output"},
		{"short_output", func(s *Snapshot) {
			s.Attempts[0].OutputUsage.OutputTokens = 30
			s.Attempts[0].OutputUsage.ReasoningTokens = 0
		}, "observed", "short_output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := outputRateTestSnapshot()
			s.TTFTMode, s.Status, s.Outcome = "ws_turn", 0, "success"
			s.Attempts[0].Kind, s.Attempts[0].Status = "ws_turn", 0
			s.Attempts[0].CacheUsage = &CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 90}
			tc.change(&s)
			v := DeriveContentObservation(s)
			require.Equal(t, tc.cache, v.Cache.Status)
			require.Equal(t, tc.rate, v.OutputRate.Status)
			if tc.cache != "observed" {
				require.Nil(t, v.Cache.ReadRatio)
			}
			if tc.rate != "estimated" {
				require.Nil(t, v.OutputRate.TokensPerSecond)
			}
		})
	}
}

func TestWSTurnCanonicalTerminalCounterValidation(t *testing.T) {
	const usage = `{"input_tokens":100,"input_tokens_details":{"cached_tokens":80},"output_tokens":120,"output_tokens_details":{"reasoning_tokens":20}}`
	for _, tc := range []struct{ name, usage, status, cache, rate string }{
		{"valid", usage, "completed", "observed", "observed"},
		{"conflicting_terminal", usage, "failed", "invalid_terminal", "observed"},
		{"unfinished_terminal", usage, "in_progress", "invalid_terminal", "observed"},
		{"duplicate_counter", strings.Replace(usage, `"input_tokens":100`, `"input_tokens":100,"input_tokens":200`, 1), "completed", "invalid_counters", "invalid_counters"},
		{"coerced_read", strings.Replace(usage, `"cached_tokens":80`, `"cached_tokens":"80"`, 1), "completed", "invalid_counters", "observed"},
		{"unknown_read", strings.Replace(usage, `"cached_tokens":80`, `"unknown":80`, 1), "completed", "not_reported", "observed"},
		{"overlapping_write", strings.Replace(usage, `"cached_tokens":80`, `"cached_tokens":80,"cache_write_tokens":21`, 1), "completed", "invalid_counters", "observed"},
		{"unknown_reasoning", strings.Replace(usage, `"reasoning_tokens":20`, `"unknown":20`, 1), "completed", "observed", "not_reported"},
		{"negative_reasoning", strings.Replace(usage, `"reasoning_tokens":20`, `"reasoning_tokens":-1`, 1), "completed", "observed", "invalid_counters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewWSTurn(time.Now(), 1, 0, [32]byte{})
			w.Content([]byte(`{"type":"response.completed","response":{"status":"` + tc.status + `","usage":` + tc.usage + `}}`))
			w.Finish(nil, false, false)
			w.WhenFinished(func(s Snapshot) {
				a := s.Attempts[0]
				require.NotNil(t, a.CacheUsage)
				require.NotNil(t, a.OutputUsage)
				require.Equal(t, tc.cache, a.CacheUsage.Status)
				require.Equal(t, tc.rate, a.OutputUsage.Status)
				v := DeriveContentObservation(s)
				require.Equal(t, tc.cache, v.Cache.Status)
				if tc.cache != "observed" {
					require.Nil(t, v.Cache.ReadRatio)
				}
				require.Nil(t, v.OutputRate.TokensPerSecond, "a terminal without text deltas must never create a speed sample")
			})
		})
	}
}

func TestWSTurnTimingFailureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, last                  string
		err                         error
		canceled, disconnected, gap bool
	}{
		{name: "failed", last: `{"type":"response.failed"}`},
		{name: "incomplete", last: `{"type":"response.incomplete"}`},
		{name: "bare error", last: `{"type":"error","error":{"message":"private-error"}}`},
		{name: "EOF", err: errors.New("private-read-error")},
		{name: "canceled", last: `{"type":"response.completed"}`, canceled: true},
		{name: "disconnected", last: `{"type":"response.completed"}`, disconnected: true},
		{name: "malformed", last: `{"type":`, gap: true},
		{name: "malformed typed delta", last: `{"type":"response.output_text.delta","delta":{"text":"hidden"}}`, gap: true},
		{name: "oversized", last: strings.Repeat("x", 16*1024*1024+1), gap: true},
		{name: "other protocol", last: `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hidden"}}`, gap: true},
		{name: "output after terminal", last: `{"type":"response.completed"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewWSTurn(time.Now().Add(-time.Second), 1, 0, [32]byte{})
			w.Content([]byte(`{"type":"response.output_text.delta","delta":"visible"}`))
			if tc.last != "" {
				w.Content([]byte(tc.last))
			}
			if tc.name == "output after terminal" {
				w.Content([]byte(`{"type":"response.output_text.delta","delta":"late"}`))
			}
			w.Finish(tc.err, tc.canceled, tc.disconnected)
			w.WhenFinished(func(s Snapshot) {
				v := DeriveContentObservation(s)
				require.NotEqual(t, "observed", v.TextStream.Status)
				if tc.gap {
					require.Nil(t, v.VisibleTextFirstMS)
					require.Equal(t, "incomplete_capture", v.TextStream.Status)
				}
				require.Nil(t, v.ResponseByteFirstMS)
				require.Nil(t, v.Cache.ReadRatio)
				raw, err := json.Marshal(s)
				require.NoError(t, err)
				require.NotContains(t, string(raw), "private-")
			})
		})
	}
}

func TestWSTurnTimingDownstreamFailureDefersPublication(t *testing.T) {
	w := NewWSTurn(time.Now(), 1, 0, [32]byte{})
	w.Content([]byte(`{"type":"response.output_text.delta","delta":"answer"}`))
	w.Content([]byte(`{"type":"response.completed"}`))
	published := false
	w.WhenFinished(func(s Snapshot) {
		published = true
		require.True(t, s.DownstreamError)
		require.Equal(t, "failed", s.Outcome)
		require.Equal(t, "partial", DeriveContentObservation(s).TextStream.Status)
	})
	require.False(t, published, "terminal observation alone is not delivery evidence")
	w.DownstreamWrite(errors.New("private-write-error"))
	w.Finish(nil, false, false)
	require.True(t, published)
}

func TestWSTurnTimingUsageSelectsOnlyPhysicalTurn(t *testing.T) {
	parent := New(time.Now(), 0)
	ctx := With(context.Background(), parent)
	turn := NewWSTurn(time.Now(), 2, 0, [32]byte{})
	bound := WithUsageTurn(ctx, turn.Collector)
	require.Same(t, turn.Collector, UsageCollector(bound), "usage must select one physical turn, not the enclosing HTTP/session collector")
	require.Same(t, parent, From(bound), "retain independent request diagnostics")
	require.Same(t, parent, UsageCollector(WithUsageTurn(ctx, nil)))
}

func TestWSTurnTimingFinishAndBindingConcurrent(t *testing.T) {
	w := NewWSTurn(time.Now(), 1, 0, [32]byte{})
	w.Content([]byte(`{"type":"response.output_text.delta","delta":"answer"}`))
	w.Content([]byte(`{"type":"response.completed"}`))
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { w.WhenFinished(func(s Snapshot) { require.Equal(t, "completed", s.Terminal) }) })
		wg.Go(func() { w.Finish(nil, false, false) })
	}
	wg.Wait()
}
