//go:build integration

package service_test

import (
	"bytes"
	"fmt"
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
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAsyncVideoSharedKeyPendingCapacity(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	gin.SetMode(gin.TestMode)
	rdb := startKeyAdmissionIntegrationRedis(t)
	for key, value := range map[string]string{
		"appendonly": "yes", "appendfsync": "always", "no-appendfsync-on-rewrite": "no", "maxmemory-policy": "noeviction",
	} {
		require.NoError(t, rdb.ConfigSet(t.Context(), key, value).Err())
	}
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
			var submissions, polls, competitors atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/competing" {
					competitors.Add(1)
					_, _ = io.WriteString(w, `{"ok":true}`)
					return
				}
				if r.Method == http.MethodGet {
					polls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					switch tc.platform {
					case service.PlatformOpenAI:
						_, _ = io.WriteString(w, `{"status":"succeeded","usage":{"completion_tokens":1}}`)
					case service.PlatformMiniMax:
						_, _ = io.WriteString(w, `{"status":"completed","metadata":{"url":"https://video.example/result.mp4"}}`)
					default:
						_, _ = io.WriteString(w, `{"status":"done","video":{"url":"https://vidgen.x.ai/result.mp4","duration":6}}`)
					}
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
				t.Run(fmt.Sprintf("service_policy=%t", servicePolicy), func(t *testing.T) {
					serviceCfg := cfg
					if !servicePolicy {
						serviceCfg = &config.Config{}
						serviceCfg.Security.URLAllowlist.AllowInsecureHTTP = true
					}
					svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, serviceCfg, nil, nil, nil, nil, nil, transport, nil, nil, nil, nil, nil, nil, nil, nil)
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					owner := service.AsyncVideoKeyOwner{UserID: 10, APIKeyID: 20, GroupID: 30, AccountID: account.ID,
						Provider: tc.endpoint.AsyncVideoProvider(), RouteFingerprint: service.AccountRouteFingerprint(account)}
					ctx := service.WithAsyncVideoKeyOwner(t.Context(), owner)
					c.Request = httptest.NewRequest(http.MethodPost, "/fixture", bytes.NewReader(body)).WithContext(ctx)
					before := submissions.Load()
					var result *service.OpenAIForwardResult
					var err error
					switch tc.platform {
					case service.PlatformOpenAI:
						result, err = svc.ForwardSeedance(ctx, c, account, tc.endpoint, "", body)
					case service.PlatformMiniMax:
						result, err = svc.ForwardMiniMaxH3(ctx, c, account, tc.endpoint, "", body)
					default:
						result, err = svc.ForwardGrokMedia(ctx, c, account, tc.endpoint, "", body, "application/json")
					}
					require.NoError(t, err, "a durable Redis authority must admit the first video creation")
					require.NotNil(t, result)
					require.Equal(t, before+1, submissions.Load())
					require.NotEmpty(t, result.ResponseID)
					lookupEndpoint := service.GrokMediaEndpointVideoStatus
					if tc.platform == service.PlatformOpenAI {
						lookupEndpoint = service.SeedanceEndpointStatus
					} else if tc.platform == service.PlatformMiniMax {
						lookupEndpoint = service.MiniMaxH3EndpointStatus
					}
					groupID := owner.GroupID
					boundAccountID, err := svc.ResolveMediaVideoRequestAccount(ctx, &groupID, result.ResponseID, owner.UserID, owner.APIKeyID, lookupEndpoint)
					require.NoError(t, err, "durable ownership must survive a missing sticky cache")
					require.Equal(t, account.ID, boundAccountID)
					request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/competing", strings.NewReader("{}"))
					require.NoError(t, err)
					request.Header.Set("Authorization", "Bearer async-fixture")
					response, err := transport.Do(request, "", 8, 1)
					if response != nil {
						_ = response.Body.Close()
					}
					require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity, "a queued remote job still owns the slot")
					beforePoll := polls.Load()
					pollContext, _ := gin.CreateTestContext(httptest.NewRecorder())
					pollContext.Request = httptest.NewRequest(http.MethodGet, "/fixture", nil).WithContext(ctx)
					var status *service.OpenAIForwardResult
					switch tc.platform {
					case service.PlatformOpenAI:
						status, err = svc.ForwardSeedance(ctx, pollContext, account, service.SeedanceEndpointStatus, result.ResponseID, nil)
					case service.PlatformMiniMax:
						status, err = svc.ForwardMiniMaxH3(ctx, pollContext, account, service.MiniMaxH3EndpointStatus, result.ResponseID, nil)
					default:
						status, err = svc.ForwardGrokMedia(ctx, pollContext, account, service.GrokMediaEndpointVideoStatus, result.ResponseID, nil, "")
					}
					require.NoError(t, err)
					require.True(t, status.AsyncVideoTerminal)
					require.Equal(t, beforePoll+1, polls.Load())
					require.NoError(t, svc.ReleaseAsyncVideoTaskLease(ctx, result.ResponseID))
					request, err = http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/competing", strings.NewReader("{}"))
					require.NoError(t, err)
					request.Header.Set("Authorization", "Bearer async-fixture")
					beforeCompetitor := competitors.Load()
					response, err = transport.Do(request, "", 8, 1)
					require.NoError(t, err, "terminal confirmation must return capacity")
					require.NoError(t, response.Body.Close())
					require.Equal(t, beforeCompetitor+1, competitors.Load())
				})
			}
		})
	}
}

