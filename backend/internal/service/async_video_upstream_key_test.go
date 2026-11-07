//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/stretchr/testify/require"
)

type asyncVideoKeyGuardStub struct {
	HTTPUpstream
	supported bool
}

func TestAsyncVideoKeyGuardPreservesExistingTaskOperations(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	for _, tc := range []struct {
		name     string
		endpoint GrokMediaEndpoint
		platform string
		method   string
		taskID   string
	}{
		{"grok-status", GrokMediaEndpointVideoStatus, PlatformGrok, http.MethodGet, "task"},
		{"seedance-status", SeedanceEndpointStatus, PlatformOpenAI, http.MethodGet, SeedanceTaskKey("task")},
		{"seedance-delete", SeedanceEndpointDelete, PlatformOpenAI, http.MethodDelete, SeedanceTaskKey("task")},
		{"minimax-status", MiniMaxH3EndpointStatus, PlatformMiniMax, http.MethodGet, MiniMaxH3TaskKey("task")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
			upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"task","request_id":"task","status":"queued"}`)}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
			account := &Account{ID: 1, Platform: tc.platform, Type: AccountTypeAPIKey, Credentials: map[string]any{
				"api_key": "fixture", "base_url": "https://fixture.example/v1", "openai_capabilities": []string{"seedance"},
			}}
			c, _ := grokMediaContentTestContext(tc.method, "/fixture", nil)
			var err error
			switch tc.platform {
			case PlatformOpenAI:
				_, err = svc.ForwardSeedance(context.Background(), c, account, tc.endpoint, tc.taskID, nil)
			case PlatformMiniMax:
				_, err = svc.ForwardMiniMaxH3(context.Background(), c, account, tc.endpoint, tc.taskID, nil)
			default:
				_, err = svc.ForwardGrokMedia(context.Background(), c, account, tc.endpoint, tc.taskID, nil, "")
			}
			require.NoError(t, err)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, tc.method, upstream.request.Method)
		})
	}
}

func (s *asyncVideoKeyGuardStub) SupportsAsyncVideoKeyGuard() bool { return s.supported }

func TestAsyncVideoKeyGuardRequestCapability(t *testing.T) {
	for _, endpoint := range []GrokMediaEndpoint{GrokMediaEndpointVideosGenerations, GrokMediaEndpointVideosEdits,
		GrokMediaEndpointVideosExtensions, SeedanceEndpointCreate, MiniMaxH3EndpointCreate} {
		t.Run(string(endpoint), func(t *testing.T) {
			for _, policy := range []string{"disabled", "shared", "regional_concurrency", "regional_scheduler"} {
				t.Run(policy, func(t *testing.T) {
					cfg := &config.Config{}
					switch policy {
					case "shared":
						cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
					case "regional_concurrency":
						cfg.Runtime.RegionalConcurrencyFile = "fixture"
					case "regional_scheduler":
						cfg.Runtime.RegionalSchedulerFile = "fixture"
					}
					req := httptest.NewRequest(http.MethodPost, "/fixture", nil)
					for _, supported := range []bool{false, true} {
						svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &asyncVideoKeyGuardStub{supported: supported}}
						got, err := svc.guardAsyncVideoKeyRequest(req, endpoint)
						if policy != "disabled" && !supported {
							require.Nil(t, got)
							require.True(t, IsUpstreamKeyAdmissionFailure(err), "%v", err)
							continue
						}
						require.NoError(t, err)
						active, required := AsyncVideoKeyGuardState(got)
						require.Equal(t, supported, active)
						require.Equal(t, supported && policy != "disabled", required)
						original, _ := AsyncVideoKeyGuardState(req)
						require.False(t, original, "the caller's request must remain unchanged")
					}
				})
			}
		})
	}

	cfg := &config.Config{}
	cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &asyncVideoKeyGuardStub{supported: true}}
	for _, endpoint := range []GrokMediaEndpoint{GrokMediaEndpointImagesGenerations, GrokMediaEndpointVideoStatus,
		GrokMediaEndpointVideoContent, SeedanceEndpointStatus, SeedanceEndpointDelete, MiniMaxH3EndpointStatus, MiniMaxH3EndpointContent} {
		req := httptest.NewRequest(http.MethodGet, "/fixture", nil)
		got, err := svc.guardAsyncVideoKeyRequest(req, endpoint)
		require.NoError(t, err)
		require.Same(t, req, got, "existing task operations and synchronous images must retain their HTTP behavior")
	}
}
