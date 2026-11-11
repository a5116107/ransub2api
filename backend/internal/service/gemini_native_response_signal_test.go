//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Gemini 原生路径带内信号契约：
//   - 上游 2xx 里的带内信号登记 ops；确定的上游故障同时失效可用证据，
//     wire 状态与客户端字节原样不变，用量照常从 usageMetadata 提取（计费不受影响）；
//   - 内容策略类不归因上游账号、不计入 SLA；错误信封与空流按上游失败归因并计入 SLA。
// ---------------------------------------------------------------------------

const geminiSignalTestFinishMessage = "The model output could not be generated. This output contains sensitive words that violate Google's [Generative AI Prohibited Use policy](https://policies.google.com/terms/generative-ai/use-policy). If you think this was an error, [send feedback](https://ai.google.dev/gemini-api/docs/troubleshooting)."

const geminiSignalTestProhibitedSSE = `data: {"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"}}],"modelVersion":"gemini-3.7-flash","responseId":"r1","usageMetadata":{"candidatesTokenCount":26,"promptTokenCount":21775,"thoughtsTokenCount":606,"totalTokenCount":22407}}

data: {"candidates":[{"content":{"parts":[{"text":" world"}],"role":"model"}}],"modelVersion":"gemini-3.7-flash","responseId":"r1","usageMetadata":{"candidatesTokenCount":1289,"promptTokenCount":21775,"thoughtsTokenCount":606,"totalTokenCount":23670}}

data: {"candidates":[{"content":{},"finishMessage":"` + geminiSignalTestFinishMessage + `","finishReason":"PROHIBITED_CONTENT"}],"modelVersion":"gemini-3.7-flash","responseId":"r1","usageMetadata":{"candidatesTokenCount":1289,"promptTokenCount":21775,"thoughtsTokenCount":606,"totalTokenCount":23670}}

`

const geminiSignalTestStopSSE = `data: {"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"}}],"usageMetadata":{"candidatesTokenCount":3,"promptTokenCount":10,"totalTokenCount":13}}

data: {"candidates":[{"content":{"parts":[{"text":"!"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":4,"promptTokenCount":10,"totalTokenCount":14}}

`

func geminiSignalTestAccount() *Account {
	return &Account{
		ID:       703,
		Name:     "gemini-signal-test",
		Platform: PlatformGemini,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "test-key",
		},
	}
}

func newGeminiSignalService(contentType string, body string) *GeminiMessagesCompatService {
	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{contentType}, "X-Request-Id": []string{"upstream-req-1"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		},
	}
	return &GeminiMessagesCompatService{
		httpUpstream:     httpStub,
		cfg:              &config.Config{},
		rateLimitService: NewRateLimitService(&errorPolicyRepoStub{}, nil, &config.Config{}, nil, nil),
	}
}

func geminiSignalTestRequest() []byte {
	return []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
}

func upstreamErrorEventsFromContext(t *testing.T, c *gin.Context) []*OpsUpstreamErrorEvent {
	t.Helper()
	v, ok := c.Get(OpsUpstreamErrorsKey)
	if !ok {
		return nil
	}
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	return events
}

func TestGeminiForwardNative_StreamProhibitedContentMarksInBandErrorAndKeepsBillingUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newGeminiSignalService("text/event-stream; charset=utf-8", geminiSignalTestProhibitedSSE)
	c, rec := newGeminiNativeTestContext(t)

	result, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
		"gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
	require.NoError(t, err)
	require.NotNil(t, result)

	// 客户端字节原样透传，wire 状态 200。
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, geminiSignalTestProhibitedSSE, rec.Body.String())

	// 用量照常提取：input=promptTokenCount，output=candidates+thoughts。
	require.Equal(t, 21775, result.Usage.InputTokens)
	require.Equal(t, 1289+606, result.Usage.OutputTokens)
	require.NotNil(t, result.FirstTokenMs)

	// 带内错误登记为请求级内容策略：不计 SLA、不归因上游账号。
	streamErrs := GetOpsStreamErrors(c)
	require.Len(t, streamErrs, 1)
	require.Equal(t, "invalid_request_error", streamErrs[0].ErrType)
	require.Equal(t, "PROHIBITED_CONTENT", streamErrs[0].Code)
	require.Equal(t, http.StatusBadRequest, streamErrs[0].IntendedStatus)
	require.False(t, streamErrs[0].CountTowardsSLA)
	require.True(t, streamErrs[0].RequestScoped)
	require.False(t, streamErrs[0].NonStream)
	require.Contains(t, streamErrs[0].Message, "finishReason=PROHIBITED_CONTENT")
	require.Contains(t, streamErrs[0].Message, "Prohibited Use policy")

	_, hasUpstreamStatus := c.Get(OpsUpstreamStatusCodeKey)
	require.False(t, hasUpstreamStatus, "内容策略不应写上游错误上下文")
	require.Empty(t, upstreamErrorEventsFromContext(t, c))
}

