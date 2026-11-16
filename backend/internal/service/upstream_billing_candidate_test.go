package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func billingCandidateAccount(t *testing.T, id int64, entries ...map[string]any) *Account {
	t.Helper()
	account := billingManualAccount()
	account.ID, account.Status, account.Schedulable, account.Concurrency = id, StatusActive, true, 2
	account.GroupIDs = []int64{7}
	account.Extra["openai_passthrough"] = true
	at := time.Now().UTC().Add(-time.Second)
	_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entries...), at)
	require.NoError(t, err)
	return account
}

func billingCandidateContext(t *testing.T, svc *OpenAIGatewayService, group *Group, body string) context.Context {
	t.Helper()
	ctx, _ := svc.WithOpenAIRequestPricingContext(profitControlTestCtx(group), &group.ID)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	return svc.WithUpstreamBillingCandidateRequest(ctx, request, []byte(body))
}

func TestBillingCandidateSchedulerFiltersAndPreservesSticky(t *testing.T) {
	for _, tc := range []struct{ engine, protocol string }{{"true", "responses_http"}, {"false", "responses_http"}, {"true", "responses_native"}, {"false", "responses_native"}, {"true", "chat_http"}, {"false", "chat_http"}, {"true", "responses_ws_v2"}, {"false", "responses_ws_v2"}} {
		t.Run("advanced="+tc.engine+"/"+tc.protocol, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
			newAccount := func(id int64, covered bool) *Account {
				entry := billingApplicabilityEntry(tc.protocol, "priority", covered)
				if tc.protocol == "responses_native" {
					entry["protocol"] = "responses_http"
				}
				account := billingCandidateAccount(t, id, entry)
				if tc.protocol == "responses_native" {
					account.Extra["openai_passthrough"] = false
				}
				if tc.protocol == "responses_ws_v2" {
					account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
				}
				if tc.protocol != "responses_http" {
					if tc.protocol == "chat_http" {
						account.Extra["openai_responses_mode"] = "force_chat_completions"
					}
					at := time.Now().UTC().Add(-time.Second)
					_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
					require.NoError(t, err)
				}
				return account
			}
			excluded := newAccount(901, false)
			covered := newAccount(902, true)
			cache := &stubGatewayCache{sessionBindings: map[string]int64{"scope-sticky": excluded.ID}}
			concurrency := &upstreamCostTrackingConcurrencyCache{loadMap: map[int64]*AccountLoadInfo{
				excluded.ID: {AccountID: excluded.ID}, covered.ID: {AccountID: covered.ID},
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, cache: cache,
				accountRepo:      schedulerTestOpenAIAccountRepo{accounts: []Account{*excluded, *covered}},
				rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(tc.engine), concurrencyService: NewConcurrencyService(concurrency)}
			group := profitControlTestGroup(7, 0.5, 0)
			ctx := billingCandidateContext(t, svc, group, `{"model":"gpt-fixture","input":"private prompt","service_tier":"priority"}`)
			if tc.protocol == "responses_ws_v2" {
				svc.cfg = passthroughLifecycleConfig()
				ctx = billingWSCandidateContext(t, svc, group, `{"type":"response.create","model":"gpt-fixture","input":"private prompt","service_tier":"priority"}`, "gpt-fixture", nil)
			}
			for _, session := range []string{"", "scope-sticky"} {
				selection, _, err := svc.SelectAccountWithScheduler(ctx, &group.ID, "", session, "gpt-fixture", nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.Equal(t, covered.ID, selection.Account.ID, "an excluded scope must not win even through a sticky binding")
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			}
			require.Equal(t, excluded.ID, cache.sessionBindings["scope-sticky"], "scope veto must preserve the original binding")
			require.Zero(t, cache.deletedSessions["scope-sticky"])
			selection, _, err := svc.SelectAccountWithScheduler(ctx, &group.ID, "", "", "gpt-fixture", map[int64]struct{}{covered.ID: {}}, OpenAIUpstreamTransportAny, false)
			require.Nil(t, selection, "failover must not readmit the excluded scope")
			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			if tc.engine == "true" {
				require.Contains(t, err.Error(), "profit_upstream_scope_excluded=1")
			}
			recovered := newAccount(excluded.ID, true)
			svc.accountRepo = schedulerTestOpenAIAccountRepo{accounts: []Account{*recovered, *covered}}
			selection, _, err = svc.SelectAccountWithScheduler(ctx, &group.ID, "", "scope-sticky", "gpt-fixture", map[int64]struct{}{covered.ID: {}}, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, recovered.ID, selection.Account.ID)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		})
	}
}

func TestBillingCandidateUsesActualTierAndEffortTransforms(t *testing.T) {
	for _, tc := range []struct {
		name, body, tier, effort string
		policy                   *OpenAIFastPolicySettings
		force, official          bool
	}{
		{name: "plain", body: `{"model":"gpt-fixture","input":"hello"}`, tier: "unspecified", effort: "unspecified"},
		{name: "group forces priority", body: `{"model":"gpt-fixture"}`, tier: "priority", effort: "unspecified", force: true},
		{name: "filter priority", body: `{"model":"gpt-fixture","service_tier":"priority"}`, tier: "unspecified", effort: "unspecified", policy: openAIFastFilterPriorityPolicy()},
		{name: "filter overrides group force", body: `{"model":"gpt-fixture"}`, tier: "unspecified", effort: "unspecified", force: true, policy: openAIFastFilterPriorityPolicy()},
		{name: "missing forces priority", body: `{"model":"gpt-fixture"}`, tier: "priority", effort: "unspecified", policy: &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierMissing, Action: OpenAIFastPolicyActionForcePriority, Scope: BetaPolicyScopeAll}}}},
		{name: "compatible passthrough none preserved", body: `{"model":"gpt-fixture","reasoning":{"effort":"none"}}`, tier: "unspecified", effort: "none"},
		{name: "official none preserved", body: `{"model":"gpt-fixture","reasoning":{"effort":"none"}}`, tier: "unspecified", effort: "none", official: true},
		{name: "high preserved", body: `{"model":"gpt-fixture","reasoning":{"effort":"high"}}`, tier: "unspecified", effort: "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newOpenAIGatewayServiceWithSettings(t, tc.policy)
			group := profitControlTestGroup(7, 0.5, 0)
			group.ForceOpenAIFast = tc.force
			entry := billingApplicabilityEntry("responses_http", tc.tier, true)
			entry["reasoning_effort"] = tc.effort
			account := billingCandidateAccount(t, 903, entry)
			if tc.official {
				account.Credentials["base_url"] = "https://api.openai.com"
			}
			// Rebind after the route change; mapping must also remain unused by
			// native passthrough, which sends the already channel-mapped model.
			account.Credentials["model_mapping"] = map[string]any{"gpt-fixture": "never-sent"}
			at := time.Now().UTC().Add(-time.Second)
			_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
			require.NoError(t, err)
			ctx := billingCandidateContext(t, svc, group, tc.body)
			vetoed, reason := openAIProfitControlVetoReason(ctx, account)
			require.False(t, vetoed, reason)
			request := httptest.NewRequest(http.MethodPost, "https://relay.example/v1/responses", nil)
			body, err := filterOpenAIResponsesNoneReasoningEffortForAccount(account, []byte(tc.body))
			require.NoError(t, err)
			body, err = svc.applyOpenAIFastPolicyToBody(ctx, account, "gpt-fixture", body)
			require.NoError(t, err)
			scope := availabilityScopeForRequest(request, body)
			require.NotNil(t, scope)
			require.Equal(t, tc.tier, scope.ServiceTier)
			require.Equal(t, tc.effort, scope.ReasoningEffort)
			require.NoError(t, guardUpstreamBillingApplicability(account, scope))
			// Both candidate and physical guard must reject the same transformed
			// scope when the supplier excludes it.
			entry["group_rate_applies"] = false
			_, err = prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
			require.NoError(t, err)
			vetoed, reason = openAIProfitControlVetoReason(ctx, account)
			require.True(t, vetoed)
			require.Equal(t, "profit_upstream_scope_excluded", reason)
			require.True(t, IsOpenAITurnAdmissionError(guardUpstreamBillingApplicability(account, scope)))
		})
	}
}

