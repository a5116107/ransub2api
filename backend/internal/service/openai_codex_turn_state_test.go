package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func newTurnStateTestContext(t *testing.T, apiKeyID int64, sessionID string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if sessionID != "" {
		c.Request.Header.Set("session_id", sessionID)
	}
	if apiKeyID > 0 {
		c.Set("api_key", &APIKey{ID: apiKeyID})
	}
	return c, rec
}

func TestOpenAICodexTurnStateSeed(t *testing.T) {
	c, _ := newTurnStateTestContext(t, 7, "sess-1")
	require.Equal(t, "7\x00sess-1", openAICodexTurnStateSeed(c))

	// 连字符形式优先（Codex CLI 标准头）
	c.Request.Header.Set("session-id", "sess-hyphen")
	require.Equal(t, "7\x00sess-hyphen", openAICodexTurnStateSeed(c))

	// 无会话标识 → 不跟踪
	cNoSession, _ := newTurnStateTestContext(t, 7, "")
	require.Empty(t, openAICodexTurnStateSeed(cNoSession))

	require.Empty(t, openAICodexTurnStateSeed(nil))
}

func TestRelayOpenAICodexTurnState_SetsHeaderAndRecordsProvenance(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 42}
	c, _ := newTurnStateTestContext(t, 7, "sess-relay")

	upstream := http.Header{}
	upstream.Set("x-codex-turn-state", "blob-A")
	svc.relayOpenAICodexTurnState(c, account, upstream)

	require.Equal(t, "blob-A", c.Writer.Header().Get("X-Codex-Turn-State"))

	raw, ok := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey(c, "blob-A"))
	require.True(t, ok)
	origin, ok := raw.(openAICodexTurnStateOrigin)
	require.True(t, ok)
	require.Equal(t, int64(42), origin.accountID)
	require.True(t, origin.expiresAt.After(time.Now()))
}

func TestRelayOpenAICodexTurnState_ClearsStaleValueWhenUpstreamAbsent(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c, _ := newTurnStateTestContext(t, 7, "sess-stale")
	// 模拟上一 failover attempt 残留的值
	c.Writer.Header().Set("X-Codex-Turn-State", "blob-old")

	svc.relayOpenAICodexTurnState(c, &Account{ID: 43}, http.Header{})

	require.Empty(t, c.Writer.Header().Get("X-Codex-Turn-State"))
	_, ok := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey(c, "blob-old"))
	require.False(t, ok)
}

func TestStageOpenAICodexTurnState_StagedHeaders(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c, _ := newTurnStateTestContext(t, 9, "sess-staged")

	// nil 集合 + 上游有值 → 创建集合并写入，但此刻还不记录溯源
	var staged http.Header
	upstream := http.Header{}
	upstream.Set("x-codex-turn-state", "blob-B")
	stageOpenAICodexTurnState(&staged, upstream)
	require.NotNil(t, staged)
	require.Equal(t, "blob-B", staged.Get("X-Codex-Turn-State"))
	_, noted := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey(c, "blob-B"))
	require.False(t, noted, "暂存阶段不得记录溯源：该 attempt 仍可能 failover 丢弃")

	// 真正提交时才记录
	svc.noteStagedOpenAICodexTurnStateCommitted(c, &Account{ID: 44}, staged)
	raw, ok := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey(c, "blob-B"))
	require.True(t, ok)
	origin, ok := raw.(openAICodexTurnStateOrigin)
	require.True(t, ok)
	require.Equal(t, int64(44), origin.accountID)

	// 上游无值 → 清除已暂存的值；nil 集合保持 nil
	stageOpenAICodexTurnState(&staged, http.Header{})
	require.Empty(t, staged.Get("X-Codex-Turn-State"))
	var nilStaged http.Header
	stageOpenAICodexTurnState(&nilStaged, http.Header{})
	require.Nil(t, nilStaged)
}

// 首输出超时导致 attempt 被丢弃时，溯源不得被该 attempt 污染——否则后续
// 请求会把客户端持有的合法 blob 误判成跨账号回带而剥离。
func TestStagedTurnState_AbandonedAttemptDoesNotPoisonProvenance(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c, _ := newTurnStateTestContext(t, 11, "sess-abandoned")

	// 账号 A 的 attempt 暂存了 blob，但从未提交（首输出超时 → failover）
	var staged http.Header
	upstreamA := http.Header{}
	upstreamA.Set("x-codex-turn-state", "blob-A")
	stageOpenAICodexTurnState(&staged, upstreamA)

	// 账号 B 接手并真正提交
	svc.relayOpenAICodexTurnState(c, &Account{ID: 52}, upstreamA)

	// 客户端回带的 blob 来自 B，出站到 B 时不得被剥离
	h := http.Header{}
	h.Set("x-codex-turn-state", "blob-A")
	svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 52}, h, openAICompatCacheScope{})
	require.Equal(t, "blob-A", h.Get("x-codex-turn-state"))

	raw, ok := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey(c, "blob-A"))
	require.True(t, ok)
	origin, ok := raw.(openAICodexTurnStateOrigin)
	require.True(t, ok)
	require.Equal(t, int64(52), origin.accountID)
}

