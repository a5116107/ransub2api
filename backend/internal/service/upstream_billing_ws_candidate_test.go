package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func billingWSCandidateContext(t *testing.T, svc *OpenAIGatewayService, group *Group, body, model string, hooks *OpenAIWSIngressHooks) context.Context {
	t.Helper()
	ctx, _ := svc.WithOpenAIRequestPricingContext(profitControlTestCtx(group), &group.ID)
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	request.Header.Set("Upgrade", "websocket")
	return svc.WithUpstreamBillingWSCandidateRequest(ctx, request, []byte(body), model, hooks)
}

func TestBillingWSCandidateScopeTransforms(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool, OpenAIWSIngressModeShared, OpenAIWSIngressModeDedicated, OpenAIWSIngressModeHTTPBridge, "passthrough_size_bridge"} {
		for _, tc := range []struct {
			name, options, tier, effort, summary, verbosity string
			encrypted                                       bool
			hooks                                           *OpenAIWSIngressHooks
			policy                                          *OpenAIFastPolicySettings
			force                                           bool
			compatibleBridge                                bool
		}{
			{name: "plain", tier: "unspecified", effort: "unspecified"},
			{name: "encrypted output", options: `,"include":["reasoning.encrypted_content"]`, tier: "unspecified", effort: "unspecified", encrypted: true},
			{name: "summary and encrypted output", options: `,"include":["reasoning.encrypted_content"],"reasoning":{"effort":"high","summary":"auto"}`, tier: "unspecified", effort: "high", summary: "auto", encrypted: true},
			{name: "text verbosity", options: `,"text":{"verbosity":"medium"}`, tier: "unspecified", effort: "unspecified", verbosity: "medium"},
			{name: "none preserved", options: `,"reasoning":{"effort":"none"}`, tier: "unspecified", effort: "none"},
			{name: "compatible bridge strips none", options: `,"reasoning":{"effort":"none"}`, tier: "unspecified", effort: "unspecified", compatibleBridge: true},
			{name: "minimal preserved", options: `,"reasoning":{"effort":"minimal"}`, tier: "unspecified", effort: "minimal"},
			{name: "ceiling", options: `,"reasoning":{"effort":"xhigh"}`, tier: "unspecified", effort: "medium", hooks: &OpenAIWSIngressHooks{MaxReasoningEffort: "medium"}},
			{name: "forced effort", tier: "unspecified", effort: "high", hooks: &OpenAIWSIngressHooks{ForcedReasoningEffort: "high"}},
			{name: "client model policy before mapping", options: `,"reasoning":{"effort":"low"}`, tier: "unspecified", effort: "high", hooks: &OpenAIWSIngressHooks{ReasoningEffortMappings: []ReasoningEffortMapping{{From: "low", To: "high", MatchType: "exact", Model: "public-fixture"}}}},
			{name: "group priority", tier: "priority", effort: "unspecified", force: true},
			{name: "priority filtered", options: `,"service_tier":"priority"`, tier: "unspecified", effort: "unspecified", policy: openAIFastFilterPriorityPolicy()},
			{name: "filter overrides group", tier: "unspecified", effort: "unspecified", force: true, policy: openAIFastFilterPriorityPolicy()},
			{name: "missing forces priority", tier: "priority", effort: "unspecified", policy: &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierMissing, Action: OpenAIFastPolicyActionForcePriority, Scope: BetaPolicyScopeAll}}}},
		} {
			if tc.compatibleBridge && mode != OpenAIWSIngressModeHTTPBridge && mode != "passthrough_size_bridge" {
				continue
			}
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				svc := newOpenAIGatewayServiceWithSettings(t, tc.policy)
				svc.cfg = passthroughLifecycleConfig()
				bridge := mode == OpenAIWSIngressModeHTTPBridge || mode == "passthrough_size_bridge"
				protocol := "responses_ws_v2"
				if bridge {
					protocol = "responses_http"
				}
				entry := billingApplicabilityEntry(protocol, tc.tier, true)
				entry["reasoning_effort"] = tc.effort
				if tc.verbosity != "" {
					entry["text_verbosity"] = tc.verbosity
				}
				if tc.summary != "" {
					entry["reasoning_summary"] = tc.summary
				}
				if tc.encrypted {
					entry["include_encrypted_content"] = true
				}
				model := "gpt-fixture"
				if mode != OpenAIWSIngressModePassthrough {
					model = "wire-fixture"
				}
				entry["model"] = model
				account := billingCandidateAccount(t, 911, entry)
				account.Extra["openai_passthrough"] = !tc.compatibleBridge
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
				if mode == "passthrough_size_bridge" {
					account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
					svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
					svc.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
				}
				account.Credentials["model_mapping"] = map[string]any{"gpt-fixture": "wire-fixture"}
				at := time.Now().UTC().Add(-time.Second)
				_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
				require.NoError(t, err)
				group := profitControlTestGroup(7, 0.5, 0)
				group.ForceOpenAIFast = tc.force
				body := `{"type":"response.create","model":"public-fixture","input":"private prompt"` + tc.options + `}`
				ctx := billingWSCandidateContext(t, svc, group, body, "gpt-fixture", tc.hooks)
				vetoed, reason := openAIProfitControlVetoReason(ctx, account)
				require.False(t, vetoed, reason)
				// Independently follow the original first-frame transformation order.
				wire, err := applyOpenAIWSReasoningEffortPolicy([]byte(body), tc.hooks, tc.hooks.forcedReasoningEffort())
				require.NoError(t, err)
				wire = svc.ReplaceModelInBody(wire, "gpt-fixture")
				wire, _, err = normalizeOpenAIResponsesWebSocketCompatibilityBodyWithConfig(svc.cfg, wire, account, false)
				require.NoError(t, err)
				if mode != OpenAIWSIngressModePassthrough || bridge {
					wire = svc.ReplaceModelInBody(wire, normalizeOpenAIModelForUpstream(account, account.GetMappedModel("gpt-fixture")))
				}
				wire, blocked, err := svc.applyOpenAIFastPolicyToWSResponseCreate(ctx, account, model, wire)
				require.NoError(t, err)
				require.Nil(t, blocked)
				request := httptest.NewRequest(http.MethodGet, "wss://relay.example/v1/responses", nil)
				if bridge {
					wire, err = prepareOpenAIWSHTTPBridgeSessionBody(account, wire, false)
					require.NoError(t, err)
					request = httptest.NewRequest(http.MethodPost, "https://relay.example/v1/responses", nil)
				}
				scope := availabilityScopeForRequest(request, wire)
				require.NotNil(t, scope)
				require.Equal(t, model, scope.Model)
				require.Equal(t, tc.tier, scope.ServiceTier)
				require.Equal(t, tc.effort, scope.ReasoningEffort)
				require.Equal(t, tc.summary, scope.ReasoningSummary)
				require.Equal(t, tc.encrypted, scope.IncludeEncryptedContent)
				require.Equal(t, tc.verbosity, scope.TextVerbosity)
				require.NoError(t, guardUpstreamBillingApplicability(account, scope))
				entry["group_rate_applies"] = false
				_, err = prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
				require.NoError(t, err)
				vetoed, reason = openAIProfitControlVetoReason(ctx, account)
				require.True(t, vetoed)
				require.Equal(t, "profit_upstream_scope_excluded", reason)
			})
		}
	}
}

func TestBillingWSCandidateBridgeDispatchBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, mode, channelModel, smartMode string
		thresholdDelta                      int64
		forceHTTP, disabledWS, deferred     bool
		wantHTTP                            bool
	}{
		{name: "fixed bridge native disabled", mode: OpenAIWSIngressModeHTTPBridge, disabledWS: true, wantHTTP: true},
		{name: "fixed bridge forced HTTP", mode: OpenAIWSIngressModeHTTPBridge, forceHTTP: true, wantHTTP: true},
		{name: "fixed bridge overrides smart WS", mode: OpenAIWSIngressModeHTTPBridge, smartMode: "ws", wantHTTP: true},
		{name: "original length exact threshold", mode: OpenAIWSIngressModePassthrough, wantHTTP: true},
		{name: "original length minus threshold", mode: OpenAIWSIngressModePassthrough, thresholdDelta: 1},
		{name: "mapped model shrinking does not cancel bridge", mode: OpenAIWSIngressModePassthrough, channelModel: "g", wantHTTP: true},
		{name: "mapped model growing does not cause bridge", mode: OpenAIWSIngressModePassthrough, channelModel: strings.Repeat("g", 100), thresholdDelta: 1},
		{name: "legacy bridge overrides smart auto", mode: OpenAIWSIngressModePassthrough, smartMode: "auto", wantHTTP: true},
		{name: "legacy bridge overrides smart HTTP", mode: OpenAIWSIngressModePassthrough, smartMode: "http", wantHTTP: true},
		{name: "legacy bridge overrides smart WS", mode: OpenAIWSIngressModePassthrough, smartMode: "ws", wantHTTP: true},
		{name: "legacy bridge overrides smart shadow", mode: OpenAIWSIngressModePassthrough, smartMode: "shadow", wantHTTP: true},
		{name: "smart without storage remains WS", mode: OpenAIWSIngressModePassthrough, smartMode: "http", thresholdDelta: 1},
		{name: "passthrough still requires native transport", mode: OpenAIWSIngressModePassthrough, forceHTTP: true, deferred: true},
		{name: "pooled normalized length stays deferred", mode: OpenAIWSIngressModeCtxPool, deferred: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &OpenAIGatewayService{cfg: passthroughLifecycleConfig()}
			svc.cfg.Gateway.OpenAIWS.ForceHTTP = tc.forceHTTP
			svc.cfg.Gateway.OpenAIWS.Enabled = !tc.disabledWS
			svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
			svc.cfg.Gateway.OpenAIWS.SmartProtocol = config.GatewayOpenAIWSSmartProtocolConfig{Enabled: tc.smartMode != "", Mode: tc.smartMode}
			body := []byte(` {"type":"response.create","model":"public-fixture","input":"private prompt"} `)
			svc.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = int64(len(body)) + tc.thresholdDelta
			channelModel := tc.channelModel
			if channelModel == "" {
				channelModel = "gpt-fixture"
			}
			wsEntry := billingApplicabilityEntry("responses_ws_v2", "unspecified", tc.wantHTTP)
			wsEntry["model"] = channelModel
			httpEntry := billingApplicabilityEntry("responses_http", "unspecified", !tc.wantHTTP)
			httpEntry["model"] = "wire-fixture"
			account := billingCandidateAccount(t, 920, wsEntry, httpEntry)
			account.Credentials["model_mapping"] = map[string]any{channelModel: "wire-fixture"}
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = tc.mode
			at := time.Now().UTC().Add(-time.Second)
			_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, wsEntry, httpEntry), at)
			require.NoError(t, err)
			ctx := billingWSCandidateContext(t, svc, profitControlTestGroup(7, 0.5, 0), string(body), channelModel, nil)
			stored := ctx.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate)
			require.NotNil(t, stored.scope)
			require.Equal(t, tc.thresholdDelta <= 0, stored.wsPassthroughBridge)
			vetoed, reason := openAIProfitControlVetoReason(ctx, account)
			require.Equal(t, !tc.deferred, vetoed, reason)
			if !tc.deferred {
				require.Equal(t, "profit_upstream_scope_excluded", reason)
			}
			require.Equal(t, "profit_upstream_scope_expired", upstreamBillingCandidateVetoReason(ctx, account, at.Add(2*time.Hour)))
			cleared := WithoutUpstreamBillingCandidateRequest(ctx).Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate)
			require.False(t, cleared.wsPassthroughBridge)
			request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			request.Header.Set("Upgrade", "websocket")
			unknown := svc.WithUpstreamBillingWSCandidateRequest(ctx, request, []byte(`{"type":"response.create","model":"gpt-fixture","tools":[{"type":"web_search"}]}`), channelModel, nil)
			require.Nil(t, unknown.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate).scope)
			require.False(t, unknown.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate).wsPassthroughBridge)
		})
	}
}