func TestAsyncVideoSharedKeyRejectionReleasesOnlyKnownFailures(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	gin.SetMode(gin.TestMode)
	redisServer := startKeyAdmissionIntegrationRedis(t)
	for key, value := range map[string]string{
		"appendonly": "yes", "appendfsync": "always", "no-appendfsync-on-rewrite": "no", "maxmemory-policy": "noeviction",
	} {
		require.NoError(t, redisServer.ConfigSet(t.Context(), key, value).Err())
	}
	tests := []struct {
		name       string
		endpoint   service.GrokMediaEndpoint
		platform   string
		body       string
		customCode bool
	}{
		{"grok", service.GrokMediaEndpointVideosGenerations, service.PlatformGrok, `{"model":"grok-imagine-video","prompt":"waves"}`, false},
		{"grok-custom-code", service.GrokMediaEndpointVideosGenerations, service.PlatformGrok, `{"model":"grok-imagine-video","prompt":"waves"}`, true},
		{"seedance", service.SeedanceEndpointCreate, service.PlatformOpenAI, `{"model":"video","content":[{"type":"text","text":"waves"}]}`, false},
		{"minimax", service.MiniMaxH3EndpointCreate, service.PlatformMiniMax, `{"model":"MiniMax-H3","prompt":"waves","seconds":"1"}`, false},
	}
	db := -1
	for _, tc := range tests {
		for _, statusCode := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusInternalServerError} {
			db++
			t.Run(fmt.Sprintf("%s/status=%d", tc.name, statusCode), func(t *testing.T) {
				rdb := redis.NewClient(&redis.Options{Addr: redisServer.Options().Addr, DB: db, MaxRetries: -1})
				t.Cleanup(func() { require.NoError(t, rdb.Close()) })
				var submissions, competitors atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/competing" {
						competitors.Add(1)
						_, _ = io.WriteString(w, `{"ok":true}`)
						return
					}
					submissions.Add(1)
					w.WriteHeader(statusCode)
					_, _ = io.WriteString(w, `{"error":"upstream fixture"}`)
				}))
				t.Cleanup(server.Close)
				cfg := &config.Config{}
				cfg.Security.URLAllowlist.AllowInsecureHTTP = true
				cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{config.UpstreamKeyFingerprint("async-fixture"): 1}}
				transport := repository.ProvideHTTPUpstream(cfg, nil, rdb, nil)
				svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, transport, nil, nil, nil, nil, nil, nil, nil, nil)
				account := &service.Account{ID: 7, Platform: tc.platform, Type: service.AccountTypeAPIKey, Concurrency: 1,
					Credentials: map[string]any{"api_key": "async-fixture", "base_url": server.URL, "account_mode": service.AccountModePayG, "openai_capabilities": []string{"seedance"}}}
				if tc.customCode {
					account.Credentials["custom_error_codes_enabled"] = true
					account.Credentials["custom_error_codes"] = []any{float64(http.StatusBadRequest)}
				}
				owner := service.AsyncVideoKeyOwner{UserID: 10, APIKeyID: 20, GroupID: 30, AccountID: account.ID,
					Provider: tc.endpoint.AsyncVideoProvider(), RouteFingerprint: service.AccountRouteFingerprint(account)}
				ctx := service.WithAsyncVideoKeyOwner(t.Context(), owner)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				body := []byte(tc.body)
				c.Request = httptest.NewRequest(http.MethodPost, "/fixture", bytes.NewReader(body)).WithContext(ctx)
				var err error
				switch tc.platform {
				case service.PlatformOpenAI:
					_, err = svc.ForwardSeedance(ctx, c, account, tc.endpoint, "", body)
				case service.PlatformMiniMax:
					_, err = svc.ForwardMiniMaxH3(ctx, c, account, tc.endpoint, "", body)
				default:
					_, err = svc.ForwardGrokMedia(ctx, c, account, tc.endpoint, "", body, "application/json")
				}
				require.Error(t, err)
				require.EqualValues(t, 1, submissions.Load())
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/competing", strings.NewReader("{}"))
				require.NoError(t, err)
				request.Header.Set("Authorization", "Bearer async-fixture")
				response, err := transport.Do(request, "", 8, 1)
				if response != nil {
					require.NoError(t, response.Body.Close())
				}
				if statusCode == http.StatusForbidden || statusCode == http.StatusTooManyRequests {
					require.NoError(t, err, "a confirmed rejection must release the video slot")
					require.EqualValues(t, 1, competitors.Load())
					require.Zero(t, rdb.ZCard(t.Context(), "concurrency:upstream_key:async_video:unknown").Val())
				} else {
					require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity, "an ambiguous response must retain the video slot")
					require.Zero(t, competitors.Load())
					require.EqualValues(t, 1, rdb.ZCard(t.Context(), "concurrency:upstream_key:async_video:unknown").Val())
				}
			})
		}
	}
}