func TestNoteStagedOpenAICodexTurnStateCommitted_NoopWithoutState(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c, _ := newTurnStateTestContext(t, 12, "sess-nostate")

	svc.noteStagedOpenAICodexTurnStateCommitted(c, &Account{ID: 60}, nil)
	svc.noteStagedOpenAICodexTurnStateCommitted(c, &Account{ID: 60}, http.Header{"X-Request-Id": []string{"rid"}})

	_, ok := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey(c, ""))
	require.False(t, ok)
}

func TestGuardOpenAICodexTurnStateEcho(t *testing.T) {
	newOutbound := func(state string) http.Header {
		h := http.Header{}
		if state != "" {
			h.Set("x-codex-turn-state", state)
		}
		return h
	}

	t.Run("same_account_keeps_echo", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "sess-g1")
		upstream := http.Header{}
		upstream.Set("x-codex-turn-state", "blob-A")
		svc.relayOpenAICodexTurnState(c, &Account{ID: 42}, upstream)

		h := newOutbound("blob-A")
		svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 42}, h, openAICompatCacheScope{})
		require.Equal(t, "blob-A", h.Get("x-codex-turn-state"))
	})

	t.Run("foreign_account_strips_echo", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "sess-g2")
		upstream := http.Header{}
		upstream.Set("x-codex-turn-state", "blob-A")
		svc.relayOpenAICodexTurnState(c, &Account{ID: 42}, upstream)

		// failover 换到账号 43：blob 由 42 铸造，必须剥离
		h := newOutbound("blob-A")
		svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 43}, h, openAICompatCacheScope{})
		require.Empty(t, h.Get("x-codex-turn-state"))
	})

	t.Run("no_provenance_passthrough", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "sess-g3")
		h := newOutbound("blob-unknown")
		svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 43}, h, openAICompatCacheScope{})
		require.Equal(t, "blob-unknown", h.Get("x-codex-turn-state"))
	})

	t.Run("expired_provenance_passthrough_and_pruned", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "sess-g4")
		svc.openaiCodexTurnStateOrigins.Store(openAICodexTurnStateKey(c, "blob-A"), openAICodexTurnStateOrigin{
			accountID: 42,
			expiresAt: time.Now().Add(-time.Minute),
		})
		h := newOutbound("blob-A")
		svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 43}, h, openAICompatCacheScope{})
		require.Equal(t, "blob-A", h.Get("x-codex-turn-state"))
		_, ok := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey(c, "blob-A"))
		require.False(t, ok)
	})

	t.Run("no_session_seed_noop", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "")
		h := newOutbound("blob-A")
		svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 43}, h, openAICompatCacheScope{})
		require.Equal(t, "blob-A", h.Get("x-codex-turn-state"))
	})

	t.Run("no_echo_noop", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "sess-g5")
		h := newOutbound("")
		svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 43}, h, openAICompatCacheScope{})
		require.Empty(t, h.Get("x-codex-turn-state"))
	})
}

func TestSweepOpenAICodexTurnStateOrigins_PrunesExpiredEntries(t *testing.T) {
	svc := &OpenAIGatewayService{}
	svc.openaiCodexTurnStateOrigins.Store("expired", openAICodexTurnStateOrigin{
		accountID: 1,
		expiresAt: time.Now().Add(-time.Minute),
	})
	svc.openaiCodexTurnStateOrigins.Store("alive", openAICodexTurnStateOrigin{
		accountID: 2,
		expiresAt: time.Now().Add(time.Hour),
	})

	// 计数器推进到触发清扫的边界
	svc.openaiCodexTurnStateWrites.Store(255)
	svc.sweepOpenAICodexTurnStateOrigins()

	_, expiredOK := svc.openaiCodexTurnStateOrigins.Load("expired")
	require.False(t, expiredOK)
	_, aliveOK := svc.openaiCodexTurnStateOrigins.Load("alive")
	require.True(t, aliveOK)
}

