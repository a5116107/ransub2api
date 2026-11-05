package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type openAIWSContinuationObservation struct {
	connection int32
	turnState  string
	payload    map[string]any
}

func newOpenAIWSContinuationUpstream(t *testing.T, closeAfterTurn bool) (*httptest.Server, <-chan openAIWSContinuationObservation) {
	t.Helper()
	var handshakes atomic.Int32
	observations := make(chan openAIWSContinuationObservation, 16)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := handshakes.Add(1)
		conn, err := upgrader.Upgrade(w, r, http.Header{"X-Codex-Turn-State": {fmt.Sprintf("minted-%d", id)}})
		if err != nil {
			t.Errorf("upgrade upstream: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for turn := 1; ; turn++ {
			var payload map[string]any
			if err := conn.ReadJSON(&payload); err != nil {
				if turn == 1 {
					t.Errorf("read upstream turn: %v", err)
				}
				return
			}
			observations <- openAIWSContinuationObservation{id, r.Header.Get(openAIWSTurnStateHeader), payload}
			if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{
				"id": fmt.Sprintf("resp_scope_%d_%d", id, turn), "model": payload["model"],
				"usage": map[string]any{"input_tokens": 2, "output_tokens": 1},
			}}); err != nil {
				t.Errorf("write upstream completion: %v", err)
				return
			}
			if closeAfterTurn {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream, observations
}

func TestWSManagedTurnStateForwardRejectsForeignEgress(t *testing.T) {
	for _, change := range []string{"same scope", "account", "credential", "endpoint", "model", "priority", "mapped alias", "filtered priority", "forced priority"} {
		t.Run(change, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			upstream, observations := newOpenAIWSContinuationUpstream(t, true)
			cfg := newOpenAIWSExecutionScopeTestConfig()
			cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 0
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{},
				openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector()}
			svc.settingService = newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings()).settingService
			t.Cleanup(svc.CloseOpenAIWSPool)
			groupID := int64(9)
			account := &Account{ID: 456, GroupIDs: []int64{groupID}, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Status: StatusActive, Schedulable: true, Concurrency: 2,
				Credentials: map[string]any{"api_key": "supplier-fixture", "base_url": upstream.URL},
				Extra:       map[string]any{"responses_websockets_v2_enabled": true}}
			if change == "mapped alias" {
				account.Credentials["model_mapping"] = map[string]any{"gpt-5.1": "gpt-5.1", "friendly-model": "gpt-5.1"}
			}
			calls := 0
			forward := func(model, tier string) *OpenAIForwardResult {
				t.Helper()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				c.Request.Header.Set("session_id", "shared-scope-session")
				c.Set("api_key", &APIKey{ID: 21, GroupID: &groupID})
				body := []byte(fmt.Sprintf(`{"model":%q,"service_tier":%q,"stream":false,"input":[{"role":"user","content":"hello"}]}`, model, tier))
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if change == "filtered priority" {
					ctx = withOpenAIFastPolicyContext(ctx, openAIFastFilterPriorityPolicy())
				}
				if change == "forced priority" && calls > 0 {
					ctx = withOpenAIFastPolicyContext(ctx, &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierMissing, Action: OpenAIFastPolicyActionForcePriority, Scope: BetaPolicyScopeAll}}})
				}
				calls++
				result, err := svc.Forward(ctx, c, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				return result
			}
			first := forward("gpt-5.1", "")
			firstObservation := <-observations
			require.Empty(t, firstObservation.turnState)
			require.Equal(t, "gpt-5.1", firstObservation.payload["model"])
			connID, ok := svc.getOpenAIWSStateStore().GetResponseConn(first.RequestID)
			require.True(t, ok)
			svc.getOpenAIWSConnPool().evictConn(account.ID, connID)
			model, tier := "gpt-5.1", ""
			switch change {
			case "account":
				account.ID++
			case "credential":
				account.Credentials["api_key"] = "replacement-fixture"
			case "endpoint":
				account.Credentials["base_url"] = upstream.URL + "/replacement"
			case "model":
				model = "gpt-5.2"
			case "priority":
				tier = "priority"
			case "mapped alias":
				model = "friendly-model"
			case "filtered priority":
				tier = "priority"
			}
			forward(model, tier)
			observation := <-observations
			state, payload := observation.turnState, observation.payload
			if change == "priority" || change == "forced priority" {
				require.Equal(t, "priority", payload["service_tier"])
			} else {
				require.Empty(t, payload["service_tier"])
			}
			if change == "mapped alias" {
				require.Equal(t, "gpt-5.1", payload["model"])
			}
			if change == "same scope" || change == "mapped alias" || change == "filtered priority" {
				require.Equal(t, "minted-1", state, "unchanged egress retains its managed continuation")
			} else {
				require.Empty(t, state, "a changed egress must not send the earlier managed turn state")
			}
			require.EqualValues(t, 2, observation.connection)
		})
	}
}

