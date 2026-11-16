package service

import (
	"context"
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
	for _, tc := range []struct {
		name, options, tier, effort string
		hooks                       *OpenAIWSIngressHooks
		policy                      *OpenAIFastPolicySettings
		force                       bool
	}{
		{name: "plain", tier: "unspecified", effort: "unspecified"},
		{name: "none preserved", options: `,"reasoning":{"effort":"none"}`, tier: "unspecified", effort: "none"},
		{name: "ceiling", options: `,"reasoning":{"effort":"xhigh"}`, tier: "unspecified", effort: "medium", hooks: &OpenAIWSIngressHooks{MaxReasoningEffort: "medium"}},
		{name: "forced effort", tier: "unspecified", effort: "high", hooks: &OpenAIWSIngressHooks{ForcedReasoningEffort: "high"}},
		{name: "client model policy before mapping", options: `,"reasoning":{"effort":"low"}`, tier: "unspecified", effort: "high", hooks: &OpenAIWSIngressHooks{ReasoningEffortMappings: []ReasoningEffortMapping{{From: "low", To: "high", MatchType: "exact", Model: "public-fixture"}}}},
		{name: "group priority", tier: "priority", effort: "unspecified", force: true},
		{name: "priority filtered", options: `,"service_tier":"priority"`, tier: "unspecified", effort: "unspecified", policy: openAIFastFilterPriorityPolicy()},
		{name: "filter overrides group", tier: "unspecified", effort: "unspecified", force: true, policy: openAIFastFilterPriorityPolicy()},
		{name: "missing forces priority", tier: "priority", effort: "unspecified", policy: &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierMissing, Action: OpenAIFastPolicyActionForcePriority, Scope: BetaPolicyScopeAll}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newOpenAIGatewayServiceWithSettings(t, tc.policy)
			svc.cfg = passthroughLifecycleConfig()
			entry := billingApplicabilityEntry("responses_ws_v2", tc.tier, true)
			entry["reasoning_effort"] = tc.effort
			account := billingCandidateAccount(t, 911, entry)
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
			account.Credentials["model_mapping"] = map[string]any{"gpt-fixture": "never-sent"}
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
			wire, blocked, err := svc.applyOpenAIFastPolicyToWSResponseCreate(ctx, account, "gpt-fixture", wire)
			require.NoError(t, err)
			require.Nil(t, blocked)
			request := httptest.NewRequest(http.MethodGet, "wss://relay.example/v1/responses", nil)
			scope := availabilityScopeForRequest(request, wire)
			require.NotNil(t, scope)
			require.Equal(t, "gpt-fixture", scope.Model)
			require.Equal(t, tc.tier, scope.ServiceTier)
			require.Equal(t, tc.effort, scope.ReasoningEffort)
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

func TestBillingWSCandidateDeferredPaths(t *testing.T) {
	for _, name := range []string{"ctx pool", "http bridge", "original byte size bridge", "smart protocol", "global force http", "account off", "mode router off", "lite", "tools", "missing type", "query", "reasoning denied", "fast blocked", "unknown policy", "gate disabled", "suppressed", "not upgraded"} {
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
			case "ctx pool":
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
			case "http bridge":
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeHTTPBridge
			case "original byte size bridge":
				svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
				svc.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 100
				body = `{"type":"response.create","model":"gpt-fixture","input":"` + strings.Repeat("x", 200) + `"}`
				require.True(t, svc.shouldBridgeOpenAIWSPassthroughFirstMessage(account, []byte(body)))
			case "smart protocol":
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
	require.Same(t, stored.fastPolicy, openAIFastPolicySettingsFromContext(ctx))
	turnCtx := WithoutUpstreamBillingCandidateRequest(ctx)
	cleared := turnCtx.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate)
	require.Nil(t, cleared.scope)
	require.Nil(t, cleared.gateway)
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
	vetoed, reason = openAIProfitControlVetoReason(unknown, account)
	require.False(t, vetoed, reason)
}

func TestBillingWSCandidatePostSlotRecheck(t *testing.T) {
	makeAccount := func(covered bool) *Account {
		entry := billingApplicabilityEntry("responses_ws_v2", "unspecified", covered)
		account := billingCandidateAccount(t, 915, entry)
		account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
		at := time.Now().UTC().Add(-time.Second)
		_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
		require.NoError(t, err)
		return account
	}
	selected, replacement := makeAccount(true), makeAccount(false)
	svc := &OpenAIGatewayService{cfg: passthroughLifecycleConfig()}
	ctx := billingWSCandidateContext(t, svc, profitControlTestGroup(7, 0.5, 0), `{"type":"response.create","model":"gpt-fixture"}`, "gpt-fixture", nil)
	vetoed, reason := openAIProfitControlVetoReason(ctx, selected)
	require.False(t, vetoed, reason)
	svc.schedulerSnapshot = NewSchedulerSnapshotService(&gatewayProfitSnapshotCache{account: replacement}, nil, gatewayProfitAccountRepo{}, nil, nil)
	latest, vetoed, reason := svc.ProfitControlVetoLatest(ctx, selected)
	require.Same(t, replacement, latest)
	require.True(t, vetoed)
	require.Equal(t, "profit_upstream_scope_excluded", reason)
}

func TestBillingCandidateMetadataDoesNotRetainInputBacking(t *testing.T) {
	backing := "gpt-fixturepriorityhigh" + strings.Repeat("private prompt", 1024)
	model, tier, effort := backing[:11], backing[11:19], backing[19:23]
	scope := &AvailabilityRequestScope{Model: model, ServiceTier: tier, ReasoningEffort: effort}
	ctx := (&OpenAIGatewayService{}).withUpstreamBillingCandidatePolicy(t.Context(), upstreamBillingCandidate{scope: scope})
	stored := ctx.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate).scope
	for _, tc := range []struct{ name, original, stored string }{
		{"model", model, stored.Model}, {"tier", tier, stored.ServiceTier}, {"effort", effort, stored.ReasoningEffort},
	} {
		require.Equal(t, tc.original, tc.stored)
		if unsafe.StringData(tc.original) == unsafe.StringData(tc.stored) {
			t.Errorf("%s retains the complete input backing", tc.name)
		}
	}
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
	for _, tc := range []struct {
		name, options, tier, effort string
		policy                      *OpenAIFastPolicySettings
		force                       bool
		hooks                       OpenAIWSIngressHooks
	}{
		{name: "plain", tier: "unspecified", effort: "unspecified"},
		{name: "none preserved", options: `,"reasoning":{"effort":"none"}`, tier: "unspecified", effort: "none"},
		{name: "effort capped", options: `,"reasoning":{"effort":"xhigh"}`, tier: "unspecified", effort: "medium", hooks: OpenAIWSIngressHooks{MaxReasoningEffort: "medium"}},
		{name: "group priority", tier: "priority", effort: "unspecified", force: true},
		{name: "priority filtered", options: `,"service_tier":"priority"`, tier: "unspecified", effort: "unspecified", policy: openAIFastFilterPriorityPolicy()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := make(chan *AvailabilityRequestScope, 3)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
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
			firstEntry := billingApplicabilityEntry("responses_ws_v2", tc.tier, true)
			firstEntry["reasoning_effort"] = tc.effort
			secondEntry := billingApplicabilityEntry("responses_ws_v2", tc.tier, true)
			secondEntry["model"], secondEntry["reasoning_effort"] = "gpt-next", tc.effort
			makeAccount := func(entries ...map[string]any) *Account {
				account := billingCandidateAccount(t, 914, entries...)
				account.Credentials["base_url"] = upstream.URL
				account.Credentials["api_key"] = "sk-test"
				account.Credentials["model_mapping"] = map[string]any{"gpt-fixture": "never-sent", "gpt-next": "gpt-next"}
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
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
			svc.cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
			svc.cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 5
			svc.cache = &stubGatewayCache{}
			svc.toolCorrector = NewCodexToolCorrector()
			svc.accountRepo, svc.requireLatestTurnAdmission = repo, true
			settingsRepo := &billingCandidateSettingRepo{SettingRepository: svc.settingService.settingRepo}
			svc.settingService = NewSettingService(settingsRepo, svc.cfg)
			group := profitControlTestGroup(7, 0.5, 0)
			group.ForceOpenAIFast = tc.force
			body := `{"type":"response.create","model":"public-fixture","input":"private prompt","stream":false` + tc.options + `}`
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
					require.Equal(t, AvailabilityRequestScope{Version: availabilityRequestScopeVersion, Protocol: "responses_ws_v2", Model: model, Capability: "text", ServiceTier: tc.tier, ReasoningEffort: tc.effort}, *scope)
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
					next := `{"type":"response.create","model":"gpt-next","input":"next turn","stream":false` + tc.options + `}`
					require.NoError(t, client.Write(t.Context(), coderws.MessageText, []byte(next)))
				}
			}
			// The final guard still rejects the revoked scope on a later turn;
			// clearing selection metadata must never authorize replay or bypass.
			require.NoError(t, client.Write(t.Context(), coderws.MessageText, []byte(body)))
			select {
			case err := <-done:
				require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("excluded turn did not terminate the relay")
			}
			require.Empty(t, observed, "excluded third turn must never reach the physical upstream")
			require.EqualValues(t, 1, settingsRepo.fastReads.Load(), "WS entry and frames must reuse the captured fast-policy snapshot")
		})
	}
}
