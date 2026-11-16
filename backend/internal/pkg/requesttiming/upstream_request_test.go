package requesttiming

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpstreamRequestMetadataParsing(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, payload string
		ws, observed            bool
		tier, effort            string
	}{
		{"responses", "responses", `{"model":"wire-model","input":"private-prompt","service_tier":"priority","reasoning":{"effort":"high"}}`, false, true, "priority", "high"},
		{"chat", "chat_completions", `{"model":"wire-model","reasoning_effort":"none","messages":[{"content":"private-prompt"}]}`, false, true, "unspecified", "none"},
		{"compact", "responses_compact", `{"model":"wire-model"}`, false, true, "unspecified", "unspecified"},
		{"ws", "responses", `{"type":"response.create","model":"wire-model"}`, true, true, "unspecified", "unspecified"},
		{"unknown_option", "responses", `{"model":"wire-model","service_tier":"future"}`, false, false, "", ""},
		{"null_option", "responses", `{"model":"wire-model","service_tier":null}`, false, false, "", ""},
		{"empty_model", "responses", `{"model":""}`, false, false, "", ""},
		{"model_type", "responses", `{"model":123}`, false, false, "", ""},
		{"duplicate_model", "responses", `{"model":"wire-model","model":"other"}`, false, false, "", ""},
		{"duplicate_effort", "responses", `{"model":"wire-model","reasoning":{"effort":"low","effort":"high"}}`, false, false, "", ""},
		{"null_reasoning", "responses", `{"model":"wire-model","reasoning":null}`, false, false, "", ""},
		{"bad_effort", "responses", `{"model":"wire-model","reasoning":{"effort":4}}`, false, false, "", ""},
		{"conflicting_effort", "responses", `{"model":"wire-model","reasoning_effort":"high"}`, false, false, "", ""},
		{"chat_responses_effort", "chat_completions", `{"model":"wire-model","reasoning":{"effort":"high"}}`, false, false, "", ""},
		{"bad_stream", "responses", `{"model":"wire-model","stream":"true"}`, false, false, "", ""},
		{"bad_json", "responses", `{"model":"wire-model"`, false, false, "", ""},
		{"array", "responses", `[{"model":"wire-model"}]`, false, false, "", ""},
		{"bad_ws_type", "responses", `{"type":"response.cancel","model":"wire-model"}`, true, false, "", ""},
		{"control_model", "responses", `{"model":"wire\nmodel"}`, false, false, "", ""},
		{"oversized_model", "responses", `{"model":"` + strings.Repeat("a", 201) + `"}`, false, false, "", ""},
		{"oversized_body", "responses", `{"model":"wire-model","input":"` + strings.Repeat("p", maxUpstreamRequestScopeBytes) + `"}`, false, false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseUpstreamRequest(tc.protocol, []byte(tc.payload), tc.ws)
			if !tc.observed {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.Equal(t, "wire-model", got.Model)
			require.Equal(t, tc.tier, got.ServiceTier)
			require.Equal(t, tc.effort, got.ReasoningEffort)
			require.Equal(t, tc.ws, got.Stream)
			raw, err := json.Marshal(got)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "private-prompt")
		})
	}
}

type upstreamScopeReader struct {
	io.Reader
	reads, closes int
	err           error
}

func (r *upstreamScopeReader) Read(p []byte) (int, error) {
	r.reads++
	n, err := r.Reader.Read(p)
	if err == io.EOF && r.err != nil {
		return n, r.err
	}
	return n, err
}
func (r *upstreamScopeReader) Close() error { r.closes++; return nil }

func TestUpstreamRequestObservesOnlyConsumedCompleteBody(t *testing.T) {
	const payload = `{"model":"wire-model","input":"private-prompt"}`
	for _, tc := range []struct {
		name     string
		length   int64
		read     int
		readErr  error
		observed bool
	}{
		{"known_length", int64(len(payload)), -1, nil, true},
		{"chunked", -1, -1, nil, true},
		{"not_consumed", int64(len(payload)), 0, nil, false},
		{"partial", int64(len(payload)), 5, nil, false},
		{"length_mismatch", int64(len(payload) + 1), -1, nil, false},
		{"read_failure", -1, -1, errors.New("fixture failure"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(time.Now(), 0)
			req, err := http.NewRequestWithContext(With(t.Context(), c), "POST", "https://fixture.invalid/v1/responses", nil)
			require.NoError(t, err)
			body := &upstreamScopeReader{Reader: strings.NewReader(payload), err: tc.readErr}
			req.Body, req.ContentLength = body, tc.length
			req, _ = StartAttempt(req, 7, 0)
			req, _ = StartTransport(req)
			observed := ObserveUpstreamRequest(req)
			require.Zero(t, body.reads, "instrumentation must never pre-read")
			require.Same(t, body, req.Body, "do not mutate the caller's request")
			if tc.read < 0 {
				data, _ := io.ReadAll(observed.Body)
				require.Equal(t, payload, string(data))
			} else if tc.read > 0 {
				_, _ = io.ReadFull(observed.Body, make([]byte, tc.read))
			}
			require.NoError(t, observed.Body.Close())
			require.Equal(t, 1, body.closes)
			c.Finish(502, false)
			c.WhenFinished(func(s Snapshot) {
				require.Len(t, s.Attempts, 2)
				require.Equal(t, tc.observed, s.Attempts[1].UpstreamRequest != nil)
				raw, err := json.Marshal(s)
				require.NoError(t, err)
				require.NotContains(t, string(raw), "private-prompt")
			})
			require.Nil(t, observed.Body.(*upstreamRequestBody).data, "release transient body storage")
		})
	}
}