func TestWSManagedTurnStateReusedHandshakeDoesNotChangeOwner(t *testing.T) {
	for _, change := range []string{"model", "priority"} {
		t.Run(change, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			upstream, observations := newOpenAIWSContinuationUpstream(t, false)
			cfg := newOpenAIWSExecutionScopeTestConfig()
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{},
				openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector()}
			t.Cleanup(svc.CloseOpenAIWSPool)
			groupID := int64(9)
			account := &Account{ID: 458, GroupIDs: []int64{groupID}, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Status: StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{"api_key": "supplier-fixture", "base_url": upstream.URL},
				Extra:       map[string]any{"responses_websockets_v2_enabled": true}}
			var sessionScope string
			forward := func(model, tier string) (*OpenAIForwardResult, string) {
				t.Helper()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				c.Request.Header.Set("session_id", "same-client-session")
				c.Set("api_key", &APIKey{ID: 21, GroupID: &groupID})
				body := []byte(fmt.Sprintf(`{"model":%q,"service_tier":%q,"stream":false,"input":[{"role":"user","content":"hello"}]}`, model, tier))
				sessionScope, _ = resolveOpenAIWSExecutionScope(c, body, 21)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				result, err := svc.Forward(ctx, c, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				return result, c.Writer.Header().Get(openAIWSTurnStateHeader)
			}
			_, state := forward("gpt-5.1", "")
			require.Equal(t, "minted-1", state)
			require.EqualValues(t, 1, (<-observations).connection)
			model, tier := "gpt-5.1", ""
			if change == "model" {
				model = "gpt-5.2"
			} else {
				tier = "priority"
			}
			second, state := forward(model, tier)
			require.EqualValues(t, 1, (<-observations).connection, "a compatible socket may serve another effective scope")
			require.Empty(t, state, "the reused handshake was minted for the earlier scope")
			_, found := svc.getOpenAIWSStateStore().GetSessionTurnState(groupID, sessionScope, openAIWSContinuationScope(account, model, tier))
			require.False(t, found, "reuse must not relabel the original handshake")
			connID, ok := svc.getOpenAIWSStateStore().GetResponseConn(second.RequestID)
			require.True(t, ok)
			svc.getOpenAIWSConnPool().evictConn(account.ID, connID)
			_, state = forward(model, tier)
			observation := <-observations
			require.EqualValues(t, 2, observation.connection)
			require.Empty(t, observation.turnState, "the new scope must reconnect without the earlier scope's handshake state")
			require.Equal(t, "minted-2", state)
			stored, found := svc.getOpenAIWSStateStore().GetSessionTurnState(groupID, sessionScope, openAIWSContinuationScope(account, model, tier))
			require.True(t, found)
			require.Equal(t, "minted-2", stored)
		})
	}
}

