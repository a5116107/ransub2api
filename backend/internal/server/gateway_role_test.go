package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil/mtls"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayRoleExposesAuthenticatedModelRoutesWithoutAdminOrPayments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}, Gateway: config.GatewayConfig{MaxBodySize: 1024 * 1024, TextMaxBodySize: 1024 * 1024}}
	h := &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: &handler.OpenAIGatewayHandler{}, AsyncImage: handler.NewAsyncImageHandler(nil, nil)}
	registerRoutes(router, h, nil, nil, nil, func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) }, nil, nil, nil, nil, nil, nil, nil, cfg, nil)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/health", 200},
		{"GET", "/v1/models", 401},
		{"POST", "/v1/responses", 401},
		{"GET", "/api/v1/admin/settings", 404},
		{"POST", "/api/v1/auth/login", 404},
		{"POST", "/api/v1/payment/orders", 404},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			require.Equal(t, tc.status, w.Code)
		})
	}
}

type regionalRoleAuthSource struct{ records []service.RegionalAuthRecord }

func (s regionalRoleAuthSource) ListRegionalAuth(context.Context, []int64) ([]service.RegionalAuthRecord, error) {
	return s.records, nil
}

type regionalRoleVideoUpstream struct {
	service.HTTPUpstream
	mode  string
	calls int
	last  video.Query
	peer  *video.Forwarder
}

func (u *regionalRoleVideoUpstream) SetAsyncVideoForwardHandler(dispatch video.Dispatch) {
	if u.peer != nil {
		u.peer.SetDispatch(dispatch)
	}
}

