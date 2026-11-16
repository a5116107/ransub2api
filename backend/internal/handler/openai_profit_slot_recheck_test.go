//go:build unit

package handler

// 槽位终检与生图跳门回归（handler 半程）：
//   - 槽位获取成功后的利润终检：越线账号释放槽位并要求调用方排除重选，
//     不写响应、不绑定粘连；
//   - openAIResponsesRequiredCapability 的请求能力映射覆盖生图与原生远程压缩。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

type profitCountingConcurrencyCache struct {
	fakeConcurrencyCache
	accountReleases atomic.Int64
}

func (c *profitCountingConcurrencyCache) ReleaseAccountSlot(context.Context, int64, string) error {
	c.accountReleases.Add(1)
	return nil
}

func profitSlotTestAccount(id int64, rate float64) *service.Account {
	now := time.Now()
	return &service.Account{
		ID:             id,
		Platform:       service.PlatformOpenAI,
		Type:           service.AccountTypeAPIKey,
		Status:         service.StatusActive,
		Schedulable:    true,
		Concurrency:    2,
		RateMultiplier: &rate,
		Extra: map[string]any{
			"upstream_billing_probe": map[string]any{
				"status":          service.UpstreamBillingProbeStatusOK,
				"received_at":     now.Add(-time.Minute),
				"fresh_until":     now.Add(30 * time.Minute),
				"last_attempt_at": now.Add(-time.Minute),
				"data": map[string]any{
					"billing_scope":            "token",
					"resolved_rate_multiplier": rate,
					"peak_rate_enabled":        false,
				},
			},
		},
	}
}

func profitSlotTestContext(t *testing.T, gw *service.OpenAIGatewayService, groupID int64, suppress bool) context.Context {
	t.Helper()
	group := &service.Group{
		ID:                   groupID,
		Platform:             service.PlatformOpenAI,
		Status:               service.StatusActive,
		Hydrated:             true,
		RateMultiplier:       1.0,
		SubscriptionType:     service.SubscriptionTypeStandard,
		ProfitControlEnabled: true,
		ProfitMinMargin:      0.5,
	}
	base := context.WithValue(context.Background(), ctxkey.Group, group)
	if suppress {
		base = service.WithOpenAIProfitControlSuppressed(base)
	}
	ctx, pricingAt := gw.WithOpenAIRequestPricingContext(base, &groupID)
	require.False(t, pricingAt.IsZero())
	return ctx
}

func profitBillingScopeTestAccount(id int64, model, protocol string, covered bool) *service.Account {
	account := profitSlotTestAccount(id, 0.3)
	account.Credentials = map[string]any{"base_url": "https://relay.example", "api_key": "fixture-key"}
	account.Extra["openai_passthrough"] = true
	account.Extra[service.UpstreamBillingProbeEnabledExtraKey] = true
	account.Extra[service.UpstreamBillingRateSyncEnabledExtraKey] = true
	snapshot := account.Extra[service.UpstreamBillingProbeExtraKey].(map[string]any)
	data := snapshot["data"].(map[string]any)
	at := time.Now().UTC().Add(-time.Minute)
	data["observed_at"] = at.Format(time.RFC3339Nano)
	data["rate_applicability"] = map[string]any{
		"schema_version": 1, "source_version": "fixture-1", "observed_at": data["observed_at"], "expires_at": at.Add(10 * time.Minute).Format(time.RFC3339Nano),
		"entries": []map[string]any{{"model": model, "protocol": protocol, "service_tier": "priority", "reasoning_effort": "unspecified", "context": "all", "group_rate_applies": covered}},
	}
	return account
}

func TestAcquireResponsesAccountSlotBillingScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{"responses_http", "chat_http", "messages_chat"} {
		for _, acquired := range []bool{false, true} {
			for _, covered := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/acquired=%v/covered=%v", protocol, acquired, covered), func(t *testing.T) {
					gw := &service.OpenAIGatewayService{}
					cache := &profitCountingConcurrencyCache{}
					h := &OpenAIGatewayHandler{gatewayService: gw,
						concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatClaude, 0)}
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					groupID := int64(50)
					path := "/v1/responses"
					body := `{"model":"gpt-fixture","service_tier":"priority"}`
					wireProtocol := protocol
					if protocol == "chat_http" {
						path = "/v1/chat/completions"
					}
					if protocol == "messages_chat" {
						path, wireProtocol = "/v1/messages", "chat_http"
						body = `{"model":"gpt-fixture","messages":[{"role":"user","content":"private prompt"}]}`
					}
					c.Request = httptest.NewRequest(http.MethodPost, path, nil)
					ctx := profitSlotTestContext(t, gw, groupID, false)
					ctx = gw.WithUpstreamBillingCandidateRequest(ctx, c.Request, []byte(body))
					c.Request = c.Request.WithContext(ctx)
					account := profitBillingScopeTestAccount(91, "gpt-fixture", wireProtocol, covered)
					if wireProtocol == "chat_http" {
						account.Extra["openai_responses_mode"] = "force_chat_completions"
					}
					if protocol == "messages_chat" {
						data := account.Extra[service.UpstreamBillingProbeExtraKey].(map[string]any)["data"].(map[string]any)
						entry := data["rate_applicability"].(map[string]any)["entries"].([]map[string]any)[0]
						entry["service_tier"], entry["reasoning_effort"] = "unspecified", "medium"
					}
					selection := &service.AccountSelectionResult{Account: account, Acquired: acquired,
						WaitPlan: &service.AccountWaitPlan{AccountID: account.ID, MaxConcurrency: 2, Timeout: time.Second, MaxWaiting: 2}}
					if acquired {
						selection.ReleaseFunc = func() { cache.accountReleases.Add(1) }
					}
					streamStarted := false
					release, result := h.acquireResponsesAccountSlot(c, &groupID, "", selection, false, &streamStarted, zap.NewNop())
					if covered {
						require.Equal(t, openAISlotAcquireOK, result)
						require.NotNil(t, release)
						require.Zero(t, cache.accountReleases.Load())
						release()
					} else {
						require.Equal(t, openAISlotAcquireProfitVetoed, result)
						require.Nil(t, release)
					}
					require.Equal(t, int64(1), cache.accountReleases.Load())
					require.Zero(t, w.Body.Len())
					require.False(t, streamStarted)
				})
			}
		}
	}
}

