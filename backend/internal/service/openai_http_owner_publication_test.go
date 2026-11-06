package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func httpResponseOwnerFixture(userID, apiKeyID int64) OpenAIHTTPResponseOwner {
	return OpenAIHTTPResponseOwner{UserID: userID, APIKeyID: apiKeyID,
		Scope: OpenAIWSContinuationScope{AccountID: 42, RouteFingerprint: [32]byte{1}, Model: "gpt-5.1"}}
}

func TestHTTPResponseOwnerRequiresSharedCapability(t *testing.T) {
	legacy := struct{ GatewayCache }{&stubGatewayCache{}}
	store := NewOpenAIWSStateStore(legacy)
	err := store.BindHTTPResponseOwner(t.Context(), 17, "resp_unsupported", httpResponseOwnerFixture(101, 201), time.Minute)
	require.ErrorIs(t, err, ErrOpenAIHTTPResponseOwnerUnavailable)
	_, found, err := store.GetHTTPResponseOwner(t.Context(), 17, "resp_unsupported")
	require.ErrorIs(t, err, ErrOpenAIHTTPResponseOwnerUnavailable)
	require.False(t, found)
}

func TestHTTPResponseOwnerLocalPublicationKeepsCreationScope(t *testing.T) {
	ctx := context.Background()
	store := NewOpenAIWSStateStore(nil)
	require.NoError(t, store.BindHTTPResponseOwner(ctx, 17, "resp_local", httpResponseOwnerFixture(101, 201), time.Minute))
	owner, found, err := store.GetHTTPResponseOwner(ctx, 17, "resp_local")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(101), owner.UserID)
	require.Equal(t, int64(201), owner.APIKeyID)
	require.Equal(t, httpResponseOwnerFixture(101, 201).Scope, owner.Scope)
	changed := owner
	changed.Scope.Model = "gpt-5.4"
	require.ErrorIs(t, store.BindHTTPResponseOwner(ctx, 17, "resp_local", changed, time.Minute), ErrOpenAIHTTPResponseOwnerUnavailable)
	unchanged, found, err := store.GetHTTPResponseOwner(ctx, 17, "resp_local")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, owner, unchanged)
}

func TestHTTPResponseContinuationKeepsCreationScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		mode := "native"
		if passthrough {
			mode = "passthrough"
		}
		for _, change := range []string{"key", "base_url", "proxy", "model", "tier", "account"} {
			t.Run(mode+"/"+change, func(t *testing.T) {
				account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{17},
					Credentials: map[string]any{"api_key": "creation-fixture", "base_url": "https://supplier.invalid/v1"}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: &turnAdmissionRepo{account: account}}
				newContext := func() *gin.Context {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					groupID := int64(17)
					c.Set("api_key", &APIKey{ID: 201, UserID: 101, GroupID: &groupID})
					SetOpenAIHTTPResponseOwner(c, 101, 201, "")
					return c
				}
				build := func(c *gin.Context, body []byte) (*http.Request, error) {
					if passthrough {
						return svc.buildUpstreamRequestOpenAIPassthrough(t.Context(), c, account, body, account.GetCredential("api_key"))
					}
					return svc.buildUpstreamRequest(t.Context(), c, account, body, account.GetCredential("api_key"), false, "", false)
				}
				first := newContext()
				_, err := build(first, []byte(`{"model":"gpt-5.1","input":"first"}`))
				require.NoError(t, err)
				svc.bindHTTPResponseAccount(t.Context(), first, account, "resp_creation_scope")
				body := []byte(`{"model":"gpt-5.1","previous_response_id":"resp_creation_scope","input":"next"}`)
				switch change {
				case "key":
					account.Credentials["api_key"] = "replacement-fixture"
				case "base_url":
					account.Credentials["base_url"] = "https://replacement.invalid/v1"
				case "proxy":
					proxyID := int64(9)
					account.ProxyID = &proxyID
				case "model":
					body = []byte(`{"model":"gpt-5.4","previous_response_id":"resp_creation_scope","input":"next"}`)
				case "tier":
					body = []byte(`{"model":"gpt-5.1","service_tier":"priority","previous_response_id":"resp_creation_scope","input":"next"}`)
				case "account":
					account.ID++
				}
				request, err := build(newContext(), body)
				require.True(t, IsOpenAITurnAdmissionError(err), "changed continuation must be rejected: %v", err)
				require.Nil(t, request)
				account.ID, account.ProxyID = 42, nil
				account.Credentials["api_key"] = "creation-fixture"
				account.Credentials["base_url"] = "https://supplier.invalid/v1"
				request, err = build(newContext(), []byte(`{"model":"gpt-5.1","previous_response_id":"resp_creation_scope","input":"next"}`))
				require.NoError(t, err, "restoring the original route must keep its continuation usable")
				require.NotNil(t, request)
			})
		}
	}
}