func (u *regionalRoleVideoUpstream) ForwardAsyncVideoLookup(ctx context.Context, query video.Query) (*http.Response, bool, error) {
	u.calls++
	u.last = query
	switch u.mode {
	case "local":
		return nil, false, nil
	case "unavailable":
		return nil, true, service.ErrUpstreamKeyPolicy
	case "mtls":
		owner, err := u.peer.Locate(ctx, query.Owner, query.TaskID)
		if err != nil {
			return nil, true, err
		}
		query.Owner = *owner
		response, err := u.peer.Query(ctx, query)
		return response, true, err
	default:
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"origin":true}`))}, true, nil
	}
}

func TestRegionalGatewayVideoQueryReachesOriginalNodeAfterLocalPolicies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	identities := mtls.Pair(t)
	keyMaterial := bytes.Repeat([]byte{1}, 32)
	lookupKey := bytes.Repeat([]byte{2}, 32)
	policy := regional.Config{RegionID: "us", MaxSnapshotAgeSeconds: 60}
	statePath := filepath.Join(t.TempDir(), "auth.snapshot")
	codec, err := regional.NewCodec(map[string][]byte{"fixture": keyMaterial})
	require.NoError(t, err)
	replica, err := regional.OpenReplica(statePath, policy, regional.SnapshotAuth, codec, service.RegionalAuthValidator([]int64{1}), true)
	require.NoError(t, err)
	var records []service.RegionalAuthRecord
	for i, platform := range []string{service.PlatformGrok, service.PlatformMiniMax, service.PlatformOpenAI, service.PlatformComposite} {
		id := int64(i + 3)
		key := &service.APIKey{ID: int64(i + 20), UserID: 1, GroupID: &id, Key: "fixture-" + platform, Status: service.StatusActive, IPWhitelist: []string{"127.0.0.1"},
			User: &service.User{ID: 1, Status: service.StatusActive}, Group: &service.Group{ID: id, Platform: platform, Status: service.StatusActive,
				BlockSubagents: true, ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"model-fixture"}}}}
		records = append(records, service.RegionalAuthRecord{APIKey: key})
	}
	payload, err := service.ExportRegionalAuth(t.Context(), regionalRoleAuthSource{records: records}, []int64{1}, lookupKey)
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, replica.Publish(t.Context(), "fixture", 1, now, now.Add(55*time.Second), payload))
	runtime := service.RegionalSchedulerRuntimeConfig{Kind: regional.SnapshotAuth, AuthUserIDs: []int64{1}, AuthLookupKey: lookupKey,
		Mode: "subscriber", Policy: policy, Keys: map[string][]byte{"fixture": keyMaterial}, KeyID: "fixture", StateFile: statePath,
		Endpoint: "https://127.0.0.1:1/internal/regional/auth", Listen: "127.0.0.1:0", CAFile: identities[0].CAFile,
		CertFile: identities[0].CertFile, KeyFile: identities[0].KeyFile, TrustedServerCertSHA256: identities[1].Pin,
		TrustedClientCertSHA256: []string{identities[1].Pin}, IntervalSeconds: 1, LifetimeSeconds: 30}
	raw, err := json.Marshal(runtime)
	require.NoError(t, err)
	configPath := filepath.Join(t.TempDir(), "regional-auth.json")
	require.NoError(t, os.WriteFile(configPath, raw, 0600))
	cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway, RegionalAuthFile: configPath, RegionalFundsFile: "fixture"},
		Gateway: config.GatewayConfig{MaxBodySize: 1 << 20, TextMaxBodySize: 1 << 20}}
	keys, err := service.ProvideAPIKeyService(nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil)
	require.NoError(t, err)
	t.Cleanup(keys.StopAuthCacheInvalidationSubscriber)
	_, err = keys.GetRegionalByKey(t.Context(), "fixture-grok")
	require.NoError(t, err, "the test must begin with a valid local authorization snapshot")
	upstream := &regionalRoleVideoUpstream{}
	svc := service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{Config: cfg, Upstream: upstream})
	t.Cleanup(svc.StopAsyncVideoKeyReleaseRecovery)
	t.Cleanup(svc.StopOpenAICodexTicketHarvester)
	h := &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: handler.NewOpenAIGatewayHandler(svc, nil, nil, nil, nil, nil, nil, nil, cfg), AsyncImage: handler.NewAsyncImageHandler(nil, nil)}
	router := gin.New()
	registerRoutes(router, h, nil, nil, nil, func(c *gin.Context) {
		t.Error("regional query must not enter the primary-backed API key middleware")
		c.AbortWithStatus(http.StatusInternalServerError)
	}, nil, nil, keys, nil, nil, nil, nil, cfg, nil)
	request := func(method, path, credential, subagent string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		if subagent != "" {
			req.Header.Set("X-OpenAI-Subagent", subagent)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	for _, tc := range []struct{ method, path, platform, operation, task string }{
		{http.MethodGet, "/v1/videos/responses", service.PlatformGrok, "video_status", "responses"},
		{http.MethodGet, "/videos/generations/task/content", service.PlatformGrok, "video_content", "task"},
		{http.MethodGet, "/v1/videos/task", service.PlatformMiniMax, "minimax_h3_status", "minimax-h3:task"},
		{http.MethodGet, "/videos/task/content", service.PlatformMiniMax, "minimax_h3_content", "minimax-h3:task"},
		{http.MethodGet, "/v1/contents/generations/tasks/task", service.PlatformOpenAI, "seedance_status", "seedance:task"},
		{http.MethodDelete, "/api/v3/contents/generations/tasks/task", service.PlatformOpenAI, "seedance_delete", "seedance:task"},
		{http.MethodGet, "/v1/videos/task", service.PlatformComposite, "video_status", "task"},
	} {
		t.Run(tc.method+tc.path+"/"+tc.platform, func(t *testing.T) {
			before := upstream.calls
			w := request(tc.method, tc.path+"?model=model-fixture", "fixture-"+tc.platform, "")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, before+1, upstream.calls)
			require.Equal(t, tc.operation, upstream.last.Operation)
			require.Equal(t, tc.task, upstream.last.TaskID)
			require.Equal(t, int64(1), upstream.last.Owner.UserID)
			require.Positive(t, upstream.last.Owner.APIKeyID)
			require.Positive(t, upstream.last.Owner.GroupID)
			require.Equal(t, "127.0.0.1", upstream.last.ClientIP)
			require.Equal(t, "model-fixture", upstream.last.Model)
			require.Equal(t, video.ForwardPolicyVersion, upstream.last.PolicyVersion)
		})
	}
	for _, tc := range []struct{ path, platform string }{
		{"/v1/videos", service.PlatformGrok},
		{"/videos/edits", service.PlatformGrok},
		{"/v1/videos/extensions", service.PlatformComposite},
		{"/videos/generations", service.PlatformMiniMax},
		{"/v3/contents/generations/tasks", service.PlatformOpenAI},
		{"/api/v3/contents/generations/tasks", service.PlatformComposite},
	} {
		t.Run("create"+tc.path+"/"+tc.platform, func(t *testing.T) {
			before := upstream.calls
			w := request(http.MethodPost, tc.path+"?model=model-fixture", "fixture-"+tc.platform, "")
			require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "regional_cost_bound_unavailable", "locally authenticated creation must reach the funds gate, which refuses this fixture's missing financial authority")
			require.Equal(t, before, upstream.calls, "creation must not use the cross-region query forwarder")
			w = request(http.MethodPost, tc.path+"?model=blocked", "fixture-"+tc.platform, "")
			require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
			w = request(http.MethodPost, tc.path+"?model=model-fixture", "fixture-"+tc.platform, "worker")
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			require.Equal(t, before, upstream.calls)
		})
	}
	// Exercise the full regional ingress through real mutual TLS and back into
	// the original service's authenticated router. SQL ownership is covered by
	// the separate regional repository integration test, not this fixture.
	addresses := make([]string, 2)
	listeners := make([]net.Listener, 2)
	for i := range listeners {
		listeners[i], err = net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addresses[i] = listeners[i].Addr().String()
	}
	for _, listener := range listeners {
		require.NoError(t, listener.Close())
	}
	resolve := func(_ context.Context, principal video.Owner, task string) (*video.Owner, error) {
		if task != "task" || principal.UserID != 1 || principal.APIKeyID != 20 || principal.GroupID != 3 || principal.Provider != "grok" {
			return nil, video.ErrUnavailable
		}
		principal.AccountID = 42
		principal.RouteFingerprint = [32]byte{1}
		return &principal, nil
	}
	var forwarders [2]*video.Forwarder
	for i := range forwarders {
		identity := identities[i]
		forwardConfig := video.ForwardConfig{Listen: addresses[i], Endpoint: "https://" + addresses[1-i],
			CAFile: identity.CAFile, CertFile: identity.CertFile, KeyFile: identity.KeyFile, PeerPin: identities[1-i].Pin,
			UserIDs: []int64{1}, APIKeyIDs: []int64{20}, AccountIDs: []int64{42}, TimeoutSeconds: 5}
		authority, peer := video.PrimaryAuthority, video.LocalAuthority("us")
		if i == 1 {
			authority, peer = peer, authority
		}
		forwarders[i], err = video.NewForwarder(forwardConfig, authority, peer, resolve)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func(f *video.Forwarder) { done <- f.Run(ctx) }(forwarders[i])
		t.Cleanup(func() {
			cancel()
			select {
			case stopErr := <-done:
				require.NoError(t, stopErr)
			case <-time.After(8 * time.Second):
				t.Error("video forwarder did not stop")
			}
		})
	}
	originUpstream := &regionalRoleVideoUpstream{peer: forwarders[0]}
	originSvc := service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{Config: cfg, Upstream: originUpstream})
	t.Cleanup(originSvc.StopAsyncVideoKeyReleaseRecovery)
	t.Cleanup(originSvc.StopOpenAICodexTicketHarvester)
	originRouter := gin.New()
	originRouter.Use(servermiddleware.RegionalAPIKeyAuth(keys, cfg), servermiddleware.GroupModelAllowlist(), servermiddleware.GroupBlockSubagents())
	var originCalls atomic.Int32
	originRouter.GET("/v1/videos/:request_id", func(c *gin.Context) {
		key, ok := servermiddleware.GetAPIKeyFromContext(c)
		if !ok {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		subject, ok := servermiddleware.GetAuthSubjectFromContext(c)
		if !ok {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		handled, err := originSvc.ForwardRegionalVideoQuery(c, service.GrokMediaEndpointVideoStatus, c.Param("request_id"), key, subject.UserID)
		if err != nil || handled {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		originCalls.Add(1)
		c.JSON(http.StatusOK, gin.H{"original_router": true})
	})
	originSvc.InstallMediaVideoForwarding(originRouter)
	upstream.mode, upstream.peer = "mtls", forwarders[1]
	w := request(http.MethodGet, "/v1/videos/task?model=model-fixture", "fixture-grok", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.JSONEq(t, `{"original_router":true}`, w.Body.String())
	require.EqualValues(t, 1, originCalls.Load())
	require.Zero(t, originUpstream.calls, "a received query must not be forwarded a second time")
	for _, tc := range []struct {
		name   string
		mutate func(*video.Query)
		status int
	}{
		{"wrong_credential", func(q *video.Query) { q.Credential = "fixture-minimax" }, http.StatusForbidden},
		{"subagent", func(q *video.Query) { q.Subagent = true }, http.StatusForbidden},
		{"model", func(q *video.Query) { q.Model = "blocked" }, http.StatusNotFound},
	} {
		t.Run("origin_"+tc.name, func(t *testing.T) {
			query := video.Query{PolicyVersion: video.ForwardPolicyVersion,
				Owner:  video.Owner{UserID: 1, APIKeyID: 20, GroupID: 3, AccountID: 42, Provider: "grok", RouteFingerprint: [32]byte{1}},
				TaskID: "task", Operation: "video_status", Credential: "fixture-grok", ClientIP: "127.0.0.1", Model: "model-fixture"}
			tc.mutate(&query)
			response, err := forwarders[1].Query(t.Context(), query)
			require.NoError(t, err)
			require.Equal(t, tc.status, response.StatusCode)
			require.NoError(t, response.Body.Close())
			require.EqualValues(t, 1, originCalls.Load())
			require.Zero(t, originUpstream.calls)
		})
	}
	upstream.mode = "remote"
	for _, tc := range []struct {
		path, credential, subagent string
		status                     int
	}{
		{"/v1/videos/task", "", "", http.StatusUnauthorized},
		{"/v1/videos/task", "fixture-unknown", "", http.StatusUnauthorized},
		{"/v1/videos/task", "fixture-grok", "worker", http.StatusForbidden},
		{"/v1/videos/task?model=blocked", "fixture-grok", "", http.StatusNotFound},
		{"/v1/images/tasks/task", "fixture-grok", "", http.StatusServiceUnavailable},
		{"/v1/responses", "fixture-grok", "", http.StatusServiceUnavailable},
	} {
		before := upstream.calls
		w := request(http.MethodGet, tc.path, tc.credential, tc.subagent)
		require.Equal(t, tc.status, w.Code, w.Body.String())
		require.Equal(t, before, upstream.calls, tc.path)
	}
	for _, tc := range []struct {
		name, remote, pin string
		status            int
	}{
		{"wrong_ip", "198.51.100.5:1234", "", http.StatusForbidden},
		{"wrong_group", "127.0.0.1:1234", "999", http.StatusForbidden},
		{"ambiguous_group", "127.0.0.1:1234", "3,4", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := upstream.calls
			req := httptest.NewRequest(http.MethodGet, "/v1/videos/task", nil)
			req.RemoteAddr = tc.remote
			req.Header.Set("Authorization", "Bearer fixture-grok")
			if tc.pin != "" {
				req.Header.Set(service.APIKeyGroupHeader, tc.pin)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Equal(t, before, upstream.calls)
		})
	}
	before := upstream.calls
	w = request(http.MethodPost, "/v1/videos", "fixture-grok", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	require.Equal(t, before, upstream.calls, "the preview must never authorize video creation")
	for _, mode := range []string{"local", "unavailable"} {
		upstream.mode = mode
		w := request(http.MethodGet, "/v1/videos/task", "fixture-grok", "")
		require.Equal(t, http.StatusServiceUnavailable, w.Code, mode+": "+w.Body.String())
	}
	noFunds := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway, RegionalAuthFile: configPath},
		Gateway: config.GatewayConfig{MaxBodySize: 1 << 20, TextMaxBodySize: 1 << 20}}
	withoutFunds := gin.New()
	registerRoutes(withoutFunds, h, nil, nil, nil, func(c *gin.Context) { t.Error("unexpected primary auth"); c.AbortWithStatus(500) }, nil, nil, keys, nil, nil, nil, nil, noFunds, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/task", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer fixture-grok")
	w = httptest.NewRecorder()
	before = upstream.calls
	withoutFunds.ServeHTTP(w, req)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	require.Equal(t, before, upstream.calls, "without the regional funds gate the preview must not enter any video handler")
}

func TestRegionalSchedulerPreviewBlocksBeforeAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway, RegionalSchedulerFile: "preview.json"}, Gateway: config.GatewayConfig{MaxBodySize: 1024, TextMaxBodySize: 1024}}
	h := &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: &handler.OpenAIGatewayHandler{}, AsyncImage: handler.NewAsyncImageHandler(nil, nil)}
	registerRoutes(router, h, nil, nil, nil, func(c *gin.Context) { t.Fatal("preview reached primary authentication") }, nil, nil, nil, nil, nil, nil, nil, cfg, nil)
	for _, path := range []string{"/v1/models", "/v1/responses", "/v1/messages", "/v1/chat/completions"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(func() string {
			if path == "/v1/models" {
				return http.MethodGet
			}
			return http.MethodPost
		}(), path, nil))
		require.Equal(t, http.StatusServiceUnavailable, w.Code, path)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	require.Equal(t, http.StatusOK, w.Code)
}

func TestRegionalAuthPreviewNeverFallsBackToPrimary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway, RegionalAuthFile: "auth.json"}, Gateway: config.GatewayConfig{MaxBodySize: 1024, TextMaxBodySize: 1024}}
	h := &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: &handler.OpenAIGatewayHandler{}, AsyncImage: handler.NewAsyncImageHandler(nil, nil)}
	registerRoutes(router, h, nil, nil, nil, func(c *gin.Context) { t.Fatal("regional auth reached primary middleware") }, nil, nil, nil, nil, nil, nil, nil, cfg, nil)
	for _, path := range []string{"/v1/models", "/v1/responses", "/v1/messages", "/v1/chat/completions", "/v1beta/models"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer key")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, 503, w.Code, path)
		require.Contains(t, w.Body.String(), "REGIONAL_AUTH_UNAVAILABLE")
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	require.Equal(t, 200, w.Code)
}