func TestBillingCandidateHTTPHandlersFilterBeforeSlot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, ingress := range []string{"chat", "messages", "messages_dispatch", "messages_account_mapping", "messages_responses", "messages_responses_dispatch", "messages_responses_account_mapping"} {
		for _, covered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/available=%v", ingress, covered), func(t *testing.T) {
				repo := &initialAdmissionRepo{}
				upstream := &apiKeyPolicyForwardingUpstream{}
				h := newInitialAdmissionHandler(t, repo, upstream)
				t.Cleanup(h.gatewayService.StopOpenAICodexTicketHarvester)
				t.Cleanup(h.gatewayService.CloseOpenAIWSPool)
				h.maxAccountSwitches = 0
				groupID := int64(3131)
				requestedModel, wireModel := "gpt-fixture", "wire-fixture"
				dispatch := strings.HasSuffix(ingress, "_dispatch") || strings.HasSuffix(ingress, "_account_mapping")
				responses := strings.Contains(ingress, "responses")
				wireProtocol, wirePath := "chat_http", "/v1/chat/completions"
				if responses {
					wireProtocol, wirePath = "responses_http", "/v1/responses"
				}
				if dispatch {
					requestedModel = "claude-sonnet-fixture"
					if strings.HasSuffix(ingress, "_dispatch") {
						wireModel = "dispatch-fixture"
					}
				}
				repo.accounts = nil
				for id := int64(1); id <= 2; id++ {
					account := profitBillingScopeTestAccount(id, wireModel, wireProtocol, covered && id == 2)
					account.Priority, account.GroupIDs = int(id), []int64{groupID}
					if !responses {
						account.Extra["openai_responses_mode"] = "force_chat_completions"
					}
					mapping := map[string]any{requestedModel: wireModel}
					if dispatch {
						// The scheduling model is only a supported routing alias;
						// forwarding resolves the original model with a fallback.
						mapping["dispatch-fixture"] = "scheduling-only-fixture"
						if strings.HasSuffix(ingress, "_dispatch") {
							delete(mapping, requestedModel)
						}
					}
					account.Credentials["model_mapping"] = mapping
					if ingress != "chat" {
						data := account.Extra[service.UpstreamBillingProbeExtraKey].(map[string]any)["data"].(map[string]any)
						entry := data["rate_applicability"].(map[string]any)["entries"].([]map[string]any)[0]
						entry["service_tier"], entry["reasoning_effort"] = "unspecified", "medium"
						if responses {
							data["rate_applicability"].(map[string]any)["schema_version"] = 3
							entry["reasoning_summary"], entry["include_encrypted_content"], entry["text_verbosity"] = "auto", true, "medium"
						}
					}
					repo.accounts = append(repo.accounts, *account)
				}
				ctx := profitSlotTestContext(t, h.gatewayService, groupID, false)
				group := ctx.Value(ctxkey.Group).(*service.Group)
				group.AllowMessagesDispatch = ingress != "chat"
				if dispatch {
					group.MessagesDispatchModelConfig.SonnetMappedModel = "dispatch-fixture"
				}
				router := gin.New()
				router.Use(func(c *gin.Context) {
					c.Request = c.Request.WithContext(ctx)
					c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 17, GroupID: &groupID, Group: group,
						User: &service.User{ID: 19, Status: service.StatusActive}, AllowLongContext: true,
						ModelMapping: map[string]string{"public-fixture": requestedModel}})
					c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 19})
					c.Next()
				})
				router.Use(middleware2.APIKeyModelPolicy())
				router.POST("/v1/chat/completions", h.ChatCompletions)
				router.POST("/v1/messages", h.Messages)
				path, body := "/v1/chat/completions", `{"model":"public-fixture","messages":[{"role":"user","content":"private prompt"}],"service_tier":"priority"}`
				if ingress != "chat" {
					path, body = "/v1/messages", `{"model":"public-fixture","messages":[{"role":"user","content":"private prompt"}],"max_tokens":1024}`
				}
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				upstream.mu.Lock()
				calls := len(upstream.bodies)
				upstream.mu.Unlock()
				if covered {
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					require.Contains(t, rec.Body.String(), "done")
					require.Equal(t, 1, calls)
					require.Equal(t, wirePath, upstream.paths[0])
					require.Equal(t, wireModel, gjson.GetBytes(upstream.bodies[0], "model").String())
					require.NotEmpty(t, repo.initialReads)
					for _, id := range repo.initialReads {
						require.EqualValues(t, 2, id, "excluded scope must not reach slot/forward admission")
					}
				} else {
					require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
					require.Zero(t, calls)
					require.Empty(t, repo.initialReads)
				}
			})
		}
	}
}