func TestHTTPResponseContinuationRechecksBeforeSend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, change := range []string{"key", "removed", "repo_failure", "same"} {
		t.Run(change, func(t *testing.T) {
			account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{17},
				Credentials: map[string]any{"api_key": "creation-fixture", "base_url": "https://supplier.invalid/v1"}}
			repo := &turnAdmissionRepo{account: account}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: repo, httpUpstream: upstream}
			newContext := func() *gin.Context {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				groupID := int64(17)
				c.Set("api_key", &APIKey{ID: 201, UserID: 101, GroupID: &groupID})
				SetOpenAIHTTPResponseOwner(c, 101, 201, "")
				return c
			}
			first := newContext()
			_, err := svc.buildUpstreamRequest(t.Context(), first, account, []byte(`{"model":"gpt-5.1","input":"first"}`), "creation-fixture", false, "", false)
			require.NoError(t, err)
			svc.bindHTTPResponseAccount(t.Context(), first, account, "resp_final_scope")
			request, err := svc.buildUpstreamRequest(t.Context(), newContext(), account, []byte(`{"model":"gpt-5.1","previous_response_id":"resp_final_scope","input":"next"}`), "creation-fixture", false, "", false)
			require.NoError(t, err)
			switch change {
			case "key":
				account.Credentials["api_key"] = "replacement-fixture"
			case "removed":
				store, ok := svc.getOpenAIWSStateStore().(*defaultOpenAIWSStateStore)
				require.True(t, ok)
				store.responseOwnerMu.Lock()
				delete(store.responseOwners, openAIWSResponseAccountMapKey(17, "resp_final_scope"))
				store.responseOwnerMu.Unlock()
			case "repo_failure":
				repo.err = ErrOpenAIHTTPResponseOwnerUnavailable
			}
			response, err := svc.doOpenAIUpstream(request, "", account)
			if change == "same" {
				require.NoError(t, err)
				require.NotNil(t, response)
				require.NoError(t, response.Body.Close())
				require.Len(t, upstream.bodies, 1)
			} else {
				require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
				require.Nil(t, response)
				require.Empty(t, upstream.bodies, "revoked or changed evidence must stop the physical send")
			}
		})
	}
}

func TestHTTPResponseOwnerUsesCapturedEgress(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "creation-fixture"}}
	svc := &OpenAIGatewayService{}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIHTTPResponseOwner(c, 101, 201, "")
	scope := openAIWSContinuationScope(account, "gpt-5.1", "priority")
	svc.guardOpenAICodexTurnStateEcho(c, account, http.Header{}, openAICompatCacheScope{Model: scope.Model, ServiceTier: scope.ServiceTier})
	account.Credentials["api_key"] = "replacement-fixture"
	svc.bindHTTPResponseAccount(t.Context(), c, account, "resp_inflight")
	owner, found, err := svc.getOpenAIWSStateStore().GetHTTPResponseOwner(t.Context(), 0, "resp_inflight")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, scope, owner.Scope, "an in-flight change must not relabel the response")
}

func TestHTTPResponseContinuationRejectsAmbiguousOrRemovedState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{
		`{"model":"gpt-5.1","input":"lost continuation"}`,
		`{"model":"gpt-5.1","previous_response_id":"resp_expected","previous_response_id":"resp_other"}`,
		`{"model":"gpt-5.1","previous_response_id":"resp_expected","Previous_Response_ID":"resp_other"}`,
		`{"model":"gpt-5.1","model":"gpt-5.4","previous_response_id":"resp_expected"}`,
		`{"model":"gpt-5.1","service_tier":"default","service_tier":"priority","previous_response_id":"resp_expected"}`,
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		SetOpenAIHTTPResponseOwner(c, 101, 201, "resp_expected")
		account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture"}}
		svc := &OpenAIGatewayService{cfg: &config.Config{}}
		request, err := svc.buildUpstreamRequest(t.Context(), c, account, []byte(body), "fixture", false, "", false)
		require.True(t, IsOpenAITurnAdmissionError(err), "%s: %v", body, err)
		require.Nil(t, request)
	}
}

