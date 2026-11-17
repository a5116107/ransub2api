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

func TestUpstreamRequestResponsesOutputOptions(t *testing.T) {
	for _, protocol := range []string{"responses", "responses_compact", "ws"} {
		for _, tc := range []struct{ name, options, summary, encrypted, verbosity string }{
			{"omitted", ``, "unspecified", "not_requested", "unspecified"},
			{"empty", `,"reasoning":{},"include":[],"text":{}`, "unspecified", "not_requested", "unspecified"},
			{"summary", `,"reasoning":{"effort":"high","summary":"concise"}`, "concise", "not_requested", "unspecified"},
			{"encrypted", `,"include":["reasoning.encrypted_content"]`, "unspecified", "requested", "unspecified"},
			{"verbosity", `,"text":{"verbosity":"low"}`, "unspecified", "not_requested", "low"},
			{"combined", `,"reasoning":{"summary":"detailed"},"include":["reasoning.encrypted_content"],"text":{"verbosity":"high"}`, "detailed", "requested", "high"},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				wireProtocol := protocol
				if protocol == "ws" {
					wireProtocol = "responses"
				}
				payload := []byte(`{"type":"response.create","model":"wire-model","input":"private-prompt"` + tc.options + `}`)
				got := parseUpstreamRequest(wireProtocol, payload, protocol == "ws")
				require.NotNil(t, got)
				raw, err := json.Marshal(got)
				require.NoError(t, err)
				var fields map[string]any
				require.NoError(t, json.Unmarshal(raw, &fields))
				require.EqualValues(t, 2, fields["version"])
				require.Equal(t, tc.summary, fields["reasoning_summary"])
				require.Equal(t, tc.encrypted, fields["encrypted_content"])
				require.Equal(t, tc.verbosity, fields["text_verbosity"])
				require.NotContains(t, string(raw), "private-prompt")
			})
		}
	}
}

func TestUpstreamRequestResponsesInvalidOutputOptions(t *testing.T) {
	for _, options := range []string{
		`"reasoning":{"summary":null}`, `"reasoning":{"summary":true}`, `"reasoning":{"summary":"future"}`,
		`"reasoning":{"summary":"none","summary":"auto"}`, `"reasoning":{"future":"private-option"}`,
		`"include":null`, `"include":true`, `"include":[null]`, `"include":["private-option"]`,
		`"include":["reasoning.encrypted_content","reasoning.encrypted_content"]`,
		`"text":null`, `"text":[]`, `"text":{"verbosity":null}`, `"text":{"verbosity":1}`,
		`"text":{"verbosity":"future"}`, `"text":{"verbosity":"low","verbosity":"high"}`,
		`"text":{"format":{"type":"json_schema","schema":{"private-schema":true}}}`,
	} {
		t.Run(options, func(t *testing.T) {
			payload := []byte(`{"type":"response.create","model":"wire-model",` + options + `}`)
			require.Nil(t, parseUpstreamRequest("responses", payload, false))
			require.Nil(t, parseUpstreamRequest("responses_compact", payload, false))
			require.Nil(t, parseUpstreamRequest("responses", payload, true))
		})
	}
}