func TestBillingCandidatePostSlotRefreshAndFinalSend(t *testing.T) {
	svc := &OpenAIGatewayService{}
	selected := billingCandidateAccount(t, 904, billingApplicabilityEntry("responses_http", "unspecified", true))
	replacement := billingCandidateAccount(t, selected.ID, billingApplicabilityEntry("responses_http", "unspecified", false))
	svc.schedulerSnapshot = NewSchedulerSnapshotService(&gatewayProfitSnapshotCache{account: replacement}, nil, gatewayProfitAccountRepo{}, nil, nil)
	// Install the gate without a group snapshot, then test account refresh.
	ctx := billingCandidateContext(t, &OpenAIGatewayService{}, profitControlTestGroup(7, 0.5, 0), `{"model":"gpt-fixture"}`)
	latest, vetoed, reason := svc.ProfitControlVetoLatest(ctx, selected)
	require.Same(t, replacement, latest)
	require.True(t, vetoed)
	require.Equal(t, "profit_upstream_scope_excluded", reason)
	// A later physical tier change is still checked against the actual body.
	request := httptest.NewRequest(http.MethodPost, "https://relay.example/v1/responses", strings.NewReader(`{"model":"gpt-fixture","service_tier":"priority"}`))
	require.NoError(t, guardUpstreamBillingApplicability(selected, availabilityScopeForRequest(request, []byte(`{"model":"gpt-fixture"}`))))
	require.True(t, IsOpenAITurnAdmissionError(guardUpstreamBillingHTTPApplicability(selected, request)))
}