func TestBillingWSCandidateSmartDispatch(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool, OpenAIWSIngressModeShared, OpenAIWSIngressModeDedicated} {
		for _, tc := range []struct {
			name, smartMode, options         string
			wantHTTP, bridgeOff, unknownHTTP bool
			history                          int
			hooks                            *OpenAIWSIngressHooks
			effort                           string
		}{
			{name: "manual HTTP", smartMode: "http", options: `,"store":true`, wantHTTP: true},
			{name: "manual WS", smartMode: "ws", options: `,"store":true,"max_output_tokens":8000`},
			{name: "shadow stays WS", smartMode: "shadow", options: `,"store":true,"max_output_tokens":8000`},
			{name: "auto cap", smartMode: "auto", options: `,"store":true,"max_output_tokens":8000`, wantHTTP: true},
			{name: "auto raw effort before ceiling", smartMode: "auto", options: `,"store":true,"reasoning":{"effort":"xhigh"}`, wantHTTP: true, effort: "low", hooks: &OpenAIWSIngressHooks{MaxReasoningEffort: "low"}},
			{name: "forced effort does not change dispatch", smartMode: "auto", options: `,"store":true,"reasoning":{"effort":"low"}`, effort: "xhigh", hooks: &OpenAIWSIngressHooks{ForcedReasoningEffort: "xhigh"}},
			{name: "history uses original model and effort", smartMode: "auto", options: `,"store":true,"reasoning":{"effort":"high"}`, history: 20, wantHTTP: true, effort: "low", hooks: &OpenAIWSIngressHooks{MaxReasoningEffort: "low"}},
			{name: "history below minimum", smartMode: "auto", options: `,"store":true`, history: 19},
			{name: "missing store", smartMode: "http"},
			{name: "false store", smartMode: "http", options: `,"store":false`},
			{name: "string store", smartMode: "http", options: `,"store":"true"`},
			{name: "unknown HTTP", smartMode: "http", options: `,"store":true`, unknownHTTP: true},
			{name: "bridge disabled", smartMode: "http", options: `,"store":true,"max_output_tokens":8000`, bridgeOff: true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				svc := &OpenAIGatewayService{cfg: passthroughLifecycleConfig()}
				svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = !tc.bridgeOff
				svc.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1 << 20
				svc.cfg.Gateway.OpenAIWS.SmartProtocol = config.GatewayOpenAIWSSmartProtocolConfig{Enabled: true, Mode: tc.smartMode, LongContextBytes: 16}
				body := `{"type":"response.create","model":"public-fixture","input":"private prompt longer than threshold"` + tc.options + `}`
				protocol, wireModel, effort := "responses_ws_v2", "gpt-fixture", tc.effort
				if effort == "" {
					effort = "unspecified"
				}
				if tc.wantHTTP {
					protocol = "responses_http"
				}
				if mode != OpenAIWSIngressModePassthrough || tc.wantHTTP {
					wireModel = "wire-fixture"
				}
				entry := billingApplicabilityEntry(protocol, "unspecified", false)
				entry["model"], entry["reasoning_effort"] = wireModel, effort
				account := billingCandidateAccount(t, 924, entry)
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
				account.Extra["openai_responses_supported"] = !tc.unknownHTTP
				account.Credentials["model_mapping"] = map[string]any{"gpt-fixture": "wire-fixture", "public-fixture": "history-fixture"}
				at := time.Now().UTC().Add(-time.Second)
				_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
				require.NoError(t, err)
				ctx := billingWSCandidateContext(t, svc, profitControlTestGroup(7, 0.5, 0), body, "gpt-fixture", tc.hooks)
				// Samples arrive after capture: selection must use the current history,
				// keyed by the raw model/effort, before channel and group rewriting.
				for i := 0; i < tc.history; i++ {
					svc.smartProtocolHistory.observe(parseOpenAISmartProtocolRequest([]byte(body)).historyKey(account, "ws"), 8000, time.Now())
				}
				decision := svc.decideOpenAIWSUpstreamProtocol(account, []byte(body))
				require.Equal(t, tc.wantHTTP, decision.Protocol == "http")
				deferred := mode != OpenAIWSIngressModePassthrough && !tc.bridgeOff && !tc.wantHTTP
				require.Equal(t, map[bool]string{true: "", false: "profit_upstream_scope_excluded"}[deferred], upstreamBillingCandidateVetoReason(ctx, account, at))
				require.Equal(t, "profit_upstream_scope_expired", upstreamBillingCandidateVetoReason(ctx, account, at.Add(2*time.Hour)))
			})
		}
	}
}

