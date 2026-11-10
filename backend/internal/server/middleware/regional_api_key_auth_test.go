package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type regionalAuthStub struct {
	err     error
	groupID int64
}

func TestRegionalVideoRequestClassifiesOnlyRegisteredAuthenticatedRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		method, path, platform string
		endpoint               service.GrokMediaEndpoint
		taskID                 string
	}{
		{http.MethodPost, "/v1/videos", service.PlatformGrok, service.GrokMediaEndpointVideosGenerations, ""},
		{http.MethodPost, "/videos/generations", service.PlatformComposite, service.GrokMediaEndpointVideosGenerations, ""},
		{http.MethodPost, "/v1/videos/edits", service.PlatformGrok, service.GrokMediaEndpointVideosEdits, ""},
		{http.MethodPost, "/videos/extensions", service.PlatformComposite, service.GrokMediaEndpointVideosExtensions, ""},
		{http.MethodPost, "/v1/videos", service.PlatformMiniMax, service.MiniMaxH3EndpointCreate, ""},
		{http.MethodPost, "/videos/generations", service.PlatformMiniMax, service.MiniMaxH3EndpointCreate, ""},
		{http.MethodPost, "/contents/generations/tasks", service.PlatformOpenAI, service.SeedanceEndpointCreate, ""},
		{http.MethodPost, "/v1/contents/generations/tasks", service.PlatformComposite, service.SeedanceEndpointCreate, ""},
		{http.MethodPost, "/v3/contents/generations/tasks", service.PlatformOpenAI, service.SeedanceEndpointCreate, ""},
		{http.MethodPost, "/api/v3/contents/generations/tasks", service.PlatformComposite, service.SeedanceEndpointCreate, ""},
		{http.MethodGet, "/v1/videos/:request_id", service.PlatformGrok, service.GrokMediaEndpointVideoStatus, "models"},
		{http.MethodGet, "/videos/:request_id/content", service.PlatformMiniMax, service.MiniMaxH3EndpointContent, "minimax-h3:models"},
		{http.MethodDelete, "/v3/contents/generations/tasks/:task_id", service.PlatformOpenAI, service.SeedanceEndpointDelete, "seedance:responses"},
		{http.MethodPost, "/v1/videos/edits", service.PlatformMiniMax, "", ""},
		{http.MethodPost, "/v1/videos/extensions", service.PlatformMiniMax, "", ""},
		{http.MethodPost, "/v1/videos", service.PlatformOpenAI, "", ""},
		{http.MethodPost, "/v3/contents/generations/tasks", service.PlatformGrok, "", ""},
		{http.MethodGet, "/v1/videos", service.PlatformGrok, "", ""},
		{http.MethodPut, "/v1/videos", service.PlatformGrok, "", ""},
		{http.MethodPost, "/v1/videos/:request_id", service.PlatformGrok, "", ""},
		{http.MethodPost, "/v1/images/generations", service.PlatformGrok, "", ""},
		{http.MethodPost, "/v1/responses", service.PlatformOpenAI, "", ""},
	} {
		t.Run(tc.method+tc.path+"/"+tc.platform, func(t *testing.T) {
			for _, identity := range []string{"valid", "missing", "wrong_group"} {
				t.Run(identity, func(t *testing.T) {
					router := gin.New()
					router.Handle(tc.method, tc.path, func(c *gin.Context) {
						if identity != "missing" {
							groupID := int64(3)
							group := &service.Group{ID: groupID, Platform: tc.platform}
							if identity == "wrong_group" {
								group.ID++
							}
							c.Set(string(ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 1, GroupID: &groupID, Group: group})
						}
						endpoint, taskID := RegionalVideoRequest(c)
						if identity == "valid" {
							require.Equal(t, tc.endpoint, endpoint)
							require.Equal(t, tc.taskID, taskID)
						} else {
							require.Empty(t, endpoint)
							require.Empty(t, taskID)
						}
					})
					path := strings.ReplaceAll(strings.ReplaceAll(tc.path, ":request_id", "models"), ":task_id", "responses")
					router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, path, nil))
				})
			}
		})
	}
}

type regionalVideoPolicyAuth struct {
	group *service.Group
}

func (s regionalVideoPolicyAuth) GetRegionalByKey(_ context.Context, key string) (*service.APIKey, error) {
	if key != "downstream-fixture" {
		return nil, service.ErrAPIKeyNotFound
	}
	return &service.APIKey{ID: 2, UserID: 1, GroupID: &s.group.ID, Group: s.group,
		User: &service.User{ID: 1, Status: service.StatusActive}, IPWhitelist: []string{"192.0.2.5"},
		CompiledIPWhitelist: ip.CompileIPRules([]string{"192.0.2.5"})}, nil
}

