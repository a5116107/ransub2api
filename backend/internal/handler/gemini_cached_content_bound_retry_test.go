//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	boundRetryUpstream429 = `{"error":{"code":429,"message":"Resource exhausted. Please try again later.","status":"RESOURCE_EXHAUSTED"}}`
	boundRetryUpstream502 = `{"error":{"code":502,"message":"Upstream service temporarily unavailable","status":"UNAVAILABLE"}}`
)

// boundRetryUpstream 前 failures 次请求返回 failStatus（默认 429），之后返回成功的生成响应。
type boundRetryUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	failures   int
	failStatus int
	bodies     []string
	onRequest  func(int)
}

func (u *boundRetryUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	u.mu.Lock()
	u.bodies = append(u.bodies, string(raw))
	n := len(u.bodies)
	u.mu.Unlock()
	if u.onRequest != nil {
		u.onRequest(n)
	}
	header := http.Header{"Content-Type": []string{"application/json"}}
	if n <= u.failures {
		if u.failStatus == http.StatusBadGateway {
			return &http.Response{StatusCode: http.StatusBadGateway, Header: header, Body: io.NopCloser(strings.NewReader(boundRetryUpstream502))}, nil
		}
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: header, Body: io.NopCloser(strings.NewReader(boundRetryUpstream429))}, nil
	}
	body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":8,"candidatesTokenCount":1,"totalTokenCount":11}}`
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (u *boundRetryUpstream) requestBodies() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.bodies...)
}

// newBoundRetryEnv 构造绑定在池模式上游网关账号 900 上的显式缓存，账号对 retryCodes 同账号重试 2 次。
func newBoundRetryEnv(t *testing.T, upstream *boundRetryUpstream, slots service.ConcurrencyCache, retryCodes ...int) *cachedBillingEnv {
	t.Helper()
	var concurrency *service.ConcurrencyService
	if slots != nil {
		concurrency = service.NewConcurrencyService(slots)
	}
	env := newCachedBillingEnvWithConcurrency(t, 500, concurrency, 900)
	creds := env.accounts[900].Credentials
	creds["base_url"] = "https://gw.example.com"
	creds[service.GeminiExplicitCacheUpstreamCredentialKey] = true
	creds["pool_mode"] = true
	creds["pool_mode_retry_count"] = float64(2)
	codes := make([]any, 0, len(retryCodes))
	for _, code := range retryCodes {
		codes = append(codes, float64(code))
	}
	creds["pool_mode_retry_status_codes"] = codes
	env.seed(time.Now().Add(time.Hour))

	env.h.geminiCompatService = service.NewGeminiMessagesCompatService(&cachedBillingAccountRepo{accounts: env.accounts},
		nil, nil, nil, nil, nil, upstream, nil, &config.Config{})
	return env
}