func TestBillingWSCandidateDeferredPaths(t *testing.T) {
	for _, name := range []string{"ctx pool bridge enabled", "ctx pool image model", "ctx pool smart WS", "global force http", "account off", "mode router off", "lite", "tools", "missing type", "query", "reasoning denied", "fast blocked", "unknown policy", "gate disabled", "suppressed", "not upgraded"} {
		t.Run(name, func(t *testing.T) {
			svc := &OpenAIGatewayService{cfg: passthroughLifecycleConfig()}
			group := profitControlTestGroup(7, 0.5, 0)
			account := billingCandidateAccount(t, 912, billingApplicabilityEntry("responses_ws_v2", "unspecified", false))
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
			body := `{"type":"response.create","model":"gpt-fixture","input":"private prompt"}`
			request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			request.Header.Set("Upgrade", "websocket")
			var hooks *OpenAIWSIngressHooks
			switch name {
			case "ctx pool bridge enabled":
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
				svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
			case "ctx pool image model":
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
				account.Credentials["model_mapping"] = map[string]any{"gpt-fixture": "gpt-image-1"}
			case "ctx pool smart WS":
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
				svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
				svc.cfg.Gateway.OpenAIWS.SmartProtocol.Enabled = true
			case "global force http":
				svc.cfg.Gateway.OpenAIWS.ForceHTTP = true
			case "account off":
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeOff
			case "mode router off":
				svc.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = false
			case "lite":
				request.Header.Set(responsesLiteHeader, "true")
			case "tools":
				body = `{"type":"response.create","model":"gpt-fixture","tools":[{"type":"web_search"}]}`
			case "missing type":
				body = `{"model":"gpt-fixture"}`
			case "query":
				request.URL.RawQuery = "unsupported=1"
			case "reasoning denied":
				body = `{"type":"response.create","model":"gpt-fixture","reasoning":{"effort":"high"}}`
				hooks = &OpenAIWSIngressHooks{MaxReasoningEffort: "low", MaxReasoningEffortOverLimit: "deny"}
			case "fast blocked":
				settings := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierPriority, Action: BetaPolicyActionBlock, Scope: BetaPolicyScopeAll}}})
				svc.settingService = settings.settingService
				body = `{"type":"response.create","model":"gpt-fixture","service_tier":"priority"}`
			case "unknown policy":
				svc.settingService = NewSettingService(&billingCandidateSettingRepo{}, &config.Config{})
			case "gate disabled":
				group.ProfitControlEnabled = false
			case "not upgraded":
				request.Header.Del("Upgrade")
			}
			entry := billingApplicabilityEntry("responses_ws_v2", "unspecified", false)
			at := time.Now().UTC().Add(-time.Second)
			_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
			require.NoError(t, err)
			ctx, _ := svc.WithOpenAIRequestPricingContext(profitControlTestCtx(group), &group.ID)
			if name == "suppressed" {
				ctx = WithOpenAIProfitControlSuppressed(profitControlTestCtx(group))
				ctx, _ = svc.WithOpenAIRequestPricingContext(ctx, &group.ID)
			}
			ctx = svc.WithUpstreamBillingWSCandidateRequest(ctx, request, []byte(body), "gpt-fixture", hooks)
			vetoed, reason := openAIProfitControlVetoReason(ctx, account)
			require.False(t, vetoed, reason)
		})
	}
}

