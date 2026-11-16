//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardResponses_ChatFallbackPreservesProvidedStartTime(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":"hello","stream":%t}`, stream))
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			responseBody := `{"id":"chatcmpl_timing","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
			contentType := "application/json"
			if stream {
				responseBody = "data: {\"id\":\"chatcmpl_timing\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"
				contentType = "text/event-stream"
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(responseBody))}}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			startTime := time.Now().Add(-time.Minute)
			result, err := svc.forwardResponsesViaRawChatCompletions(context.Background(), c, forceChatResponsesFallbackAccount(), body, startTime)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.GreaterOrEqual(t, result.Duration, time.Minute)
			if stream {
				require.NotNil(t, result.FirstTokenMs)
				require.GreaterOrEqual(t, *result.FirstTokenMs, int(time.Minute.Milliseconds()))
			}
		})
	}
}

func TestBillingCandidateHTTPMatchesPhysicalWire(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{"chat_http", "responses_http"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name, inputOptions, tier, effort, model     string
				policy                                      *OpenAIFastPolicySettings
				force, passthrough, nativeOnly, chatIngress bool
				history                                     bool
			}{
				{name: "default", tier: "unspecified", effort: "unspecified"},
				{name: "compatible none omitted", inputOptions: `,"reasoning":{"effort":"none"}`, tier: "unspecified", effort: "unspecified"},
				{name: "passthrough none preserved", inputOptions: `,"reasoning":{"effort":"none"}`, tier: "unspecified", effort: "none", passthrough: true},
				{name: "priority high", inputOptions: `,"service_tier":"priority","reasoning":{"effort":"high"}`, tier: "priority", effort: "high"},
				{name: "group force", tier: "priority", effort: "unspecified", force: true},
				{name: "filter", inputOptions: `,"service_tier":"priority"`, tier: "unspecified", effort: "unspecified", policy: openAIFastFilterPriorityPolicy()},
				{name: "missing tier forces priority", tier: "priority", effort: "unspecified", policy: &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierMissing, Action: OpenAIFastPolicyActionForcePriority, Scope: BetaPolicyScopeAll}}}},
				{name: "native minimal becomes none", inputOptions: `,"reasoning":{"effort":"minimal"}`, tier: "unspecified", effort: "none", nativeOnly: true},
				{name: "mapped model fast policy", inputOptions: `,"service_tier":"priority"`, tier: "unspecified", effort: "unspecified", nativeOnly: true, policy: &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierPriority, Action: BetaPolicyActionFilter, Scope: BetaPolicyScopeAll, ModelWhitelist: []string{"wire-fixture"}, FallbackAction: BetaPolicyActionPass}}}},
				{name: "chat ingress plain", tier: "unspecified", effort: "unspecified", chatIngress: true},
				{name: "chat ingress none", inputOptions: `,"reasoning_effort":"none"`, tier: "unspecified", effort: "none", chatIngress: true},
				{name: "chat ingress minimal", inputOptions: `,"reasoning_effort":"minimal"`, tier: "unspecified", effort: "minimal", chatIngress: true},
				{name: "chat ingress glm xhigh", inputOptions: `,"reasoning_effort":"xhigh"`, tier: "unspecified", effort: "max", model: "glm-4.7", chatIngress: true},
				{name: "chat ingress glm medium", inputOptions: `,"reasoning_effort":"medium"`, tier: "unspecified", effort: "high", model: "glm-4.7", chatIngress: true},
				{name: "chat ingress deepseek user only", tier: "unspecified", effort: "unspecified", model: "deepseek-v4-flash", chatIngress: true},
				{name: "chat ingress deepseek hidden history", tier: "unspecified", effort: "unspecified", model: "deepseek-v4-flash", chatIngress: true, history: true},
				{name: "chat ingress priority high", inputOptions: `,"service_tier":"priority","reasoning_effort":"high"`, tier: "priority", effort: "high", chatIngress: true},
				{name: "chat ingress group force", tier: "priority", effort: "unspecified", force: true, chatIngress: true},
				{name: "chat ingress filter", inputOptions: `,"service_tier":"priority"`, tier: "unspecified", effort: "unspecified", policy: openAIFastFilterPriorityPolicy(), chatIngress: true},
				{name: "chat ingress mapped filter", inputOptions: `,"service_tier":"priority"`, tier: "unspecified", effort: "unspecified", policy: &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierPriority, Action: BetaPolicyActionFilter, Scope: BetaPolicyScopeAll, ModelWhitelist: []string{"wire-fixture"}, FallbackAction: BetaPolicyActionPass}}}, chatIngress: true},
			} {
				if tc.nativeOnly && protocol != "responses_http" {
					continue
				}
				if tc.chatIngress && protocol != "chat_http" {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/stream=%v", protocol, tc.name, stream), func(t *testing.T) {
					mappedModel := "wire-fixture"
					if tc.model != "" {
						mappedModel = tc.model
					}
					model := mappedModel
					if protocol == "responses_http" && tc.passthrough {
						model = "gpt-fixture"
					}
					observed := make(chan *AvailabilityRequestScope, 2)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
						payload, err := io.ReadAll(request.Body)
						if err != nil {
							t.Errorf("read physical request: %v", err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						observed <- availabilityScopeForRequest(request, payload)
						if protocol == "responses_http" {
							response := fmt.Sprintf(`{"id":"resp_scope","object":"response","status":"completed","model":%q,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`, model)
							if stream {
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\ndata: [DONE]\n\n", response)
							} else {
								w.Header().Set("Content-Type", "application/json")
								_, _ = io.WriteString(w, response)
							}
							return
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_scope\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, `{"id":"chatcmpl_scope","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
						}
					}))
					t.Cleanup(server.Close)
					entry := billingApplicabilityEntry(protocol, tc.tier, true)
					entry["model"], entry["reasoning_effort"] = model, tc.effort
					account := billingCandidateAccount(t, 910, entry)
					if protocol == "chat_http" {
						account.Extra["openai_responses_mode"] = "force_chat_completions"
					}
					account.Extra["openai_passthrough"] = tc.passthrough
					account.Credentials["base_url"] = server.URL
					account.Credentials["model_mapping"] = map[string]any{"gpt-fixture": mappedModel}
					at := time.Now().UTC().Add(-time.Second)
					_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
					require.NoError(t, err)
					svc := newOpenAIGatewayServiceWithSettings(t, tc.policy)
					svc.cfg = rawChatCompletionsTestConfig()
					svc.accountRepo, svc.requireLatestTurnAdmission = &turnAdmissionRepo{account: account}, true
					svc.httpUpstream = &codexModelsHTTPUpstreamStub{do: func(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
						if err := CheckHTTPUpstreamSendGuard(request.Context()); err != nil {
							return nil, err
						}
						return server.Client().Do(request)
					}}
					body := []byte(fmt.Sprintf(`{"model":"gpt-fixture","input":"private prompt","stream":%v%s}`, stream, tc.inputOptions))
					path := "/v1/responses"
					if tc.chatIngress {
						path = "/v1/chat/completions"
						body = []byte(fmt.Sprintf(`{"model":"gpt-fixture","messages":[{"role":"user","content":"private prompt"}],"stream":%v%s}`, stream, tc.inputOptions))
						if tc.history {
							body = []byte(fmt.Sprintf(`{"model":"gpt-fixture","messages":[{"role":"assistant","content":"private previous answer"},{"role":"user","content":"private continuation"}],"stream":%v%s}`, stream, tc.inputOptions))
						}
					}
					group := profitControlTestGroup(7, 0.5, 0)
					group.ForceOpenAIFast = tc.force
					ctx, _ := svc.WithOpenAIRequestPricingContext(profitControlTestCtx(group), &group.ID)
					request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
					ctx = svc.WithUpstreamBillingCandidateRequest(ctx, request, body)
					vetoed, reason := openAIProfitControlVetoReason(ctx, account)
					require.False(t, vetoed, reason)
					forward := func() (*OpenAIForwardResult, error) {
						c, _ := gin.CreateTestContext(httptest.NewRecorder())
						c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)).WithContext(ctx)
						if tc.chatIngress {
							return svc.ForwardAsChatCompletions(ctx, c, account, body, "", "")
						}
						return svc.Forward(ctx, c, account, body)
					}
					result, err := forward()
					if tc.history {
						require.Nil(t, result)
						require.True(t, IsOpenAITurnAdmissionError(err))
						require.ErrorContains(t, err, "profit_upstream_scope_unknown")
						require.Empty(t, observed, "hidden history without its own declaration must never reach the physical server")
						return
					}
					require.NoError(t, err)
					require.NotNil(t, result)
					select {
					case scope := <-observed:
						require.NotNil(t, scope)
						require.Equal(t, AvailabilityRequestScope{Version: availabilityRequestScopeVersion, Protocol: protocol, Model: model, Capability: "text", ServiceTier: tc.tier, ReasoningEffort: tc.effort, Stream: stream}, *scope)
					default:
						t.Fatal("physical upstream did not receive the covered request")
					}
					entry["group_rate_applies"] = false
					_, err = prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
					require.NoError(t, err)
					vetoed, reason = openAIProfitControlVetoReason(ctx, account)
					require.True(t, vetoed)
					require.Equal(t, "profit_upstream_scope_excluded", reason)
					// Exercise the physical guard independently of candidate prediction.
					ctx = WithoutUpstreamBillingCandidateRequest(ctx)
					result, err = forward()
					require.Nil(t, result)
					require.True(t, IsOpenAITurnAdmissionError(err), "changed scope must still stop at the original final admission")
					require.Empty(t, observed, "an excluded scope must not produce a physical send")
				})
			}
		}
	}
}