func TestBillingCandidateResponsesToChatScope(t *testing.T) {
	for _, tc := range []struct {
		name, body, model, tier, effort string
		policy                          *OpenAIFastPolicySettings
		covered, force, deferScope      bool
		passthrough, official           bool
	}{
		{name: "covered", body: `{"model":"gpt-fixture","input":"private prompt"}`, covered: true},
		{name: "excluded", body: `{"model":"gpt-fixture","input":"private prompt"}`},
		{name: "mapped model", body: `{"model":"gpt-fixture","input":[{"role":"user","content":"private prompt"}]}`, model: "wire-fixture"},
		{name: "mapped model covered", body: `{"model":"gpt-fixture","input":"private prompt"}`, model: "wire-fixture", covered: true},
		{name: "compatible none omitted", body: `{"model":"gpt-fixture","input":"private prompt","stream":true,"reasoning":{"effort":"none"}}`},
		{name: "passthrough none preserved", body: `{"model":"gpt-fixture","input":"private prompt","reasoning":{"effort":"none"}}`, effort: "none", passthrough: true},
		{name: "official none preserved", body: `{"model":"gpt-fixture","input":"private prompt","reasoning":{"effort":"none"}}`, effort: "none", official: true},
		{name: "explicit high", body: `{"model":"gpt-fixture","input":"private prompt","reasoning":{"effort":"high"}}`, effort: "high"},
		{name: "group priority", body: `{"model":"gpt-fixture","input":"private prompt"}`, force: true, tier: "priority"},
		{name: "priority filtered", body: `{"model":"gpt-fixture","input":"private prompt","service_tier":"priority"}`, policy: openAIFastFilterPriorityPolicy()},
		{name: "policy block retains original error", body: `{"model":"gpt-fixture","input":"private prompt","service_tier":"priority"}`, tier: "priority", deferScope: true, policy: &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierPriority, Action: BetaPolicyActionBlock, Scope: BetaPolicyScopeAll}}}},
		{name: "instructions only", body: `{"model":"gpt-fixture","instructions":"private system"}`},
		{name: "missing content deferred", body: `{"model":"gpt-fixture"}`, deferScope: true},
		{name: "empty array deferred", body: `{"model":"gpt-fixture","input":[]}`, deferScope: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := billingApplicabilityEntry("chat_http", "unspecified", tc.covered)
			if tc.model != "" {
				entry["model"] = tc.model
			}
			if tc.tier != "" {
				entry["service_tier"] = tc.tier
			}
			if tc.effort != "" {
				entry["reasoning_effort"] = tc.effort
			}
			account := billingCandidateAccount(t, 909, entry)
			account.Extra["openai_responses_mode"] = "force_chat_completions"
			account.Extra["openai_passthrough"] = tc.passthrough
			if tc.official {
				account.Credentials["base_url"] = "https://api.openai.com"
			}
			if tc.model != "" {
				account.Credentials["model_mapping"] = map[string]any{"gpt-fixture": tc.model}
			}
			at := time.Now().UTC().Add(-time.Second)
			_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
			require.NoError(t, err)
			svc := newOpenAIGatewayServiceWithSettings(t, tc.policy)
			group := profitControlTestGroup(7, 0.5, 0)
			group.ForceOpenAIFast = tc.force
			ctx := billingCandidateContext(t, svc, group, tc.body)
			vetoed, reason := openAIProfitControlVetoReason(ctx, account)
			require.Equal(t, !tc.covered && !tc.deferScope, vetoed, reason)
			if vetoed {
				require.Equal(t, "profit_upstream_scope_excluded", reason)
			}
		})
	}
}