func TestAcquireResponsesAccountSlotProfitRecheck(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gw := &service.OpenAIGatewayService{}
	groupID := int64(50)

	newHandler := func(cache *profitCountingConcurrencyCache) *OpenAIGatewayHandler {
		return &OpenAIGatewayHandler{
			gatewayService:    gw,
			concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatClaude, 0),
		}
	}
	newSelection := func(account *service.Account) *service.AccountSelectionResult {
		return &service.AccountSelectionResult{
			Account:  account,
			Acquired: false,
			WaitPlan: &service.AccountWaitPlan{AccountID: account.ID, MaxConcurrency: 2, Timeout: time.Second, MaxWaiting: 2},
		}
	}

	for _, acquired := range []bool{false, true} {
		t.Run(fmt.Sprintf("sale rate change rejects slot acquired=%v", acquired), func(t *testing.T) {
			cache := &profitCountingConcurrencyCache{}
			h := newHandler(cache)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(profitSlotTestContext(t, gw, groupID, false))
			account := profitSlotTestAccount(5, 0.3)
			markup := 2.0
			account.GroupRateMultiplier = &markup
			selection := newSelection(account)
			if acquired {
				selection.Acquired = true
				selection.ReleaseFunc = func() { cache.accountReleases.Add(1) }
			}
			streamStarted := false
			release, result := h.acquireResponsesAccountSlot(c, &groupID, "", selection, false, &streamStarted, zap.NewNop())
			require.Equal(t, openAISlotAcquireProfitVetoed, result)
			require.Nil(t, release)
			require.Equal(t, int64(1), cache.accountReleases.Load())
			require.Zero(t, w.Body.Len())
			require.False(t, streamStarted)
		})
		for _, healthy := range []bool{false, true} {
			name := "synced wait slot rejects failed evidence"
			if acquired {
				name = "synced acquired slot rejects failed evidence"
			}
			if healthy {
				name += " after recovery admits"
			}
			t.Run(name, func(t *testing.T) {
				cache := &profitCountingConcurrencyCache{}
				h := newHandler(cache)
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(profitSlotTestContext(t, gw, groupID, false))
				account := profitSlotTestAccount(4, 0.3)
				account.Extra[service.UpstreamBillingProbeEnabledExtraKey] = true
				account.Extra[service.UpstreamBillingRateSyncEnabledExtraKey] = true
				snapshot, ok := account.Extra[service.UpstreamBillingProbeExtraKey].(map[string]any)
				require.True(t, ok)
				if !healthy {
					snapshot["status"] = service.UpstreamBillingProbeStatusFailed
				}
				selection := newSelection(account)
				if acquired {
					selection.Acquired = true
					selection.ReleaseFunc = func() { cache.accountReleases.Add(1) }
				}
				streamStarted := false
				release, result := h.acquireResponsesAccountSlot(c, &groupID, "", selection, false, &streamStarted, zap.NewNop())
				if healthy {
					require.Equal(t, openAISlotAcquireOK, result)
					require.NotNil(t, release)
					require.Zero(t, cache.accountReleases.Load())
					release()
				} else {
					require.Equal(t, openAISlotAcquireProfitVetoed, result)
					require.Nil(t, release)
				}
				require.Equal(t, int64(1), cache.accountReleases.Load())
				require.Zero(t, w.Body.Len())
				require.False(t, streamStarted)
			})
		}
	}

	t.Run("veto releases slot and requests reschedule without writing response", func(t *testing.T) {
		cache := &profitCountingConcurrencyCache{}
		h := newHandler(cache)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(profitSlotTestContext(t, gw, groupID, false))
		streamStarted := false

		release, result := h.acquireResponsesAccountSlot(c, &groupID, "", newSelection(profitSlotTestAccount(1, 0.8)), false, &streamStarted, zap.NewNop())
		require.Equal(t, openAISlotAcquireProfitVetoed, result)
		require.Nil(t, release)
		require.Zero(t, w.Body.Len(), "利润终检否决不得写出任何响应")
		require.Equal(t, int64(1), cache.accountReleases.Load(), "否决后必须立即释放已获取的槽位")
	})

	t.Run("qualifying account acquires normally", func(t *testing.T) {
		cache := &profitCountingConcurrencyCache{}
		h := newHandler(cache)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(profitSlotTestContext(t, gw, groupID, false))
		streamStarted := false

		release, result := h.acquireResponsesAccountSlot(c, &groupID, "", newSelection(profitSlotTestAccount(2, 0.3)), false, &streamStarted, zap.NewNop())
		require.Equal(t, openAISlotAcquireOK, result)
		require.NotNil(t, release)
		release()
	})

	t.Run("image intent suppression keeps official behavior", func(t *testing.T) {
		cache := &profitCountingConcurrencyCache{}
		h := newHandler(cache)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(profitSlotTestContext(t, gw, groupID, true))
		streamStarted := false

		release, result := h.acquireResponsesAccountSlot(c, &groupID, "", newSelection(profitSlotTestAccount(3, 0.8)), false, &streamStarted, zap.NewNop())
		require.Equal(t, openAISlotAcquireOK, result, "生图意图跳门：过贵账号照常获取（图片边界不装门）")
		require.NotNil(t, release)
		release()
	})
}

func TestOpenAIResponsesRequiredCapabilityForRequest(t *testing.T) {
	require.Equal(t, service.OpenAIEndpointCapabilityResponses, openAIResponsesRequiredCapability(true, service.PlatformOpenAI))
	require.Equal(t, service.OpenAIEndpointCapabilityChatCompletions, openAIResponsesRequiredCapability(false, service.PlatformOpenAI))
	require.Equal(t, service.OpenAIEndpointCapabilityChatCompletions, openAIResponsesRequiredCapability(true, service.PlatformGrok))
	require.Equal(t, service.OpenAIEndpointCapabilityResponses, openAIResponsesRequiredCapabilityForRequest(false, true, service.PlatformOpenAI))
	require.Equal(t, service.OpenAIEndpointCapabilityChatCompletions, openAIResponsesRequiredCapabilityForRequest(false, true, service.PlatformGrok))
}