// 场景：codex 子智能体经 HTTP 入站、网关内部走 WS 池转发时，与父线程共用 session_id 头。
// turn state 必须落在执行作用域键下，而不是按 session_id 算出的会话哈希下，
// 否则子智能体会覆盖父线程的绑定；同一线程在 WS 接入与 HTTP 路径之间也才能共享状态。
func TestOpenAIGatewayService_Forward_WSv2_TurnStateBoundToExecutionScope(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respHeader := http.Header{}
		respHeader.Set("x-codex-turn-state", "turn-state-from-upstream")
		conn, err := upgrader.Upgrade(w, r, respHeader)
		if err != nil {
			t.Errorf("upgrade websocket failed: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		var request map[string]any
		if err := conn.ReadJSON(&request); err != nil {
			t.Errorf("read ws request failed: %v", err)
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":    "resp_exec_scope_http",
				"model": "gpt-5.1",
				"usage": map[string]any{"input_tokens": 2, "output_tokens": 1},
			},
		}); err != nil {
			t.Errorf("write response.completed failed: %v", err)
		}
	}))
	defer wsServer.Close()

	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 0

	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
	}
	groupID := int64(9)
	account := &Account{
		GroupIDs: []int64{9},

		ID:          456,
		Name:        "openai-http-exec-scope",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": wsServer.URL},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
	}

	reqBody := []byte(`{"model":"gpt-5.1","stream":false,"input":[{"type":"input_text","text":"hello"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Request.Header.Set("session_id", "root-session")
	c.Request.Header.Set(openAIWSTurnMetadataHeader, `{"session_id":"root-session","thread_id":"child-thread"}`)
	c.Set("api_key", &APIKey{ID: 21, GroupID: &groupID})

	result, err := svc.Forward(context.Background(), c, account, reqBody)
	require.NoError(t, err)
	require.NotNil(t, result)

	legacyHash := svc.GenerateSessionHash(c, reqBody)
	scope, _ := resolveOpenAIWSExecutionScope(c, reqBody, 21)
	require.NotEmpty(t, legacyHash)
	require.Len(t, scope, 16)
	require.NotEqual(t, legacyHash, scope)

	store := svc.getOpenAIWSStateStore()
	_, turnStateOnLegacy := store.GetSessionTurnState(groupID, legacyHash, openAIWSContinuationScope(account, "gpt-5.1", ""))
	require.False(t, turnStateOnLegacy, "turn state 不得绑定到按 session_id 算出的会话哈希")
	turnState, turnStateOnScope := store.GetSessionTurnState(groupID, scope, openAIWSContinuationScope(account, "gpt-5.1", ""))
	require.True(t, turnStateOnScope, "turn state 应绑定到执行作用域")
	require.Equal(t, "turn-state-from-upstream", turnState)
}

// Forward 在转发前会按账号 namespace 改写请求体里的 client_metadata，指纹 full 模式还会给每个
// 请求注入同一个 thread_id。执行作用域必须取自客户端原始请求：否则同一 API key 下不同
// session_id 的会话会落到同一个键共用 turn state，客户端自带线程标识时也会与 WS 接入路径
// 按原始报文算出的键对不上。
func TestOpenAIGatewayService_Forward_WSv2_ExecutionScopeUsesOriginalIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1

	completed := func(id string) []byte {
		return []byte(`{"type":"response.completed","response":{"id":"` + id + `","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
	}
	captureConn := &openAIWSCaptureConn{events: [][]byte{completed("resp_a"), completed("resp_b"), completed("resp_c")}}
	handshake := http.Header{}
	handshake.Set(openAIWSTurnStateHeader, "turn-state-from-upstream")
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: captureConn, handshake: handshake})
	defer pool.Close()

	stateStore := NewOpenAIWSStateStore(nil)
	svc := &OpenAIGatewayService{
		cfg:                cfg,
		httpUpstream:       &httpUpstreamRecorder{},
		cache:              &stubGatewayCache{},
		openaiWSResolver:   NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:      NewCodexToolCorrector(),
		openaiWSPool:       pool,
		openaiWSStateStore: stateStore,
	}
	groupID := int64(9)
	const apiKeyID = int64(21)
	account := &Account{
		GroupIDs: []int64{9},

		ID:          457,
		Name:        "openai-oauth-fingerprint-full",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "oauth-token-1"},
		Extra: map[string]any{
			"responses_websockets_v2_enabled": true,
			codexFingerprintModeExtraKey:      "full",
			codexFingerprintSeedExtraKey:      "11111111-1111-4111-8111-aaaaaaaaaaaa",
		},
	}

	forward := func(sessionID, body string) (*gin.Context, []byte) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
		c.Request.Header.Set("session_id", sessionID)
		c.Set("api_key", &APIKey{ID: apiKeyID, GroupID: &groupID})
		raw := []byte(body)
		result, err := svc.Forward(context.Background(), c, account, raw)
		require.NoError(t, err)
		require.NotNil(t, result)
		return c, raw
	}

	plainBody := `{"model":"gpt-5.1","stream":false,"input":[{"type":"input_text","text":"hello"}]}`
	cA, rawA := forward("session-a", plainBody)
	cB, rawB := forward("session-b", plainBody)
	for _, sent := range captureConn.writes {
		require.Equal(t, "gpt-5.4", sent["model"], "OAuth normalization determines the effective continuation model")
	}
	continuationScope := openAIWSContinuationScope(account, "gpt-5.4", "")

	scopeA, _ := resolveOpenAIWSExecutionScope(cA, rawA, apiKeyID)
	scopeB, _ := resolveOpenAIWSExecutionScope(cB, rawB, apiKeyID)
	require.NotEmpty(t, scopeA)
	require.NotEqual(t, scopeA, scopeB, "不同 session_id 的会话必须落在不同的作用域")
	_, boundA := stateStore.GetSessionTurnState(groupID, scopeA, continuationScope)
	require.True(t, boundA, "会话 A 的 turn state 应落在按原始 session_id 算出的作用域")
	_, boundB := stateStore.GetSessionTurnState(groupID, scopeB, continuationScope)
	require.True(t, boundB, "会话 B 的 turn state 应落在按原始 session_id 算出的作用域")

	injected := resolveCodexFingerprintIDs(account, "", codexFingerprintFull)
	require.NotNil(t, injected)
	injectedScope, _ := deriveOpenAISessionHashes(fmt.Sprintf("openai_ws_exec:%d|thread=%s", apiKeyID, injected.threadID))
	_, boundToInjected := stateStore.GetSessionTurnState(groupID, injectedScope, continuationScope)
	require.False(t, boundToInjected, "指纹收敛注入的固定 thread_id 不得成为状态键")

	threadBody := `{"model":"gpt-5.1","stream":false,"client_metadata":{"thread_id":"child-thread"},"input":[{"type":"input_text","text":"hello"}]}`
	cC, rawC := forward("session-c", threadBody)
	scopeC, threadC := resolveOpenAIWSExecutionScope(cC, rawC, apiKeyID)
	require.Equal(t, "child-thread", threadC)
	_, boundC := stateStore.GetSessionTurnState(groupID, scopeC, continuationScope)
	require.True(t, boundC, "客户端自带线程标识时，键必须与按原始报文算出的一致，不受账号 namespace 改写影响")
}