func TestBillingCandidateNativeResponsesScope(t *testing.T) {
	for _, tc := range []struct {
		name, body, model, tier, effort string
		policy                          *OpenAIFastPolicySettings
		covered, force, official        bool
	}{
		{name: "covered", body: `{"model":"gpt-fixture","input":"private prompt"}`, covered: true},
		{name: "excluded", body: `{"model":"gpt-fixture","input":"private prompt"}`},
		{name: "mapped excluded", body: `{"model":"gpt-fixture","input":"private prompt"}`, model: "wire-fixture"},
		{name: "mapped covered", body: `{"model":"gpt-fixture","input":"private prompt"}`, model: "wire-fixture", covered: true},
		{name: "compatible none omitted", body: `{"model":"gpt-fixture","input":"private prompt","reasoning":{"effort":"none"}}`},
		{name: "official none preserved", body: `{"model":"gpt-fixture","input":"private prompt","reasoning":{"effort":"none"}}`, effort: "none", official: true},
		{name: "minimal normalized to none", body: `{"model":"gpt-fixture","input":"private prompt","reasoning":{"effort":"minimal"}}`, effort: "none"},
		{name: "high retained", body: `{"model":"gpt-fixture","input":"private prompt","reasoning":{"effort":"high"}}`, effort: "high"},
		{name: "group priority", body: `{"model":"gpt-fixture","input":"private prompt"}`, force: true, tier: "priority"},
		{name: "priority filtered", body: `{"model":"gpt-fixture","input":"private prompt","service_tier":"priority"}`, policy: openAIFastFilterPriorityPolicy()},
		{name: "missing forces priority", body: `{"model":"gpt-fixture","input":"private prompt"}`, tier: "priority", policy: &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierMissing, Action: OpenAIFastPolicyActionForcePriority, Scope: BetaPolicyScopeAll}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.model == "" {
				tc.model = "gpt-fixture"
			}
			if tc.tier == "" {
				tc.tier = "unspecified"
			}
			if tc.effort == "" {
				tc.effort = "unspecified"
			}
			entry := billingApplicabilityEntry("responses_http", tc.tier, tc.covered)
			entry["model"], entry["reasoning_effort"] = tc.model, tc.effort
			account := billingCandidateAccount(t, 915, entry)
			account.Extra["openai_passthrough"] = false
			account.Credentials["model_mapping"] = map[string]any{"gpt-fixture": tc.model}
			if tc.official {
				account.Credentials["base_url"] = "https://api.openai.com"
			}
			at := time.Now().UTC().Add(-time.Second)
			_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
			require.NoError(t, err)
			svc := newOpenAIGatewayServiceWithSettings(t, tc.policy)
			group := profitControlTestGroup(7, 0.5, 0)
			group.ForceOpenAIFast = tc.force
			ctx := billingCandidateContext(t, svc, group, tc.body)
			vetoed, reason := openAIProfitControlVetoReason(ctx, account)
			require.Equal(t, !tc.covered, vetoed, reason)
			if vetoed {
				require.Equal(t, "profit_upstream_scope_excluded", reason)
			}
		})
	}
}