func TestGeminiForwardNative_StreamErrorEnvelopeMarksUpstreamFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `data: {"candidates":[{"content":{"parts":[{"text":"partial"}],"role":"model"}}],"usageMetadata":{"candidatesTokenCount":2,"promptTokenCount":10,"totalTokenCount":12}}

data: {"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}

`
	svc := newGeminiSignalService("text/event-stream", body)
	c, rec := newGeminiNativeTestContext(t)

	result, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
		"gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, body, rec.Body.String(), "错误信封也原样透传，交给 SDK 客户端按错误处理")
	require.Equal(t, 10, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)

	streamErrs := GetOpsStreamErrors(c)
	require.Len(t, streamErrs, 1)
	require.Equal(t, "rate_limit_error", streamErrs[0].ErrType)
	require.Equal(t, "RESOURCE_EXHAUSTED", streamErrs[0].Code)
	require.Equal(t, http.StatusTooManyRequests, streamErrs[0].IntendedStatus)
	require.True(t, streamErrs[0].CountTowardsSLA)
	require.False(t, streamErrs[0].RequestScoped)
	require.False(t, streamErrs[0].NonStream)
	require.Equal(t, "Resource has been exhausted (e.g. check quota).", streamErrs[0].Message)

	status, ok := c.Get(OpsUpstreamStatusCodeKey)
	require.True(t, ok)
	require.Equal(t, http.StatusTooManyRequests, status)

	events := upstreamErrorEventsFromContext(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "stream_failed", events[0].Kind)
	require.Equal(t, int64(703), events[0].AccountID)
	require.Equal(t, PlatformGemini, events[0].Platform)
	require.Equal(t, http.StatusTooManyRequests, events[0].UpstreamStatusCode)
	require.Equal(t, "upstream-req-1", events[0].UpstreamRequestID)
}

func TestGeminiForwardNative_StreamPromptBlockedMarksContentPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `data: {"promptFeedback":{"blockReason":"SAFETY","safetyRatings":[{"category":"HARM_CATEGORY_HARASSMENT","probability":"HIGH","blocked":true}]},"usageMetadata":{"promptTokenCount":10,"totalTokenCount":10}}

`
	svc := newGeminiSignalService("text/event-stream", body)
	c, rec := newGeminiNativeTestContext(t)

	result, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
		"gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
	require.NoError(t, err)
	require.Equal(t, body, rec.Body.String())
	require.Equal(t, 10, result.Usage.InputTokens)

	streamErrs := GetOpsStreamErrors(c)
	require.Len(t, streamErrs, 1)
	require.Equal(t, "invalid_request_error", streamErrs[0].ErrType)
	require.Equal(t, "SAFETY", streamErrs[0].Code)
	require.Contains(t, streamErrs[0].Message, "blockReason=SAFETY")
	require.False(t, streamErrs[0].CountTowardsSLA)
	require.Empty(t, upstreamErrorEventsFromContext(t, c))
}

// Vertex 拦截 prompt 时 usageMetadata 可能只带 cachedContentTokenCount、缺 promptTokenCount：
// input 下限为 0，缓存命中照上游所报计入 cache_read。
func TestGeminiForwardNative_StreamPromptBlockedWithOnlyCachedTokensKeepsInputNonNegative(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `data: {"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"},"usageMetadata":{"cachedContentTokenCount":12980,"totalTokenCount":0,"trafficType":"ON_DEMAND"},"modelVersion":"gemini-3.5-flash-lite"}

`
	svc := newGeminiSignalService("text/event-stream", body)
	c, rec := newGeminiNativeTestContext(t)

	result, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
		"gemini-3.5-flash-lite", "streamGenerateContent", true, geminiSignalTestRequest())
	require.NoError(t, err)
	require.Equal(t, body, rec.Body.String())
	require.Equal(t, 0, result.Usage.InputTokens)
	require.Equal(t, 12980, result.Usage.CacheReadInputTokens)
	require.Equal(t, 0, result.Usage.OutputTokens)

	streamErrs := GetOpsStreamErrors(c)
	require.Len(t, streamErrs, 1)
	require.Equal(t, "PROHIBITED_CONTENT", streamErrs[0].Code)
}