func TestUpstreamRequestBoundedAndUnsupportedBodiesRemainUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, encoding string
		length                       int64
	}{
		{"bound", "POST", "/v1/responses", "", maxUpstreamRequestScopeBytes + 1},
		{"compressed", "POST", "/v1/responses", "gzip", -1},
		{"handshake", "GET", "/v1/responses", "", -1},
		{"messages_not_covered", "POST", "/v1/messages", "", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(time.Now(), 0)
			req, err := http.NewRequestWithContext(With(t.Context(), c), tc.method, "https://fixture.invalid"+tc.path, strings.NewReader(`{"model":"wire-model"}`))
			require.NoError(t, err)
			req.ContentLength = tc.length
			req.Header.Set("Content-Encoding", tc.encoding)
			req, _ = StartAttempt(req, 7, 0)
			req, _ = StartTransport(req)
			require.Same(t, req, ObserveUpstreamRequest(req))
		})
	}
	// Unknown length still has the same hard cap; no read failure reaches caller.
	c := New(time.Now(), 0)
	req, _ := http.NewRequestWithContext(With(t.Context(), c), "POST", "https://fixture.invalid/v1/responses", io.NopCloser(strings.NewReader(strings.Repeat("x", maxUpstreamRequestScopeBytes+1))))
	req, _ = StartAttempt(req, 7, 0)
	req, _ = StartTransport(req)
	req = ObserveUpstreamRequest(req)
	n, err := io.Copy(io.Discard, req.Body)
	require.NoError(t, err)
	require.EqualValues(t, maxUpstreamRequestScopeBytes+1, n)
	require.Nil(t, req.Body.(*upstreamRequestBody).data)
	c.Finish(502, false)
	c.WhenFinished(func(s Snapshot) { require.Nil(t, s.Attempts[1].UpstreamRequest) })
}

func TestUpstreamRequestIndependentTurnsSnapshotsAndChangedHandoffs(t *testing.T) {
	w := NewWSTurn(time.Now(), 7, 0, [32]byte{1})
	w.RecordRequest([]byte(`{"type":"response.create","model":"wire-model"}`))
	w.Finish(nil, false, false)
	w.WhenFinished(func(s Snapshot) {
		require.Equal(t, "wire-model", s.Attempts[0].UpstreamRequest.Model)
		s.Attempts[0].UpstreamRequest.Model = "changed snapshot"
	})
	w.WhenFinished(func(s Snapshot) { require.Equal(t, "wire-model", s.Attempts[0].UpstreamRequest.Model) })
	w.RecordRequest([]byte(`{"type":"response.create","model":"late-model"}`))
	w.WhenFinished(func(s Snapshot) { require.Equal(t, "wire-model", s.Attempts[0].UpstreamRequest.Model) })
	second := NewWSTurn(time.Now(), 7, 0, [32]byte{1})
	second.RecordRequest([]byte(`{"type":"response.create","model":"other-model"}`))
	second.Finish(nil, false, false)
	second.WhenFinished(func(s Snapshot) { require.Equal(t, "other-model", s.Attempts[0].UpstreamRequest.Model) })
	var absent *WSTurnCapture
	absent.RecordRequest(nil)
	require.Nil(t, ObserveUpstreamRequest(nil))
	require.NotNil(t, ObserveUpstreamRequest((&http.Request{}).WithContext(context.Background())))

	c := New(time.Now(), 0)
	req, _ := http.NewRequestWithContext(With(t.Context(), c), "POST", "https://fixture.invalid/v1/responses", strings.NewReader(`{"model":"first-model"}`))
	req, _ = StartAttempt(req, 7, 0)
	req, _ = StartTransport(req)
	_, err := io.Copy(io.Discard, ObserveUpstreamRequest(req).Body)
	require.NoError(t, err)
	// Even an unsupported redirect must invalidate the earlier body identity.
	next := req.Clone(req.Context())
	next.Method = "GET"
	ObserveUpstreamRequest(next)
	c.Finish(200, false)
	c.WhenFinished(func(s Snapshot) { require.Equal(t, "changed_request", s.Attempts[1].UpstreamRequest.Status) })
}

func TestUpstreamRequestConcurrentReadClose(t *testing.T) {
	c := New(time.Now(), 0)
	r, w := io.Pipe()
	req, _ := http.NewRequestWithContext(With(t.Context(), c), "POST", "https://fixture.invalid/v1/responses", r)
	req, _ = StartAttempt(req, 7, 0)
	req, _ = StartTransport(req)
	req = ObserveUpstreamRequest(req)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(io.Discard, req.Body) }()
	go func() { defer wg.Done(); _ = req.Body.Close() }()
	wg.Wait()
	_ = w.Close()
	c.Finish(502, false)
	c.WhenFinished(func(s Snapshot) { require.Nil(t, s.Attempts[1].UpstreamRequest) })
}