func TestHTTPResponseContinuationWSKeepsCreationScope(t *testing.T) {
	for _, change := range []string{"key", "endpoint", "model", "tier"} {
		t.Run(change, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			upstream, observations := newOpenAIWSContinuationUpstream(t, false)
			cfg := newOpenAIWSExecutionScopeTestConfig()
			cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 0
			account := &Account{ID: 42, GroupIDs: []int64{17}, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Status: StatusActive, Schedulable: true, Concurrency: 2,
				Credentials: map[string]any{"api_key": "creation-fixture", "base_url": upstream.URL},
				Extra:       map[string]any{"responses_websockets_v2_enabled": true}}
			svc := &OpenAIGatewayService{cfg: cfg, accountRepo: &turnAdmissionRepo{account: account},
				httpUpstream: &httpUpstreamRecorder{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector()}
			t.Cleanup(svc.CloseOpenAIWSPool)
			forward := func(model, tier, previous string) (*OpenAIForwardResult, error) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				groupID := int64(17)
				c.Set("api_key", &APIKey{ID: 201, UserID: 101, GroupID: &groupID})
				SetOpenAIHTTPResponseOwner(c, 101, 201, previous)
				body := []byte(fmt.Sprintf(`{"model":%q,"service_tier":%q,"previous_response_id":%q,"input":"next","stream":false}`, model, tier, previous))
				return svc.Forward(t.Context(), c, account, body)
			}
			first, err := forward("gpt-5.1", "", "")
			require.NoError(t, err)
			require.NotNil(t, first)
			<-observations
			owned, err := svc.ValidateOpenAIHTTPResponseOwner(t.Context(), 17, first.RequestID, 101, 201)
			require.NoError(t, err)
			require.True(t, owned, "an authenticated HTTP request over WS must publish creation evidence")
			model, tier := "gpt-5.1", ""
			switch change {
			case "key":
				account.Credentials["api_key"] = "replacement-fixture"
			case "endpoint":
				account.Credentials["base_url"] = upstream.URL + "/replacement"
			case "model":
				model = "gpt-5.4"
			case "tier":
				tier = "priority"
			}
			result, err := forward(model, tier, first.RequestID)
			require.True(t, IsOpenAITurnAdmissionError(err), "changed continuation must stop before WS send: %v", err)
			require.Nil(t, result)
			require.Empty(t, observations)
			account.Credentials["api_key"], account.Credentials["base_url"] = "creation-fixture", upstream.URL
			result, err = forward("gpt-5.1", "", first.RequestID)
			require.NoError(t, err)
			require.NotNil(t, result)
			observation := <-observations
			require.Equal(t, first.RequestID, observation.payload["previous_response_id"])
		})
	}
}

func TestHTTPResponseContinuationSIWCDoesNotDiscardPreviousResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIHTTPResponseOwner(c, 101, 201, "resp_expected")
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc := &OpenAIGatewayService{}
	request, err := svc.buildSIWCRequest(t.Context(), c, account, []byte(`{"model":"gpt-5.1","previous_response_id":"resp_expected","input":"next"}`), "fixture-token")
	require.True(t, IsOpenAITurnAdmissionError(err), "a stateless route must not silently replay a continuation: %v", err)
	require.Nil(t, request)
}

func TestHTTPResponseContinuationWSRechecksAfterDial(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := newOpenAIWSExecutionScopeTestConfig()
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	account := &Account{ID: 42, GroupIDs: []int64{17}, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 2,
		Credentials: map[string]any{"api_key": "creation-fixture", "base_url": "https://supplier.invalid"},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true}}
	svc := &OpenAIGatewayService{cfg: cfg, accountRepo: &turnAdmissionRepo{account: account},
		httpUpstream: &httpUpstreamRecorder{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector()}
	t.Cleanup(svc.CloseOpenAIWSPool)
	store, ok := svc.getOpenAIWSStateStore().(*defaultOpenAIWSStateStore)
	require.True(t, ok)
	owner := OpenAIHTTPResponseOwner{UserID: 101, APIKeyID: 201, Scope: openAIWSContinuationScope(account, "gpt-5.1", "")}
	require.NoError(t, store.BindHTTPResponseOwner(t.Context(), 17, "resp_before_dial", owner, time.Minute))
	upstream := newStagedPassthroughConn()
	svc.getOpenAIWSConnPool().setClientDialerForTest(turnAdmissionDialerFunc(func(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
		store.responseOwnerMu.Lock()
		delete(store.responseOwners, openAIWSResponseAccountMapKey(17, "resp_before_dial"))
		store.responseOwnerMu.Unlock()
		return upstream, http.StatusSwitchingProtocols, http.Header{}, nil
	}))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	groupID := int64(17)
	c.Set("api_key", &APIKey{ID: 201, UserID: 101, GroupID: &groupID})
	SetOpenAIHTTPResponseOwner(c, 101, 201, "resp_before_dial")
	result, err := svc.Forward(t.Context(), c, account, []byte(`{"model":"gpt-5.1","previous_response_id":"resp_before_dial","input":"next","stream":false}`))
	require.True(t, IsOpenAITurnAdmissionError(err), "revocation during dial must stop the physical frame: %v", err)
	require.Nil(t, result)
	require.Empty(t, upstream.writes)
}