func TestBillingWSCandidateLifetimeAndPolicySnapshot(t *testing.T) {
	svc := newOpenAIGatewayServiceWithSettings(t, openAIFastFilterPriorityPolicy())
	svc.cfg = passthroughLifecycleConfig()
	svc.cfg.Gateway.OpenAIWS.SmartProtocol = config.GatewayOpenAIWSSmartProtocolConfig{Enabled: true, Mode: "ws"}
	repo := &billingCandidateSettingRepo{SettingRepository: svc.settingService.settingRepo}
	svc.settingService = NewSettingService(repo, svc.cfg)
	entry := billingApplicabilityEntry("responses_ws_v2", "unspecified", false)
	account := billingCandidateAccount(t, 913, entry)
	account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
	at := time.Now().UTC().Add(-time.Second)
	_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
	require.NoError(t, err)
	ctx := billingWSCandidateContext(t, svc, profitControlTestGroup(7, 0.5, 0), `{"type":"response.create","model":"gpt-fixture","input":"private prompt","service_tier":"priority"}`, "gpt-fixture", nil)
	for i := 0; i < 100; i++ {
		vetoed, reason := openAIProfitControlVetoReason(ctx, account)
		require.True(t, vetoed)
		require.Equal(t, "profit_upstream_scope_excluded", reason)
	}
	stored := ctx.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate)
	require.Equal(t, "gpt-fixture", stored.scope.Model)
	require.NotNil(t, stored.wsSmartRequest)
	require.Equal(t, "gpt-fixture", stored.wsSmartRequest.model)
	require.Same(t, stored.fastPolicy, openAIFastPolicySettingsFromContext(ctx))
	turnCtx := WithoutUpstreamBillingCandidateRequest(ctx)
	cleared := turnCtx.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate)
	require.Nil(t, cleared.scope)
	require.Nil(t, cleared.gateway)
	require.Nil(t, cleared.wsSmartRequest)
	_, blocked, err := svc.applyOpenAIFastPolicyToWSResponseCreate(turnCtx, account, "gpt-fixture", []byte(`{"type":"response.create","model":"gpt-fixture","service_tier":"priority"}`))
	require.NoError(t, err)
	require.Nil(t, blocked)
	vetoed, reason := openAIProfitControlVetoReason(turnCtx, account)
	require.False(t, vetoed, reason)
	require.EqualValues(t, 1, repo.reads.Load(), "selection and every WS turn reuse the same policy snapshot")
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	request.Header.Set("Upgrade", "websocket")
	unknown := svc.WithUpstreamBillingWSCandidateRequest(ctx, request, []byte(`{"type":"response.create","model":"gpt-fixture","tools":[{"type":"web_search"}]}`), "gpt-fixture", nil)
	require.Nil(t, unknown.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate).scope)
	require.Nil(t, unknown.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate).wsSmartRequest)
	vetoed, reason = openAIProfitControlVetoReason(unknown, account)
	require.False(t, vetoed, reason)
}

func TestBillingWSCandidatePostSlotRecheck(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool, OpenAIWSIngressModeShared, OpenAIWSIngressModeDedicated, OpenAIWSIngressModeHTTPBridge, "passthrough_size_bridge", "smart_http", "smart_ws"} {
		t.Run(mode, func(t *testing.T) {
			makeAccount := func(covered bool) *Account {
				protocol := "responses_ws_v2"
				if mode == OpenAIWSIngressModeHTTPBridge || mode == "passthrough_size_bridge" || mode == "smart_http" {
					protocol = "responses_http"
				}
				entry := billingApplicabilityEntry(protocol, "unspecified", covered)
				account := billingCandidateAccount(t, 915, entry)
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
				if mode == "passthrough_size_bridge" || strings.HasPrefix(mode, "smart_") {
					account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
				}
				account.Extra["openai_responses_supported"] = true
				at := time.Now().UTC().Add(-time.Second)
				_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
				require.NoError(t, err)
				return account
			}
			selected, replacement := makeAccount(true), makeAccount(false)
			svc := &OpenAIGatewayService{cfg: passthroughLifecycleConfig()}
			if mode == "passthrough_size_bridge" {
				svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
				svc.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
			}
			if strings.HasPrefix(mode, "smart_") {
				svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
				svc.cfg.Gateway.OpenAIWS.SmartProtocol = config.GatewayOpenAIWSSmartProtocolConfig{Enabled: true, Mode: strings.TrimPrefix(mode, "smart_")}
			}
			ctx := billingWSCandidateContext(t, svc, profitControlTestGroup(7, 0.5, 0), `{"type":"response.create","model":"gpt-fixture","store":true}`, "gpt-fixture", nil)
			vetoed, reason := openAIProfitControlVetoReason(ctx, selected)
			require.False(t, vetoed, reason)
			svc.schedulerSnapshot = NewSchedulerSnapshotService(&gatewayProfitSnapshotCache{account: replacement}, nil, gatewayProfitAccountRepo{}, nil, nil)
			latest, vetoed, reason := svc.ProfitControlVetoLatest(ctx, selected)
			require.Same(t, replacement, latest)
			require.True(t, vetoed)
			require.Equal(t, "profit_upstream_scope_excluded", reason)
		})
	}
}

