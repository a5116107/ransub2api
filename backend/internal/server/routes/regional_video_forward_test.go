//go:build unit

package routes

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
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
						require.Contains(t, w.Body.String(), "regional_cost_bound_unavailable", "local execution without its original financial ledger must remain blocked")
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
				group := &service.Group{ID: 3, Platform: route.platform, RateMultiplier: 1, AllowImageGeneration: true}
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

// A forwarding proof alone cannot authorize an unfunded task at the origin:
// video completion still requires its permanent financial reservation.
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

type regionalFundedVideoRouteLedger struct {
	ledger.VideoTaskLedger
	record      ledger.VideoTaskRecord
	resolveErr  error
	completeErr error
	resolves    int
	completes   int
}

func (l *regionalFundedVideoRouteLedger) ResolveVideoTaskOwner(_ context.Context, principal video.Owner, taskID string) (*video.Owner, error) {
	l.resolves++
	owner := l.record.Reservation.VideoOrigin.Owner
	if l.resolveErr != nil {
		return nil, l.resolveErr
	}
	if !video.SamePrincipal(principal, owner) || taskID != l.record.TaskID {
		return nil, ledger.ErrConflict
	}
	return &owner, nil
}

func (l *regionalFundedVideoRouteLedger) LoadVideoTask(_ context.Context, owner video.Owner, taskID string) (ledger.VideoTaskRecord, error) {
	if owner != l.record.Reservation.VideoOrigin.Owner || taskID != l.record.TaskID {
		return ledger.VideoTaskRecord{}, ledger.ErrConflict
	}
	return l.record, nil
}

func (l *regionalFundedVideoRouteLedger) Complete(context.Context, ledger.Event) error {
	l.completes++
	return l.completeErr
}

type regionalFundedVideoRouteUpstream struct {
	*regionalVideoRouteUpstream
	service.AsyncVideoKeyLeaseController
	provider string
	sends    int
	releases int
}

func (*regionalFundedVideoRouteUpstream) LoadAsyncVideoKeyLeaseByTask(context.Context, service.AsyncVideoKeyOwner, string) (*service.PersistentUpstreamKeyLease, error) {
	return nil, nil
}

func (u *regionalFundedVideoRouteUpstream) ReleaseAsyncVideoKeyLeaseByTask(context.Context, service.AsyncVideoKeyOwner, string) error {
	u.releases++
	return nil
}

func (u *regionalFundedVideoRouteUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.sends++
	status, body := http.StatusOK, `{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`
	headers := http.Header{"Content-Type": {"application/json"}}
	switch {
	case u.provider == "minimax-h3" && strings.HasSuffix(req.URL.Path, "/content"):
		status, body = http.StatusFound, ""
		headers.Set("Location", "https://cdn.example/video.mp4")
	case u.provider == "minimax-h3":
		body = `{"status":"completed","metadata":{"url":"https://cdn.example/video.mp4"}}`
	case u.provider == "seedance" && req.Method == http.MethodDelete:
		body = `{"status":"cancelled"}`
	case u.provider == "seedance":
		body = `{"status":"succeeded","usage":{"completion_tokens":12345}}`
	case strings.HasSuffix(req.URL.Path, "/content") || strings.HasSuffix(req.URL.Path, ".mp4"):
		body = "fixture-video"
		headers.Set("Content-Type", "video/mp4")
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestRegionalVideoFundsGateAllowsOnlyOriginalFundedQueries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []struct{ method, path, provider, platform string }{
		{http.MethodGet, "/v1/videos/task", "grok", service.PlatformGrok},
		{http.MethodGet, "/videos/task/content", "grok", service.PlatformGrok},
		{http.MethodGet, "/v1/videos/task", "minimax-h3", service.PlatformMiniMax},
		{http.MethodGet, "/videos/task/content", "minimax-h3", service.PlatformMiniMax},
		{http.MethodGet, "/v3/contents/generations/tasks/task", "seedance", service.PlatformOpenAI},
		{http.MethodDelete, "/api/v3/contents/generations/tasks/task", "seedance", service.PlatformOpenAI},
	} {
		for _, scenario := range []string{"allowed", "missing_task", "sql_unavailable", "wrong_group", "wrong_user", "changed_route", "settlement_unavailable"} {
			t.Run(route.provider+route.method+route.path+"/"+scenario, func(t *testing.T) {
				cfg := &config.Config{RunMode: config.RunModeStandard, Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway, RegionalFundsFile: "fixture"}, Gateway: config.GatewayConfig{MaxBodySize: 1 << 20, TextMaxBodySize: 1 << 20}}
				// Existing queries keep their original price while new admission is paused.
				group := &service.Group{ID: 3, Platform: route.platform, RateMultiplier: 1}
				account := service.Account{ID: 4, Platform: route.platform, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 5, GroupIDs: []int64{group.ID}, Credentials: map[string]any{"api_key": "original-key", "base_url": "https://video.example", "openai_capabilities": []string{"seedance"}}}
				owner := video.Owner{UserID: 1, APIKeyID: 2, GroupID: group.ID, AccountID: account.ID, Provider: route.provider, RouteFingerprint: service.AccountRouteFingerprint(&account)}
				price := ledger.Price{Model: "video-alias", Video: &ledger.VideoPrice{VideoRequest: ledger.VideoRequest{Provider: route.provider, UpstreamModel: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5}, Basis: "request", UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 10}}
				taskID := "minimax-h3:task"
				switch route.provider {
				case "grok":
					price.Video.VideoRequest = ledger.VideoRequest{Provider: route.provider, UpstreamModel: "grok-imagine-video", Resolution: "720p", DurationSeconds: 6}
					price.Video.Basis, taskID = "second", "task"
				case "seedance":
					price.Video.VideoRequest = ledger.VideoRequest{Provider: route.provider, UpstreamModel: "ep-seedance"}
					price.Video.Basis, price.Video.UnitNano, price.Video.MaxCompletionTokens = "completion_tokens", 28_000_000_000, 1_000_000
					taskID = "seedance:task"
				}
				maximum, err := price.VideoMaximum(price.Video.VideoRequest)
				require.NoError(t, err)
				l := &regionalFundedVideoRouteLedger{record: ledger.VideoTaskRecord{TaskID: taskID, State: "started", Price: price, Reservation: ledger.Reservation{RequestID: "original-financial-request", UserID: owner.UserID, APIKeyID: owner.APIKeyID, AccountID: owner.AccountID, Model: price.Model, PricingVersion: price.Version(), Maximum: maximum, AdmittedAt: time.Now().Add(-time.Minute), PayloadSHA256: strings.Repeat("a", 64), Video: &price.Video.VideoRequest, VideoOrigin: &ledger.VideoOrigin{Member: "video:00000000-0000-4000-8000-000000000001", Owner: owner}}}}
				switch scenario {
				case "missing_task":
					l.resolveErr = sql.ErrNoRows
				case "sql_unavailable":
					l.resolveErr = errors.New("original SQL unavailable")
				case "changed_route":
					account.Credentials["api_key"] = "replacement-key"
				case "settlement_unavailable":
					l.completeErr = errors.New("settlement temporarily unavailable")
				}
				upstream := &regionalFundedVideoRouteUpstream{regionalVideoRouteUpstream: &regionalVideoRouteUpstream{mode: "local"}, provider: route.provider}
				readinessCalls := 0
				meter := &service.RegionalMetering{Ledger: l, Readiness: func(context.Context) error {
					readinessCalls++
					return errors.New("new admission paused")
				}, Admit: func(context.Context, *service.APIKey, *service.Account, []byte) (ledger.Reservation, ledger.Price, error) {
					panic("queries must never reserve new funds")
				}}
				repo := compatibleImagesAccounts{accounts: []service.Account{account}}
				concurrency := service.NewConcurrencyService(testutil.StubConcurrencyCache{})
				svc := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, &testutil.StubGatewayCache{}, cfg, nil, concurrency, nil, nil, nil, upstream, nil, nil, service.NewGrokTokenProvider(repo, nil), nil, nil, nil, nil, nil, service.WithRegionalMetering(meter))
				t.Cleanup(svc.StopAsyncVideoKeyReleaseRecovery)
				t.Cleanup(svc.StopOpenAICodexTicketHarvester)
				billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
				t.Cleanup(billing.Stop)
				h := &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: handler.NewOpenAIGatewayHandler(svc, concurrency, billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg), AsyncImage: handler.NewAsyncImageHandler(nil, nil)}
				router := gin.New()
				RegisterGatewayRoutes(router, h, func(c *gin.Context) {
					if c.GetHeader("Authorization") != "Bearer downstream-fixture" {
						c.AbortWithStatus(http.StatusUnauthorized)
						return
					}
					groupID, userID := group.ID, owner.UserID
					if scenario == "wrong_group" {
						groupID++
					}
					if scenario == "wrong_user" {
						userID++
					}
					c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{ID: owner.APIKeyID, UserID: owner.UserID, GroupID: &groupID, Group: group, User: &service.User{ID: owner.UserID}})
					c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: userID, Concurrency: 5})
					c.Next()
				}, nil, nil, nil, nil, nil, cfg)
				req := httptest.NewRequest(route.method, route.path, nil)
				req.Header.Set("Authorization", "Bearer downstream-fixture")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				if scenario != "allowed" {
					require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
					if scenario == "settlement_unavailable" {
						require.Contains(t, w.Body.String(), "regional_video_settlement_unavailable")
					} else {
						require.Zero(t, upstream.sends)
					}
					require.Zero(t, upstream.releases)
					return
				}
				wantStatus, wantSends := http.StatusOK, 1
				if route.provider == "minimax-h3" && strings.HasSuffix(route.path, "/content") {
					wantStatus = http.StatusFound
				}
				if route.provider == "grok" && strings.HasSuffix(route.path, "/content") {
					wantSends = 2
				}
				require.Equal(t, wantStatus, w.Code, w.Body.String())
				require.Equal(t, wantSends, upstream.sends)
				require.Equal(t, 1, l.completes)
				require.Equal(t, 1, upstream.releases)
				require.Zero(t, readinessCalls, "existing task must not require new admission readiness")
				before := l.resolves
				create := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"grok-imagine-video","prompt":"fixture"}`))
				create.Header.Set("Authorization", "Bearer downstream-fixture")
				denied := httptest.NewRecorder()
				router.ServeHTTP(denied, create)
				require.Equal(t, http.StatusServiceUnavailable, denied.Code)
				require.Contains(t, denied.Body.String(), "regional_cost_bound_unavailable")
				require.Equal(t, before, l.resolves, "creation must not reuse an existing task's financial authority")
				wantReadiness := 1
				if route.provider == "seedance" {
					wantReadiness = 0 // OpenAI groups cannot create through the Grok /videos route.
				}
				require.Equal(t, wantReadiness, readinessCalls)
				require.Equal(t, wantSends, upstream.sends)
			})
		}
	}
}
