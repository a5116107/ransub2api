package service

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type wsResponseOwnerAdmissionRepo struct {
	AccountRepository
	latest atomic.Pointer[Account]
}

func (r *wsResponseOwnerAdmissionRepo) GetOpenAITurnAdmission(context.Context, int64) (*Account, *Account, error) {
	return r.latest.Load(), nil, nil
}

func TestWSResponseOwnerNativeIngress(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModeDedicated, OpenAIWSIngressModePassthrough} {
		for _, change := range []string{"same", "model", "tier", "unknown", "revoked", "key", "endpoint", "tenant", "other_key", "group", "http_roundtrip", "new_root", "identical_model"} {
			t.Run(mode+"/"+change, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				observations := make(chan map[string]any, 8)
				var requests atomic.Int32
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer func() { _ = conn.Close() }()
					for {
						var payload map[string]any
						if conn.ReadJSON(&payload) != nil {
							return
						}
						id := requests.Add(1)
						observations <- payload
						if conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{
							"id": fmt.Sprintf("resp_native_owner_%d", id), "model": payload["model"],
							"usage": map[string]any{"input_tokens": 2, "output_tokens": 1},
						}}) != nil {
							return
						}
					}
				}))
				t.Cleanup(upstream.Close)
				cfg := newOpenAIWSExecutionScopeTestConfig()
				cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
				cfg.Gateway.OpenAIWS.IngressModeDefault = mode
				cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
				account := &Account{ID: 42, GroupIDs: []int64{17}, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
					Status: StatusActive, Schedulable: true, Concurrency: 2,
					Credentials: map[string]any{"api_key": "creation-fixture", "base_url": upstream.URL},
					Extra:       map[string]any{"responses_websockets_v2_enabled": true, "openai_apikey_responses_websockets_v2_mode": mode}}
				repo := &wsResponseOwnerAdmissionRepo{}
				repo.latest.Store(account)
				svc := &OpenAIGatewayService{cfg: cfg, accountRepo: repo,
					httpUpstream: &httpUpstreamRecorder{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector()}
				t.Cleanup(svc.CloseOpenAIWSPool)
				results := make(chan error, 3)
				var userID, keyID, groupID atomic.Int64
				userID.Store(101)
				keyID.Store(201)
				groupID.Store(17)
				gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := coderws.Accept(w, r, nil)
					if err != nil {
						results <- err
						return
					}
					defer func() { _ = conn.CloseNow() }()
					ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
					defer cancel()
					_, first, err := conn.Read(ctx)
					if err == nil {
						c, _ := gin.CreateTestContext(httptest.NewRecorder())
						c.Request = r
						user, key, group := userID.Load(), keyID.Load(), groupID.Load()
						c.Set("api_key", &APIKey{ID: key, UserID: user, GroupID: &group})
						SetOpenAIResponseOwner(c, user, key, gjson.GetBytes(first, "previous_response_id").String())
						err = svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "creation-fixture", first, nil)
					}
					results <- err
				}))
				t.Cleanup(gateway.Close)
				ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
				defer cancel()
				connect := func() *coderws.Conn {
					client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(gateway.URL, "http"), nil)
					require.NoError(t, err)
					t.Cleanup(func() { _ = client.CloseNow() })
					return client
				}
				write := func(client *coderws.Conn, model, tier, previous string) {
					body := fmt.Sprintf(`{"type":"response.create","model":%q,"service_tier":%q,"previous_response_id":%q,"store":true,"input":"next"}`, model, tier, previous)
					if change == "identical_model" {
						body = fmt.Sprintf(`{"type":"response.create","model":%q,"model":%q,"service_tier":%q,"previous_response_id":%q,"store":true,"input":"next"}`, model, model, tier, previous)
					}
					require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(body)))
				}
				client := connect()
				previous := ""
				if change == "unknown" {
					previous = "resp_unobserved"
				}
				write(client, "gpt-5.1", "", previous)
				_, first, readErr := client.Read(ctx)
				if change == "unknown" {
					require.Error(t, readErr, "unobserved continuation must not reach a provider")
					require.True(t, IsOpenAITurnAdmissionError(<-results))
					require.Empty(t, observations)
					return
				}
				require.NoError(t, readErr)
				<-observations
				previous = gjson.GetBytes(first, "response.id").String()
				require.NotEmpty(t, previous)
				require.Eventually(t, func() bool {
					owner, found, err := svc.getOpenAIWSStateStore().GetResponseOwner(ctx, 17, previous)
					return err == nil && found && owner.Valid() && owner.UserID == 101 && owner.Scope == openAIWSContinuationScope(account, "gpt-5.1", "")
				}, time.Second, time.Millisecond, "native responses must publish immutable tenant and egress evidence")
				if change == "http_roundtrip" {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					group := int64(17)
					c.Set("api_key", &APIKey{ID: 201, UserID: 101, GroupID: &group})
					SetOpenAIResponseOwner(c, 101, 201, previous)
					result, err := svc.Forward(ctx, c, account, []byte(fmt.Sprintf(`{"model":"gpt-5.1","previous_response_id":%q,"input":"http continuation","stream":false}`, previous)))
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, previous, (<-observations)["previous_response_id"])
					previous = result.RequestID
					require.NotEmpty(t, previous)
				}
				if change == "tenant" || change == "other_key" || change == "group" || change == "new_root" {
					require.NoError(t, client.Close(coderws.StatusNormalClosure, "fixture reconnect"))
					require.NoError(t, <-results)
					switch change {
					case "tenant":
						userID.Store(102)
					case "other_key":
						keyID.Store(202)
					case "group":
						groupID.Store(18)
					}
					client = connect()
				}
				if change == "new_root" {
					write(client, "gpt-5.1", "", previous)
					_, _, err := client.Read(ctx)
					require.NoError(t, err)
					<-observations
				}
				model, tier := "gpt-5.1", ""
				if change == "model" {
					model = "gpt-5.4"
				}
				if change == "tier" {
					tier = "priority"
				}
				if change == "key" || change == "endpoint" {
					latest := *account
					latest.Credentials = maps.Clone(account.Credentials)
					if change == "key" {
						latest.Credentials["api_key"] = "replacement-fixture"
					} else {
						latest.Credentials["base_url"] = upstream.URL + "/replacement"
					}
					repo.latest.Store(&latest)
				}
				if change == "revoked" || change == "new_root" {
					store, ok := svc.getOpenAIWSStateStore().(*defaultOpenAIWSStateStore)
					require.True(t, ok)
					store.responseOwnerMu.Lock()
					delete(store.responseOwners, openAIWSResponseAccountMapKey(17, previous))
					store.responseOwnerMu.Unlock()
				}
				if change == "new_root" {
					previous = ""
				}
				write(client, model, tier, previous)
				_, _, readErr = client.Read(ctx)
				if change == "same" || change == "other_key" || change == "http_roundtrip" || change == "new_root" || change == "identical_model" {
					require.NoError(t, readErr)
					observation := <-observations
					require.Equal(t, previous, observation["previous_response_id"])
					if change == "other_key" {
						owner, found, err := svc.getOpenAIWSStateStore().GetResponseOwner(ctx, 17, previous)
						require.NoError(t, err)
						require.True(t, found)
						require.Equal(t, int64(201), owner.APIKeyID, "another key must not relabel the original creation")
					}
					require.NoError(t, client.Close(coderws.StatusNormalClosure, "fixture complete"))
					require.NoError(t, <-results)
					return
				}
				require.Error(t, readErr, "a changed or revoked continuation must close locally")
				require.True(t, IsOpenAITurnAdmissionError(<-results))
				require.Empty(t, observations)
				if change != "revoked" {
					repo.latest.Store(account)
					userID.Store(101)
					keyID.Store(201)
					groupID.Store(17)
					recovered := connect()
					write(recovered, "gpt-5.1", "", previous)
					_, _, err := recovered.Read(ctx)
					require.NoError(t, err, "the original source must remain usable after a rejected scope change")
					require.Equal(t, previous, (<-observations)["previous_response_id"])
					require.NoError(t, recovered.Close(coderws.StatusNormalClosure, "fixture recovered"))
					require.NoError(t, <-results)
				}
			})
		}
	}
}