func TestBillingCandidateChatResponsesConversionExactOutputScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		for _, effort := range []string{"", "high"} {
			t.Run(fmt.Sprintf("stream=%v/effort=%s", stream, effort), func(t *testing.T) {
				observed := make(chan []byte, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					payload, err := io.ReadAll(request.Body)
					if err != nil {
						t.Errorf("read converted request: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if request.URL.Path != "/v1/responses" {
						t.Errorf("conversion used unexpected path %s", request.URL.Path)
						w.WriteHeader(http.StatusNotFound)
						return
					}
					observed <- payload
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_scope\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"wire-fixture\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\ndata: [DONE]\n\n")
				}))
				t.Cleanup(server.Close)
				outgoing := billingApplicabilityEntry("responses_http", "priority", true)
				ingress := billingApplicabilityEntry("chat_http", "priority", false)
				for _, entry := range []map[string]any{outgoing, ingress} {
					entry["model"] = "wire-fixture"
					if effort != "" {
						entry["reasoning_effort"] = effort
					}
				}
				account := billingCandidateAccount(t, 921, outgoing, ingress)
				account.Credentials["base_url"] = server.URL
				account.Credentials["model_mapping"] = map[string]any{"gpt-fixture": "wire-fixture"}
				at := time.Now().UTC().Add(-time.Second)
				_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, outgoing, ingress), at)
				require.NoError(t, err)
				require.Equal(t, APIProtocolResponses, resolveUpstreamProtocol(account, APIProtocolChatCompletions, "", nil))
				repo := &turnAdmissionRepo{account: account}
				svc := newOpenAIGatewayServiceWithSettings(t, nil)
				svc.cfg = rawChatCompletionsTestConfig()
				svc.accountRepo, svc.requireLatestTurnAdmission = repo, true
				svc.httpUpstream = &codexModelsHTTPUpstreamStub{do: func(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
					if err := CheckHTTPUpstreamSendGuard(request.Context()); err != nil {
						return nil, err
					}
					return server.Client().Do(request)
				}}
				options := ""
				if effort != "" {
					options = fmt.Sprintf(`,"reasoning_effort":%q`, effort)
				}
				body := []byte(fmt.Sprintf(`{"model":"gpt-fixture","messages":[{"role":"user","content":"private prompt"}],"service_tier":"priority","stream":%v%s}`, stream, options))
				group := profitControlTestGroup(7, 0.5, 0)
				ctx, _ := svc.WithOpenAIRequestPricingContext(profitControlTestCtx(group), &group.ID)
				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
				ctx = svc.WithUpstreamBillingCandidateRequest(ctx, request, body)
				vetoed, reason := openAIProfitControlVetoReason(ctx, account)
				require.True(t, vetoed, "a plain-text declaration must not grant converted output options")
				require.Equal(t, "profit_upstream_scope_unknown", reason)
				forward := func(selected *Account) (*OpenAIForwardResult, error) {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
					return svc.ForwardAsChatCompletions(ctx, c, selected, body, "", "")
				}
				result, err := forward(account)
				require.Nil(t, result)
				require.True(t, IsOpenAITurnAdmissionError(err))
				require.ErrorContains(t, err, "profit_upstream_scope_unknown")
				require.Empty(t, observed)

				// A legacy snapshot is a differential control, not strict supply.
				// It proves the conversion and destination work without granting the
				// encrypted output options a plain-text applicability contract.
				legacy := *account
				legacy.Extra = maps.Clone(account.Extra)
				delete(legacy.Extra, UpstreamBillingManualApplicabilityExtraKey)
				repo.account = &legacy
				result, err = forward(&legacy)
				require.NoError(t, err)
				require.NotNil(t, result)
				select {
				case payload := <-observed:
					require.Equal(t, "wire-fixture", gjson.GetBytes(payload, "model").String())
					require.True(t, gjson.GetBytes(payload, "stream").Bool(), "Responses conversion always streams upstream")
					require.Equal(t, "priority", gjson.GetBytes(payload, "service_tier").String())
					require.Equal(t, "reasoning.encrypted_content", gjson.GetBytes(payload, "include.0").String())
					if effort != "" {
						require.Equal(t, effort, gjson.GetBytes(payload, "reasoning.effort").String())
						require.Equal(t, "auto", gjson.GetBytes(payload, "reasoning.summary").String())
					}
					scope := availabilityScopeForRequest(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), payload)
					require.NotNil(t, scope)
					require.True(t, scope.IncludeEncryptedContent)
				default:
					t.Fatal("legacy control did not send the converted request")
				}
				repo.account = account
				result, err = forward(account)
				require.Nil(t, result)
				require.ErrorContains(t, err, "profit_upstream_scope_unknown")
				require.Empty(t, observed, "restoring the declaration must restore pre-send rejection")

				outgoing["include_encrypted_content"] = true
				if effort != "" {
					outgoing["reasoning_summary"] = "auto"
				}
				_, err = prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, outgoing, ingress), at)
				require.NoError(t, err)
				vetoed, reason = openAIProfitControlVetoReason(ctx, account)
				require.False(t, vetoed, "the exact output declaration authorizes the actual converted scope: %s", reason)
				result, err = forward(account)
				require.NoError(t, err)
				require.NotNil(t, result)
				select {
				case payload := <-observed:
					scope := availabilityScopeForRequest(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), payload)
					require.NotNil(t, scope)
					status, _ := upstreamBillingScopeVerdictAt(account, scope, time.Now())
					require.Equal(t, "covered", status)
					require.True(t, scope.Stream)
					require.Equal(t, "wire-fixture", scope.Model)
					require.True(t, scope.IncludeEncryptedContent)
				default:
					t.Fatal("exact output declaration did not send the converted request")
				}
				outgoing["group_rate_applies"] = false
				_, err = prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, outgoing, ingress), at)
				require.NoError(t, err)
				result, err = forward(account)
				require.Nil(t, result)
				require.ErrorContains(t, err, "profit_upstream_scope_excluded")
				require.Empty(t, observed, "revoked output applicability must stop before network send")
			})
		}
	}
}

