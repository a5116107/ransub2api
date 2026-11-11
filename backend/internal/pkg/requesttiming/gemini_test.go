package requesttiming

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGeminiContentTypedBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, data, terminal string
		reasoning, visible   bool
		nonText, invalid     bool
	}{
		{name: "text", data: `{"candidates":[{"content":{"parts":[{"text":"private text"}]}}]}`, visible: true},
		{name: "reasoning", data: `{"candidates":[{"content":{"parts":[{"thought":true,"text":"private reasoning"}]}}]}`, reasoning: true},
		{name: "empty reasoning", data: `{"candidates":[{"content":{"parts":[{"thought":true,"text":""}]}}]}`},
		{name: "signature", data: `{"candidates":[{"content":{"parts":[{"thoughtSignature":"private signature"}]}}]}`},
		{name: "tool", data: `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"fixture","args":{"input":"private args"}}}]}}]}`, nonText: true},
		{name: "media", data: `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"private image"}}]}}]}`, nonText: true},
		{name: "metadata", data: `{"modelVersion":"fixture","usageMetadata":{"candidatesTokenCount":123,"thoughtsTokenCount":10}}`},
		{name: "stop", data: `{"candidates":[{"finishReason":"STOP"}]}`, terminal: "completed"},
		{name: "max tokens", data: `{"candidates":[{"finishReason":"MAX_TOKENS"}]}`, terminal: "incomplete"},
		{name: "safety", data: `{"candidates":[{"finishReason":"SAFETY"}]}`, terminal: "incomplete"},
		{name: "unknown finish", data: `{"candidates":[{"finishReason":"FUTURE_REASON"}]}`, terminal: "incomplete"},
		{name: "prompt block", data: `{"promptFeedback":{"blockReason":"SAFETY"}}`, terminal: "incomplete"},
		{name: "unspecified prompt", data: `{"promptFeedback":{"blockReason":"BLOCKED_REASON_UNSPECIFIED"}}`},
		{name: "error", data: `{"error":{"code":503,"message":"private error"}}`, terminal: "failed"},
		{name: "null error", data: `{"error":null}`},
		{name: "wrapped", data: `{"response":{"candidates":[{"content":{"parts":[{"text":"private text"}]},"finishReason":"STOP"}]}}`, visible: true, terminal: "completed"},
		{name: "outer error wins", data: `{"error":{"code":503},"response":{"candidates":[{"finishReason":"STOP"}]}}`, terminal: "failed"},
		{name: "inner error wins", data: `{"response":{"error":{"code":503},"candidates":[{"finishReason":"STOP"}]}}`, terminal: "failed"},
		{name: "outer candidate discarded", data: `{"candidates":[{"content":{"parts":[{"text":"private outer"}]},"finishReason":"STOP"}],"response":{}}`},
		{name: "non-primary", data: `{"candidates":[{"index":1,"content":{"parts":[{"text":"private secondary"}]},"finishReason":"STOP"}]}`},
		{name: "primary not first", data: `{"candidates":[{"index":1,"finishReason":"MAX_TOKENS"},{"index":0,"content":{"parts":[{"text":"private primary"}]},"finishReason":"STOP"}]}`, visible: true, terminal: "completed"},
		{name: "foreign", data: `{"type":"response.output_text.delta","delta":"private foreign"}`, invalid: true},
		{name: "bad text", data: `{"candidates":[{"content":{"parts":[{"text":123}]}}]}`, invalid: true},
		{name: "bad thought", data: `{"candidates":[{"content":{"parts":[{"text":"private text","thought":"true"}]}}]}`, invalid: true},
		{name: "bad index", data: `{"candidates":[{"index":"0","finishReason":"STOP"}]}`, invalid: true},
		{name: "malformed", data: `{"candidates":`, invalid: true},
		{name: "nested envelope", data: `{"response":{"response":{"candidates":[{"finishReason":"STOP"}]}}}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(time.Now(), 0)
			r := httptest.NewRequest(http.MethodPost, "/v1internal:streamGenerateContent", nil).WithContext(With(t.Context(), c))
			r, _ = StartAttempt(r, 1, 0)
			r, trace := StartTransport(r)
			GeminiContent(r.Context(), []byte(tc.data), true)
			trace.Response(&http.Response{StatusCode: http.StatusOK}, nil)
			c.Finish(http.StatusOK, false)
			c.WhenFinished(func(s Snapshot) {
				a := s.Attempts[1]
				v := DeriveContentObservation(s)
				require.Equal(t, tc.terminal, a.Terminal)
				require.Equal(t, tc.reasoning, v.ReasoningFirstMS != nil)
				require.Equal(t, tc.visible, v.VisibleTextFirstMS != nil)
				require.Equal(t, "typed_gemini_content", v.TextStream.Source)
				_, incomplete := a.Events["typed_content_truncated"]
				require.Equal(t, tc.invalid, incomplete)
				if tc.invalid {
					require.Equal(t, "incomplete_capture", v.TextStream.Status)
				} else if tc.visible {
					require.Equal(t, 1, *v.TextStream.Chunks)
				}
				if tc.nonText {
					require.NotNil(t, a.TextStream)
					require.True(t, a.TextStream.NonTextOutput)
				}
				require.Nil(t, s.Attempts[0].TextStream)
				require.Nil(t, v.OutputRate.TokensPerSecond)
				require.Nil(t, v.Cache.ReadRatio)
				raw, err := json.Marshal(s)
				require.NoError(t, err)
				require.NotContains(t, string(raw), "private")
			})
		})
	}
}

func TestGeminiContentJSONDoesNotInventStream(t *testing.T) {
	c := New(time.Now(), 0)
	r := httptest.NewRequest(http.MethodPost, "/v1beta/models/fixture:generateContent", nil).WithContext(With(t.Context(), c))
	r, _ = StartAttempt(r, 1, 0)
	r, trace := StartTransport(r)
	GeminiContent(r.Context(), []byte(`{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}]}`), false)
	trace.Response(&http.Response{StatusCode: http.StatusOK}, nil)
	c.Finish(http.StatusOK, false)
	c.WhenFinished(func(s Snapshot) {
		v := DeriveContentObservation(s)
		require.NotNil(t, v.VisibleTextFirstMS)
		require.Equal(t, "completed", s.Attempts[1].Terminal)
		require.Equal(t, "not_collected", v.TextStream.Status)
		require.Nil(t, v.TextStream.Chunks)
		require.Nil(t, v.TextStream.MaxGapMS)
	})
}

func TestGeminiContentTerminalAndRetryIsolation(t *testing.T) {
	for _, tc := range []struct {
		trailing string
		invalid  bool
	}{
		{`{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":120}}`, false},
		{`{"error":{"code":503}}`, true},
		{`{"candidates":[{"content":{"parts":[{"text":"late output"}]}}]}`, true},
	} {
		t.Run(tc.trailing, func(t *testing.T) {
			c := New(time.Now(), 0)
			base := httptest.NewRequest(http.MethodPost, "/v1beta/models/fixture:streamGenerateContent", nil).WithContext(With(t.Context(), c))
			r, _ := StartAttempt(base, 1, 0)
			r, first := StartTransport(r)
			GeminiContent(r.Context(), []byte(`{"candidates":[{"content":{"parts":[{"text":"first"}]},"finishReason":"STOP"}]}`), true)
			GeminiContent(r.Context(), []byte(tc.trailing), true)
			first.Response(&http.Response{StatusCode: http.StatusOK}, nil)
			c.WhenFinished(func(s Snapshot) {
				require.Equal(t, tc.invalid, s.Attempts[1].TextStream.Invalid)
				v := DeriveContentObservation(s)
				require.Nil(t, v.VisibleTextFirstMS, "the final attempt must not inherit the previous route's first text")
				require.Nil(t, v.TextStream.Chunks)
				require.EqualValues(t, 2, s.Attempts[3].AccountID)
			})
			r, _ = StartAttempt(base, 2, 0)
			r, last := StartTransport(r)
			GeminiContent(r.Context(), []byte(`{"candidates":[{"finishReason":"STOP"}]}`), true)
			last.Response(&http.Response{StatusCode: http.StatusOK}, nil)
			c.Finish(http.StatusOK, false)
		})
	}
}