func TestGeminiForwardNative_EmptyStreamMarksUpstreamFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newGeminiSignalService("text/event-stream", "")
	c, rec := newGeminiNativeTestContext(t)

	result, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
		"gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Zero(t, rec.Body.Len())
	require.Nil(t, result.FirstTokenMs)

	streamErrs := GetOpsStreamErrors(c)
	require.Len(t, streamErrs, 1)
	require.Equal(t, "upstream_error", streamErrs[0].ErrType)
	require.Equal(t, geminiSignalEmptyStreamReason, streamErrs[0].Code)
	require.Equal(t, http.StatusBadGateway, streamErrs[0].IntendedStatus)
	require.True(t, streamErrs[0].CountTowardsSLA)

	events := upstreamErrorEventsFromContext(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "stream_failed", events[0].Kind)
	require.Equal(t, int64(703), events[0].AccountID)
}

func TestGeminiForwardNative_NormalStreamLeavesNoMark(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newGeminiSignalService("text/event-stream", geminiSignalTestStopSSE)
	c, rec := newGeminiNativeTestContext(t)

	result, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
		"gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
	require.NoError(t, err)
	require.Equal(t, geminiSignalTestStopSSE, rec.Body.String())
	require.Equal(t, 4, result.Usage.OutputTokens)
	require.Empty(t, GetOpsStreamErrors(c))
	require.Empty(t, upstreamErrorEventsFromContext(t, c))
	_, hasUpstreamStatus := c.Get(OpsUpstreamStatusCodeKey)
	require.False(t, hasUpstreamStatus)
}

func TestGeminiForwardNative_NonStreamProhibitedContentMarksInBandError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `{"candidates":[{"content":{},"finishMessage":"` + geminiSignalTestFinishMessage + `","finishReason":"PROHIBITED_CONTENT"}],"modelVersion":"gemini-3.7-flash","usageMetadata":{"candidatesTokenCount":120,"promptTokenCount":900,"thoughtsTokenCount":30,"totalTokenCount":1050}}`
	svc := newGeminiSignalService("application/json", body)
	c, rec := newGeminiNativeTestContext(t)

	result, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
		"gemini-3.7-flash", "generateContent", false, geminiSignalTestRequest())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, body, rec.Body.String())
	require.Equal(t, 900, result.Usage.InputTokens)
	require.Equal(t, 150, result.Usage.OutputTokens)

	streamErrs := GetOpsStreamErrors(c)
	require.Len(t, streamErrs, 1)
	require.Equal(t, "invalid_request_error", streamErrs[0].ErrType)
	require.Equal(t, "PROHIBITED_CONTENT", streamErrs[0].Code)
	require.False(t, streamErrs[0].CountTowardsSLA)
	require.True(t, streamErrs[0].RequestScoped)
	require.True(t, streamErrs[0].NonStream)
	require.Empty(t, upstreamErrorEventsFromContext(t, c))
}

func TestGeminiForwardNative_NonStreamErrorEnvelopeOn200MarksUpstreamFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `{"error":{"code":500,"message":"Internal error encountered.","status":"INTERNAL"}}`
	svc := newGeminiSignalService("application/json", body)
	c, rec := newGeminiNativeTestContext(t)

	result, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
		"gemini-3.7-flash", "generateContent", false, geminiSignalTestRequest())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, body, rec.Body.String())

	streamErrs := GetOpsStreamErrors(c)
	require.Len(t, streamErrs, 1)
	require.Equal(t, "upstream_error", streamErrs[0].ErrType)
	require.Equal(t, "INTERNAL", streamErrs[0].Code)
	require.Equal(t, http.StatusInternalServerError, streamErrs[0].IntendedStatus)
	require.True(t, streamErrs[0].CountTowardsSLA)

	require.True(t, streamErrs[0].NonStream)

	events := upstreamErrorEventsFromContext(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "http_error", events[0].Kind)
}