type regionalVideoPolicyUpstream struct {
	service.HTTPUpstream
	dispatch video.Dispatch
	forwards int
}

func (s *regionalVideoPolicyUpstream) SetAsyncVideoForwardHandler(dispatch video.Dispatch) {
	s.dispatch = dispatch
}

func (s *regionalVideoPolicyUpstream) ForwardAsyncVideoLookup(context.Context, video.Query) (*http.Response, bool, error) {
	s.forwards++
	return nil, true, service.ErrUpstreamKeyPolicy
}

func TestRegionalVideoOriginRechecksAuthenticatedGroupPolicies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []string{"allowed", "wrong_key", "wrong_group", "wrong_user", "wrong_ip", "model_denied", "client_denied", "subagent_denied"} {
		t.Run(scenario, func(t *testing.T) {
			group := &service.Group{ID: 3, Platform: service.PlatformGrok, Status: service.StatusActive,
				CodexCLIOnly: true, BlockSubagents: true, ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"model-fixture"}}}
			cfg := &config.Config{}
			upstream := &regionalVideoPolicyUpstream{}
			svc := service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{Config: cfg, Upstream: upstream})
			t.Cleanup(svc.StopAsyncVideoKeyReleaseRecovery)
			t.Cleanup(svc.StopOpenAICodexTicketHarvester)
			router := gin.New()
			router.Use(RegionalAPIKeyAuth(regionalVideoPolicyAuth{group: group}, cfg), GroupModelAllowlist(), GroupCodexCLIOnly(nil), GroupBlockSubagents())
			handlerCalls := 0
			router.GET("/v1/videos/:request_id", func(c *gin.Context) {
				handlerCalls++
				key, ok := GetAPIKeyFromContext(c)
				require.True(t, ok)
				subject, ok := GetAuthSubjectFromContext(c)
				require.True(t, ok)
				id, pinned := PinnedAPIKeyGroup(c)
				require.True(t, pinned)
				require.Equal(t, int64(3), id)
				handled, err := svc.ForwardRegionalVideoQuery(c, service.GrokMediaEndpointVideoStatus, c.Param("request_id"), key, subject.UserID)
				if err != nil || handled {
					c.Status(http.StatusForbidden)
					return
				}
				c.Status(http.StatusNoContent)
			})
			svc.InstallMediaVideoForwarding(router)
			require.NotNil(t, upstream.dispatch)
			query := video.Query{PolicyVersion: video.ForwardPolicyVersion, Owner: video.Owner{UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: 4, Provider: "grok", RouteFingerprint: [32]byte{1}},
				TaskID: "task", Operation: "video_status", Credential: "downstream-fixture", ClientIP: "192.0.2.5",
				UserAgent: "wrapper/1.2.3", Originator: "codex_cli_rs", Model: "model-fixture"}
			want := http.StatusForbidden
			switch scenario {
			case "allowed":
				want = http.StatusNoContent
			case "wrong_key":
				query.Credential, want = "wrong-fixture", http.StatusUnauthorized
			case "wrong_group":
				query.Owner.GroupID++
			case "wrong_user":
				query.Owner.UserID++
			case "wrong_ip":
				query.ClientIP = "198.51.100.5"
			case "model_denied":
				query.Model, want = "blocked-fixture", http.StatusNotFound
			case "client_denied":
				query.Originator = "unknown-client"
			case "subagent_denied":
				query.Subagent = true
			}
			w := httptest.NewRecorder()
			upstream.dispatch(w, httptest.NewRequest(http.MethodPost, "/private", nil), query)
			require.Equal(t, want, w.Code, w.Body.String())
			if scenario == "allowed" || scenario == "wrong_user" {
				require.Equal(t, 1, handlerCalls)
			} else {
				require.Zero(t, handlerCalls)
			}
			require.Zero(t, upstream.forwards, "origin requests must never be forwarded for a second hop")
		})
	}
}

