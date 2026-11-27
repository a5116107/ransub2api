package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIStreamBareServerFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		want         bool
	}{
		{"production internal error", `"type":"server_error","code":"internal_server_error","message":"siwc executor: stream ended without terminal event"`, true},
		{"server type", `"type":"server_error","message":"upstream failed"`, true},
		{"server code", `"code":"server_error","message":"upstream failed"`, true},
		{"explicit 502", `"status_code":502,"message":"upstream failed"`, true},
		{"explicit 503 alias", `"status":503,"message":"upstream failed"`, true},
		{"explicit 504", `"status_code":504,"message":"upstream failed"`, true},
		{"unknown remains conservative", `"message":"upstream rejected request"`, false},
		{"invalid parameter with server code", `"type":"invalid_request_error","code":"server_error","message":"unknown parameter"`, false},
		{"policy with server status", `"status":502,"code":"content_policy_violation","message":"blocked"`, false},
		{"cyber policy with server type", `"type":"server_error","code":"cyber_policy","message":"blocked"`, false},
		{"encrypted history", `"status":502,"code":"invalid_encrypted_content","message":"invalid history"`, false},
		{"context window", `"type":"server_error","code":"context_length_exceeded","message":"input exceeds the context window"`, false},
		{"permission with server code", `"status":403,"code":"server_error","message":"forbidden content"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(`{"type":"error","error":{` + tc.fields + `}}`)
			require.Equal(t, tc.want, openAIStreamErrorEventShouldFailover(payload, extractOpenAISSEErrorMessage(payload)))
		})
	}
}

func TestOpenAIStreamBareServerFailureBeforeAndAfterOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, output := range []bool{false, true} {
			name := "native"
			if passthrough {
				name = "passthrough"
			}
			if output {
				name += "/after_output"
			} else {
				name += "/before_output"
			}
			t.Run(name, func(t *testing.T) {
				c, rec := newNonStreamingFailoverContext(t)
				svc := newNonStreamingFailoverService()
				account := &Account{ID: 6, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
				body := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_failed\"}}\n\n"
				if output {
					body += "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial answer\"}\n\n"
				}
				body += "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"code\":\"internal_server_error\",\"message\":\"siwc executor: stream ended without terminal event\"}}\n\n"
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
				var err error
				if passthrough {
					_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
				} else {
					_, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
				}
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.Equal(t, !output, errors.As(err, &failover), err.Error())
				if output {
					require.Contains(t, rec.Body.String(), "partial answer")
					require.NotContains(t, rec.Body.String(), "response.completed")
				} else {
					require.Empty(t, rec.Body.String(), "discard failed attempt metadata before choosing another account")
					require.False(t, c.Writer.Written())
					require.Equal(t, http.StatusBadGateway, failover.StatusCode)
				}
			})
		}
	}
}

func TestOpenAINonStreamingBareServerFailureBeforeOutput(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "passthrough"}[passthrough], func(t *testing.T) {
			c, rec := newNonStreamingFailoverContext(t)
			svc := newNonStreamingFailoverService()
			body := sseTerminalBody("error", `{"type":"error","error":{"type":"server_error","code":"internal_server_error","message":"siwc executor: stream ended without terminal event"}}`)
			var err error
			if passthrough {
				_, err = svc.handlePassthroughSSEToJSON(newNonStreamingSSEResponse(), c, newNonStreamingFailoverAccount(), body, "model", "model")
			} else {
				_, err = svc.handleSSEToJSON(newNonStreamingSSEResponse(), c, newNonStreamingFailoverAccount(), body, "model", "model")
			}
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Empty(t, rec.Body.String())
			require.False(t, c.Writer.Written())
		})
	}
}

func TestOpenAIStreamReadFailureAfterOutputRetainsUpstreamAttribution(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "passthrough"}[passthrough], func(t *testing.T) {
			c, rec := newNonStreamingFailoverContext(t)
			svc := newNonStreamingFailoverService()
			account := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			upstreamErr := errors.New("read tcp private-address: read: connection reset by peer")
			body := &openAIStreamReadThenErrorCloser{reader: strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial answer\"}\n\n"), err: upstreamErr}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"upstream-read-failure"}}, Body: body}
			var err error
			if passthrough {
				_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
			} else {
				_, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
			}
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover), "committed output must never be replayed")
			require.Contains(t, rec.Body.String(), "partial answer")
			require.NotContains(t, rec.Body.String(), "response.completed")
			if passthrough {
				require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed\n"))
				require.Contains(t, rec.Body.String(), OpenAIUpstreamStreamReadErrorCode)
			}
			events, ok := c.Get(OpsUpstreamErrorsKey)
			require.True(t, ok, "read failure must retain provider attribution rather than become a platform error")
			upstreamEvents, ok := events.([]*OpsUpstreamErrorEvent)
			require.True(t, ok)
			require.Len(t, upstreamEvents, 1)
			require.Equal(t, int64(9), upstreamEvents[0].AccountID)
			require.Equal(t, "upstream-read-failure", upstreamEvents[0].UpstreamRequestID)
			require.Equal(t, "stream_read_error", upstreamEvents[0].Kind)
			require.Zero(t, upstreamEvents[0].UpstreamStatusCode, "connection reset is not an upstream HTTP 502")
			require.NotContains(t, upstreamEvents[0].Message, "private-address")
			marked, ok := GetOpsStreamError(c)
			require.True(t, ok)
			require.NotNil(t, marked.TransportFailure)
			require.Equal(t, OpenAIUpstreamStreamReadErrorCode, marked.Code)
			require.Zero(t, marked.UpstreamStatus)
			require.True(t, marked.CountTowardsSLA)
		})
	}
}

func TestOpenAIStreamReadFailureExcludesLocalCancellationAndLimits(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, ErrUpstreamResponseBodyTooLarge, nil} {
		c, _ := newNonStreamingFailoverContext(t)
		recordOpenAIStreamReadFailure(c, &Account{ID: 9}, "request", false, err)
		_, marked := GetOpsStreamError(c)
		require.False(t, marked)
		_, exists := c.Get(OpsUpstreamErrorsKey)
		require.False(t, exists)
	}
	c, _ := newNonStreamingFailoverContext(t)
	ctx, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(ctx)
	recordOpenAIStreamReadFailure(c, &Account{ID: 9}, "request", false, errors.New("read reset"))
	_, marked := GetOpsStreamError(c)
	require.False(t, marked)
}

func TestOpenAIStreamReadFailureUserDetailsDoNotInventHTTP2(t *testing.T) {
	status := http.StatusBadGateway
	detail := ToUserErrorRequestDetail(&OpsErrorLogDetail{OpsErrorLog: OpsErrorLog{Type: "transport_error", Phase: "network", StatusCode: http.StatusOK, Message: "private-address"}, UpstreamStatusCode: &status,
		ErrorBody: `{"error":{"code":"upstream_stream_read_error","reset_reason":"INTERNAL_ERROR","cause":"private-address"}}`})
	require.Nil(t, detail.UpstreamStatusCode)
	require.Empty(t, detail.ResetReason)
	require.NotContains(t, detail.ErrorBody, "HTTP/2")
	require.NotContains(t, detail.ErrorBody, "INTERNAL_ERROR")
	require.NotContains(t, detail.ErrorBody, "private-address")
	require.Contains(t, detail.ErrorBody, OpenAIUpstreamStreamReadErrorCode)
	require.Contains(t, detail.Message, "Do not automatically retry")
	for _, body := range []string{"", "null", "{invalid", `{"error":{"code":"unknown"}}`} {
		unknown := ToUserErrorRequestDetail(&OpsErrorLogDetail{OpsErrorLog: OpsErrorLog{Type: "transport_error"}, ErrorBody: body})
		require.NotContains(t, unknown.ErrorBody, "HTTP/2")
		require.Contains(t, unknown.ErrorBody, OpenAIUpstreamStreamReadErrorCode)
	}
	verified := ToUserErrorRequestDetail(&OpsErrorLogDetail{OpsErrorLog: OpsErrorLog{Type: "transport_error"},
		ErrorBody: `{"error":{"code":"upstream_http2_stream_error"}}`})
	require.Contains(t, verified.ErrorBody, "HTTP/2")
	require.Contains(t, verified.ErrorBody, OpenAIUpstreamHTTP2StreamErrorCode)
}