func TestForwardResponses_ForceChatCompletionsRoutesNonStreamingToChatCompletions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":false,"service_tier":"priority"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_resp_chat_json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_json","object":"chat.completion","model":"gpt-5.4","service_tier":"default","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,"prompt_tokens_details":{"cached_tokens":1}}}`,
		)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}
	SetActualOpenAIUpstreamEndpoint(c, "/v1/responses")

	result, err := svc.Forward(context.Background(), c, forceChatResponsesFallbackAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "http://upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.Equal(t, "/v1/chat/completions", GetActualOpenAIUpstreamEndpoint(c))
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(upstream.lastReq.Context()))
	require.Equal(t, "hello", gjson.GetBytes(upstream.lastBody, "messages.0.content").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "input").Exists())
	require.Equal(t, "response", gjson.Get(rec.Body.String(), "object").String())
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "output.0.content.0.text").String())
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Equal(t, 1, result.Usage.CacheReadInputTokens)
	require.NotNil(t, result.ServiceTier)
	require.Equal(t, "priority", *result.ServiceTier)
	require.Equal(t, "default", result.UpstreamResponseServiceTier)
	require.False(t, result.Stream)
}

// Scenario: 第三方无推理模型不收到兼容档位。
func TestForwardResponses_ForceChatCompletionsOmitsNoneReasoningEffort(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"company-coding-model","input":"hello","reasoning":{"effort":"none"},"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_none","object":"chat.completion","model":"company-coding-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.Forward(context.Background(), c, forceChatResponsesFallbackAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "company-coding-model", gjson.GetBytes(upstream.lastBody, "model").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "reasoning_effort").Exists())
	require.Nil(t, result.ReasoningEffort)
}

