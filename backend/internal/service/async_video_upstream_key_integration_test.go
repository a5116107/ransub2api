//go:build integration

package service_test

import (
	"bytes"
	"context"
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

type asyncVideoBindAckLostUpstream struct {
	service.HTTPUpstream
	service.AsyncVideoKeyLeaseController
	service.AsyncVideoKeyGuard
	binds atomic.Int32
}

func (s *asyncVideoBindAckLostUpstream) BindAsyncVideoKeyLease(ctx context.Context, handoff *service.AsyncVideoKeyHandoff, taskID string) error {
	err := s.AsyncVideoKeyLeaseController.BindAsyncVideoKeyLease(ctx, handoff, taskID)
	if s.binds.Add(1) == 1 && err == nil {
		return io.ErrUnexpectedEOF
	}
	return err
}

func TestAsyncVideoSharedKeyBindAckLossRecovers(t *testing.T) {
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
		{"grok", service.GrokMediaEndpointVideosGenerations, service.PlatformGrok, `{"model":"grok-imagine-video","prompt":"waves"}`},
		{"seedance", service.SeedanceEndpointCreate, service.PlatformOpenAI, `{"model":"video","content":[{"type":"text","text":"waves"}]}`},
		{"minimax", service.MiniMaxH3EndpointCreate, service.PlatformMiniMax, `{"model":"MiniMax-H3","prompt":"waves","seconds":"1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var submissions atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				submissions.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"task-fixture","request_id":"task-fixture","status":"queued"}`)
			}))
			t.Cleanup(server.Close)
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{config.UpstreamKeyFingerprint("async-fixture"): 1}}
			transport := repository.ProvideHTTPUpstream(cfg, nil, rdb, nil)
			controller, ok := transport.(service.AsyncVideoKeyLeaseController)
			require.True(t, ok)
			guard, ok := transport.(service.AsyncVideoKeyGuard)
			require.True(t, ok)
			upstream := &asyncVideoBindAckLostUpstream{HTTPUpstream: transport, AsyncVideoKeyLeaseController: controller, AsyncVideoKeyGuard: guard}
			account := &service.Account{ID: 7, Platform: tc.platform, Type: service.AccountTypeAPIKey, Concurrency: 1,
				Credentials: map[string]any{"api_key": "async-fixture", "base_url": server.URL, "account_mode": service.AccountModePayG, "openai_capabilities": []string{"seedance"}}}
			owner := service.AsyncVideoKeyOwner{UserID: 10, APIKeyID: 20, GroupID: 30, AccountID: account.ID,
				Provider: tc.endpoint.AsyncVideoProvider(), RouteFingerprint: service.AccountRouteFingerprint(account)}
			ctx := service.WithAsyncVideoKeyOwner(t.Context(), owner)
			body := []byte(tc.body)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/fixture", bytes.NewReader(body)).WithContext(ctx)
			svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
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
			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotEmpty(t, result.ResponseID)
			require.Equal(t, int32(1), submissions.Load(), "binding recovery must not create another upstream job")
			require.Equal(t, int32(2), upstream.binds.Load())
			lease, err := controller.LoadAsyncVideoKeyLeaseByTask(ctx, owner, result.ResponseID)
			require.NoError(t, err)
			require.NotNil(t, lease, "the acknowledged task retains its original shared slot")
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/competing", nil)
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer async-fixture")
			response, err := transport.Do(request, "", 8, 1)
			if response != nil {
				_ = response.Body.Close()
			}
			require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
			require.NoError(t, controller.ReleaseAsyncVideoKeyLeaseByTask(ctx, owner, result.ResponseID))
		})
	}
}

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
			var pollOverride, createOverride atomic.Value
			pollOverride.Store("")
			createOverride.Store("")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/competing" {
					competitors.Add(1)
					_, _ = io.WriteString(w, `{"ok":true}`)
					return
				}
				if r.Method == http.MethodGet {
					polls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if body, ok := pollOverride.Load().(string); ok && body != "" {
						_, _ = io.WriteString(w, body)
						return
					}
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
				if body, ok := createOverride.Load().(string); ok && body != "" {
					_, _ = io.WriteString(w, body)
					return
				}
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
					submit := func(c *gin.Context) (*service.OpenAIForwardResult, error) {
						switch tc.platform {
						case service.PlatformOpenAI:
							return svc.ForwardSeedance(ctx, c, account, tc.endpoint, "", body)
						case service.PlatformMiniMax:
							return svc.ForwardMiniMaxH3(ctx, c, account, tc.endpoint, "", body)
						default:
							return svc.ForwardGrokMedia(ctx, c, account, tc.endpoint, "", body, "application/json")
						}
					}
					result, err := submit(c)
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
					tryCompeting := func() error {
						request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/competing", strings.NewReader("{}"))
						require.NoError(t, requestErr)
						request.Header.Set("Authorization", "Bearer async-fixture")
						response, sendErr := transport.Do(request, "", 8, 1)
						if response != nil {
							require.NoError(t, response.Body.Close())
						}
						return sendErr
					}
					require.ErrorIs(t, tryCompeting(), service.ErrUpstreamKeyCapacity, "a queued remote job still owns the slot")
					poll := func() (*service.OpenAIForwardResult, error) {
						pollContext, _ := gin.CreateTestContext(httptest.NewRecorder())
						pollContext.Request = httptest.NewRequest(http.MethodGet, "/fixture", nil).WithContext(ctx)
						switch tc.platform {
						case service.PlatformOpenAI:
							return svc.ForwardSeedance(ctx, pollContext, account, service.SeedanceEndpointStatus, result.ResponseID, nil)
						case service.PlatformMiniMax:
							return svc.ForwardMiniMaxH3(ctx, pollContext, account, service.MiniMaxH3EndpointStatus, result.ResponseID, nil)
						default:
							return svc.ForwardGrokMedia(ctx, pollContext, account, service.GrokMediaEndpointVideoStatus, result.ResponseID, nil, "")
						}
					}
					terminal := "done"
					switch tc.platform {
					case service.PlatformOpenAI:
						terminal = "succeeded"
					case service.PlatformMiniMax:
						terminal = "completed"
					}
					controller, ok := transport.(service.AsyncVideoKeyLeaseController)
					require.True(t, ok)
					for _, bad := range []string{
						fmt.Sprintf(`{"id":"another-task","status":%q}`, terminal),
						fmt.Sprintf(`{"id":"task-fixture","status":%q,"status":"pending"}`, terminal),
						fmt.Sprintf(`{"id":"task-fixture","id":"another-task","status":%q}`, terminal),
						fmt.Sprintf(`{"id":"task-fixture","status":%q}garbage`, terminal),
					} {
						pollOverride.Store(bad)
						beforePoll := polls.Load()
						status, pollErr := poll()
						require.Error(t, pollErr, "invalid evidence must not produce a releasable terminal result")
						require.Nil(t, status)
						require.Equal(t, beforePoll+1, polls.Load())
						require.ErrorIs(t, tryCompeting(), service.ErrUpstreamKeyCapacity)
						lease, loadErr := controller.LoadAsyncVideoKeyLeaseByTask(ctx, owner, result.ResponseID)
						require.NoError(t, loadErr)
						require.NotNil(t, lease, "a malformed or foreign terminal must retain the original durable member")
						require.Equal(t, before+1, submissions.Load(), "poll failures must not submit another video")
					}
					pollOverride.Store("")
					beforePoll := polls.Load()
					status, err := poll()
					require.NoError(t, err)
					require.True(t, status.AsyncVideoTerminal)
					require.Equal(t, beforePoll+1, polls.Load())
					require.NoError(t, svc.ReleaseAsyncVideoTaskLease(ctx, result.ResponseID))
					beforeCompetitor := competitors.Load()
					require.NoError(t, tryCompeting(), "terminal confirmation must return capacity")
					require.Equal(t, beforeCompetitor+1, competitors.Load())
					ambiguousCreates := []string{
						`{"id":"task-fixture","id":"other-task"}`,
						`{"id":123}`,
						`{"id":" "}`,
						`{"id":"task-fixture"}garbage`,
					}
					if tc.platform != service.PlatformOpenAI {
						ambiguousCreates = append(ambiguousCreates, `{"id":"task-fixture","task_id":"other-task"}`)
					}
					for _, ambiguous := range ambiguousCreates {
						createOverride.Store(ambiguous)
						createRecorder := httptest.NewRecorder()
						createContext, _ := gin.CreateTestContext(createRecorder)
						createContext.Request = httptest.NewRequest(http.MethodPost, "/fixture", bytes.NewReader(body)).WithContext(ctx)
						beforeCreate := submissions.Load()
						created, createErr := submit(createContext)
						require.Error(t, createErr)
						require.Nil(t, created)
						require.Empty(t, createRecorder.Body.String())
						require.Equal(t, beforeCreate+1, submissions.Load(), "an ambiguous success must not replay submission")
						require.ErrorIs(t, tryCompeting(), service.ErrUpstreamKeyCapacity)
						members, readErr := rdb.ZRange(ctx, "concurrency:upstream_key:async_video:unknown", 0, -1).Result()
						require.NoError(t, readErr)
						require.Len(t, members, 1, "an ambiguous creation must retain its unknown durable member")
						// The fixture creates no remote job; model explicit reconciliation, not a timeout release.
						require.NoError(t, svc.ReleaseAsyncVideoTaskLeaseByMember(ctx, members[0]))
						require.NoError(t, tryCompeting(), "only explicit reconciliation restores the unknown member's capacity")
					}
					createOverride.Store("")
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