func TestGeminiForwardNative_StreamErrorEnvelopeAfterContentFilterWins(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `data: {"candidates":[{"content":{},"finishReason":"SAFETY"}]}

data: {"error":{"code":503,"message":"The model is overloaded. Please try again later.","status":"UNAVAILABLE"}}

`
	svc := newGeminiSignalService("text/event-stream", body)
	c, rec := newGeminiNativeTestContext(t)

	_, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
		"gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
	require.NoError(t, err)
	require.Equal(t, body, rec.Body.String())

	streamErrs := GetOpsStreamErrors(c)
	require.Len(t, streamErrs, 1)
	require.Equal(t, "UNAVAILABLE", streamErrs[0].Code, "错误信封优先于内容过滤")
	require.Equal(t, "upstream_error", streamErrs[0].ErrType)
	require.Equal(t, http.StatusServiceUnavailable, streamErrs[0].IntendedStatus)
	require.True(t, streamErrs[0].CountTowardsSLA)
	require.Len(t, upstreamErrorEventsFromContext(t, c), 1)
}

func TestGeminiForwardNative_StreamNonSSEJSONBodyIsInspected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Run("json array with content filter", func(t *testing.T) {
		body := `[{
  "candidates": [{"content": {"parts": [{"text": "hello"}], "role": "model"}}],
  "usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 1}
}
,
{
  "candidates": [{"content": {}, "finishReason": "PROHIBITED_CONTENT"}]
}
]
`
		svc := newGeminiSignalService("application/json", body)
		c, rec := newGeminiNativeTestContext(t)
		_, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
			"gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
		require.NoError(t, err)
		require.Equal(t, body, rec.Body.String())
		streamErrs := GetOpsStreamErrors(c)
		require.Len(t, streamErrs, 1)
		require.Equal(t, "PROHIBITED_CONTENT", streamErrs[0].Code)
		require.True(t, streamErrs[0].RequestScoped)
	})
	t.Run("json object error envelope", func(t *testing.T) {
		body := `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}` + "\n"
		svc := newGeminiSignalService("application/json", body)
		c, _ := newGeminiNativeTestContext(t)
		_, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
			"gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
		require.NoError(t, err)
		streamErrs := GetOpsStreamErrors(c)
		require.Len(t, streamErrs, 1)
		require.Equal(t, "RESOURCE_EXHAUSTED", streamErrs[0].Code)
		require.True(t, streamErrs[0].CountTowardsSLA)
	})
	t.Run("healthy json object leaves no mark", func(t *testing.T) {
		body := `{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1}}` + "\n"
		svc := newGeminiSignalService("application/json", body)
		c, _ := newGeminiNativeTestContext(t)
		_, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
			"gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
		require.NoError(t, err)
		require.Empty(t, GetOpsStreamErrors(c))
		require.Empty(t, upstreamErrorEventsFromContext(t, c))
	})
}

func TestGeminiForwardNative_StreamOversizedNonSSEBodyLeavesNoMark(t *testing.T) {
	gin.SetMode(gin.TestMode)
	filler := strings.Repeat("x", geminiSSEFallbackBodyLimit)
	body := `{"candidates":[{"content":{"parts":[{"text":"` + filler + `"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1}}` + "\n"
	svc := newGeminiSignalService("application/json", body)
	c, rec := newGeminiNativeTestContext(t)
	_, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
		"gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
	require.NoError(t, err)
	require.Equal(t, len(body), rec.Body.Len())
	require.Empty(t, GetOpsStreamErrors(c), "超限的兜底体放弃判定，不得记成空流")
	require.Empty(t, upstreamErrorEventsFromContext(t, c))
}

func TestGeminiForwardNative_StreamNonSSEEmptyBodyIsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, body := range map[string]string{
		"empty object":     "{}\n",
		"empty candidates": `{"candidates":[],"usageMetadata":{"promptTokenCount":3}}` + "\n",
		"empty array":      "[]\n",
		"array of empties": "[{\"candidates\":[]}\n,\n{}]\n",
	} {
		t.Run(name, func(t *testing.T) {
			svc := newGeminiSignalService("application/json", body)
			c, rec := newGeminiNativeTestContext(t)
			_, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
				"gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
			require.NoError(t, err)
			require.Equal(t, body, rec.Body.String())
			streamErrs := GetOpsStreamErrors(c)
			require.Len(t, streamErrs, 1)
			require.Equal(t, geminiSignalEmptyStreamReason, streamErrs[0].Code)
			require.True(t, streamErrs[0].CountTowardsSLA)
		})
	}
}