func TestWriteOpenAIPassthroughResponseHeaders_RelaysAndClearsTurnState(t *testing.T) {
	// filter=nil 走 content-type 兜底分支；turn-state 强制放行不依赖 filter。
	dst := http.Header{}
	src := http.Header{}
	src.Set("X-Codex-Turn-State", "blob-P")
	writeOpenAIPassthroughResponseHeaders(dst, src, nil)
	require.Equal(t, "blob-P", dst.Get("X-Codex-Turn-State"))

	// 上游缺失时清除残留（failover 换号防串扰）
	writeOpenAIPassthroughResponseHeaders(dst, http.Header{"Content-Type": []string{"application/json"}}, nil)
	require.Empty(t, dst.Get("X-Codex-Turn-State"))
}

func TestWriteOpenAIPassthroughResponseHeaders_RelaysReasoningIncluded(t *testing.T) {
	dst := http.Header{}
	src := http.Header{}
	src.Set("X-Reasoning-Included", "1")

	writeOpenAIPassthroughResponseHeaders(
		dst,
		src,
		responseheaders.CompileHeaderFilter(config.ResponseHeaderConfig{}),
	)
	require.Equal(t, "1", dst.Get("X-Reasoning-Included"))
}

func TestEnsureOpenAIRemoteCompactionV2BetaFeature(t *testing.T) {
	t.Run("absent_sets_feature", func(t *testing.T) {
		h := http.Header{}
		ensureOpenAIRemoteCompactionV2BetaFeature(h)
		require.Equal(t, "remote_compaction_v2", h.Get("x-codex-beta-features"))
	})

	t.Run("present_unchanged", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-codex-beta-features", "responses_websockets_v2, remote_compaction_v2")
		ensureOpenAIRemoteCompactionV2BetaFeature(h)
		require.Equal(t, "responses_websockets_v2, remote_compaction_v2", h.Get("x-codex-beta-features"))
	})

	t.Run("other_tokens_merged", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-codex-beta-features", "responses_websockets_v2")
		ensureOpenAIRemoteCompactionV2BetaFeature(h)
		require.Equal(t, "responses_websockets_v2,remote_compaction_v2", h.Get("x-codex-beta-features"))
	})

	t.Run("multi_line_values_merged_single_line", func(t *testing.T) {
		h := http.Header{}
		h.Add("x-codex-beta-features", "feature_a")
		h.Add("x-codex-beta-features", "feature_b")
		ensureOpenAIRemoteCompactionV2BetaFeature(h)
		require.Equal(t, []string{"feature_a,feature_b,remote_compaction_v2"}, h.Values("x-codex-beta-features"))
	})
}

// 对齐真实 Codex：该头是会话级常量，挂在 OAuth 的每个请求上，而不是只在
// 压缩回合出现（codex-rs build_model_client_beta_features_header）。
func TestApplyOpenAICodexBetaFeatures(t *testing.T) {
	oauthAccount := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKeyAccount := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	t.Run("oauth_plain_request_gets_default_codex_shape", func(t *testing.T) {
		c, _ := newTurnStateTestContext(t, 7, "sess-beta")
		h := http.Header{}
		applyOpenAICodexBetaFeatures(c, oauthAccount, h)
		require.Equal(t, "remote_compaction_v2", h.Get("x-codex-beta-features"),
			"OAuth 的普通请求也必须带会话级 beta 头")
	})

	t.Run("client_declared_header_preserved", func(t *testing.T) {
		c, _ := newTurnStateTestContext(t, 7, "sess-beta")
		h := http.Header{}
		h.Set("x-codex-beta-features", "some_other_feature")
		applyOpenAICodexBetaFeatures(c, oauthAccount, h)
		require.Equal(t, "some_other_feature", h.Get("x-codex-beta-features"),
			"客户端显式声明的能力集不得被网关改写（非空即视为用户已关闭 v2）")
	})

	t.Run("native_v2_forces_feature_even_when_client_trimmed_it", func(t *testing.T) {
		c, _ := newTurnStateTestContext(t, 7, "sess-beta")
		MarkOpenAINativeCompactionV2(c)
		h := http.Header{}
		h.Set("x-codex-beta-features", "some_other_feature")
		applyOpenAICodexBetaFeatures(c, oauthAccount, h)
		require.Contains(t, h.Get("x-codex-beta-features"), "remote_compaction_v2",
			"body 带 compaction_trigger 是实锤，必须确保 v2 在列")
		require.Contains(t, h.Get("x-codex-beta-features"), "some_other_feature")
	})

	t.Run("native_v2_applies_to_non_oauth_too", func(t *testing.T) {
		c, _ := newTurnStateTestContext(t, 7, "sess-beta")
		MarkOpenAINativeCompactionV2(c)
		h := http.Header{}
		applyOpenAICodexBetaFeatures(c, apiKeyAccount, h)
		require.Equal(t, "remote_compaction_v2", h.Get("x-codex-beta-features"))
	})

	t.Run("non_oauth_plain_request_untouched", func(t *testing.T) {
		c, _ := newTurnStateTestContext(t, 7, "sess-beta")
		h := http.Header{}
		applyOpenAICodexBetaFeatures(c, apiKeyAccount, h)
		require.Empty(t, h.Get("x-codex-beta-features"),
			"非 Codex 后端不做会话级注入")
	})

	t.Run("nil_account_plain_request_untouched", func(t *testing.T) {
		c, _ := newTurnStateTestContext(t, 7, "sess-beta")
		h := http.Header{}
		applyOpenAICodexBetaFeatures(c, nil, h)
		require.Empty(t, h.Get("x-codex-beta-features"))
	})
}