func TestBillingCandidateDeferredPathsAndCompatibility(t *testing.T) {
	for _, name := range []string{"legacy", "gate disabled", "suppressed", "native dynamic transport", "native mapped media", "chat without text", "managed credentials", "shadow credentials", "websocket", "tools", "lite", "compact", "conflicting fields", "reset unknown", "policy block", "policy unavailable"} {
		t.Run(name, func(t *testing.T) {
			account := billingCandidateAccount(t, 905, billingApplicabilityEntry("responses_http", "unspecified", false))
			svc := &OpenAIGatewayService{}
			group := profitControlTestGroup(7, 0.5, 0)
			body, path, method := `{"model":"gpt-fixture"}`, "/v1/responses", http.MethodPost
			ctx := profitControlTestCtx(group)
			switch name {
			case "legacy":
				delete(account.Extra, UpstreamBillingManualApplicabilityExtraKey)
			case "gate disabled":
				group.ProfitControlEnabled = false
			case "suppressed":
				ctx = WithOpenAIProfitControlSuppressed(ctx)
			case "native dynamic transport":
				account.Extra["openai_passthrough"] = false
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
				svc.cfg = passthroughLifecycleConfig()
			case "native mapped media":
				account.Extra["openai_passthrough"] = false
				account.Credentials["model_mapping"] = map[string]any{"gpt-fixture": "gpt-image-1"}
			case "chat without text":
				account.Extra["openai_responses_mode"] = "force_chat_completions"
			case "managed credentials", "shadow credentials":
				at := time.Now().UTC().Add(-time.Second)
				account = billingApplicabilityAccount(t, at, billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", false)))
				account.Extra["openai_passthrough"] = true
				if name == "managed credentials" {
					account.Extra["openai_connections_managed"] = true
				} else {
					parentID := int64(908)
					account.ParentAccountID = &parentID
				}
			case "websocket":
				method, path, body = http.MethodGet, "wss://relay.example/v1/responses", `{"type":"response.create","model":"gpt-fixture"}`
			case "tools":
				body = `{"model":"gpt-fixture","tools":[{"type":"web_search"}]}`
			case "compact":
				path = "/v1/responses/compact"
			case "conflicting fields":
				body = `{"model":"other","model":"gpt-fixture"}`
			case "policy block":
				svc = newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierPriority, Action: BetaPolicyActionBlock, Scope: BetaPolicyScopeAll}}})
				body = `{"model":"gpt-fixture","service_tier":"priority"}`
			case "policy unavailable":
				svc.settingService = NewSettingService(&billingCandidateSettingRepo{}, &config.Config{})
			}
			if strings.HasPrefix(name, "native ") || name == "chat without text" {
				at := time.Now().UTC().Add(-time.Second)
				_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, billingApplicabilityEntry("responses_http", "unspecified", false)), at)
				require.NoError(t, err)
			}
			ctx, _ = svc.WithOpenAIRequestPricingContext(ctx, &group.ID)
			request := httptest.NewRequest(method, path, strings.NewReader(body))
			if name == "lite" {
				request.Header.Set(responsesLiteHeader, "true")
			}
			ctx = svc.WithUpstreamBillingCandidateRequest(ctx, request, []byte(body))
			if name == "reset unknown" {
				ctx = svc.WithUpstreamBillingCandidateRequest(ctx, request, []byte(`{"model":"gpt-fixture","tools":[{"type":"web_search"}]}`))
			}
			vetoed, reason := openAIProfitControlVetoReason(ctx, account)
			require.False(t, vetoed, reason)
		})
	}
}

type billingCandidateSettingRepo struct {
	SettingRepository
	reads     atomic.Int64
	fastReads atomic.Int64
}

func (r *billingCandidateSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	r.reads.Add(1)
	if key == SettingKeyOpenAIFastPolicySettings {
		r.fastReads.Add(1)
	}
	if r.SettingRepository == nil {
		return "", errors.New("fixture settings unavailable")
	}
	return r.SettingRepository.GetValue(ctx, key)
}

func TestBillingCandidateDeferredMediaKeepsDeclarationValidity(t *testing.T) {
	for _, state := range []string{"active", "expired", "binding_changed"} {
		t.Run(state, func(t *testing.T) {
			at := time.Now().UTC().Add(-time.Second)
			entry := billingApplicabilityEntry("responses_http", "unspecified", true)
			account := billingCandidateAccount(t, 916, entry)
			account.Extra["openai_passthrough"] = false
			account.Credentials["model_mapping"] = map[string]any{"gpt-fixture": "gpt-image-1"}
			_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
			require.NoError(t, err)
			expected := ""
			if state == "expired" {
				at = at.Add(2 * time.Hour)
			} else if state == "binding_changed" {
				account.Credentials["api_key"] = "changed-fixture-key"
			}
			if state != "active" {
				expected = "profit_upstream_scope_" + state
			}
			ctx := billingCandidateContext(t, &OpenAIGatewayService{}, profitControlTestGroup(7, 0.5, 0), `{"model":"gpt-fixture","input":"private prompt"}`)
			require.Equal(t, expected, upstreamBillingCandidateVetoReason(ctx, account, at), "deferring a media scope must retain generic declaration rejection")
		})
	}
}

