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
			require.Nil(t, v.Cache.ReadRatio, "WS cache and TPS require their own contract")
			require.Nil(t, v.OutputRate.TokensPerSecond)
			raw, err := json.Marshal(s)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "private-")
			require.Less(t, len(raw), 4096)
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