func TestForwardResponses_PassthroughFlagWithUnsupportedResponsesUsesAccountMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		path := path
		t.Run(path, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.4-channel","input":"hello","stream":false}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")

			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"chatcmpl_mapping","object":"chat.completion","model":"gpt-5.4-account","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
				)),
			}}
			svc := &OpenAIGatewayService{
				cfg:          rawChatCompletionsTestConfig(),
				httpUpstream: upstream,
			}
			account := rawChatCompletionsTestAccount()
			account.Credentials["model_mapping"] = map[string]any{
				"gpt-5.4-channel": "gpt-5.4-account",
			}
			account.Credentials["compact_model_mapping"] = map[string]any{
				"gpt-5.4-account": "gpt-5.4-compact",
			}
			account.Extra = map[string]any{
				"openai_passthrough":                     true,
				openai_compat.ExtraKeyResponsesSupported: false,
			}

			result, err := svc.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, "http://upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
			require.Equal(t, "gpt-5.4-account", gjson.GetBytes(upstream.lastBody, "model").String())
		})
	}
}

func TestForwardResponses_ForceChatCompletionsRoutesStreamingToChatCompletions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"he"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"gpt-5.4","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_resp_chat_stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.Forward(context.Background(), c, forceChatResponsesFallbackAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "http://upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream_options.include_usage").Bool())
	require.Contains(t, rec.Body.String(), "event: response.output_text.delta")
	require.Contains(t, rec.Body.String(), `"delta":"he"`)
	require.Contains(t, rec.Body.String(), "event: response.completed")
	require.Contains(t, rec.Body.String(), `"input_tokens":4`)
	require.Contains(t, rec.Body.String(), "data: [DONE]")
	require.Equal(t, 4, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.True(t, result.Stream)
	require.NotNil(t, result.FirstTokenMs)
}

