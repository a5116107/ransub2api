package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPrismErrorDoesNotAppendFallback(t *testing.T) {
	for _, status := range []int{400, 401, 409, 422, 429, 502, 200} {
		for _, mode := range []string{"json", "stream", "stream_started"} {
			func() {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"type":"unsupported_request","message":"fixture"}}`)
				}))
				defer server.Close()
				cfg := &config.Config{}
				cfg.Gateway.PrismBrowser = config.GatewayPrismBrowserConfig{Enabled: true, BaseURL: server.URL + "/v1", APIKey: "fixture"}
				account := &service.Account{ID: 300, Status: service.StatusActive, Schedulable: true, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
					Credentials: map[string]any{"access_token": "fixture"}, Extra: map[string]any{"openai_prism_browser": true}}
				gateway := service.NewOpenAIGatewayService(excelBPSErrorAccountRepo{account: account}, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				body, err := json.Marshal(map[string]any{"model": "gpt-6.1-sol", "input": "hi", "stream": mode != "json"})
				require.NoError(t, err)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				if mode == "stream_started" {
					c.Header("Content-Type", "text/event-stream")
					_, err = c.Writer.WriteString(": keepalive\n\n")
					require.NoError(t, err)
					c.Writer.Flush()
				}
				before := c.Writer.Size()
				_, err = gateway.Forward(context.Background(), c, account, body)
				require.Error(t, err)
				response := rec.Body.String()
				if !openAIForwardErrorAlreadyCommunicated(c, before, err) {
					require.False(t, (&OpenAIGatewayHandler{}).ensureForwardErrorResponse(c, false))
				}
				require.Equal(t, response, rec.Body.String())
				if mode == "stream_started" {
					require.Equal(t, 1, strings.Count(response, "event: response.failed"))
					for _, line := range strings.Split(response, "\n") {
						if strings.HasPrefix(line, "data: ") {
							require.True(t, json.Valid([]byte(strings.TrimPrefix(line, "data: "))))
						}
					}
				} else {
					require.True(t, json.Valid(rec.Body.Bytes()), response)
					require.NotContains(t, response, "event:")
				}
				require.True(t, service.IsPrismBrowserAttempt(c, 300))
			}()
		}
	}
}

func TestPrismAdmissionErrorDoesNotAppendFallback(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	for _, mode := range []string{"json", "stream", "stream_started"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.PrismBrowser = config.GatewayPrismBrowserConfig{Enabled: true, BaseURL: server.URL + "/v1", APIKey: "fixture"}
			cfg.Runtime.RegionalConcurrencyFile = "fixture"
			account := &service.Account{ID: 300, Status: service.StatusActive, Schedulable: true, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "fixture"}, Extra: map[string]any{"openai_prism_browser": true}}
			gateway := service.NewOpenAIGatewayService(excelBPSErrorAccountRepo{account: account}, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			body, err := json.Marshal(map[string]any{"model": "gpt-6.1-sol", "input": "hi", "stream": mode != "json"})
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if mode == "stream_started" {
				c.Header("Content-Type", "text/event-stream")
				_, err = c.Writer.WriteString(": keepalive\n\n")
				require.NoError(t, err)
				c.Writer.Flush()
			}
			before := c.Writer.Size()
			original := rec.Body.String()
			_, err = gateway.Forward(context.Background(), c, account, body)
			require.True(t, service.IsUpstreamKeyAdmissionFailure(err))
			var failover *service.UpstreamFailoverError
			require.True(t, errors.As(err, &failover))
			require.Equal(t, original, rec.Body.String(), "admission must not write before failover")
			require.True(t, openAIForwardMayFailover(c, before, failover))
			require.Zero(t, requests.Load())
			(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, failover, mode == "stream_started")
			response := rec.Body.String()
			require.Contains(t, response, "admission_unavailable")
			if mode == "stream_started" {
				require.Equal(t, 1, strings.Count(response, "event: response.failed"))
			} else {
				require.Equal(t, http.StatusServiceUnavailable, rec.Code)
				require.True(t, json.Valid(rec.Body.Bytes()), response)
			}
		})
	}
}

type prismScopeNativeUpstream struct {
	service.HTTPUpstream
	calls int
}

func (u *prismScopeNativeUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"native fixture"}}`))}, nil
}

func TestPrismUnselectedModelsUseExistingNativeRoute(t *testing.T) {
	for _, model := range []string{"gpt-4o-audio-preview", "gpt-5.6-sol"} {
		t.Run(model, func(t *testing.T) {
			upstream := &prismScopeNativeUpstream{}
			cfg := &config.Config{}
			// An unreachable adapter must not interfere with unselected models.
			cfg.Gateway.PrismBrowser = config.GatewayPrismBrowserConfig{Enabled: true, BaseURL: "http://127.0.0.1:1/v1", APIKey: "fixture"}
			account := &service.Account{ID: 300, Status: service.StatusActive, Schedulable: true, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "fixture"}, Extra: map[string]any{"openai_prism_browser": true,
					service.PrismBrowserModelsKey: []string{"gpt-6.1-sol"}, "openai_passthrough": true}}
			gateway := service.NewOpenAIGatewayService(excelBPSErrorAccountRepo{account: account}, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
			body, err := json.Marshal(map[string]any{"model": model, "input": "hi"})
			require.NoError(t, err)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			_, _ = gateway.Forward(context.Background(), c, account, body)
			require.Equal(t, 1, upstream.calls)
			require.False(t, service.IsPrismBrowserAttempt(c, account.ID))
		})
	}
}
