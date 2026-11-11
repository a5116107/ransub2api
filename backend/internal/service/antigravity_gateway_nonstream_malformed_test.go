package service

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAntigravityMalformedFunctionCallNonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name   string
		frames string
		status int
	}{
		{"signature only", `data: {"response":{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL","content":{"parts":[{"thoughtSignature":"test-signature"}]}}]}}` + "\n\n", http.StatusBadGateway},
		{"error after parts", `data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":{}}}]}}]}}` + "\n\n" + `data: {"response":{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}}` + "\n\n", http.StatusBadGateway},
		{"error before usage", `data: {"response":{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}}` + "\n\n" + `data: {"response":{"usageMetadata":{"promptTokenCount":1}}}` + "\n\n", http.StatusBadGateway},
		{"normal tool", `data: {"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"read","args":{}}}]}}]}}` + "\n\n", http.StatusOK},
		{"normal text", `data: {"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"hello"}]}}]}}` + "\n\n", http.StatusOK},
	} {
		for _, protocol := range []string{"responses", "chat"} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, nil)
				c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/responses", nil)
				resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tc.frames))}
				handle := svc.handleResponsesNonStreamingFromAntigravity
				if protocol == "chat" {
					handle = svc.handleChatCompletionsNonStreamingFromAntigravity
				}
				_, err := handle(c, resp, time.Now(), "test-model")
				require.Equal(t, tc.status, recorder.Code)
				if tc.status == http.StatusBadGateway {
					require.Error(t, err)
					require.Contains(t, recorder.Body.String(), `"type":"malformed_function_call"`)
					require.Contains(t, recorder.Body.String(), "MALFORMED_FUNCTION_CALL")
					require.NotContains(t, recorder.Body.String(), `"status":"completed"`)
				} else {
					require.NoError(t, err)
					if protocol == "responses" {
						require.Contains(t, recorder.Body.String(), `"status":"completed"`)
					} else {
						require.Contains(t, recorder.Body.String(), `"choices"`)
					}
				}
			})
		}
	}
}

func TestAntigravityNonStreamingUsageTail(t *testing.T) {
	for _, tc := range []struct {
		name, tail            string
		input, output, cached int
	}{
		{"separate terminal", `{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":4,"candidatesTokenCount":7,"thoughtsTokenCount":2}}`, 6, 9, 4},
		{"usage after terminal", `{"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":4,"candidatesTokenCount":7,"thoughtsTokenCount":2}}`, 6, 9, 4},
		{"explicit zero", `{"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0}}`, 0, 0, 0},
		{"no replacement", `{"candidates":[{"finishReason":"STOP"}]}`, 8, 3, 0},
	} {
		for _, path := range []string{"messages", "chat", "responses"} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				wire := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}}` + "\n\n" + "data: {\"response\":" + tc.tail + "}\n\n"
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}
				svc := newAntigravityCompatService(config.GatewayConfig{}, nil)
				c, recorder := newAntigravityCompatContext(http.MethodPost, "/fixture", nil)
				handle := svc.handleClaudeStreamToNonStreaming
				switch path {
				case "chat":
					handle = svc.handleChatCompletionsNonStreamingFromAntigravity
				case "responses":
					handle = svc.handleResponsesNonStreamingFromAntigravity
				}
				result, err := handle(c, resp, time.Now(), "fixture")
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Contains(t, recorder.Body.String(), "hello")
				require.Equal(t, tc.input, result.usage.InputTokens)
				require.Equal(t, tc.output, result.usage.OutputTokens)
				require.Equal(t, tc.cached, result.usage.CacheReadInputTokens)
			})
		}
	}
}