func TestAsyncVideoCreateDoesNotFollowRedirect(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name     string
		endpoint service.GrokMediaEndpoint
		platform string
		body     string
	}{
		{"grok", service.GrokMediaEndpointVideosGenerations, service.PlatformGrok, `{"model":"grok-imagine-video","prompt":"waves"}`},
		{"seedance", service.SeedanceEndpointCreate, service.PlatformOpenAI, `{"model":"video","content":[{"type":"text","text":"waves"}]}`},
		{"minimax", service.MiniMaxH3EndpointCreate, service.PlatformMiniMax, `{"model":"MiniMax-H3","prompt":"waves","seconds":"1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var initial, redirected atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				redirected.Add(1)
				_, _ = io.WriteString(w, `{"id":"task","request_id":"task"}`)
			}))
			t.Cleanup(destination.Close)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				initial.Add(1)
				w.Header().Set("Location", destination.URL+"/create")
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			t.Cleanup(server.Close)
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			transport := repository.ProvideHTTPUpstream(cfg, nil, nil, nil)
			svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, transport, nil, nil, nil, nil, nil, nil, nil, nil)
			account := &service.Account{ID: 7, Platform: tc.platform, Type: service.AccountTypeAPIKey, Concurrency: 1,
				Credentials: map[string]any{"api_key": "fixture", "base_url": server.URL, "account_mode": service.AccountModePayG, "openai_capabilities": []string{"seedance"}}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			body := []byte(tc.body)
			c.Request = httptest.NewRequest(http.MethodPost, "/fixture", bytes.NewReader(body)).WithContext(t.Context())
			var err error
			switch tc.platform {
			case service.PlatformOpenAI:
				_, err = svc.ForwardSeedance(t.Context(), c, account, tc.endpoint, "", body)
			case service.PlatformMiniMax:
				_, err = svc.ForwardMiniMaxH3(t.Context(), c, account, tc.endpoint, "", body)
			default:
				_, err = svc.ForwardGrokMedia(t.Context(), c, account, tc.endpoint, "", body, "application/json")
			}
			require.Error(t, err)
			require.True(t, service.IsAsyncVideoSubmissionUnconfirmed(err), "%v", err)
			require.Empty(t, recorder.Body.String(), "the handler, not the upstream redirect, owns the error response")
			require.Empty(t, recorder.Header().Get("Location"))
			require.EqualValues(t, 1, initial.Load())
			require.Zero(t, redirected.Load(), "a redirect must not replay the submission")
		})
	}
}

func TestAsyncVideoAdminPreviewSharesFinalKeyGuard(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	gin.SetMode(gin.TestMode)
	rdb := startKeyAdmissionIntegrationRedis(t)
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid request"}`)
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{config.UpstreamKeyFingerprint("async-fixture"): 1}}
	transport := repository.ProvideHTTPUpstream(cfg, nil, rdb, nil)
	account := &service.Account{ID: 7, Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "async-fixture", "base_url": server.URL}}
	repo := &quotaIntegrationAccountRepo{account: account}
	for _, usePolicy := range []bool{true, false} {
		serviceCfg := cfg
		if !usePolicy {
			serviceCfg = &config.Config{}
			serviceCfg.Security.URLAllowlist.AllowInsecureHTTP = true
		}
		svc := service.NewAccountTestService(repo, nil, nil, nil, nil, transport, serviceCfg, nil)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/admin/fixture", nil).WithContext(t.Context())
		err := svc.TestAccountConnection(c, account.ID, "grok-imagine-video", "waves", service.AccountTestModeGrokVideo)
		require.Error(t, err)
		require.Contains(t, recorder.Body.String(), "shared upstream Key")
		require.Zero(t, sends.Load(), "a preview with a restricted Key must not submit a remote task")
	}
	account.Credentials["api_key"] = "unrestricted-fixture"
	svc := service.NewAccountTestService(repo, nil, nil, nil, nil, transport, cfg, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/fixture", nil).WithContext(t.Context())
	err := svc.TestAccountConnection(c, account.ID, "grok-imagine-video", "waves", service.AccountTestModeGrokVideo)
	require.Error(t, err)
	require.Contains(t, recorder.Body.String(), "returned 400")
	require.EqualValues(t, 1, sends.Load(), "an unrestricted preview retains the prior transport behavior")
}
