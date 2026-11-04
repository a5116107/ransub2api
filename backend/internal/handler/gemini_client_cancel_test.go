//go:build unit

package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// clientCancelUpstream 模拟上游尚未响应时客户端断开：Do 期间取消请求 context，
// 并按 net/http 的形态返回包裹 context.Canceled 的 *url.Error。
type clientCancelUpstream struct {
	service.HTTPUpstream
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (u *clientCancelUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	u.cancel()
	return nil, &url.Error{Op: "Post", URL: req.URL.String(), Err: context.Canceled}
}

type geminiClientCancelFixture struct {
	handler        *GatewayHandler
	group          *service.Group
	apiKey         *service.APIKey
	upstream       *clientCancelUpstream
	schedulerCache *bucketRecordingSchedulerCache
	ctx            context.Context
}

func newGeminiClientCancelFixture(t *testing.T) *geminiClientCancelFixture {
	t.Helper()
	groupID := int64(9200)
	accountID := int64(9201)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformGemini, Status: service.StatusActive}
	account := &service.Account{
		ID:            accountID,
		Platform:      service.PlatformGemini,
		Type:          service.AccountTypeAPIKey,
		Credentials:   map[string]any{"api_key": "test-key"},
		Status:        service.StatusActive,
		Schedulable:   true,
		Concurrency:   1,
		Priority:      1,
		AccountGroups: []service.AccountGroup{{AccountID: accountID, GroupID: groupID}},
	}
	schedulerCache := &bucketRecordingSchedulerCache{fakeSchedulerCache: &fakeSchedulerCache{accounts: []*service.Account{account}}}
	h, cleanup := newTestGatewayHandlerWithSchedulerCache(t, group, schedulerCache)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	upstream := &clientCancelUpstream{cancel: cancel}
	h.geminiCompatService = service.NewGeminiMessagesCompatService(nil, nil, nil, nil, nil, nil, upstream, nil, &config.Config{})

	apiKey := &service.APIKey{
		ID: 9202, UserID: 9203, GroupID: &groupID, Group: group, Status: service.StatusActive,
		User: &service.User{ID: 9203, Concurrency: 10, Balance: 100},
	}
	return &geminiClientCancelFixture{handler: h, group: group, apiKey: apiKey, upstream: upstream, schedulerCache: schedulerCache, ctx: ctx}
}

// serve 经 ops 错误日志中间件执行请求，返回响应与 ops 队列中的条目数。
func (f *geminiClientCancelFixture) serve(t *testing.T, route, path, body string, call func(*gin.Context)) (*httptest.ResponseRecorder, int64) {
	t.Helper()
	setupOpsErrorLogTestQueue(t, 4)
	ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(ops))
	router.POST(route, func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), f.apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: f.apiKey.UserID, Concurrency: 10})
		call(c)
	})

	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req = req.WithContext(context.WithValue(f.ctx, ctxkey.Group, f.group))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec, OpsErrorLogQueueLength()
}

// 原生入口：上游响应前客户端断开，响应未提交，标记 499；纯客户端取消不落 ops 错误日志。
func TestGeminiV1BetaModels_ClientCancelBeforeUpstreamResponseMarks499(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := newGeminiClientCancelFixture(t)

	rec, opsQueued := f.serve(t,
		"/v1beta/models/*modelAction",
		"/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		f.handler.GeminiV1BetaModels,
	)

	require.Equal(t, int32(1), f.upstream.calls.Load())
	require.Equal(t, statusClientClosedRequest, rec.Code)
	require.Zero(t, rec.Body.Len())
	require.Zero(t, opsQueued)
}

// Chat Completions 兼容入口（Gemini 分组）：同一场景标记 499，不再补写 502。
func TestGatewayChatCompletions_GeminiClientCancelBeforeUpstreamResponseMarks499(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := newGeminiClientCancelFixture(t)

	rec, opsQueued := f.serve(t,
		"/v1/chat/completions",
		"/v1/chat/completions",
		`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		f.handler.ChatCompletions,
	)

	require.Equal(t, int32(1), f.upstream.calls.Load())
	require.Equal(t, statusClientClosedRequest, rec.Code)
	require.Zero(t, rec.Body.Len())
	require.Zero(t, opsQueued)
}

func TestIndependentGatewayHandlersPreserveFinalAdmissionGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, platform := range []string{service.PlatformGemini, service.PlatformAntigravity} {
		modes := []string{"messages", "chat", "native"}
		if platform == service.PlatformAntigravity {
			modes = append(modes, "responses", "legacy messages")
		}
		for _, mode := range modes {
			t.Run(platform+"/"+mode, func(t *testing.T) {
				f := newGeminiClientCancelFixture(t)
				f.group.ProfitControlEnabled, f.group.RateMultiplier, f.group.ProfitMinMargin = true, 1, 0.5
				rate := 0.4
				account := f.schedulerCache.accounts[0]
				account.RateMultiplier = &rate
				account.GroupIDs = []int64{f.group.ID}
				if platform == service.PlatformAntigravity {
					f.group.Platform, account.Platform, account.Type = platform, platform, service.AccountTypeOAuth
					account.Credentials = map[string]any{"access_token": "fixture-token", "project_id": "fixture-project", "model_mapping": map[string]any{"gemini-2.5-flash": "gemini-2.5-flash"}}
					if mode == "legacy messages" {
						account.Type = service.AccountTypeUpstream
						account.Credentials["api_key"] = "fixture-key"
						account.Credentials["base_url"] = "https://fixture.invalid"
					}
					f.handler.antigravityGatewayService = service.NewAntigravityGatewayService(nil, nil, nil, service.NewAntigravityTokenProvider(nil, nil, nil), nil, f.upstream, nil, nil)
				}
				var route, path, body string
				var call func(*gin.Context)
				switch mode {
				case "messages", "legacy messages":
					route, path = "/v1/messages", "/v1/messages"
					body = `{"model":"gemini-2.5-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
					call = f.handler.Messages
				case "chat":
					route, path = "/v1/chat/completions", "/v1/chat/completions"
					body = `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`
					call = f.handler.ChatCompletions
				case "responses":
					route, path = "/v1/responses", "/v1/responses"
					body = `{"model":"gemini-2.5-flash","input":"hi"}`
					call = f.handler.Responses
				case "native":
					route, path = "/v1beta/models/*modelAction", "/v1beta/models/gemini-2.5-flash:generateContent"
					body = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
					call = f.handler.GeminiV1BetaModels
					if platform == service.PlatformAntigravity {
						route, path = "/antigravity/v1beta/models/*modelAction", "/antigravity/v1beta/models/gemini-2.5-flash:generateContent"
						call = func(c *gin.Context) {
							c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.ForcePlatform, platform))
							c.Set(string(middleware.ContextKeyForcePlatform), platform)
							f.handler.GeminiV1BetaModels(c)
						}
					}
				}
				rec, _ := f.serve(t, route, path, body, call)
				require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "Account eligibility changed")
				require.Zero(t, f.upstream.calls.Load(), "cached slot admission cannot replace the missing final authority")
			})
		}
	}
}