func TestUpstreamRequestResponsesOutputPhysicalCapture(t *testing.T) {
	const payload = `{"model":"wire-model","stream":true,"reasoning":{"summary":"auto"},"include":["reasoning.encrypted_content"],"text":{"verbosity":"medium"},"input":"private-prompt"}`
	c := New(time.Now(), 0)
	req, err := http.NewRequestWithContext(With(t.Context(), c), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(payload))
	require.NoError(t, err)
	req, _ = StartAttempt(req, 7, 0)
	req, _ = StartTransport(req)
	observed := ObserveUpstreamRequest(req)
	RecordUpstreamCredentials(observed.Context(), []string{strings.Repeat("a", 64)})
	actual, err := io.ReadAll(observed.Body)
	require.NoError(t, err)
	require.Equal(t, payload, string(actual), "observation must preserve the exact outgoing bytes")
	require.NoError(t, observed.Body.Close())
	c.Finish(200, false)
	c.WhenFinished(func(s Snapshot) {
		got := DerivePhysicalUpstream(s)
		require.Len(t, got.Attempts, 1)
		metadata := got.Attempts[0].UpstreamRequest
		require.NotNil(t, metadata)
		require.Equal(t, 2, metadata.Version)
		require.Equal(t, "auto", metadata.ReasoningSummary)
		require.Equal(t, "requested", metadata.EncryptedContent)
		require.Equal(t, "medium", metadata.TextVerbosity)
		raw, err := json.Marshal(s)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "private-prompt")
	})
	for _, verbosity := range []string{"low", "high"} {
		w := NewWSTurn(time.Now(), 7, 0, [32]byte{1})
		w.RecordRequest([]byte(`{"type":"response.create","model":"wire-model","text":{"verbosity":"` + verbosity + `"}}`))
		w.Finish(nil, false, false)
		w.WhenFinished(func(s Snapshot) {
			got := DerivePhysicalUpstream(s)
			require.Len(t, got.Attempts, 1)
			require.NotNil(t, got.Attempts[0].UpstreamRequest)
			require.Equal(t, verbosity, got.Attempts[0].UpstreamRequest.TextVerbosity)
		})
	}
}

func TestUpstreamRequestGeminiMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, path, payload, mode, level, thoughts, source string
		budget                                             int64
		stream, valid                                      bool
	}{
		{name: "native_unspecified", path: "/v1beta/models/wire-gemini:generateContent", payload: `{}`, mode: "unspecified", thoughts: "unspecified", source: "http_transport_url_body", valid: true},
		{name: "vertex_level", path: "/v1/projects/private-project/locations/private-region/publishers/google/models/wire-gemini:streamGenerateContent", payload: `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"minimal","includeThoughts":false}}}`, mode: "level", level: "minimal", thoughts: "disabled", source: "http_transport_url_body", stream: true, valid: true},
		{name: "dynamic", path: "/v1beta/models/wire-gemini:streamGenerateContent", payload: `{"generationConfig":{"thinkingConfig":{"thinkingBudget":-1,"includeThoughts":true}}}`, mode: "budget_dynamic", thoughts: "enabled", source: "http_transport_url_body", stream: true, valid: true},
		{name: "explicit_zero", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`, mode: "budget_zero", thoughts: "unspecified", source: "http_transport_url_body", valid: true},
		{name: "fixed_budget", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{"thinkingConfig":{"thinkingBudget":4096}}}`, mode: "budget_fixed", budget: 4096, thoughts: "unspecified", source: "http_transport_url_body", valid: true},
		{name: "uppercase_level", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH"}}}`, mode: "level", level: "HIGH", thoughts: "unspecified", source: "http_transport_url_body", valid: true},
		{name: "empty_thinking", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{"thinkingConfig":{}}}`, mode: "unspecified", thoughts: "unspecified", source: "http_transport_url_body", valid: true},
		{name: "internal", path: "/v1internal:streamGenerateContent", payload: `{"project":"private-project","model":"wire-gemini","request":{"contents":[{"parts":[{"text":"private-prompt"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}}`, mode: "level", level: "high", thoughts: "unspecified", source: "http_transport_body", stream: true, valid: true},
		{name: "internal_missing_request", path: "/v1internal:generateContent", payload: `{"model":"wire-gemini"}`},
		{name: "internal_nested_envelope", path: "/v1internal:generateContent", payload: `{"model":"wire-gemini","request":{"request":{}}}`},
		{name: "internal_conflicting_model", path: "/v1internal:generateContent", payload: `{"model":"wire-gemini","request":{"model":"other"}}`},
		{name: "internal_outer_config", path: "/v1internal:generateContent", payload: `{"model":"wire-gemini","generationConfig":{},"request":{}}`},
		{name: "duplicate_model", path: "/v1internal:generateContent", payload: `{"model":"wire-gemini","model":"other","request":{}}`},
		{name: "native_body_model", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"model":"other"}`},
		{name: "body_stream", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"stream":true}`},
		{name: "bad_model_path", path: "/v1beta/models/private-project/wire-gemini:generateContent", payload: `{}`},
		{name: "encoded_model_path", path: "/v1beta/models/wire%2Fgemini:generateContent", payload: `{}`},
		{name: "control_model", path: "/v1internal:generateContent", payload: `{"model":"wire\ngemini","request":{}}`},
		{name: "resource_model", path: "/v1internal:generateContent", payload: `{"model":"projects/private-project/models/wire-gemini","request":{}}`},
		{name: "duplicate_config", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{},"generationConfig":{}}`},
		{name: "null_config", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":null}`},
		{name: "null_thinking", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{"thinkingConfig":null}}`},
		{name: "duplicate_budget", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{"thinkingConfig":{"thinkingBudget":0,"thinkingBudget":-1}}}`},
		{name: "both_modes", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{"thinkingConfig":{"thinkingBudget":4096,"thinkingLevel":"high"}}}`},
		{name: "fraction", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{"thinkingConfig":{"thinkingBudget":1.5}}}`},
		{name: "unsafe_integer", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{"thinkingConfig":{"thinkingBudget":9007199254740992}}}`},
		{name: "negative", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{"thinkingConfig":{"thinkingBudget":-2}}}`},
		{name: "null_budget", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{"thinkingConfig":{"thinkingBudget":null}}}`},
		{name: "unknown_level", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"extreme"}}}`},
		{name: "bad_thoughts", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{"thinkingConfig":{"includeThoughts":"true"}}}`},
		{name: "unknown_thinking_field", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"generationConfig":{"thinkingConfig":{"future":true}}}`},
		{name: "foreign_config", path: "/v1beta/models/wire-gemini:generateContent", payload: `{"thinking":{"type":"disabled"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(time.Now(), 0)
			req, err := http.NewRequestWithContext(With(t.Context(), c), "POST", "https://fixture.invalid"+tc.path+"?key=private-key", strings.NewReader(tc.payload))
			require.NoError(t, err)
			req, _ = StartAttempt(req, 7, 0)
			req, _ = StartTransport(req)
			observed := ObserveUpstreamRequest(req)
			RecordUpstreamCredentials(observed.Context(), []string{strings.Repeat("a", 64)})
			wire, err := io.ReadAll(observed.Body)
			require.NoError(t, err)
			require.Equal(t, tc.payload, string(wire))
			require.NoError(t, observed.Body.Close())
			c.Finish(200, false)
			c.WhenFinished(func(s Snapshot) {
				got := s.Attempts[1].UpstreamRequest
				if !tc.valid {
					require.Nil(t, got)
					return
				}
				require.NotNil(t, got)
				require.Equal(t, "wire-gemini", got.Model)
				require.Equal(t, tc.source, got.Source)
				require.Equal(t, tc.stream, got.Stream)
				require.Equal(t, tc.mode, got.ThinkingMode)
				require.Equal(t, tc.budget, got.ThinkingBudgetTokens)
				raw, err := json.Marshal(got)
				require.NoError(t, err)
				var metadata map[string]any
				require.NoError(t, json.Unmarshal(raw, &metadata))
				require.Equal(t, tc.thoughts, metadata["include_thoughts"])
				if tc.level != "" {
					require.Equal(t, tc.level, metadata["thinking_level"])
				} else {
					require.NotContains(t, metadata, "thinking_level")
				}
				physical := DerivePhysicalUpstream(s)
				require.Equal(t, got, physical.Attempts[0].UpstreamRequest)
				for _, secret := range []string{"private-prompt", "private-project", "private-region", "private-key", "fixture.invalid"} {
					require.NotContains(t, string(raw), secret)
				}
			})
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
		{"unknown_endpoint", "POST", "/v1/unknown", "", -1},
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

func TestUpstreamRequestMessagesMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, fields, tier, effort, speed, thinking string
		budget                                      int64
		valid                                       bool
	}{
		{name: "unspecified", tier: "unspecified", effort: "unspecified", speed: "unspecified", thinking: "unspecified", valid: true},
		{name: "fast_adaptive", fields: `,"service_tier":"standard_only","speed":"fast","thinking":{"type":"adaptive"},"output_config":{"effort":"max"}`, tier: "standard_only", effort: "max", speed: "fast", thinking: "adaptive", valid: true},
		{name: "budget", fields: `,"thinking":{"type":"enabled","budget_tokens":4096}`, tier: "unspecified", effort: "unspecified", speed: "unspecified", thinking: "enabled", budget: 4096, valid: true},
		{name: "disabled", fields: `,"thinking":{"type":"disabled"},"speed":"standard","service_tier":"auto","output_config":{"effort":"high"}`, tier: "auto", effort: "high", speed: "standard", thinking: "disabled", valid: true},
		{name: "bad_tier", fields: `,"service_tier":"priority"`},
		{name: "null_speed", fields: `,"speed":null`},
		{name: "unknown_speed", fields: `,"speed":"turbo"`},
		{name: "unknown_thinking", fields: `,"thinking":{"type":"turbo"}`},
		{name: "null_thinking", fields: `,"thinking":null`},
		{name: "missing_thinking_type", fields: `,"thinking":{}`},
		{name: "duplicate_thinking", fields: `,"thinking":{"type":"adaptive","type":"disabled"}`},
		{name: "missing_budget", fields: `,"thinking":{"type":"enabled"}`},
		{name: "zero_budget", fields: `,"thinking":{"type":"enabled","budget_tokens":0}`},
		{name: "fraction_budget", fields: `,"thinking":{"type":"enabled","budget_tokens":1024.5}`},
		{name: "unsafe_budget", fields: `,"thinking":{"type":"enabled","budget_tokens":9007199254740992}`},
		{name: "irrelevant_budget", fields: `,"thinking":{"type":"adaptive","budget_tokens":4096}`},
		{name: "duplicate_effort", fields: `,"output_config":{"effort":"low","effort":"max"}`},
		{name: "unknown_effort", fields: `,"output_config":{"effort":"turbo"}`},
		{name: "null_output_config", fields: `,"output_config":null`},
		{name: "foreign_effort", fields: `,"reasoning_effort":"high"`},
		{name: "foreign_reasoning", fields: `,"reasoning":{"effort":"high"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"model":"wire-messages","stream":true,"messages":[{"role":"user","content":"private-prompt"}]` + tc.fields + `}`
			c := New(time.Now(), 0)
			req, err := http.NewRequestWithContext(With(t.Context(), c), "POST", "https://fixture.invalid/v1/messages?beta=true", strings.NewReader(payload))
			require.NoError(t, err)
			req, _ = StartAttempt(req, 7, 0)
			req, _ = StartTransport(req)
			observed := ObserveUpstreamRequest(req)
			wire, err := io.ReadAll(observed.Body)
			require.NoError(t, err)
			require.Equal(t, payload, string(wire))
			require.NoError(t, observed.Body.Close())
			c.Finish(502, false)
			c.WhenFinished(func(s Snapshot) {
				got := s.Attempts[1].UpstreamRequest
				if !tc.valid {
					require.Nil(t, got)
					return
				}
				require.NotNil(t, got)
				require.Equal(t, "messages", got.Protocol)
				require.Equal(t, "wire-messages", got.Model)
				require.Equal(t, tc.tier, got.ServiceTier)
				require.Equal(t, tc.effort, got.ReasoningEffort)
				require.True(t, got.Stream)
				raw, err := json.Marshal(got)
				require.NoError(t, err)
				var metadata map[string]any
				require.NoError(t, json.Unmarshal(raw, &metadata))
				require.Equal(t, tc.speed, metadata["speed"])
				require.Equal(t, tc.thinking, metadata["thinking_mode"])
				if tc.budget > 0 {
					require.Equal(t, float64(tc.budget), metadata["thinking_budget_tokens"])
				} else {
					require.NotContains(t, metadata, "thinking_budget_tokens")
				}
				require.NotContains(t, string(raw), "private-prompt")
			})
		})
	}
}