func (e *cachedBillingEnv) generateWithCache() *httptest.ResponseRecorder {
	return e.serve(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent",
		`{"cachedContent":"cachedContents/abc","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		gin.Params{{Key: "modelAction", Value: "/gemini-2.5-pro:generateContent"}}, e.h.GeminiV1BetaModels)
}

func TestGeminiV1BetaModels_BoundCachePoolModeRetriesUpstream429(t *testing.T) {
	upstream := &boundRetryUpstream{failures: 1}
	env := newBoundRetryEnv(t, upstream, nil, 429)

	rec := env.generateWithCache()

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	bodies := upstream.requestBodies()
	require.Len(t, bodies, 2, "429 后在持有缓存的账号上重试一次")
	for _, body := range bodies {
		require.Contains(t, body, `"cachedContents/up1"`, "每次重试都引用上游缓存名")
	}
}

func TestGeminiV1BetaModels_BoundCacheRetryExhaustedPassesThroughUpstream429(t *testing.T) {
	upstream := &boundRetryUpstream{failures: 10}
	env := newBoundRetryEnv(t, upstream, nil, 429)

	rec := env.generateWithCache()

	require.Len(t, upstream.requestBodies(), 3, "首发 + pool_mode_retry_count 次同账号重试")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.JSONEq(t, boundRetryUpstream429, rec.Body.String(), "重试用尽后原样回写上游网关的响应")
}

// 502 在池模式重试状态码内时同样同账号重试；用尽后原样回写，不按空响应临时封禁本账号。
func TestGeminiV1BetaModels_BoundCacheRetryExhaustedOn502KeepsAccountSchedulable(t *testing.T) {
	upstream := &boundRetryUpstream{failures: 10, failStatus: http.StatusBadGateway}
	env := newBoundRetryEnv(t, upstream, nil, 429, 502)

	rec := env.generateWithCache()

	require.Len(t, upstream.requestBodies(), 3)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.JSONEq(t, boundRetryUpstream502, rec.Body.String())
}

func TestGeminiV1BetaModels_BoundCacheRejectsChangedUpstreamModel(t *testing.T) {
	for _, action := range []string{"generateContent", "streamGenerateContent", "countTokens"} {
		for _, change := range []string{"mapping", "missing_provenance", "vertex_project", "vertex_location", "vertex_model", "invalid_vertex_location"} {
			t.Run(action+"/"+change, func(t *testing.T) {
				upstream := &boundRetryUpstream{}
				slots := &boundCacheModelSlots{}
				env := newBoundRetryEnv(t, upstream, slots, 429)
				account := env.accounts[900]
				if strings.Contains(change, "vertex") {
					account.Type = service.AccountTypeServiceAccount
					account.Credentials["project_id"] = "original-project"
					account.Credentials["location"] = "us-central1"
					resource, err := service.GeminiCachedContentUpstreamModelResource(account, "gemini-2.5-pro")
					require.NoError(t, err)
					env.repo.records["abc"].UpstreamModel = resource
				}
				switch change {
				case "mapping":
					account.Credentials["model_mapping"] = map[string]any{"gemini-2.5-pro": "gemini-3.8-flash"}
				case "missing_provenance":
					env.repo.records["abc"].UpstreamModel = ""
				case "vertex_project":
					account.Credentials["project_id"] = "replacement-project"
				case "vertex_location":
					account.Credentials["location"] = "europe-west1"
				case "vertex_model":
					account.Credentials["model_mapping"] = map[string]any{"gemini-2.5-pro": "gemini-3.8-flash"}
				case "invalid_vertex_location":
					account.Credentials["location"] = "invalid/location"
				}
				record := *env.repo.records["abc"]
				rec := env.serve(http.MethodPost, "/v1beta/models/gemini-2.5-pro:"+action,
					`{"cachedContent":"cachedContents/abc","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
					gin.Params{{Key: "modelAction", Value: "/gemini-2.5-pro:" + action}}, func(c *gin.Context) {
						env.h.GeminiV1BetaModels(c)
						require.True(t, service.IsOpsRequestScopedError(c), "cache mismatch must not penalize upstream health")
					})
				require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), service.GeminiCachedContentNotFoundMessage)
				require.Empty(t, upstream.requestBodies(), "a mismatched cache must never reach the upstream")
				require.Equal(t, record, *env.repo.records["abc"], "rejection must preserve the cache and its expiry")
				require.Empty(t, env.repo.deleted)
				slots.mu.Lock()
				require.Zero(t, slots.active)
				require.Equal(t, 1, slots.released)
				slots.mu.Unlock()
				account.Type = service.AccountTypeAPIKey
				account.Credentials["model_mapping"] = map[string]any{"gemini-2.5-pro": "gemini-2.5-pro"}
				env.repo.records["abc"].UpstreamModel = "models/gemini-2.5-pro"
				recovered := env.generateWithCache()
				require.Equal(t, http.StatusOK, recovered.Code, recovered.Body.String())
				require.Len(t, upstream.requestBodies(), 1)
				require.Contains(t, upstream.requestBodies()[0], `"cachedContents/up1"`)
			})
		}
	}
}

type boundCacheModelSlots struct {
	fakeConcurrencyCache
	mu       sync.Mutex
	active   int
	released int
}

func (s *boundCacheModelSlots) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active++
	return true, nil
}

func (s *boundCacheModelSlots) ReleaseAccountSlot(context.Context, int64, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	s.released++
	return nil
}

func TestGeminiV1BetaModels_BoundCacheRechecksModelOnRetry(t *testing.T) {
	upstream := &boundRetryUpstream{failures: 1}
	env := newBoundRetryEnv(t, upstream, nil, 429)
	upstream.onRequest = func(n int) {
		if n == 1 {
			env.accounts[900].Credentials["model_mapping"] = map[string]any{"gemini-2.5-pro": "gemini-3.8-flash"}
		}
	}
	rec := env.generateWithCache()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Len(t, upstream.requestBodies(), 1, "mapping changed after the first 429; the retry must not send")
	require.Empty(t, env.repo.deleted)
	env.accounts[900].Credentials["model_mapping"] = map[string]any{"gemini-2.5-pro": "gemini-2.5-pro"}
	recovered := env.generateWithCache()
	require.Equal(t, http.StatusOK, recovered.Code, recovered.Body.String())
	require.Len(t, upstream.requestBodies(), 2)
}