func TestForwardResponses_ChatFallbackRejectsInvalidToolArgumentsAtOutputLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-v4-flash","input":"run the command","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_length_tool","object":"chat.completion.chunk","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_length","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"ssh root@HOST"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_length_tool","object":"chat.completion.chunk","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":4,"completion_tokens":6492,"total_tokens":6496}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_length_tool"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.Forward(context.Background(), c, forceChatResponsesFallbackAccount(), body)
	require.ErrorContains(t, err, "invalid JSON")
	require.NotNil(t, result)
	require.Equal(t, 4, result.Usage.InputTokens)
	require.Equal(t, 6492, result.Usage.OutputTokens)
	require.NotContains(t, rec.Body.String(), "response.function_call_arguments.done")
	require.NotContains(t, rec.Body.String(), "response.output_item.done")
	require.NotContains(t, rec.Body.String(), "data: [DONE]")
}

func TestForwardResponses_DeepSeekReasoningOnlyStreamFailsWithoutVisibleText(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-reasoner","input":"hello","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_reasoning","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":""},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_reasoning","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"reasoning_content":"visible fallback"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_reasoning","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_deepseek_reasoning_responses_stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.Forward(context.Background(), c, forceChatResponsesFallbackAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.NotContains(t, rec.Body.String(), "event: response.output_text.delta")
	require.Contains(t, rec.Body.String(), "event: response.failed")
	require.Contains(t, rec.Body.String(), `"code":"upstream_reasoning_only"`)
	require.Len(t, upstream.requests, 1)
	require.Contains(t, rec.Body.String(), "data: [DONE]")
}