func TestGeminiForwardNative_StreamWithoutDataEventsIsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, body := range map[string]string{
		"keepalive comments only": ": keepalive\n\n: keepalive\n\n",
		"done marker only":        "data: [DONE]\n\n",
		"blank data only":         "data: \n\n",
		"whitespace only":         "\n\n   \n",
	} {
		t.Run(name, func(t *testing.T) {
			svc := newGeminiSignalService("text/event-stream", body)
			c, rec := newGeminiNativeTestContext(t)
			_, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
				"gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
			require.NoError(t, err)
			require.Equal(t, body, rec.Body.String())
			streamErrs := GetOpsStreamErrors(c)
			require.Len(t, streamErrs, 1)
			require.Equal(t, geminiSignalEmptyStreamReason, streamErrs[0].Code)
			require.True(t, streamErrs[0].CountTowardsSLA)
			require.False(t, streamErrs[0].NonStream)
		})
	}
}

func TestGeminiForwardNative_StreamOtherFinishReasonLeavesNoMark(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `data: {"candidates":[{"content":{"parts":[{"text":"full answer"}],"role":"model"},"finishReason":"OTHER"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}

`
	svc := newGeminiSignalService("text/event-stream", body)
	c, _ := newGeminiNativeTestContext(t)
	result, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
		"gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
	require.NoError(t, err)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Empty(t, GetOpsStreamErrors(c))
	require.Empty(t, upstreamErrorEventsFromContext(t, c))
}

func TestGeminiForwardNative_StreamMalformedFunctionCallLeavesNoMark(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `data: {"candidates":[{"content":{"parts":[{"text":"partial"}],"role":"model"},"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}

`
	svc := newGeminiSignalService("text/event-stream", body)
	c, _ := newGeminiNativeTestContext(t)
	result, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
		"gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
	require.NoError(t, err)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Empty(t, GetOpsStreamErrors(c))
	require.Empty(t, upstreamErrorEventsFromContext(t, c))
}

func TestGeminiForwardNative_NonStreamEmptyBodyMarksEmptyResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, body := range map[string]string{
		"blank":            "",
		"empty object":     "{}",
		"empty candidates": `{"candidates":[],"usageMetadata":{"promptTokenCount":3,"totalTokenCount":3}}`,
	} {
		t.Run(name, func(t *testing.T) {
			svc := newGeminiSignalService("application/json", body)
			c, rec := newGeminiNativeTestContext(t)
			result, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
				"gemini-3.7-flash", "generateContent", false, geminiSignalTestRequest())
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, body, rec.Body.String())
			streamErrs := GetOpsStreamErrors(c)
			require.Len(t, streamErrs, 1)
			require.Equal(t, geminiSignalEmptyResponseReason, streamErrs[0].Code)
			require.Equal(t, "upstream_error", streamErrs[0].ErrType)
			require.True(t, streamErrs[0].CountTowardsSLA)
			require.True(t, streamErrs[0].NonStream)
			events := upstreamErrorEventsFromContext(t, c)
			require.Len(t, events, 1)
			require.Equal(t, "http_error", events[0].Kind)
		})
	}
}

func TestGeminiForwardNative_CountTokensBodyLeavesNoMark(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newGeminiSignalService("application/json", `{"totalTokens":12}`)
	c, rec := newGeminiNativeTestContext(t)
	_, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
		"gemini-3.7-flash", "countTokens", false, geminiSignalTestRequest())
	require.NoError(t, err)
	require.Equal(t, `{"totalTokens":12}`, rec.Body.String())
	require.Empty(t, GetOpsStreamErrors(c))
	require.Empty(t, upstreamErrorEventsFromContext(t, c))
}