// WS 握手与 HTTP 出站必须给出同一份会话级 beta 头：真实 Codex 的
// build_websocket_headers 复用 build_responses_headers（client.rs），
// 两侧不一致还会让预热连接与实际请求落进不同的连接池兼容分桶。
func TestBuildOpenAIWSHeaders_CarriesSessionBetaFeatures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	decision := OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}

	build := func(t *testing.T, account *Account, clientBeta string) http.Header {
		t.Helper()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		if clientBeta != "" {
			c.Request.Header.Set("x-codex-beta-features", clientBeta)
		}
		headers, _, err := svc.buildOpenAIWSHeaders(
			context.Background(), c, account, "test-token", decision,
			true, "", "", "", "gpt-5.6-codex", "",
		)
		require.NoError(t, err)
		return headers
	}

	oauthAccount := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "test-account"},
	}

	headers := build(t, oauthAccount, "")
	require.Equal(t, "remote_compaction_v2", headers.Get("x-codex-beta-features"),
		"WS 握手也必须带会话级 beta 头")

	declared := build(t, oauthAccount, "some_other_feature")
	require.Equal(t, []string{"some_other_feature"}, declared.Values("x-codex-beta-features"),
		"客户端已声明时原样保留")

	apiKeyHeaders := build(t, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, "")
	require.Empty(t, apiKeyHeaders.Get("x-codex-beta-features"),
		"非 Codex 后端不注入")
}

func TestCodexTurnStateEgressRejectsChangedRouteOrScope(t *testing.T) {
	for _, path := range []string{"responses", "passthrough"} {
		for _, change := range []string{"same", "key", "base_url", "protocol", "proxy", "model", "tier", "oauth_refresh", "oauth_account"} {
			t.Run(path+"/"+change, func(t *testing.T) {
				svc := &OpenAIGatewayService{cfg: &config.Config{}}
				account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "original-fixture", "base_url": "https://first.invalid/v1"}}
				if change == "oauth_refresh" || change == "oauth_account" {
					account.Type = AccountTypeOAuth
					account.Credentials = map[string]any{"access_token": "original-access", "refresh_token": "original-refresh", "chatgpt_account_id": "original-account"}
				}
				build := func(c *gin.Context, body []byte) *http.Request {
					t.Helper()
					var req *http.Request
					var err error
					if path == "responses" {
						req, err = svc.buildUpstreamRequest(t.Context(), c, account, body, "fixture-token", true, "fixture-session", false)
					} else {
						req, err = svc.buildUpstreamRequestOpenAIPassthrough(t.Context(), c, account, body, "fixture-token")
					}
					require.NoError(t, err)
					return req
				}
				body := []byte(`{"model":"gpt-5.3-codex","input":[],"stream":true}`)
				first, _ := newTurnStateTestContext(t, 7, "same-session")
				build(first, body)
				upstream := http.Header{}
				upstream.Set(openAICodexTurnStateHeader, "original-state")
				svc.relayOpenAICodexTurnState(first, account, upstream)
				switch change {
				case "key":
					account.Credentials["api_key"] = "replacement-fixture"
				case "base_url":
					account.Credentials["base_url"] = "https://second.invalid/v1"
				case "protocol":
					account.Credentials["api_protocol"] = APIProtocolChatCompletions
				case "proxy":
					id := int64(5)
					account.ProxyID = &id
				case "model":
					body = []byte(`{"model":"gpt-5.4","input":[],"stream":true}`)
				case "tier":
					body = []byte(`{"model":"gpt-5.3-codex","service_tier":"priority","input":[],"stream":true}`)
				case "oauth_refresh":
					account.Credentials["access_token"] = "renewed-access"
					account.Credentials["refresh_token"] = "renewed-refresh"
				case "oauth_account":
					account.Credentials["chatgpt_account_id"] = "replacement-account"
				}
				next, _ := newTurnStateTestContext(t, 7, "same-session")
				next.Request.Header.Set(openAICodexTurnStateHeader, "original-state")
				req := build(next, body)
				if change == "same" || change == "oauth_refresh" {
					require.Equal(t, "original-state", req.Header.Get(openAICodexTurnStateHeader))
				} else {
					require.Empty(t, req.Header.Get(openAICodexTurnStateHeader))
				}
				upstream.Set(openAICodexTurnStateHeader, "recovered-state")
				svc.relayOpenAICodexTurnState(next, account, upstream)
				recovered, _ := newTurnStateTestContext(t, 7, "same-session")
				recovered.Request.Header.Set(openAICodexTurnStateHeader, "recovered-state")
				require.Equal(t, "recovered-state", build(recovered, body).Header.Get(openAICodexTurnStateHeader))
			})
		}
	}
}

