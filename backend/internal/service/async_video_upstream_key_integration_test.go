//go:build integration

package service_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAsyncVideoSharedKeyPendingCapacity(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	gin.SetMode(gin.TestMode)
	rdb := startKeyAdmissionIntegrationRedis(t)
	for _, tc := range []struct {
		name     string
		endpoint service.GrokMediaEndpoint
		platform string
		body     string
	}{
		{"grok-create", service.GrokMediaEndpointVideosGenerations, service.PlatformGrok, `{"model":"grok-imagine-video","prompt":"waves"}`},
		{"grok-edit", service.GrokMediaEndpointVideosEdits, service.PlatformGrok, `{"model":"grok-imagine-video","prompt":"waves","video":{"url":"https://example.com/a.mp4"}}`},
		{"grok-extension", service.GrokMediaEndpointVideosExtensions, service.PlatformGrok, `{"model":"grok-imagine-video","prompt":"waves","video":{"url":"https://example.com/a.mp4"}}`},
		{"seedance", service.SeedanceEndpointCreate, service.PlatformOpenAI, `{"model":"video","content":[{"type":"text","text":"waves"}]}`},
		{"minimax", service.MiniMaxH3EndpointCreate, service.PlatformMiniMax, `{"model":"MiniMax-H3","prompt":"waves","seconds":"1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var submissions, competitors atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/competing" {
					competitors.Add(1)
					_, _ = io.WriteString(w, `{"ok":true}`)
					return
				}
				submissions.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"task-fixture","request_id":"task-fixture","status":"queued"}`)
			}))
			t.Cleanup(server.Close)
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{config.UpstreamKeyFingerprint("async-fixture"): 1}}
			transport := repository.ProvideHTTPUpstream(cfg, nil, rdb, nil)
			account := &service.Account{ID: 7, Platform: tc.platform, Type: service.AccountTypeAPIKey, Concurrency: 1,
				Credentials: map[string]any{"api_key": "async-fixture", "base_url": server.URL, "account_mode": service.AccountModePayG, "openai_capabilities": []string{"seedance"}}}
			body := []byte(tc.body)
			for _, servicePolicy := range []bool{true, false} {
				serviceCfg := cfg
				if !servicePolicy {
					serviceCfg = &config.Config{}
					serviceCfg.Security.URLAllowlist.AllowInsecureHTTP = true
				}
				svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, serviceCfg, nil, nil, nil, nil, nil, transport, nil, nil, nil, nil, nil, nil, nil, nil)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/fixture", bytes.NewReader(body)).WithContext(t.Context())
				before := submissions.Load()
				var result *service.OpenAIForwardResult
				var err error
				switch tc.platform {
				case service.PlatformOpenAI:
					result, err = svc.ForwardSeedance(t.Context(), c, account, tc.endpoint, "", body)
				case service.PlatformMiniMax:
					result, err = svc.ForwardMiniMaxH3(t.Context(), c, account, tc.endpoint, "", body)
				default:
					result, err = svc.ForwardGrokMedia(t.Context(), c, account, tc.endpoint, "", body, "application/json")
				}
				if err != nil {
					require.True(t, service.IsUpstreamKeyAdmissionFailure(err), "%v", err)
					require.Nil(t, result)
					require.Equal(t, before, submissions.Load(), "a locally refused create must not reach the provider")
					continue
				}
				require.NotNil(t, result)
				require.Equal(t, before+1, submissions.Load())
				require.NotEmpty(t, result.ResponseID)
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/competing", strings.NewReader("{}"))
				require.NoError(t, err)
				request.Header.Set("Authorization", "Bearer async-fixture")
				response, err := transport.Do(request, "", 8, 1)
				if response != nil {
					_ = response.Body.Close()
				}
				require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity, "a queued remote job still owns the slot")
				require.Zero(t, competitors.Load())
			}
		})
	}
}
