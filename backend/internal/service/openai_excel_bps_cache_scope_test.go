package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func excelBPSCacheScopeToken(t *testing.T, subject, account, nonce string, extra ...map[string]any) string {
	t.Helper()
	values := map[string]any{
		"iss": "https://auth.fixture", "sub": subject, "nonce": nonce,
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account},
	}
	for _, claims := range extra {
		for key, value := range claims {
			values[key] = value
		}
	}
	claims, err := json.Marshal(values)
	require.NoError(t, err)
	return "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
}

func TestExcelBPSCacheScopeUsesEffectiveRouteAndModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name                  string
		firstModel, nextModel string
		firstGroup, nextGroup int64
		nextTier              string
		same                  bool
		configure             func(first, next *Account)
	}{
		{name: "unchanged", same: true},
		{name: "effective model whitespace", nextModel: " gpt-5.6-sol ", same: true},
		{name: "effective model changes", nextModel: "gpt-6-astra"},
		{name: "downstream product changes", nextGroup: 11},
		{name: "filtered fast tier remains same", nextTier: "priority", same: true},
		{name: "normal OAuth refresh", same: true, configure: func(first, next *Account) {
			first.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "before")
			next.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "after")
		}},
		{name: "single and array audience refresh", same: true, configure: func(first, next *Account) {
			first.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "before", map[string]any{"aud": "bps-client"})
			next.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "after", map[string]any{"aud": []string{"bps-client"}})
		}},
		{name: "audience order is immaterial", same: true, configure: func(first, next *Account) {
			first.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "before", map[string]any{"aud": []string{"bps", "shared"}})
			next.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "after", map[string]any{"aud": []string{"shared", "bps"}})
		}},
		{name: "token audience changes", configure: func(first, next *Account) {
			first.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "same", map[string]any{"aud": []string{"bps-one"}})
			next.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "same", map[string]any{"aud": []string{"bps-two"}})
		}},
		{name: "token client changes", configure: func(first, next *Account) {
			first.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "same", map[string]any{"client_id": "client-one"})
			next.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "same", map[string]any{"client_id": "client-two"})
		}},
		{name: "token authorized party changes", configure: func(first, next *Account) {
			first.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "same", map[string]any{"azp": "client-one"})
			next.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "same", map[string]any{"azp": "client-two"})
		}},
		{name: "OAuth subject spaces remain distinct", configure: func(first, next *Account) {
			first.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "same")
			next.Credentials["access_token"] = excelBPSCacheScopeToken(t, " actor-one ", "test-account", "same")
		}},
		{name: "OAuth subject changes", configure: func(first, next *Account) {
			first.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "same")
			next.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-two", "test-account", "same")
		}},
		{name: "OAuth token account changes", configure: func(first, next *Account) {
			first.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "token-account-one", "same")
			next.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "token-account-two", "same")
		}},
		{name: "opaque credential changes", configure: func(_, next *Account) {
			next.Credentials["access_token"] = "other-opaque-fixture"
		}},
		{name: "upstream account changes", configure: func(_, next *Account) {
			next.Credentials["chatgpt_account_id"] = "different-account"
		}},
		{name: "proxy endpoint changes", configure: func(_, next *Account) {
			next.Proxy = &Proxy{Protocol: "http", Host: "127.0.0.1", Port: 19012}
		}},
		{name: "aliases map to same actual model", firstModel: "alias-one", nextModel: "alias-two", same: true, configure: func(first, next *Account) {
			for _, account := range []*Account{first, next} {
				account.Credentials["model_mapping"] = map[string]any{"alias-one": "gpt-5.6-sol", "alias-two": "gpt-5.6-sol"}
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first, next := excelAccount(), excelAccount()
			first.GroupIDs, next.GroupIDs = []int64{10, 11}, []int64{10, 11}
			if tc.configure != nil {
				tc.configure(first, next)
			}
			if tc.firstModel == "" {
				tc.firstModel = "gpt-5.6-sol"
			}
			if tc.nextModel == "" {
				tc.nextModel = tc.firstModel
			}
			if tc.firstGroup == 0 {
				tc.firstGroup = 10
			}
			if tc.nextGroup == 0 {
				tc.nextGroup = tc.firstGroup
			}
			upstream := &httpUpstreamRecorder{}
			svc := openAIClientToolsTestService(upstream)
			forward := func(account *Account, model, tier string, group int64) (string, string) {
				t.Helper()
				upstream.resp = &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(excelBPSToolPolicyResponse))}
				body, err := json.Marshal(map[string]any{"model": model, "stream": false, "input": "fixture", "service_tier": tier})
				require.NoError(t, err)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				c.Request.Header.Set("session_id", "bps-cache-"+t.Name())
				c.Set("api_key", &APIKey{ID: 42, UserID: 7, GroupID: &group})
				_, err = svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.False(t, gjson.GetBytes(upstream.lastBody, "service_tier").Exists())
				cache := gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String()
				task := gjson.GetBytes(upstream.lastBody, "metadata.task_id").String()
				require.NotEmpty(t, cache)
				require.NotEmpty(t, task)
				return cache, task
			}
			firstCache, firstTask := forward(first, tc.firstModel, "", tc.firstGroup)
			nextCache, nextTask := forward(next, tc.nextModel, tc.nextTier, tc.nextGroup)
			if tc.same {
				require.Equal(t, firstCache, nextCache)
				require.Equal(t, firstTask, nextTask)
			} else {
				require.NotEqual(t, firstCache, nextCache)
				require.NotEqual(t, firstTask, nextTask)
			}
		})
	}
}