func TestBillingCandidateDeferredPolicyKeepsDeclarationValidity(t *testing.T) {
	for _, protocol := range []string{"responses_http", "chat_http"} {
		for _, state := range []string{"active", "expired", "binding_changed"} {
			t.Run(protocol+"/"+state, func(t *testing.T) {
				entry := billingApplicabilityEntry(protocol, "priority", true)
				account := billingCandidateAccount(t, 917, entry)
				if protocol == "chat_http" {
					account.Extra["openai_responses_mode"] = "force_chat_completions"
				}
				at := time.Now().UTC().Add(-time.Second)
				_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
				require.NoError(t, err)
				svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{
					ServiceTier: OpenAIFastTierPriority, Action: BetaPolicyActionBlock, Scope: BetaPolicyScopeAll,
				}}})
				ctx := billingCandidateContext(t, svc, profitControlTestGroup(7, 0.5, 0), `{"model":"gpt-fixture","input":"private prompt","service_tier":"priority"}`)
				expected := ""
				if state == "expired" {
					at = at.Add(2 * time.Hour)
				} else if state == "binding_changed" {
					account.Credentials["api_key"] = "changed-fixture-key"
				}
				if state != "active" {
					expected = "profit_upstream_scope_" + state
				}
				require.Equal(t, expected, upstreamBillingCandidateVetoReason(ctx, account, at), "policy error defers only precise scope, not declaration validity")
			})
		}
	}
}

func TestBillingCandidateGlobalDeclarationValidity(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"unresolved valid scope", ""},
		{"expired automatic", "profit_upstream_scope_expired"},
		{"invalid automatic", "profit_upstream_scope_invalid"},
		{"changed manual binding", "profit_upstream_scope_binding_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().UTC().Add(-time.Minute)
			var declaration any = billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", true))
			if tc.name == "expired automatic" {
				declaration.(map[string]any)["expires_at"] = at.Add(time.Second).Format(time.RFC3339Nano)
			} else if tc.name == "invalid automatic" {
				declaration = nil
			}
			account := billingApplicabilityAccount(t, at, declaration)
			if tc.name == "changed manual binding" {
				account = billingCandidateAccount(t, 907, billingApplicabilityEntry("responses_http", "unspecified", true))
				account.Credentials["api_key"] = "changed-fixture-key"
			}
			group := profitControlTestGroup(7, 0.5, 0)
			ctx, _ := (&OpenAIGatewayService{}).WithOpenAIRequestPricingContext(profitControlTestCtx(group), &group.ID)
			vetoed, reason := openAIProfitControlVetoReason(ctx, account)
			require.Equal(t, tc.reason != "", vetoed)
			require.Equal(t, tc.reason, reason)
		})
	}
}

func TestBillingCandidatePolicySnapshotAndPromptIsolation(t *testing.T) {
	svc := newOpenAIGatewayServiceWithSettings(t, openAIFastFilterPriorityPolicy())
	repo := &billingCandidateSettingRepo{SettingRepository: svc.settingService.settingRepo}
	svc.settingService = NewSettingService(repo, &config.Config{})
	group := profitControlTestGroup(7, 0.5, 0)
	ctx := context.WithValue(profitControlTestCtx(group), ctxkey.UserID, int64(42))
	ctx, _ = svc.WithOpenAIRequestPricingContext(ctx, &group.ID)
	body := []byte(`{"model":"gpt-fixture","input":"private prompt","service_tier":"priority"}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx = svc.WithUpstreamBillingCandidateRequest(ctx, request, body)
	stored := ctx.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate)
	require.NotNil(t, stored.scope)
	require.True(t, stored.policyKnown)
	require.Same(t, stored.fastPolicy, openAIFastPolicySettingsFromContext(ctx))
	require.Equal(t, AvailabilityRequestScope{Version: availabilityRequestScopeVersion, Protocol: "responses_http", Model: "gpt-fixture", Capability: "text", ServiceTier: "priority", ReasoningEffort: "unspecified"}, *stored.scope)
	// The caller may reuse/mutate its body buffer without changing the scope.
	for i := range body {
		body[i] = 'x'
	}
	account := billingCandidateAccount(t, 906, billingApplicabilityEntry("responses_http", "unspecified", true))
	for i := 0; i < 100; i++ {
		vetoed, reason := openAIProfitControlVetoReason(ctx, account)
		require.False(t, vetoed, reason)
	}
	_, err := svc.applyOpenAIFastPolicyToBody(ctx, account, "gpt-fixture", []byte(`{"model":"gpt-fixture","service_tier":"priority"}`))
	require.NoError(t, err)
	require.EqualValues(t, 1, repo.reads.Load(), "candidate iteration and final forwarding reuse one request policy read")
}