func TestCodexTurnStateProvenanceUsesCapturedEgress(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "staged"}[staged], func(t *testing.T) {
			svc := &OpenAIGatewayService{}
			account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "original-fixture"}}
			scope := openAICompatCacheScope{Model: "gpt-5.3-codex", ServiceTier: "priority"}
			fingerprint := AccountRouteFingerprint(account)
			c, _ := newTurnStateTestContext(t, 7, "in-flight")
			svc.guardOpenAICodexTurnStateEcho(c, account, http.Header{}, scope)
			account.Credentials["api_key"] = "replacement-fixture"
			response := http.Header{}
			response.Set(openAICodexTurnStateHeader, "state-from-original-route")
			if staged {
				var headers http.Header
				stageOpenAICodexTurnState(&headers, response)
				_, exists := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey(c, "state-from-original-route"))
				require.False(t, exists)
				svc.noteStagedOpenAICodexTurnStateCommitted(c, account, headers)
			} else {
				svc.relayOpenAICodexTurnState(c, account, response)
			}
			raw, ok := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey(c, "state-from-original-route"))
			require.True(t, ok)
			origin, ok := raw.(openAICodexTurnStateOrigin)
			require.True(t, ok)
			require.Equal(t, fingerprint, origin.routeFingerprint)
			require.Equal(t, scope, origin.cacheScope)
			next, _ := newTurnStateTestContext(t, 7, "in-flight")
			header := http.Header{}
			header.Set(openAICodexTurnStateHeader, "state-from-original-route")
			svc.guardOpenAICodexTurnStateEcho(next, account, header, scope)
			require.Empty(t, header.Get(openAICodexTurnStateHeader))
			account.Credentials["api_key"] = "original-fixture"
			header.Set(openAICodexTurnStateHeader, "state-from-original-route")
			svc.guardOpenAICodexTurnStateEcho(next, account, header, scope)
			require.Equal(t, "state-from-original-route", header.Get(openAICodexTurnStateHeader))
		})
	}
}

