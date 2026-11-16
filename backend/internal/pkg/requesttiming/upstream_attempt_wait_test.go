package requesttiming

import (
	"bufio"
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

func physicalWaitRows(t *testing.T, s Snapshot) []map[string]Measurement {
	t.Helper()
	raw, err := json.Marshal(DeriveLatency(s))
	require.NoError(t, err)
	var result struct {
		Attempts []map[string]json.RawMessage `json:"attempts"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	rows := make([]map[string]Measurement, len(result.Attempts))
	for i, attempt := range result.Attempts {
		rows[i] = make(map[string]Measurement)
		for _, key := range []string{"post_upload_first_byte_wait", "post_upload_first_event_wait", "post_upload_first_reasoning_wait", "post_upload_first_text_wait"} {
			value, exists := attempt[key]
			require.True(t, exists, "physical attempt must publish %s", key)
			var measurement Measurement
			require.NoError(t, json.Unmarshal(value, &measurement))
			rows[i][key] = measurement
		}
	}
	return rows
}

func TestUpstreamAttemptWaitsKeepPhysicalScope(t *testing.T) {
	first := latencyAttempt(1, 7, 100, 400)
	first.Status = 502
	first.Events["first_byte"] = 160
	last := latencyAttempt(2, 8, 500, 900)
	last.Status = 200
	last.Events["first_byte"] = 550
	last.Events["first_sse_event_flush"] = 560
	last.Events["first_reasoning"] = 570
	last.Events["first_visible_text"] = 580
	s := Snapshot{TotalMS: 1000, Status: 200, Events: map[string]float64{"first_byte": 1, "first_sse_event_flush": 1, "first_reasoning": 1, "first_visible_text": 1}, Attempts: []Attempt{first, last}}
	got := physicalWaitRows(t, s)
	require.Len(t, got, 2)
	require.Equal(t, 20.0, *got[0]["post_upload_first_byte_wait"].MS)
	for _, key := range []string{"post_upload_first_event_wait", "post_upload_first_reasoning_wait", "post_upload_first_text_wait"} {
		require.Equal(t, "not_collected", got[0][key].Status, "failed attempt cannot borrow the later success")
		require.Nil(t, got[0][key].MS)
	}
	for key, expected := range map[string]float64{"post_upload_first_byte_wait": 10, "post_upload_first_event_wait": 20, "post_upload_first_reasoning_wait": 30, "post_upload_first_text_wait": 40} {
		require.Equal(t, "measured", got[1][key].Status)
		require.Equal(t, expected, *got[1][key].MS)
	}
	require.True(t, DeriveLatency(s).Attempts[0].Failed)
	require.False(t, DeriveLatency(s).Attempts[1].Failed)
}

func TestUpstreamAttemptWaitsValidateBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		modify       func(*Attempt)
	}{
		{"zero", "measured", func(a *Attempt) { a.Events["first_visible_text"] = a.Events["request_written"] }},
		{"legacy write alias", "measured", func(a *Attempt) {
			a.Events["wrote_request"] = a.Events["request_written"]
			delete(a.Events, "request_written")
		}},
		{"missing write", "not_collected", func(a *Attempt) { delete(a.Events, "request_written") }},
		{"write failed", "write_failed", func(a *Attempt) { a.Events["request_write_failed"] = 35 }},
		{"missing event", "not_collected", func(a *Attempt) { delete(a.Events, "first_visible_text") }},
		{"before write", "invalid_boundary", func(a *Attempt) { a.Events["first_visible_text"] = 39 }},
		{"after request", "invalid_boundary", func(a *Attempt) { a.Events["first_visible_text"] = 1001 }},
		{"nonfinite event", "invalid_boundary", func(a *Attempt) { a.Events["first_visible_text"] = math.NaN() }},
		{"infinite event", "invalid_boundary", func(a *Attempt) { a.Events["first_visible_text"] = math.Inf(1) }},
		{"unfinished", "invalid_boundary", func(a *Attempt) { a.EndMS = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := latencyAttempt(1, 7, 0, 200)
			a.Events["first_visible_text"] = 150
			tc.modify(&a)
			got := physicalWaitRows(t, Snapshot{TotalMS: 1000, Attempts: []Attempt{a}})[0]["post_upload_first_text_wait"]
			require.Equal(t, tc.status, got.Status)
			if tc.status != "measured" {
				require.Nil(t, got.MS)
			} else if tc.name == "zero" {
				require.Equal(t, 0.0, *got.MS)
			} else {
				require.Equal(t, 110.0, *got.MS)
			}
		})
	}
}

func TestUpstreamAttemptBufferedParserKeepsSeparateNetworkBoundary(t *testing.T) {
	first := latencyAttempt(1, 7, 0, 200)
	first.Events["first_visible_text"] = 250
	first.Events["first_byte"] = 250
	last := latencyAttempt(2, 8, 300, 600)
	last.Events["first_visible_text"] = 650
	s := Snapshot{TotalMS: 700, Attempts: []Attempt{first, last}}
	got := physicalWaitRows(t, s)
	require.Equal(t, "measured", got[0]["post_upload_first_text_wait"].Status)
	require.Equal(t, 210.0, *got[0]["post_upload_first_text_wait"].MS, "buffered parsing before the next attempt remains measurable")
	require.Equal(t, "invalid_boundary", got[0]["post_upload_first_byte_wait"].Status, "network EOF does not extend for delayed parsing")
	require.Nil(t, got[0]["post_upload_first_byte_wait"].MS)
	require.Equal(t, 310.0, *got[1]["post_upload_first_text_wait"].MS, "the final parser may finish before the application request completes")
}

func TestUpstreamAttemptParserWaitNeverCrossesNextPhysicalSend(t *testing.T) {
	for _, event := range []string{"first_semantic", "first_visible", "first_sse_event_flush", "first_reasoning", "first_visible_text"} {
		t.Run(event, func(t *testing.T) {
			first := latencyAttempt(1, 7, 0, 200)
			first.Events[event] = 450
			last := latencyAttempt(2, 8, 300, 600)
			s := Snapshot{TotalMS: 1000, Attempts: []Attempt{first, last}}
			derived := DeriveLatency(s)
			raw, err := json.Marshal(derived.Attempts[0])
			require.NoError(t, err)
			var fields map[string]Measurement
			var all map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &all))
			fields = make(map[string]Measurement)
			for _, key := range []string{"post_upload_first_content_wait", "post_upload_first_visible_wait", "post_upload_first_event_wait", "post_upload_first_reasoning_wait", "post_upload_first_text_wait"} {
				if value, exists := all[key]; exists {
					var measurement Measurement
					require.NoError(t, json.Unmarshal(value, &measurement))
					fields[key] = measurement
				}
			}
			key := map[string]string{"first_semantic": "post_upload_first_content_wait", "first_visible": "post_upload_first_visible_wait", "first_sse_event_flush": "post_upload_first_event_wait", "first_reasoning": "post_upload_first_reasoning_wait", "first_visible_text": "post_upload_first_text_wait"}[event]
			require.Equal(t, "invalid_boundary", fields[key].Status)
			require.Nil(t, fields[key].MS)
		})
	}
}

func TestUpstreamAttemptWaitsDiscardIncompleteTypedCapture(t *testing.T) {
	a := latencyAttempt(1, 7, 0, 200)
	a.Events["first_byte"] = 80
	a.Events["first_sse_event_flush"] = 100
	a.Events["first_reasoning"] = 110
	a.Events["first_visible_text"] = 120
	a.Events["typed_content_truncated"] = 130
	got := DeriveLatency(Snapshot{TotalMS: 250, Attempts: []Attempt{a}}).Attempts[0]
	require.Equal(t, "measured", got.FirstByteWait.Status, "capture loss cannot erase independent network evidence")
	require.Equal(t, 40.0, *got.FirstByteWait.MS)
	for _, value := range []Measurement{got.FirstEventWait, got.FirstReasoningWait, got.FirstTextWait} {
		require.Equal(t, "incomplete_capture", value.Status)
		require.Nil(t, value.MS, "a partial typed capture cannot establish the first parsed event")
	}
}

func TestUpstreamAttemptHTTPRetryKeepsFailureAndSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/failed" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"fixture"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n"+
			"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"fixture\"}\n\n"+
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"fixture\"}\n\n"+
			"data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()
	collector := New(time.Now(), 0)
	for i, path := range []string{"/failed", "/success"} {
		req, err := http.NewRequestWithContext(With(t.Context(), collector), http.MethodPost, server.URL+path, strings.NewReader(`{"model":"fixture"}`))
		require.NoError(t, err)
		req, _ = StartAttempt(req, int64(7+i), 0)
		req, trace := StartTransport(req)
		resp, err := server.Client().Do(req)
		trace.Response(resp, err)
		require.NoError(t, err)
		if i == 0 {
			_, err = io.Copy(io.Discard, resp.Body)
			require.NoError(t, err)
		} else {
			ctx := ResponseContext(req.Context(), resp)
			writer := httptest.NewRecorder()
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				data, ok := strings.CutPrefix(scanner.Text(), "data: ")
				if !ok {
					continue
				}
				require.True(t, Content(ctx, []byte(data)))
				_, err = writer.Write(append(scanner.Bytes(), '\n', '\n'))
				require.NoError(t, err)
				writer.Flush()
				SSEEventFlushed(ctx)
			}
			require.NoError(t, scanner.Err())
		}
		require.NoError(t, resp.Body.Close())
	}
	collector.Finish(http.StatusOK, false)
	collector.WhenFinished(func(s Snapshot) {
		got := DeriveLatency(s)
		require.Len(t, got.Attempts, 2, "only physical attempts contribute waits")
		require.Equal(t, 1, got.AccountSwitches)
		first, last := got.Attempts[0], got.Attempts[1]
		require.True(t, first.Failed)
		require.False(t, last.Failed)
		require.Equal(t, "measured", first.FirstByteWait.Status)
		for _, value := range []Measurement{first.FirstEventWait, first.FirstReasoningWait, first.FirstTextWait} {
			require.Equal(t, "not_collected", value.Status, "the failed send cannot borrow the successful retry's events")
			require.Nil(t, value.MS)
		}
		for _, value := range []Measurement{last.FirstByteWait, last.FirstEventWait, last.FirstReasoningWait, last.FirstTextWait} {
			require.Equal(t, "measured", value.Status)
			require.NotNil(t, value.MS)
			require.GreaterOrEqual(t, *value.MS, 0.0)
		}
	})
}