func TestCollectGeminiSSEObserved_SeesTerminalChunkEvenWhenAggregateDropsIt(t *testing.T) {
	var observed []string
	collected, usage, stats, err := collectGeminiSSEObserved(strings.NewReader(geminiSignalTestProhibitedSSE), false, func(raw []byte) {
		observed = append(observed, string(raw))
	})
	require.NoError(t, err)
	require.Len(t, observed, 3)
	require.Equal(t, 3, stats.dataEvents)
	require.Empty(t, stats.fallback.Bytes())
	require.Equal(t, 21775, usage.InputTokens)

	var hit bool
	for _, raw := range observed {
		if sig, ok := detectGeminiResponseSignal([]byte(raw)); ok {
			hit = true
			require.Equal(t, geminiSignalContentFilter, sig.Kind)
		}
	}
	require.True(t, hit)
	// 聚合结果沿用"最后一个带 parts 的块"，本身不携带末块的 finishReason；检测必须逐事件进行。
	aggregated, err := json.Marshal(collected)
	require.NoError(t, err)
	require.NotContains(t, string(aggregated), "PROHIBITED_CONTENT")
}

type geminiAvailabilityFaultRepo struct {
	AccountRepository
	availability atomic.Int64
	quality      atomic.Int64
	signal       chan struct{}
}

func (r *geminiAvailabilityFaultRepo) ApplyAvailabilityFault(context.Context, int64) error {
	r.availability.Add(1)
	if r.signal != nil {
		select {
		case r.signal <- struct{}{}:
		default:
		}
	}
	return nil
}

func (r *geminiAvailabilityFaultRepo) ApplyQuality5xx(context.Context, int64) error {
	r.quality.Add(1)
	// The production repository atomically fences availability with quality.
	return r.ApplyAvailabilityFault(context.Background(), 0)
}

func TestGeminiForwardNative_AvailabilitySemanticFaults(t *testing.T) {
	for _, stream := range []bool{false, true} {
		mode := "json"
		if stream {
			mode = "stream"
		}
		t.Run(mode, func(t *testing.T) {
			for _, tc := range []struct {
				name, payload         string
				availability, quality int64
			}{
				{"service fault", `{"error":{"code":503,"status":"UNAVAILABLE","message":"fixture fault"}}`, 1, 1},
				{"quota fault", `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`, 1, 0},
				{"authentication fault", `{"error":{"code":401}}`, 1, 0},
				{"request timeout", `{"error":{"code":408}}`, 1, 0},
				{"status without code", `{"error":{"status":"UNAVAILABLE"}}`, 1, 1},
				{"authentication status", `{"error":{"status":"UNAUTHENTICATED"}}`, 1, 0},
				{"quota status", `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, 1, 0},
				{"deadline status", `{"error":{"status":"DEADLINE_EXCEEDED"}}`, 1, 1},
				{"internal status", `{"error":{"status":"INTERNAL"}}`, 1, 1},
				{"invalid request", `{"error":{"code":400,"status":"INVALID_ARGUMENT"}}`, 0, 0},
				{"permission denied", `{"error":{"code":403,"status":"PERMISSION_DENIED"}}`, 0, 0},
				{"policy blocked", `{"promptFeedback":{"blockReason":"SAFETY"}}`, 0, 0},
				{"completed", `{"candidates":[{"content":{"parts":[{"text":"fixture answer"}]},"finishReason":"STOP"}]}`, 0, 0},
				{"quoted error", `{"candidates":[{"content":{"parts":[{"text":"error 503 UNAVAILABLE"}]},"finishReason":"STOP"}]}`, 0, 0},
				{"unclassified error", `{"error":{"message":"fixture unclassified error"}}`, 0, 0},
				{"unknown status", `{"error":{"status":"FUTURE_STATUS"}}`, 0, 0},
				{"string code", `{"error":{"code":"503"}}`, 0, 0},
				{"fractional code", `{"error":{"code":503.9}}`, 0, 0},
				{"invalid code with known status", `{"error":{"code":null,"status":"UNAVAILABLE"}}`, 0, 0},
				{"out of range code", `{"error":{"code":200,"status":"UNAVAILABLE"}}`, 0, 0},
				{"huge code", `{"error":{"code":999999999999999999999}}`, 0, 0},
				{"duplicate code", `{"error":{"code":503,"code":400}}`, 0, 0},
				{"escaped duplicate code", `{"error":{"code":503,"\u0063ode":400}}`, 0, 0},
				{"duplicate status", `{"error":{"status":"UNAVAILABLE","status":"INVALID_ARGUMENT"}}`, 0, 0},
				{"duplicate envelope", `{"error":{"code":503},"error":{"code":400}}`, 0, 0},
				{"empty response", `{}`, 1, 0},
				{"empty candidates", `{"candidates":[]}`, 1, 0},
				{"tool response", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"fixture","args":{}}}]},"finishReason":"STOP"}]}`, 0, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					for _, probe := range []bool{false, true} {
						ctx := t.Context()
						if probe {
							ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
						}
						contentType, body, action := "application/json", tc.payload, "generateContent"
						if stream {
							contentType, body, action = "text/event-stream", "data: "+tc.payload+"\n\n", "streamGenerateContent"
						}
						svc := newGeminiSignalService(contentType, body)
						repo := &geminiAvailabilityFaultRepo{}
						svc.rateLimitService.qualityTrigger = &quality5xxTrigger{availability: repo, immediate: repo}
						c, rec := newGeminiNativeTestContext(t)
						c.Request = c.Request.WithContext(ctx)
						result, err := svc.ForwardNative(ctx, c, geminiSignalTestAccount(), "gemini-3.7-flash", action, stream, geminiSignalTestRequest())
						require.NoError(t, err)
						require.NotNil(t, result)
						require.Equal(t, http.StatusOK, rec.Code)
						require.Equal(t, body, rec.Body.String(), "observation must preserve the upstream wire response")
						wantAvailability, wantQuality := tc.availability, tc.quality
						if probe {
							wantAvailability, wantQuality = 0, 0
						}
						require.Equal(t, wantAvailability, repo.availability.Load(), "probe=%t", probe)
						require.Equal(t, wantQuality, repo.quality.Load(), "probe=%t", probe)
					}
				})
			}
		})
	}
}

