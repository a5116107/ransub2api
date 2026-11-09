//go:build unit

package routes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type regionalVideoRouteUpstream struct {
	service.HTTPUpstream
	mode     string
	calls    int
	last     video.Query
	dispatch video.Dispatch
}

func (u *regionalVideoRouteUpstream) SetAsyncVideoForwardHandler(dispatch video.Dispatch) {
	u.dispatch = dispatch
}

func (u *regionalVideoRouteUpstream) ForwardAsyncVideoLookup(_ context.Context, query video.Query) (*http.Response, bool, error) {
	u.calls++
	u.last = query
	if u.mode == "local" {
		return nil, false, nil
	}
	if u.mode == "unavailable" {
		return nil, true, service.ErrUpstreamKeyPolicy
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"origin":true}`))}, true, nil
}

func (u *regionalVideoRouteUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return nil, service.ErrUpstreamKeyPolicy
}

func TestRegionalVideoQueryForwardingBeforeFundsGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, platform := range []string{service.PlatformGrok, service.PlatformMiniMax, service.PlatformOpenAI, service.PlatformComposite} {
		for _, mode := range []string{"remote", "local", "unavailable"} {
			t.Run(platform+"/"+mode, func(t *testing.T) {
				cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway, RegionalFundsFile: "fixture"}, Gateway: config.GatewayConfig{MaxBodySize: 1 << 20, TextMaxBodySize: 1 << 20}}
				upstream := &regionalVideoRouteUpstream{mode: mode}
				svc := service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{Config: cfg, Upstream: upstream})
				t.Cleanup(svc.StopAsyncVideoKeyReleaseRecovery)
				t.Cleanup(svc.StopOpenAICodexTicketHarvester)
				h := &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: handler.NewOpenAIGatewayHandler(svc, nil, nil, nil, nil, nil, nil, nil, cfg), AsyncImage: handler.NewAsyncImageHandler(nil, nil)}
				router := gin.New()
				group := &service.Group{ID: 3, Platform: platform}
				RegisterGatewayRoutes(router, h, func(c *gin.Context) {
					if c.GetHeader("Authorization") != "Bearer downstream-fixture" {
						c.AbortWithStatus(http.StatusUnauthorized)
						return
					}
					c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 1, GroupID: &group.ID, Group: group})
					c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 1})
					c.Next()
				}, nil, nil, nil, nil, nil, cfg)
				request := func(method, path, credential string) *httptest.ResponseRecorder {
					req := httptest.NewRequest(method, path, nil)
					req.Header.Set("Authorization", "Bearer "+credential)
					w := httptest.NewRecorder()
					router.ServeHTTP(w, req)
					return w
				}
				check := func(method, path, provider, operation, task string) {
					t.Helper()
					before := upstream.calls
					w := request(method, path, "downstream-fixture")
					want := http.StatusServiceUnavailable
					if mode == "remote" {
						want = http.StatusOK
					}
					require.Equal(t, want, w.Code, "%s %s: %s", method, path, w.Body.String())
					require.Equal(t, before+1, upstream.calls, "%s %s must try only the origin lookup", method, path)
					require.Equal(t, provider, upstream.last.Owner.Provider)
					require.Equal(t, operation, upstream.last.Operation)
					require.Equal(t, task, upstream.last.TaskID)
					if mode == "local" {
						require.Contains(t, w.Body.String(), "regional_cost_bound_unavailable", "local execution must remain behind the original funds gate")
					}
				}
				for _, taskName := range []string{"task", "models", "usage", "responses"} {
					if platform != service.PlatformOpenAI {
						for _, prefix := range []string{"/v1", ""} {
							for _, action := range []string{"", "/generations", "/edits", "/extensions"} {
								for _, suffix := range []string{"", "/content"} {
									provider, operation, task := "grok", "video_status", taskName
									if suffix != "" {
										operation = "video_content"
									}
									if platform == service.PlatformMiniMax {
										provider, operation, task = "minimax-h3", "minimax_h3_status", service.MiniMaxH3TaskKey(taskName)
										if suffix != "" {
											operation = "minimax_h3_content"
										}
									}
									check(http.MethodGet, prefix+"/videos"+action+"/"+taskName+suffix, provider, operation, task)
								}
							}
						}
					}
					if platform == service.PlatformOpenAI || platform == service.PlatformComposite {
						for _, prefix := range []string{"/api/v3", "/v3", "/v1", ""} {
							check(http.MethodGet, prefix+"/contents/generations/tasks/"+taskName, "seedance", "seedance_status", service.SeedanceTaskKey(taskName))
							check(http.MethodDelete, prefix+"/contents/generations/tasks/"+taskName, "seedance", "seedance_delete", service.SeedanceTaskKey(taskName))
						}
					}
				}
				before := upstream.calls
				for _, path := range []string{"/v1/videos", "/v1/videos/generations", "/v1/videos/edits", "/v1/videos/extensions", "/v1/contents/generations/tasks", "/v1/images/generations", "/v1/messages"} {
					w := request(http.MethodPost, path, "downstream-fixture")
					require.Equal(t, http.StatusServiceUnavailable, w.Code, path)
					require.Contains(t, w.Body.String(), "regional_cost_bound_unavailable")
				}
				for _, path := range []string{"/v1/images/tasks/task", "/images/tasks/task"} {
					w := request(http.MethodGet, path, "downstream-fixture")
					require.Equal(t, http.StatusServiceUnavailable, w.Code, path)
					require.Contains(t, w.Body.String(), "regional_cost_bound_unavailable")
				}
				w := request(http.MethodGet, "/v1/videos/task", "wrong-key")
				require.Equal(t, http.StatusUnauthorized, w.Code)
				require.Equal(t, before, upstream.calls, "generation and failed authentication must never enter the forwarding client")
			})
		}
	}
}

func TestRegionalVideoFundsGateHonorsGroupPolicies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []struct{ method, path, platform string }{
		{http.MethodGet, "/v1/videos/task", service.PlatformGrok},
		{http.MethodGet, "/videos/generations/task/content", service.PlatformGrok},
		{http.MethodGet, "/v1/contents/generations/tasks/task", service.PlatformOpenAI},
		{http.MethodDelete, "/api/v3/contents/generations/tasks/task", service.PlatformOpenAI},
	} {
		for _, scenario := range []string{"allowed", "subagent", "parent", "metadata", "codex_only", "model_allowlist", "inconsistent_group"} {
			t.Run(route.method+route.path+"/"+scenario, func(t *testing.T) {
				cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway, RegionalFundsFile: "fixture"}, Gateway: config.GatewayConfig{MaxBodySize: 1 << 20, TextMaxBodySize: 1 << 20}}
				upstream := &regionalVideoRouteUpstream{mode: "remote"}
				svc := service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{Config: cfg, Upstream: upstream})
				t.Cleanup(svc.StopAsyncVideoKeyReleaseRecovery)
				t.Cleanup(svc.StopOpenAICodexTicketHarvester)
				h := &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: handler.NewOpenAIGatewayHandler(svc, nil, nil, nil, nil, nil, nil, nil, cfg), AsyncImage: handler.NewAsyncImageHandler(nil, nil)}
				group := &service.Group{ID: 3, Platform: route.platform}
				groupID := group.ID
				path, header, value, want := route.path, "", "", http.StatusForbidden
				switch scenario {
				case "allowed":
					group.BlockSubagents = true
					want = http.StatusOK
				case "subagent":
					group.BlockSubagents, header, value = true, "X-OpenAI-Subagent", "worker"
				case "parent":
					group.BlockSubagents, header, value = true, "X-Codex-Parent-Thread-Id", "parent-fixture"
				case "metadata":
					group.BlockSubagents, header, value = true, "X-Codex-Turn-Metadata", `{"subagent_kind":"review"}`
				case "codex_only":
					group.CodexCLIOnly = true
				case "model_allowlist":
					group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"allowed-fixture"}}
					path += "?model=blocked-fixture"
					want = http.StatusNotFound
				case "inconsistent_group":
					groupID++
					want = http.StatusServiceUnavailable
				}
				router := gin.New()
				RegisterGatewayRoutes(router, h, func(c *gin.Context) {
					c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 1, GroupID: &groupID, Group: group})
					c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 1})
					c.Next()
				}, nil, nil, nil, nil, nil, cfg)
				req := httptest.NewRequest(route.method, path, nil)
				req.Header.Set("Authorization", "Bearer downstream-fixture")
				if header != "" {
					req.Header.Set(header, value)
				}
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				require.Equal(t, want, w.Code, w.Body.String())
				if want == http.StatusOK {
					require.Equal(t, 1, upstream.calls)
				} else {
					require.Zero(t, upstream.calls, "disallowed requests must stop before origin lookup")
				}
			})
		}
	}
}

// Even a validated forwarded lookup cannot bypass the originating node's
// coordinated funds gate: video completion may itself trigger a charge.
func TestRegionalVideoOriginFundsGateRejectsUnmeteredLookup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway, RegionalFundsFile: "fixture"}, Gateway: config.GatewayConfig{MaxBodySize: 1 << 20, TextMaxBodySize: 1 << 20}}
	upstream := &regionalVideoRouteUpstream{mode: "local"}
	svc := service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{Config: cfg, Upstream: upstream})
	t.Cleanup(svc.StopAsyncVideoKeyReleaseRecovery)
	t.Cleanup(svc.StopOpenAICodexTicketHarvester)
	h := &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: handler.NewOpenAIGatewayHandler(svc, nil, nil, nil, nil, nil, nil, nil, cfg), AsyncImage: handler.NewAsyncImageHandler(nil, nil)}
	group := &service.Group{ID: 3, Platform: service.PlatformGrok}
	router := gin.New()
	RegisterGatewayRoutes(router, h, func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer downstream-fixture" || c.GetHeader(service.APIKeyGroupHeader) != "3" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 1, GroupID: &group.ID, Group: group})
		c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 1})
		c.Next()
	}, nil, nil, nil, nil, nil, cfg)
	svc.InstallMediaVideoForwarding(router)
	require.NotNil(t, upstream.dispatch)
	query := video.Query{PolicyVersion: video.ForwardPolicyVersion, Owner: video.Owner{UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: 4, Provider: "grok", RouteFingerprint: [32]byte{1}},
		TaskID: "task", Operation: "video_status", Credential: "downstream-fixture", ClientIP: "192.0.2.5"}
	w := httptest.NewRecorder()
	upstream.dispatch(w, httptest.NewRequest(http.MethodPost, "/private", nil), query)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "regional_cost_bound_unavailable")
	require.Zero(t, upstream.calls, "the origin must not transfer the lookup or execute an unmetered request")
}