func TestForwardResponses_AutoSupportedAccountStillUsesResponsesEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_resp_native"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"resp_native","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}],"status":"completed"}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}`,
		)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{
		openai_compat.ExtraKeyResponsesMode:      string(openai_compat.ResponsesSupportModeAuto),
		openai_compat.ExtraKeyResponsesSupported: true,
	}

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "http://upstream.example/v1/responses", upstream.lastReq.URL.String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "input").Exists())
	require.False(t, gjson.GetBytes(upstream.lastBody, "messages").Exists())
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "output.0.content.0.text").String())
}

func forceChatResponsesFallbackAccount() *Account {
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{
		openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceChatCompletions),
	}
	return account
}

// reasoningRecordingCache 记录 reasoning 缓存写入、并按需响应回查。
type reasoningRecordingCache struct {
	stubGatewayCache
	mu      sync.Mutex
	sets    map[string]string
	getResp map[string]string
}

func (c *reasoningRecordingCache) SetReasoningContent(_ context.Context, itemID string, content string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sets == nil {
		c.sets = make(map[string]string)
	}
	c.sets[itemID] = content
	return nil
}

func (c *reasoningRecordingCache) GetReasoningContent(_ context.Context, itemID string) (string, error) {
	if v, ok := c.getResp[itemID]; ok {
		return v, nil
	}
	return "", ErrReasoningContentNotFound
}

func (c *reasoningRecordingCache) snapshotSets() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.sets))
	for k, v := range c.sets {
		out[k] = v
	}
	return out
}

// 流式响应里的 reasoning_content 应按 reasoning item id 写入缓存，供后续轮次
// 客户端不回传明文 summary 时回注（DeepSeek thinking mode 400 修复的写入侧）。
func TestForwardResponses_ChatFallbackCachesStreamedReasoning(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-reasoner","input":"hello","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_rc","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_rc","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"reasoning_content":"think "},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_rc","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"reasoning_content":"first"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_rc","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"chatcmpl_rc","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_reasoning_cache_stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	cache := &reasoningRecordingCache{}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
		cache:        cache,
	}

	result, err := svc.Forward(context.Background(), c, forceChatResponsesFallbackAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)

	sets := cache.snapshotSets()
	require.Len(t, sets, 1, "应恰好缓存一个 reasoning item")
	for itemID, content := range sets {
		require.NotEmpty(t, itemID)
		require.Equal(t, "think first", content)
	}
}

// 请求侧：encrypted-only reasoning item（无明文 summary）经缓存回查补回
// reasoning_content；带明文 summary 的 item 顺手回写缓存（自愈）。
func TestForwardResponses_ChatFallbackRestoresReasoningFromCache(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{
		"model":"deepseek-reasoner",
		"stream":false,
		"input":[
			{"type":"reasoning","id":"item_plain","summary":[{"type":"summary_text","text":"plain thinking"}]},
			{"type":"function_call","call_id":"call_0","name":"get_value","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_0","output":"ok"},
			{"type":"reasoning","id":"item_enc1","summary":[],"encrypted_content":"opaque"},
			{"type":"function_call","call_id":"call_1","name":"get_value","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"go on"}]}
		]
	}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_reasoning_cache_restore"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_restore","object":"chat.completion","model":"deepseek-reasoner","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		)),
	}}
	cache := &reasoningRecordingCache{
		getResp: map[string]string{"item_enc1": "cached thinking"},
	}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
		cache:        cache,
	}

	result, err := svc.Forward(context.Background(), c, forceChatResponsesFallbackAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)

	// 明文 summary 的 assistant 工具调用消息：reasoning_content 来自 summary 本身。
	require.Equal(t, "plain thinking", gjson.GetBytes(upstream.lastBody, "messages.0.reasoning_content").String())
	require.Equal(t, "call_0", gjson.GetBytes(upstream.lastBody, "messages.0.tool_calls.0.id").String())
	require.Equal(t, "tool", gjson.GetBytes(upstream.lastBody, "messages.1.role").String())
	// encrypted-only 的 assistant 工具调用消息：reasoning_content 来自缓存回查。
	require.Equal(t, "cached thinking", gjson.GetBytes(upstream.lastBody, "messages.2.reasoning_content").String())
	require.Equal(t, "call_1", gjson.GetBytes(upstream.lastBody, "messages.2.tool_calls.0.id").String())
	require.Equal(t, "tool", gjson.GetBytes(upstream.lastBody, "messages.3.role").String())

	// 明文 summary 的 item 被回写进缓存（自愈）。
	require.Equal(t, "plain thinking", cache.snapshotSets()["item_plain"])
}