func (s regionalAuthStub) GetRegionalByKey(_ context.Context, key string) (*service.APIKey, error) {
	if s.err != nil {
		return nil, s.err
	}
	if key != "valid" {
		return nil, service.ErrAPIKeyNotFound
	}
	keyAuth := &service.APIKey{ID: 1, UserID: 2, User: &service.User{ID: 2, Status: service.StatusActive}, IPWhitelist: []string{"127.0.0.1"}, CompiledIPWhitelist: ip.CompileIPRules([]string{"127.0.0.1"})}
	if s.groupID > 0 {
		keyAuth.GroupID = &s.groupID
		keyAuth.Group = &service.Group{ID: s.groupID, Status: service.StatusActive}
	}
	return keyAuth, nil
}
func TestRegionalAuthMiddlewareLocalIdentityAndIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, header, key, path, remote string
		err                             error
		status                          int
		pin                             string
		groupID                         int64
	}{
		{name: "bearer", header: "Authorization", key: "Bearer valid", status: 204},
		{name: "anthropic", header: "x-api-key", key: "valid", status: 204},
		{name: "google", header: "x-goog-api-key", key: "valid", status: 204},
		{name: "missing", status: 401},
		{name: "unknown", header: "x-api-key", key: "wrong", status: 401},
		{name: "revoked", header: "x-api-key", key: "valid", err: service.ErrAPIKeyNotFound, status: 401},
		{name: "expired-view", header: "x-api-key", key: "valid", err: service.ErrRegionalAuthUnavailable, status: 503},
		{name: "ip-denied", header: "x-api-key", key: "valid", remote: "192.0.2.1:1234", status: 403},
		{name: "query-key", path: "/?key=valid", status: 400},
		{name: "local group pin", header: "x-api-key", key: "valid", pin: "2", groupID: 2, status: 204},
		{name: "wrong local group", header: "x-api-key", key: "valid", pin: "3", groupID: 2, status: 403},
		{name: "unassigned pin", header: "x-api-key", key: "valid", pin: "2", status: 403},
		{name: "invalid local pin", header: "x-api-key", key: "valid", pin: "2,3", groupID: 2, status: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.Use(RegionalAPIKeyAuth(regionalAuthStub{err: tc.err, groupID: tc.groupID}, &config.Config{}))
			router.GET("/", func(c *gin.Context) {
				key, ok := GetAPIKeyFromContext(c)
				require.True(t, ok)
				require.Equal(t, int64(1), key.ID)
				c.Status(204)
			})
			path := tc.path
			if path == "" {
				path = "/"
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = "127.0.0.1:1234"
			if tc.remote != "" {
				req.RemoteAddr = tc.remote
			}
			if tc.header != "" {
				req.Header.Set(tc.header, tc.key)
			}
			if tc.pin != "" {
				req.Header.Set(service.APIKeyGroupHeader, tc.pin)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, tc.status, w.Code, w.Body.String())
		})
	}
}

type regionalRevocationStub struct {
	calls int
	limit int
}

func (s *regionalRevocationStub) GetRegionalByKey(context.Context, string) (*service.APIKey, error) {
	s.calls++
	if s.calls > 1 {
		return nil, service.ErrAPIKeyNotFound
	}
	return &service.APIKey{ID: 1, UserID: 2, Status: service.StatusAPIKeyActive, ConcurrencyLimit: s.limit, User: &service.User{ID: 2, Status: service.StatusActive}}, nil
}
func TestRegionalAuthInstallsLocalTurnRevalidationAndAdmissionOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		method, path string
		limit        int
	}{{http.MethodGet, "/v1/responses", 0}, {http.MethodPost, "/v1/responses", 2}} {
		t.Run(tc.method, func(t *testing.T) {
			auth := &regionalRevocationStub{limit: tc.limit}
			r := gin.New()
			r.Use(RegionalAPIKeyAuth(auth, &config.Config{}))
			r.Handle(tc.method, tc.path, func(c *gin.Context) {
				require.True(t, service.HasAPIKeyAdmissionOwner(c.Request.Context()))
				require.True(t, service.HasAPIKeyQueueAuthRevalidator(c.Request.Context()))
				_, installed, err := (&service.ConcurrencyService{}).RevalidateAPIKeyQueueTurn(c.Request.Context())
				require.True(t, installed)
				require.Error(t, err)
				require.Equal(t, 2, auth.calls)
				c.Status(http.StatusNoContent)
			})
			request := httptest.NewRequest(tc.method, tc.path, nil)
			request.Header.Set("Authorization", "Bearer fixture")
			response := httptest.NewRecorder()
			r.ServeHTTP(response, request)
			require.Equal(t, http.StatusNoContent, response.Code)
		})
	}
}