func TestCodexTurnStateConcurrentOriginsDoNotAllowForeignScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newContext := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Request.Header.Set("session_id", "concurrent-session")
		c.Set("api_key", &APIKey{ID: 7})
		return c
	}
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	var wg sync.WaitGroup
	failures := make(chan struct{}, 400)
	for worker := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scope := openAICompatCacheScope{Model: "gpt-5.3-codex"}
			if worker%2 != 0 {
				scope.ServiceTier = "priority"
			}
			for range 100 {
				c := newContext()
				svc.guardOpenAICodexTurnStateEcho(c, account, http.Header{}, scope)
				response := http.Header{}
				response.Set(openAICodexTurnStateHeader, "fixture-state")
				svc.relayOpenAICodexTurnState(c, account, response)
				next := newContext()
				echo := response.Clone()
				svc.guardOpenAICodexTurnStateEcho(next, account, echo, openAICompatCacheScope{Model: "foreign-model"})
				if echo.Get(openAICodexTurnStateHeader) != "" {
					failures <- struct{}{}
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	require.Empty(t, failures)
}

func TestCodexTurnStateEchoFollowsSpecificCommittedBlob(t *testing.T) {
	svc := &OpenAIGatewayService{}
	first, _ := newTurnStateTestContext(t, 7, "parallel-session")
	second, _ := newTurnStateTestContext(t, 7, "parallel-session")
	a := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	b := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	scope := openAICompatCacheScope{Model: "gpt-5.3-codex"}
	for _, attempt := range []struct {
		c       *gin.Context
		account *Account
		state   string
	}{{first, a, "state-A"}, {second, b, "state-B"}} {
		svc.guardOpenAICodexTurnStateEcho(attempt.c, attempt.account, http.Header{}, scope)
		h := http.Header{}
		h.Set(openAICodexTurnStateHeader, attempt.state)
		svc.relayOpenAICodexTurnState(attempt.c, attempt.account, h)
	}
	for _, tc := range []struct {
		state   string
		account *Account
		want    string
	}{{"state-A", a, "state-A"}, {"state-A", b, ""}, {"state-B", b, "state-B"}, {"state-B", a, ""}} {
		t.Run(tc.state+"/"+tc.account.Name+strconv.FormatInt(tc.account.ID, 10), func(t *testing.T) {
			c, _ := newTurnStateTestContext(t, 7, "parallel-session")
			h := http.Header{}
			h.Set(openAICodexTurnStateHeader, tc.state)
			svc.guardOpenAICodexTurnStateEcho(c, tc.account, h, scope)
			require.Equal(t, tc.want, h.Get(openAICodexTurnStateHeader))
		})
	}
}

func TestCodexTurnStateClientWSEchoUsesEgressGuard(t *testing.T) {
	for _, change := range []string{"same", "account", "key", "endpoint", "model", "tier"} {
		t.Run(change, func(t *testing.T) {
			svc := &OpenAIGatewayService{}
			a := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-key", "base_url": "https://first.invalid"}}
			c, _ := newTurnStateTestContext(t, 7, "ws-client")
			scope := openAICompatCacheScope{Model: "gpt-5.3-codex"}
			svc.guardOpenAICodexTurnStateEcho(c, a, http.Header{}, scope)
			minted := http.Header{}
			minted.Set(openAICodexTurnStateHeader, "http-minted")
			svc.relayOpenAICodexTurnState(c, a, minted)
			switch change {
			case "account":
				a.ID++
			case "key":
				a.Credentials["api_key"] = "new-fixture-key"
			case "endpoint":
				a.Credentials["base_url"] = "https://second.invalid"
			case "model":
				scope.Model = "gpt-5.4"
			case "tier":
				scope.ServiceTier = "priority"
			}
			next, _ := newTurnStateTestContext(t, 7, "ws-client")
			headers, _, err := svc.buildOpenAIWSHeaders(t.Context(), next, a, "fixture-token", OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}, false, "http-minted", "", "", scope.Model, scope.ServiceTier)
			require.NoError(t, err)
			if change == "same" {
				require.Equal(t, "http-minted", headers.Get(openAICodexTurnStateHeader))
			} else {
				require.Empty(t, headers.Get(openAICodexTurnStateHeader))
			}
		})
	}
}

type codexTurnStateSharedTestCache struct {
	stubGatewayCache
	mu       sync.Mutex
	origins  map[string]OpenAIWSContinuationScope
	readErr  error
	writeErr error
}

func (s *codexTurnStateSharedTestCache) SetOpenAICodexTurnStateOrigin(_ context.Context, key string, scope OpenAIWSContinuationScope, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	if s.origins == nil {
		s.origins = make(map[string]OpenAIWSContinuationScope)
	}
	if previous, found := s.origins[key]; found && previous != scope {
		return errors.New("fixture provenance conflict")
	}
	s.origins[key] = scope
	return nil
}

func (s *codexTurnStateSharedTestCache) GetOpenAICodexTurnStateOrigin(_ context.Context, key string) (OpenAIWSContinuationScope, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	origin, found := s.origins[key]
	return origin, found, s.readErr
}

func TestCodexTurnStateSharedOriginsAndFailureRecovery(t *testing.T) {
	store := &codexTurnStateSharedTestCache{}
	writer := &OpenAIGatewayService{cache: store}
	reader := &OpenAIGatewayService{cache: store}
	a := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	b := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	scope := openAICompatCacheScope{Model: "gpt-5.3-codex"}
	c, _ := newTurnStateTestContext(t, 7, "private-session")
	writer.guardOpenAICodexTurnStateEcho(c, a, http.Header{}, scope)
	minted := http.Header{}
	minted.Set(openAICodexTurnStateHeader, "private-state")
	writer.relayOpenAICodexTurnState(c, a, minted)
	require.Equal(t, "private-state", c.Writer.Header().Get(openAICodexTurnStateHeader))
	_, local := writer.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey(c, "private-state"))
	require.False(t, local, "shared mode has no competing local authority")
	for key := range store.origins {
		require.Len(t, key, 64)
		require.NotContains(t, key, "private-session")
		require.NotContains(t, key, "private-state")
	}
	check := func(account *Account, want string) {
		t.Helper()
		next, _ := newTurnStateTestContext(t, 7, "private-session")
		h := minted.Clone()
		reader.guardOpenAICodexTurnStateEcho(next, account, h, scope)
		require.Equal(t, want, h.Get(openAICodexTurnStateHeader))
	}
	check(a, "private-state")
	check(b, "")
	store.readErr = errors.New("fixture store offline")
	check(a, "")
	store.readErr = nil
	check(a, "private-state")
	for _, staged := range []bool{false, true} {
		failed, _ := newTurnStateTestContext(t, 7, "write-failure")
		writer.guardOpenAICodexTurnStateEcho(failed, a, http.Header{}, scope)
		store.writeErr = errors.New("fixture write rejected")
		if staged {
			var headers http.Header
			stageOpenAICodexTurnState(&headers, minted)
			failed.Writer.Header().Set(openAICodexTurnStateHeader, "private-state")
			writer.noteStagedOpenAICodexTurnStateCommitted(failed, a, headers)
			require.Empty(t, headers.Get(openAICodexTurnStateHeader))
		} else {
			writer.relayOpenAICodexTurnState(failed, a, minted)
		}
		require.Empty(t, failed.Writer.Header().Get(openAICodexTurnStateHeader))
		store.writeErr = nil
		writer.relayOpenAICodexTurnState(failed, a, minted)
		require.Equal(t, "private-state", failed.Writer.Header().Get(openAICodexTurnStateHeader))
	}
}