func TestGeminiForwardNative_AvailabilityFaultSequences(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType string
		stream                  bool
		availability, quality   int64
	}{
		{"repeated fault", "data: {\"error\":{\"code\":503}}\n\ndata: {\"error\":{\"code\":503}}\n\n", "text/event-stream", true, 1, 1},
		{"request then fault", "data: {\"error\":{\"code\":400}}\n\ndata: {\"error\":{\"code\":503}}\n\n", "text/event-stream", true, 1, 1},
		{"availability then quality", "data: {\"error\":{\"code\":401}}\n\ndata: {\"error\":{\"code\":503}}\n\n", "text/event-stream", true, 2, 1},
		{"quality then availability", "data: {\"error\":{\"code\":503}}\n\ndata: {\"error\":{\"code\":429}}\n\n", "text/event-stream", true, 1, 1},
		{"empty then answer", "data: {}\n\n" + geminiSignalTestStopSSE, "text/event-stream", true, 0, 0},
		{"empty events", "data: {}\n\ndata: {\"candidates\":[]}\n\n", "text/event-stream", true, 1, 0},
		{"heartbeat only", ": keepalive\n\ndata: [DONE]\n\n", "text/event-stream", true, 1, 0},
		{"raw JSON fault", `{ "error": { "code": 503 } }`, "application/json", true, 1, 1},
		{"raw JSON unknown", `{ "error": { "message": "fixture" } }`, "application/json", true, 0, 0},
		{"array request then fault", `[{"error":{"code":400}},{"error":{"code":503}}]`, "application/json", false, 1, 1},
		{"array unknown then fault", `[{"error":{"message":"fixture"}},{"error":{"code":503}}]`, "application/json", false, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newGeminiSignalService(tc.contentType, tc.body)
			limiter, queue := qualitySignalTestLimiter(t)
			repo := &geminiAvailabilityFaultRepo{}
			limiter.qualityTrigger.availability, limiter.qualityTrigger.immediate = repo, repo
			svc.rateLimitService = limiter
			c, rec := newGeminiNativeTestContext(t)
			action := "generateContent"
			if tc.stream {
				action = "streamGenerateContent"
			}
			result, err := svc.ForwardNative(c.Request.Context(), c, geminiSignalTestAccount(), "gemini-3.7-flash", action, tc.stream, geminiSignalTestRequest())
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, tc.body, rec.Body.String())
			require.Equal(t, tc.availability, repo.availability.Load())
			require.Equal(t, tc.quality, repo.quality.Load())
			require.Equal(t, tc.quality > 0, queue.Exists(quality5xxPendingKey))
			if tc.quality > 0 {
				require.Equal(t, []string{"703"}, qualityTestMembers(t, queue))
			}
		})
	}
}