func TestExcelBPSAccountIDSupportsSingleAudienceAccessToken(t *testing.T) {
	account := excelAccount()
	delete(account.Credentials, "chatgpt_account_id")
	token := excelBPSCacheScopeToken(t, "actor", "token-account", "fixture", map[string]any{"aud": "bps-client"})
	require.Equal(t, "token-account", excelBPSAccountID(account, token))
}

func TestExcelBPSToolReplayStaysWithItsRouteAndModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, change := range []string{"model", "upstream account", "OAuth subject", "downstream product"} {
		t.Run(change, func(t *testing.T) {
			account, other := excelAccount(), excelAccount()
			account.GroupIDs, other.GroupIDs = []int64{10, 11}, []int64{10, 11}
			account.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "before")
			other.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-one", "test-account", "after")
			otherModel, otherGroup := "gpt-5.6-sol", int64(10)
			switch change {
			case "model":
				otherModel = "gpt-6-astra"
			case "upstream account":
				other.Credentials["chatgpt_account_id"] = "other-account"
			case "OAuth subject":
				other.Credentials["access_token"] = excelBPSCacheScopeToken(t, "actor-two", "test-account", "after")
			case "downstream product":
				otherGroup = 11
			}
			upstream := &httpUpstreamRecorder{}
			svc := openAIClientToolsTestService(upstream)
			send := func(account *Account, model string, group int64, input any, wire string) (*httptest.ResponseRecorder, error) {
				t.Helper()
				upstream.resp = &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}
				body, err := json.Marshal(map[string]any{"model": model, "stream": false, "input": input,
					"tools": []any{map[string]any{"type": "function", "name": "shell"}}})
				require.NoError(t, err)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				c.Request.Header.Set("session_id", "bps-replay-"+t.Name())
				c.Set("api_key", &APIKey{ID: 42, UserID: 7, GroupID: &group})
				_, err = svc.Forward(context.Background(), c, account, body)
				return recorder, err
			}
			wire := excelBPSRepairWire(t, "scope", "Run client tool", `{"name":"shell","arguments":{"cmd":"fixture"}}`)
			first, err := send(account, "gpt-5.6-sol", 10, "fixture", wire)
			require.NoError(t, err)
			require.Contains(t, first.Body.String(), "call_scope")
			continuation := []any{map[string]any{"type": "function_call_output", "call_id": "call_scope", "output": "done"}}
			before := len(upstream.requests)
			refused, err := send(other, otherModel, otherGroup, continuation, excelBPSToolPolicyResponse)
			require.Error(t, err, "a different source must not reuse the original native tool item")
			require.Equal(t, http.StatusBadRequest, refused.Code)
			require.Len(t, upstream.requests, before, "a missing native owner must be rejected before sending")
			resumed, err := send(account, "gpt-5.6-sol", 10, continuation, excelBPSToolPolicyResponse)
			require.NoError(t, err, fmt.Sprintf("the original route remains usable: %s", resumed.Body.String()))
			require.Contains(t, string(upstream.lastBody), "run_officejs")
		})
	}
}

func TestExcelBPSCatalogStaysWithEffectiveModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{}
	svc := openAIClientToolsTestService(upstream)
	account := excelAccount()
	send := func(model string, withTools bool) string {
		t.Helper()
		upstream.resp = &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(excelBPSToolPolicyResponse))}
		request := map[string]any{"model": model, "stream": false, "input": "fixture"}
		if withTools {
			request["tools"] = []any{map[string]any{"type": "function", "name": "scope_test_tool"}}
		}
		body, err := json.Marshal(request)
		require.NoError(t, err)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		c.Request.Header.Set("session_id", "bps-catalog-"+t.Name())
		c.Set("api_key", &APIKey{ID: 42, UserID: 7})
		_, err = svc.Forward(context.Background(), c, account, body)
		require.NoError(t, err)
		return string(upstream.lastBody)
	}
	require.Contains(t, send("gpt-5.6-sol", true), "scope_test_tool")
	require.Contains(t, send("gpt-5.6-sol", false), "scope_test_tool", "the original model inherits its catalog")
	require.NotContains(t, send("gpt-6-astra", false), "scope_test_tool", "another model must not inherit that catalog")
	require.Contains(t, send("gpt-5.6-sol", false), "scope_test_tool", "a foreign request must not erase the original catalog")
}

func TestExcelBPSManagedProxyPoolChangesRouteAndCacheIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("api_key", &APIKey{ID: 42, UserID: 7})
	first, alternate, fallback := excelAccount(), excelAccount(), excelAccount()
	for _, account := range []*Account{first, alternate, fallback} {
		account.Extra["openai_excel_bps_mihomo"] = true
	}
	alternate.Extra[ExcelBPSProxySourceKey] = ExcelBPSProxySourceIPPool
	fallback.Extra[ExcelBPSProxySourceKey] = "unrecognized"
	firstRoute := AccountRouteFingerprint(first)
	require.NotEqual(t, firstRoute, AccountRouteFingerprint(alternate), "different active proxy pools are different routes")
	require.Equal(t, firstRoute, AccountRouteFingerprint(fallback), "unknown values use the default effective pool")
	firstScope := excelBPSCacheScope(c, first, "gpt-5.6-sol", "session", "test-account", "test-token")
	require.NotEqual(t, firstScope, excelBPSCacheScope(c, alternate, "gpt-5.6-sol", "session", "test-account", "test-token"))
	require.Equal(t, firstScope, excelBPSCacheScope(c, fallback, "gpt-5.6-sol", "session", "test-account", "test-token"))
	direct, directWithInactiveSetting := excelAccount(), excelAccount()
	directWithInactiveSetting.Extra[ExcelBPSProxySourceKey] = ExcelBPSProxySourceIPPool
	require.Equal(t, AccountRouteFingerprint(direct), AccountRouteFingerprint(directWithInactiveSetting), "an inactive pool setting is not a route change")
}