func TestCodexTurnStateSIWCUsesNormalizedScopeAndCurrentAccount(t *testing.T) {
	svc := &OpenAIGatewayService{}
	a := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "fixture-workspace"}, Extra: map[string]any{OpenAISIWCEnabledKey: true}}
	b := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	c, _ := newTurnStateTestContext(t, 7, "siwc-attempt")
	scope := openAICompatCacheScope{Model: "gpt-5.4", ServiceTier: "priority"}
	svc.guardOpenAICodexTurnStateEcho(c, b, http.Header{}, openAICompatCacheScope{Model: "previous-attempt"})
	c.Request.Header.Set(openAICodexTurnStateHeader, "client-private-state")
	req, err := svc.buildUpstreamRequestOpenAIPassthrough(t.Context(), c, a, []byte(`{"model":"gpt-5.4","service_tier":"priority","input":[]}`), "fixture-grant")
	require.NoError(t, err)
	require.Empty(t, req.Header.Get(openAICodexTurnStateHeader), "SIWC must retain its existing header allowlist")
	a.Credentials["chatgpt_account_id"] = "changed-while-in-flight"
	minted := http.Header{}
	minted.Set(openAICodexTurnStateHeader, "siwc-minted")
	svc.relayOpenAICodexTurnState(c, a, minted)
	raw, found := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey(c, "siwc-minted"))
	require.True(t, found)
	origin, ok := raw.(openAICodexTurnStateOrigin)
	require.True(t, ok)
	require.Equal(t, a.ID, origin.accountID)
	require.Equal(t, scope, origin.cacheScope)
	require.NotEqual(t, AccountRouteFingerprint(a), origin.routeFingerprint, "in-flight changes cannot relabel the response")
}

func TestCodexTurnStatePassthroughAbandonedAttemptDoesNotClaimBlob(t *testing.T) {
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Codex-Turn-State": {"reused-provider-state"}},
			Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"temporarily unavailable\"}}}\n\n"))},
		{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}, "X-Codex-Turn-State": {"reused-provider-state"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"resp_recovered","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))},
	}}
	svc := openAIClientToolsTestService(upstream)
	a := &Account{ID: 42, Status: StatusActive, Schedulable: true, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-key"}}
	c, _ := newTurnStateTestContext(t, 7, "abandoned-passthrough")
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":"hello"}`)
	_, err := svc.forwardOpenAIPassthrough(t.Context(), c, a, body, body, "gpt-5.4", false, nil, true, time.Now())
	require.Error(t, err)
	require.False(t, c.Writer.Written(), "the failed attempt did not send a response")
	_, found := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey(c, "reused-provider-state"))
	require.False(t, found, "an abandoned passthrough attempt must not claim the opaque blob")
	a.ID = 43
	body = []byte(`{"model":"gpt-5.4","stream":false,"input":"hello"}`)
	_, err = svc.forwardOpenAIPassthrough(t.Context(), c, a, body, body, "gpt-5.4", false, nil, false, time.Now())
	require.NoError(t, err)
	require.Equal(t, "reused-provider-state", c.Writer.Header().Get(openAICodexTurnStateHeader))
}

type codexTurnStateCommitWriter struct {
	gin.ResponseWriter
	onWrite func()
}

func (w *codexTurnStateCommitWriter) Write(data []byte) (int, error) {
	w.onWrite()
	return w.ResponseWriter.Write(data)
}