func TestBillingCandidateMetadataDoesNotRetainInputBacking(t *testing.T) {
	backing := "gpt-fixturepriorityhigh" + strings.Repeat("private prompt", 1024)
	model, tier, effort := backing[:11], backing[11:19], backing[19:23]
	scope := &AvailabilityRequestScope{Model: model, ServiceTier: tier, ReasoningEffort: effort, ThinkingMode: model, ProtocolOptions: tier, TextVerbosity: effort}
	ctx := (&OpenAIGatewayService{}).withUpstreamBillingCandidatePolicy(t.Context(), upstreamBillingCandidate{scope: scope})
	stored := ctx.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate).scope
	for _, tc := range []struct{ name, original, stored string }{
		{"model", model, stored.Model}, {"tier", tier, stored.ServiceTier}, {"effort", effort, stored.ReasoningEffort},
		{"thinking", model, stored.ThinkingMode}, {"protocol options", tier, stored.ProtocolOptions},
		{"text verbosity", effort, stored.TextVerbosity},
	} {
		require.Equal(t, tc.original, tc.stored)
		if unsafe.StringData(tc.original) == unsafe.StringData(tc.stored) {
			t.Errorf("%s retains the complete input backing", tc.name)
		}
	}
	svc := &OpenAIGatewayService{}
	group := profitControlTestGroup(7, .5, 0)
	ctx, _ = svc.WithOpenAIRequestPricingContext(profitControlTestCtx(group), &group.ID)
	body := []byte(`{"model":"gpt-fixture","messages":[{"role":"user","content":"private prompt"}],"thinking":{"type":"enabled","budget_tokens":4096}}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	request.Header.Set("Anthropic-Beta", "fast-mode-2026-02-01")
	ctx = svc.WithUpstreamBillingCandidateRequest(ctx, request, body, model)
	candidate := ctx.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate)
	require.NotNil(t, candidate.scope)
	require.True(t, candidate.messagesFast)
	require.Equal(t, model, candidate.messagesDispatchModel)
	if unsafe.StringData(model) == unsafe.StringData(candidate.messagesDispatchModel) {
		t.Error("Messages dispatch model retains the complete input backing")
	}
	for index := range body {
		body[index] = 'x'
	}
	require.Equal(t, "gpt-fixture", candidate.scope.Model)
	require.Equal(t, "enabled", candidate.scope.ThinkingMode)
	ctx = svc.WithUpstreamBillingCandidateRequest(ctx, request, body)
	candidate = ctx.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate)
	require.Nil(t, candidate.scope, "invalid next request must discard the previous descriptor")
	require.Empty(t, candidate.messagesDispatchModel)
	require.False(t, candidate.messagesFast)
}

type billingWSCandidateAccountRepo struct {
	AccountRepository
	account atomic.Pointer[Account]
}

func (r *billingWSCandidateAccountRepo) GetOpenAITurnAdmission(context.Context, int64) (*Account, *Account, error) {
	return r.account.Load(), nil, nil
}

func TestBillingWSCandidateMatchesPhysicalFramesAndRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool, OpenAIWSIngressModeShared, OpenAIWSIngressModeDedicated, OpenAIWSIngressModeHTTPBridge, "passthrough_size_bridge", "smart_passthrough_http", "smart_pool_http", "smart_passthrough_ws", "smart_shadow_ws", "smart_auto_http"} {
		ingressMode, smartMode := mode, ""
		switch mode {
		case "smart_passthrough_http":
			ingressMode, smartMode = OpenAIWSIngressModePassthrough, "http"
		case "smart_pool_http":
			ingressMode, smartMode = OpenAIWSIngressModeCtxPool, "http"
		case "smart_passthrough_ws":
			ingressMode, smartMode = OpenAIWSIngressModePassthrough, "ws"
		case "smart_shadow_ws":
			ingressMode, smartMode = OpenAIWSIngressModePassthrough, "shadow"
		case "smart_auto_http":
			ingressMode, smartMode = OpenAIWSIngressModePassthrough, "auto"
		}
		for _, tc := range []struct {
			name, options, tier, effort, summary, verbosity string
			encrypted                                       bool
			policy                                          *OpenAIFastPolicySettings
			force                                           bool
			compatibleBridge                                bool
			hooks                                           OpenAIWSIngressHooks
		}{
			{name: "plain", tier: "unspecified", effort: "unspecified"},
			{name: "encrypted output", options: `,"include":["reasoning.encrypted_content"]`, tier: "unspecified", effort: "unspecified", encrypted: true},
			{name: "summary and encrypted output", options: `,"include":["reasoning.encrypted_content"],"reasoning":{"effort":"high","summary":"auto"}`, tier: "unspecified", effort: "high", summary: "auto", encrypted: true},
			{name: "text verbosity", options: `,"text":{"verbosity":"low"}`, tier: "unspecified", effort: "unspecified", verbosity: "low"},
			{name: "none preserved", options: `,"reasoning":{"effort":"none"}`, tier: "unspecified", effort: "none"},
			{name: "compatible bridge strips none", options: `,"reasoning":{"effort":"none"}`, tier: "unspecified", effort: "unspecified", compatibleBridge: true},
			{name: "minimal preserved", options: `,"reasoning":{"effort":"minimal"}`, tier: "unspecified", effort: "minimal"},
			{name: "effort capped", options: `,"reasoning":{"effort":"xhigh"}`, tier: "unspecified", effort: "medium", hooks: OpenAIWSIngressHooks{MaxReasoningEffort: "medium"}},
			{name: "group priority", tier: "priority", effort: "unspecified", force: true},
			{name: "priority filtered", options: `,"service_tier":"priority"`, tier: "unspecified", effort: "unspecified", policy: openAIFastFilterPriorityPolicy()},
		} {
			bridge := mode == OpenAIWSIngressModeHTTPBridge || mode == "passthrough_size_bridge" || smartMode == "http" || smartMode == "auto"
			if tc.compatibleBridge && !bridge {
				continue
			}
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				protocol := "responses_ws_v2"
				if bridge {
					protocol = "responses_http"
				}
				wireModel := func(model string) string {
					if ingressMode != OpenAIWSIngressModePassthrough || bridge {
						return "wire-" + model
					}
					return model
				}
				observed := make(chan *AvailabilityRequestScope, 3)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if bridge {
						payload, err := io.ReadAll(request.Body)
						if err != nil {
							t.Errorf("read physical bridge request: %v", err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						observed <- availabilityScopeForRequest(request, payload)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}}\n\ndata: [DONE]\n\n")
						return
					}
					conn, err := coderws.Accept(w, request, nil)
					if err != nil {
						t.Errorf("accept physical upstream: %v", err)
						return
					}
					defer func() { _ = conn.CloseNow() }()
					wsRequest := request.Clone(request.Context())
					address := *request.URL
					address.Scheme = "ws"
					wsRequest.URL = &address
					for {
						_, payload, err := conn.Read(request.Context())
						if err != nil {
							return
						}
						observed <- availabilityScopeForRequest(wsRequest, payload)
						if err := conn.Write(request.Context(), coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_fixture","usage":{"input_tokens":4,"output_tokens":2}}}`)); err != nil {
							return
						}
					}
				}))
				defer upstream.Close()
				firstEntry := billingApplicabilityEntry(protocol, tc.tier, true)
				firstEntry["model"] = wireModel("gpt-fixture")
				firstEntry["reasoning_effort"] = tc.effort
				secondEntry := billingApplicabilityEntry(protocol, tc.tier, true)
				secondEntry["model"], secondEntry["reasoning_effort"] = wireModel("gpt-next"), tc.effort
				for _, entry := range []map[string]any{firstEntry, secondEntry} {
					if tc.verbosity != "" {
						entry["text_verbosity"] = tc.verbosity
					}
					if tc.summary != "" {
						entry["reasoning_summary"] = tc.summary
					}
					if tc.encrypted {
						entry["include_encrypted_content"] = true
					}
				}
				makeAccount := func(entries ...map[string]any) *Account {
					account := billingCandidateAccount(t, 914, entries...)
					account.Extra["openai_passthrough"] = !tc.compatibleBridge
					account.Extra["openai_responses_supported"] = true
					account.Credentials["base_url"] = upstream.URL
					account.Credentials["api_key"] = "sk-test"
					account.Credentials["model_mapping"] = map[string]any{"gpt-fixture": "wire-gpt-fixture", "gpt-next": "wire-gpt-next"}
					account.Extra["openai_apikey_responses_websockets_v2_mode"] = ingressMode
					if mode == "passthrough_size_bridge" {
						account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
					}
					at := time.Now().UTC().Add(-time.Second)
					_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entries...), at)
					require.NoError(t, err)
					return account
				}
				selected := makeAccount(firstEntry)
				repo := &billingWSCandidateAccountRepo{}
				repo.account.Store(selected)
				svc := newOpenAIGatewayServiceWithSettings(t, tc.policy)
				svc.cfg = passthroughLifecycleConfig()
				if mode == "passthrough_size_bridge" {
					svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
					svc.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
				}
				if smartMode != "" {
					svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
					svc.cfg.Gateway.OpenAIWS.SmartProtocol = config.GatewayOpenAIWSSmartProtocolConfig{Enabled: true, Mode: smartMode}
				}
				svc.cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
				svc.cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 5
				defer svc.getOpenAIWSConnPool().Close()
				svc.cache = &stubGatewayCache{}
				svc.toolCorrector = NewCodexToolCorrector()
				svc.accountRepo, svc.requireLatestTurnAdmission = repo, true
				svc.httpUpstream = &codexModelsHTTPUpstreamStub{do: func(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
					if err := CheckHTTPUpstreamSendGuard(request.Context()); err != nil {
						return nil, err
					}
					return upstream.Client().Do(request)
				}}
				settingsRepo := &billingCandidateSettingRepo{SettingRepository: svc.settingService.settingRepo}
				svc.settingService = NewSettingService(settingsRepo, svc.cfg)
				group := profitControlTestGroup(7, 0.5, 0)
				group.ForceOpenAIFast = tc.force
				body := `{"type":"response.create","model":"public-fixture","input":"private prompt","store":true,"max_output_tokens":8000,"stream":false` + tc.options + `}`
				controlCtx := billingWSCandidateContext(t, svc, group, body, "gpt-fixture", &tc.hooks)
				vetoed, reason := openAIProfitControlVetoReason(controlCtx, selected)
				require.False(t, vetoed, reason)
				settled := make(chan int, 3)
				hooks := tc.hooks
				hooks.InitialRequestModel = "public-fixture"
				hooks.MapRequestModel = func(_ int, original string) (string, error) {
					if original == "public-fixture" {
						return "gpt-fixture", nil
					}
					return original, nil
				}
				hooks.AfterTurn = func(turn int, _ *OpenAIForwardResult, _ error) { settled <- turn }
				server, done := startPassthroughLifecycleServer(t, controlCtx, svc, selected, &hooks)
				defer server.Close()
				client := dialPassthroughLifecycleClientWithPayload(t, server, body)
				defer func() { _ = client.CloseNow() }()
				for turn := 1; turn <= 2; turn++ {
					select {
					case scope := <-observed:
						require.NotNil(t, scope)
						model := "gpt-fixture"
						if turn == 2 {
							model = "gpt-next"
						}
						version := availabilityRequestScopeVersion
						if tc.summary != "" || tc.encrypted {
							version = availabilityResponsesOutputScopeVersion
						}
						if tc.verbosity != "" {
							version = availabilityResponsesTextScopeVersion
						}
						require.Equal(t, AvailabilityRequestScope{Version: version, Protocol: protocol, Stream: bridge, Model: wireModel(model), Capability: "text", ServiceTier: tc.tier, ReasoningEffort: tc.effort, ReasoningSummary: tc.summary, IncludeEncryptedContent: tc.encrypted, TextVerbosity: tc.verbosity}, *scope)
					case <-time.After(3 * time.Second):
						select {
						case err := <-done:
							t.Fatalf("relay ended before physical turn %d: %v", turn, err)
						default:
							t.Fatalf("missing physical upstream turn %d", turn)
						}
					}
					_, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
					require.NoError(t, err)
					select {
					case got := <-settled:
						require.Equal(t, turn, got)
					case <-time.After(3 * time.Second):
						t.Fatal("turn settlement did not complete")
					}
					if turn == 1 {
						firstEntry["group_rate_applies"] = false
						replacement := makeAccount(firstEntry, secondEntry)
						repo.account.Store(replacement)
						// The captured selection descriptor now vetoes the old model.
						// It must not leak into admission for the next physical frame.
						vetoed, reason = openAIProfitControlVetoReason(controlCtx, replacement)
						require.True(t, vetoed)
						require.Equal(t, "profit_upstream_scope_excluded", reason)
						next := `{"type":"response.create","model":"gpt-next","input":"next turn","store":true,"stream":false` + tc.options + `}`
						require.NoError(t, client.Write(t.Context(), coderws.MessageText, []byte(next)))
					}
				}
				// The final guard still rejects the revoked scope on a later turn;
				// clearing selection metadata must never authorize replay or bypass.
				require.NoError(t, client.Write(t.Context(), coderws.MessageText, []byte(body)))
				select {
				case err := <-done:
					if smartMode != "" && smartMode != "shadow" {
						// Strict smart sessions intentionally hide the internal admission
						// error behind the original close/no-replay contract.
						var closeErr *OpenAIWSClientCloseError
						require.ErrorAs(t, err, &closeErr)
						require.Equal(t, coderws.StatusInternalError, closeErr.StatusCode())
						require.Equal(t, "upstream session ended; request was not replayed", closeErr.Reason())
					} else {
						require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("excluded turn did not terminate the relay")
				}
				require.Empty(t, observed, "excluded third turn must never reach the physical upstream")
				require.EqualValues(t, 1, settingsRepo.fastReads.Load(), "WS entry and frames must reuse the captured fast-policy snapshot")
			})
		}
	}
}