func TestGeminiForwardNative_AvailabilityOAuthWrappedFaults(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, payload string
			faults        int64
		}{
			{"known", `{"response":{"error":{"code":503}}}`, 1},
			{"duplicate wrapper", `{"response":{"error":{"code":503}},"response":{"error":{"code":400}}}`, 0},
			{"duplicate code", `{"response":{"error":{"code":503,"code":400}}}`, 0},
			{"unknown", `{"response":{"error":{"message":"fixture"}}}`, 0},
			{"empty", `{"response":{}}`, 1},
		} {
			t.Run(tc.name+"/"+map[bool]string{false: "aggregate", true: "stream"}[stream], func(t *testing.T) {
				svc := newGeminiSignalService("text/event-stream", "data: "+tc.payload+"\n\n")
				svc.tokenProvider = &GeminiTokenProvider{}
				repo := &geminiAvailabilityFaultRepo{}
				svc.rateLimitService.qualityTrigger = &quality5xxTrigger{availability: repo, immediate: repo}
				account := geminiSignalTestAccount()
				account.Type = AccountTypeOAuth
				account.Credentials["access_token"], account.Credentials["project_id"] = "fixture-token", "fixture-project"
				c, _ := newGeminiNativeTestContext(t)
				action := "generateContent"
				if stream {
					action = "streamGenerateContent"
				}
				result, err := svc.ForwardNative(c.Request.Context(), c, account, "gemini-3.7-flash", action, stream, geminiSignalTestRequest())
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, tc.faults, repo.availability.Load())
				if tc.name == "known" {
					require.EqualValues(t, 1, repo.quality.Load())
				} else {
					require.Zero(t, repo.quality.Load())
				}
			})
		}
	}
}

func TestGeminiForwardNative_AvailabilityCanceledEmptyDoesNotFault(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			svc := newGeminiSignalService("application/json", "")
			stub, ok := svc.httpUpstream.(*geminiCompatHTTPUpstreamStub)
			require.True(t, ok)
			stub.response.Body = &geminiAbortingStreamBody{data: bytes.NewReader(nil), abortErr: io.EOF, onAbort: cancel}
			repo := &geminiAvailabilityFaultRepo{}
			svc.rateLimitService.qualityTrigger = &quality5xxTrigger{availability: repo, immediate: repo}
			c, _ := newGeminiNativeTestContext(t)
			c.Request = c.Request.WithContext(ctx)
			action := "generateContent"
			if stream {
				action, stub.response.Header = "streamGenerateContent", http.Header{"Content-Type": {"text/event-stream"}}
			}
			_, err := svc.ForwardNative(ctx, c, geminiSignalTestAccount(), "gemini-3.7-flash", action, stream, geminiSignalTestRequest())
			require.NoError(t, err)
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.Zero(t, repo.availability.Load())
			require.Zero(t, repo.quality.Load())
		})
	}
}

func TestGeminiForwardNative_AvailabilityFaultBeforeStreamClosure(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"error\":{\"code\":503,\"status\":\"UNAVAILABLE\"}}\n\n")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server has no HTTP flusher")
			return
		}
		flusher.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	defer close(release)
	svc := newGeminiSignalService("", "")
	svc.cfg = rawChatCompletionsTestConfig()
	svc.httpUpstream = &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		return server.Client().Do(req)
	}}
	account := geminiSignalTestAccount()
	account.Credentials["base_url"] = server.URL
	repo := &geminiAvailabilityFaultRepo{signal: make(chan struct{}, 1)}
	svc.rateLimitService.qualityTrigger = &quality5xxTrigger{availability: repo, immediate: repo}
	c, rec := newGeminiNativeTestContext(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := svc.ForwardNative(ctx, c, account, "gemini-3.7-flash", "streamGenerateContent", true, geminiSignalTestRequest())
		done <- err
	}()
	select {
	case <-repo.signal:
		require.EqualValues(t, 1, repo.availability.Load())
		require.EqualValues(t, 1, repo.quality.Load())
	case <-time.After(2 * time.Second):
		t.Error("the explicit upstream fault must invalidate the old pass before the connection closes")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("native forwarding did not finish after cancellation")
	}
	require.Equal(t, http.StatusOK, rec.Code)
}