func (w *codexTurnStateCommitWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

func TestCodexTurnStatePassthroughCommitsOnlyWithActualOutput(t *testing.T) {
	for _, heartbeat := range []bool{false, true} {
		t.Run(strconv.FormatBool(heartbeat), func(t *testing.T) {
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			if heartbeat {
				svc.cfg.Gateway.StreamKeepaliveInterval = 1
			}
			a := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			c, _ := newTurnStateTestContext(t, 7, "stream-commit")
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			svc.guardOpenAICodexTurnStateEcho(c, a, http.Header{}, openAICompatCacheScope{Model: "gpt-5.4"})
			firstHeader := make(chan string, 1)
			firstWrite := make(chan struct{})
			var once sync.Once
			c.Writer = &codexTurnStateCommitWriter{ResponseWriter: c.Writer, onWrite: func() {
				once.Do(func() {
					firstHeader <- c.Writer.Header().Get(openAICodexTurnStateHeader)
					close(firstWrite)
				})
			}}
			reader, producer := io.Pipe()
			t.Cleanup(func() { _ = reader.Close(); _ = producer.Close() })
			producerDone := make(chan error, 1)
			go func() {
				if heartbeat {
					select {
					case <-firstWrite:
					case <-ctx.Done():
						_ = producer.CloseWithError(ctx.Err())
						producerDone <- ctx.Err()
						return
					}
				}
				_, err := io.WriteString(producer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_commit\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
				_ = producer.Close()
				producerDone <- err
			}()
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Codex-Turn-State": {"stream-minted"}}, Body: reader}
			_, err := svc.handleStreamingResponsePassthrough(ctx, resp, c, a, time.Now(), "gpt-5.4", "gpt-5.4")
			require.NoError(t, err)
			require.NoError(t, <-producerDone)
			if heartbeat {
				require.Empty(t, <-firstHeader, "a keepalive must not expose account-specific state")
			} else {
				require.Equal(t, "stream-minted", <-firstHeader)
			}
			_, found := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey(c, "stream-minted"))
			require.Equal(t, !heartbeat, found, "only state actually sent in response headers may claim provenance")
		})
	}
}

func TestCodexTurnStateWSAbandonedAttemptDoesNotClaimBlob(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run("shared_"+strconv.FormatBool(shared)+"/stream_"+strconv.FormatBool(stream), func(t *testing.T) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := upgrader.Upgrade(w, r, http.Header{"X-Codex-Turn-State": {"reused-ws-state"}})
					if err != nil {
						t.Errorf("upgrade: %v", err)
						return
					}
					defer func() { _ = conn.Close() }()
					var payload map[string]any
					if err := conn.ReadJSON(&payload); err != nil {
						t.Errorf("read turn: %v", err)
						return
					}
					message := `{"type":"response.completed","response":{"id":"resp_recovered","model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`
					if r.Header.Get("Authorization") == "Bearer failing-fixture" {
						message = `{"type":"error","error":{"code":"server_is_overloaded","message":"temporarily unavailable"}}`
					}
					if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
						t.Errorf("write response: %v", err)
					}
				}))
				t.Cleanup(upstream.Close)
				cfg := newOpenAIWSExecutionScopeTestConfig()
				cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 0
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, toolCorrector: NewCodexToolCorrector()}
				if shared {
					svc.cache = &codexTurnStateSharedTestCache{}
				}
				t.Cleanup(svc.CloseOpenAIWSPool)
				account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
					Status: StatusActive, Schedulable: true, Concurrency: 1,
					Credentials: map[string]any{"api_key": "failing-fixture", "base_url": upstream.URL}}
				c, _ := newTurnStateTestContext(t, 7, "abandoned-ws")
				forward := func(token string) error {
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					_, err := svc.forwardOpenAIWSV2(ctx, c, account,
						map[string]any{"model": "gpt-5.4", "stream": stream, "input": "hello"}, "", "", token,
						OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}, false, stream,
						"gpt-5.4", "gpt-5.4", time.Now(), 1, "", nil)
					return err
				}
				require.Error(t, forward("failing-fixture"))
				require.False(t, c.Writer.Written())
				key := openAICodexTurnStateKey(c, "reused-ws-state")
				if store := svc.openAICodexTurnStateStore(); store != nil {
					_, found, err := store.GetOpenAICodexTurnStateOrigin(t.Context(), key)
					require.NoError(t, err)
					require.False(t, found, "an abandoned WS handshake did not reach the client")
				} else {
					_, found := svc.openaiCodexTurnStateOrigins.Load(key)
					require.False(t, found, "an abandoned WS handshake did not reach the client")
				}
				require.Empty(t, c.Writer.Header().Get(openAICodexTurnStateHeader))
				account.ID = 43
				account.Credentials["api_key"] = "recovered-fixture"
				require.NoError(t, forward("recovered-fixture"))
				require.True(t, c.Writer.Written())
				require.Equal(t, "reused-ws-state", c.Writer.Header().Get(openAICodexTurnStateHeader))
			})
		}
	}
}
