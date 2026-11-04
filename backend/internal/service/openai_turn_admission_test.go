package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type turnAdmissionRepo struct {
	AccountRepository
	account, parent *Account
	accounts        []*Account
	err             error
	reads           int
}

func (r *turnAdmissionRepo) GetOpenAITurnAdmission(context.Context, int64) (*Account, *Account, error) {
	r.reads++
	if len(r.accounts) > 0 {
		return r.accounts[min(r.reads-1, len(r.accounts)-1)], r.parent, r.err
	}
	return r.account, r.parent, r.err
}

func strictTurnAdmissionPlatformsForTest() []string {
	return append([]string{PlatformOpenAI, PlatformGrok}, slices.Sorted(maps.Keys(providerProfiles))...)
}

func TestOpenAITurnAdmissionStaticKeyBinding(t *testing.T) {
	for _, key := range []any{"replacement-fixture", "", nil} {
		t.Run(fmt.Sprintf("key_%v", key), func(t *testing.T) {
			selected := &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{"api_key": "original-fixture", "base_url": "https://fixture.invalid"}}
			latest := *selected
			latest.Credentials = maps.Clone(selected.Credentials)
			latest.Credentials["api_key"] = key
			svc := &OpenAIGatewayService{accountRepo: &turnAdmissionRepo{account: &latest}}
			_, err := svc.admitOpenAITurn(t.Context(), nil, selected, "gpt-5.1")
			var denied *OpenAITurnAdmissionError
			require.ErrorAs(t, err, &denied)
			require.Equal(t, "account_binding_changed", denied.Reason)
			binding := svc.bindOpenAIWSHandshake(selected, "gpt-5.1", make(http.Header))
			require.Error(t, svc.checkOpenAIWSBinding(t.Context(), &latest, "gpt-5.1", binding), "an established WS must not outlive static credential replacement")
			require.Equal(t, "original-fixture", selected.GetCredential("api_key"))
		})
	}
}

func TestOpenAITurnAdmissionFreshProfitEvidence(t *testing.T) {
	for _, platform := range strictTurnAdmissionPlatformsForTest() {
		for _, tc := range []struct {
			name, reason string
			mutate       func(*Account)
		}{
			{name: "unchanged"},
			{name: "rate increased", reason: openAIProfitFilterReasonThreshold, mutate: func(a *Account) { rate := 0.9; a.RateMultiplier = &rate }},
			{name: "sale rate changed", reason: openAIProfitFilterReasonAccountSaleRateMismatch, mutate: func(a *Account) { rate := 1.1; a.GroupRateMultiplier = &rate }},
			{name: "evidence removed", reason: openAIProfitFilterReasonUpstreamRateUnavailable, mutate: func(a *Account) { delete(a.Extra, UpstreamBillingProbeExtraKey) }},
		} {
			t.Run(platform+"/"+tc.name, func(t *testing.T) {
				selected := profitControlSyncedTestAccount(time.Now().UTC())
				selected.Platform = platform
				selected.Status, selected.Schedulable = StatusActive, true
				latest := *selected
				latest.Extra = maps.Clone(selected.Extra)
				if tc.mutate != nil {
					tc.mutate(&latest)
				}
				svc := &OpenAIGatewayService{accountRepo: &turnAdmissionRepo{account: &latest}}
				ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: platform, threshold: 0.5})
				_, err := svc.admitOpenAITurn(ctx, nil, selected, "gpt-5.1")
				if tc.reason == "" {
					require.NoError(t, err)
				} else {
					var denied *OpenAITurnAdmissionError
					require.ErrorAs(t, err, &denied)
					require.Equal(t, tc.reason, denied.Reason)
				}
				_, err = svc.admitOpenAITurn(t.Context(), nil, selected, "gpt-5.1")
				require.NoError(t, err, "the unconfigured profit gate must preserve existing behavior")
			})
		}
	}
}

func TestOpenAITurnAdmissionProfitGateBypassesLocalCache(t *testing.T) {
	for _, platform := range strictTurnAdmissionPlatformsForTest() {
		selected := profitControlSyncedTestAccount(time.Now().UTC())
		selected.Platform = platform
		selected.Status, selected.Schedulable = StatusActive, true
		repo := &turnAdmissionRepo{account: selected}
		cfg := &config.Config{}
		cfg.Gateway.Scheduling.OpenAITurnAdmissionCacheTTLSeconds = 300
		svc := &OpenAIGatewayService{accountRepo: repo, cfg: cfg}
		_, err := svc.admitOpenAITurn(t.Context(), nil, selected, "gpt-5.1")
		require.NoError(t, err)
		if platform == PlatformOpenAI {
			require.Equal(t, 1, repo.reads)
		} else {
			require.Zero(t, repo.reads, "ungated providers keep the existing direct-call boundary")
		}
		previousReads := repo.reads
		svc.openAITurnAdmissionCache.store(selected, nil)
		latest := *selected
		rate := 0.9
		latest.RateMultiplier = &rate
		repo.account = &latest
		ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: platform, threshold: 0.5})
		_, err = svc.admitOpenAITurn(ctx, nil, selected, "gpt-5.1")
		var denied *OpenAITurnAdmissionError
		require.ErrorAs(t, err, &denied)
		require.Equal(t, openAIProfitFilterReasonThreshold, denied.Reason)
		require.Equal(t, previousReads+1, repo.reads, "strict admission must not reuse an unexpired process cache")
	}
}

func TestOpenAIHTTPFinalSendRechecksPurchasedRoute(t *testing.T) {
	for _, platform := range strictTurnAdmissionPlatformsForTest() {
		for _, tc := range []struct {
			name, reason string
			mutate       func(*Account)
		}{
			{name: "unchanged"},
			{name: "static key replaced", reason: "account_binding_changed", mutate: func(a *Account) { a.Credentials["api_key"] = "replacement-fixture" }},
			{name: "static key cleared", reason: "account_binding_changed", mutate: func(a *Account) { a.Credentials["api_key"] = "" }},
			{name: "endpoint changed", reason: "account_binding_changed", mutate: func(a *Account) { a.Credentials["base_url"] = "https://different.invalid" }},
			{name: "protocol endpoint changed", reason: "account_binding_changed", mutate: func(a *Account) {
				a.Credentials["api_base_urls"] = map[string]any{APIProtocolResponses: "https://different.invalid"}
			}},
			{name: "protocol changed", reason: "account_binding_changed", mutate: func(a *Account) { a.Credentials["api_protocol"] = APIProtocolAnthropic }},
			{name: "account mode changed", reason: "account_binding_changed", mutate: func(a *Account) { a.Credentials["account_mode"] = AccountModeCoding }},
			{name: "protocol rules changed", reason: "account_binding_changed", mutate: func(a *Account) {
				a.Credentials[protocolRulesCredentialKey] = []any{map[string]any{"pattern": "gpt-*", "protocol": APIProtocolResponses}}
			}},
			{name: "group removed", reason: "group_membership_changed", mutate: func(a *Account) { a.GroupIDs = nil }},
			{name: "disabled", reason: "account_ineligible", mutate: func(a *Account) { a.Status = "disabled" }},
			{name: "purchase rate increased", reason: openAIProfitFilterReasonThreshold, mutate: func(a *Account) { rate := 0.9; a.RateMultiplier = &rate }},
			{name: "sale rate changed", reason: openAIProfitFilterReasonAccountSaleRateMismatch, mutate: func(a *Account) { rate := 1.1; a.GroupRateMultiplier = &rate }},
			{name: "evidence removed", reason: openAIProfitFilterReasonUpstreamRateUnavailable, mutate: func(a *Account) { delete(a.Extra, UpstreamBillingProbeExtraKey) }},
		} {
			t.Run(platform+"/"+tc.name, func(t *testing.T) {
				selected := profitControlSyncedTestAccount(time.Now().UTC())
				selected.Platform = platform
				selected.Status, selected.Schedulable = StatusActive, true
				selected.GroupIDs = []int64{9}
				selected.Credentials = map[string]any{"api_key": "original-fixture", "base_url": "https://fixture.invalid"}
				latest := *selected
				latest.Extra, latest.Credentials = maps.Clone(selected.Extra), maps.Clone(selected.Credentials)
				if tc.mutate != nil {
					tc.mutate(&latest)
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: &turnAdmissionRepo{account: &latest}, httpUpstream: upstream}
				ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: platform, threshold: 0.5})
				req := httptest.NewRequest(http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(`{"model":"gpt-5.1"}`)).WithContext(ctx)
				req.Header.Set("Authorization", "Bearer original-fixture")
				response, err := svc.doOpenAIUpstream(req, "", selected)
				if response != nil {
					require.NoError(t, response.Body.Close())
				}
				if tc.reason == "" {
					require.NoError(t, err)
					require.NotNil(t, response)
					require.NotNil(t, upstream.lastReq)
					require.Equal(t, "Bearer original-fixture", upstream.lastReq.Header.Get("Authorization"))
					return
				}
				var denied *OpenAITurnAdmissionError
				require.ErrorAs(t, err, &denied)
				require.Equal(t, tc.reason, denied.Reason)
				require.Nil(t, upstream.lastReq, "a prepared request must not send old credentials against new evidence")
			})
		}
	}
}

func TestOpenAIHTTPFinalSendRechecksPinnedGroup(t *testing.T) {
	for _, actual := range []string{"", "2", "3", "2,3"} {
		t.Run("actual_"+actual, func(t *testing.T) {
			selected := profitControlSyncedTestAccount(time.Now().UTC())
			selected.Credentials = map[string]any{"api_key": "pinned-route-fixture", "base_url": "https://fixture.invalid"}
			selected.Status, selected.Schedulable = StatusActive, true
			selected.GroupIDs = []int64{9}
			selected.Credentials[credKeyHeaderOverrideEnabled] = true
			selected.Credentials[credKeyHeaderOverrides] = map[string]any{"x-sub2api-group-id": "2"}
			snapshot := decodeUpstreamBillingProbeSnapshot(selected.Extra)
			snapshot.Data["selected_group_id"] = "2"
			selected.Extra[UpstreamBillingProbeExtraKey] = snapshot
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: &turnAdmissionRepo{account: selected}, httpUpstream: upstream}
			ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, threshold: 0.5})
			req := httptest.NewRequest(http.MethodPost, "https://fixture.invalid/v1/responses", nil).WithContext(ctx)
			if actual != "" {
				req.Header.Set(APIKeyGroupHeader, actual)
			}
			response, err := svc.doOpenAIUpstream(req, "", selected)
			if actual == "2" {
				require.NoError(t, err)
				require.NotNil(t, response)
				require.NoError(t, response.Body.Close())
				require.NotNil(t, upstream.lastReq)
			} else {
				var denied *OpenAITurnAdmissionError
				require.ErrorAs(t, err, &denied)
				require.Equal(t, "account_binding_changed", denied.Reason)
				require.Nil(t, upstream.lastReq)
			}
		})
	}
}

func TestOpenAIHTTPFinalSendRejectsUnobservedBillingTarget(t *testing.T) {
	for _, platform := range strictTurnAdmissionPlatformsForTest() {
		for _, tc := range []struct {
			name, target, host string
			allowed            bool
		}{
			{name: "observed responses", target: "https://fixture.invalid/tenant/v1/responses", allowed: true},
			{name: "observed chat", target: "https://fixture.invalid/tenant/v1/chat/completions", allowed: true},
			{name: "observed messages", target: "https://fixture.invalid/tenant/v1/messages", allowed: true},
			{name: "observed compact", target: "https://fixture.invalid/tenant/v1/responses/compact", allowed: true},
			{name: "different site", target: "https://other.invalid/tenant/v1/responses"},
			{name: "different tenant", target: "https://fixture.invalid/other/v1/responses"},
			{name: "prefix collision", target: "https://fixture.invalid/tenant-2/v1/responses"},
			{name: "different scheme", target: "http://fixture.invalid/tenant/v1/responses"},
			{name: "different port", target: "https://fixture.invalid:8443/tenant/v1/responses"},
			{name: "different version", target: "https://fixture.invalid/tenant/v2/responses"},
			{name: "alternate resource", target: "https://fixture.invalid/tenant/v1/images/generations"},
			{name: "host override", target: "https://fixture.invalid/tenant/v1/responses", host: "other.invalid"},
			{name: "query routing", target: "https://fixture.invalid/tenant/v1/responses?group=other"},
			{name: "encoded separator", target: "https://fixture.invalid/tenant%2Fv1/responses"},
		} {
			t.Run(platform+"/"+tc.name, func(t *testing.T) {
				account := profitControlSyncedTestAccount(time.Now().UTC())
				account.Platform, account.Status, account.Schedulable = platform, StatusActive, true
				account.GroupIDs = []int64{9}
				account.Credentials = map[string]any{"api_key": "target-fixture", "base_url": "https://fixture.invalid/tenant", "api_protocol": APIProtocolAdaptive,
					"api_base_urls": map[string]any{APIProtocolChatCompletions: "https://fixture.invalid/tenant", APIProtocolResponses: "https://other.invalid/tenant"}}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: &turnAdmissionRepo{account: account}, httpUpstream: upstream}
				ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: platform, threshold: 0.5})
				req := httptest.NewRequest(http.MethodPost, tc.target, nil).WithContext(ctx)
				if tc.host != "" {
					req.Host = tc.host
				}
				resp, err := svc.doOpenAIUpstream(req, "", account)
				if resp != nil {
					require.NoError(t, resp.Body.Close())
				}
				if tc.allowed {
					require.NoError(t, err)
					require.NotNil(t, upstream.lastReq)
					require.True(t, HTTPUpstreamRedirectsDisabled(upstream.lastReq.Context()))
				} else {
					var denied *OpenAITurnAdmissionError
					require.ErrorAs(t, err, &denied)
					require.Equal(t, openAIProfitFilterReasonUpstreamRateUnavailable, denied.Reason)
					require.Nil(t, upstream.lastReq, "unobserved billing targets must be rejected before egress")
				}
			})
		}
	}
}

func TestUpstreamBillingRequestTargetUsesObservedAPIRoot(t *testing.T) {
	for _, tc := range []struct {
		name, base, target string
		allowed            bool
	}{
		{"versioned base", "https://fixture.invalid/tenant/v4", "https://fixture.invalid/tenant/v4/responses", true},
		{"root base", "https://fixture.invalid/", "https://fixture.invalid/v1/messages", true},
		{"case insensitive host", "https://fixture.invalid/tenant", "https://FIXTURE.INVALID/tenant/v1/messages", true},
		{"full endpoint is not billing root", "https://fixture.invalid/tenant/v1/responses", "https://fixture.invalid/tenant/v1/responses", false},
		{"billing root under full endpoint", "https://fixture.invalid/tenant/v1/responses", "https://fixture.invalid/tenant/v1/responses/v1/responses", true},
		{"duplicate version is another root", "https://fixture.invalid/tenant/v1", "https://fixture.invalid/tenant/v1/v1/messages", false},
		{"encoded separator is another root", "https://fixture.invalid/tenant%2Fone", "https://fixture.invalid/tenant%2Fone/v1/messages", false},
		{"probe builder decodes separator", "https://fixture.invalid/tenant%2Fone", "https://fixture.invalid/tenant/one/v1/messages", true},
		{"probe builder trims decoded trailing separator", "https://fixture.invalid/tenant%2F", "https://fixture.invalid/tenant/v1/messages", true},
		{"query bound by probe", "https://fixture.invalid/tenant?route=cheap", "https://fixture.invalid/tenant/v1/messages?route=cheap", true},
		{"query differs from probe", "https://fixture.invalid/tenant?route=cheap", "https://fixture.invalid/tenant/v1/messages?route=premium", false},
		{"fragment not sent to probe", "https://fixture.invalid/tenant", "https://fixture.invalid/tenant/v1/messages#other", false},
		{"userinfo not probe identity", "https://fixture.invalid/tenant", "https://other@fixture.invalid/tenant/v1/messages", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": tc.base}}
			req := httptest.NewRequest(http.MethodPost, tc.target, nil)
			require.Equal(t, tc.allowed, upstreamBillingRequestTargetConfirmed(account, req))
		})
	}
	require.False(t, upstreamBillingRequestTargetConfirmed(nil, nil))
	require.False(t, upstreamBillingRequestTargetConfirmed(&Account{}, &http.Request{}))
}

func TestOpenAIHTTPBillingTargetCompatibility(t *testing.T) {
	for _, gated := range []bool{false, true} {
		for _, platform := range strictTurnAdmissionPlatformsForTest() {
			t.Run(fmt.Sprintf("%s/gated_%v", platform, gated), func(t *testing.T) {
				account := profitControlSyncedTestAccount(time.Now().UTC())
				account.Platform, account.Status, account.Schedulable = platform, StatusActive, true
				account.GroupIDs = []int64{9}
				account.Credentials = map[string]any{"api_key": "manual-fixture", "base_url": "https://fixture.invalid"}
				// With a gate this explicitly exercises the manual-rate path. Without a
				// gate, automatic sync must retain the preexisting transport behavior.
				if gated {
					account.Extra[UpstreamBillingRateSyncEnabledExtraKey] = false
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: &turnAdmissionRepo{account: account}, httpUpstream: upstream}
				ctx := t.Context()
				if gated {
					ctx = context.WithValue(ctx, openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: platform, threshold: 0.5})
				}
				req := httptest.NewRequest(http.MethodPost, "https://other.invalid/v1/responses", nil).WithContext(ctx)
				resp, err := svc.doOpenAIUpstream(req, "", account)
				require.NoError(t, err)
				require.NotNil(t, resp)
				require.NoError(t, resp.Body.Close())
				require.NotNil(t, upstream.lastReq)
				require.False(t, HTTPUpstreamRedirectsDisabled(upstream.lastReq.Context()))
			})
		}
	}
}

func TestOpenAIHTTPBillingTargetMatchesRealProbe(t *testing.T) {
	var businessCalls, premiumCalls atomic.Int32
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cheap/v1/sub2api/billing":
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":0.4,"resolved_rate_multiplier":0.4,"effective_rate_multiplier":0.4,"peak_rate_enabled":false,"observed_at":"`+now.Format(time.RFC3339Nano)+`"}`)
			require.NoError(t, err)
		case "/cheap/v1/chat/completions":
			businessCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case "/premium/v1/responses":
			premiumCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	account := profitControlSyncedTestAccount(now)
	account.Platform, account.Status, account.Schedulable = PlatformKimi, StatusActive, true
	account.GroupIDs = []int64{9}
	account.Credentials = map[string]any{"api_key": "actual-billing-target-fixture", "api_protocol": APIProtocolAdaptive,
		"base_url":      server.URL + "/legacy",
		"api_base_urls": map[string]any{APIProtocolChatCompletions: server.URL + "/cheap", APIProtocolResponses: server.URL + "/premium"}}
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		return server.Client().Do(req)
	}}
	probe := newUpstreamBillingProbeTestService(repo, upstream, &upstreamBillingProbeSettingRepo{})
	snapshot, err := probe.ProbeAccount(t.Context(), account.ID)
	require.NoError(t, err)
	require.Equal(t, UpstreamBillingProbeStatusOK, snapshot.Status)
	require.NotNil(t, snapshot.SyncedRateMultiplier)
	latest, err := repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: &turnAdmissionRepo{account: latest}, httpUpstream: upstream}
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: account.Platform, threshold: 0.5})
	// Use production endpoint builders: the Responses adapter's configured base
	// is a different tenant, although both routes share one server and API key.
	premiumURL := buildOpenAIResponsesURLForPlatform(account.Platform, account.GetCNProtocolBaseURL(APIProtocolResponses))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, premiumURL, nil)
	require.NoError(t, err)
	resp, err := svc.doOpenAIUpstream(req, "", latest)
	require.Nil(t, resp)
	var denied *OpenAITurnAdmissionError
	require.ErrorAs(t, err, &denied)
	require.Equal(t, openAIProfitFilterReasonUpstreamRateUnavailable, denied.Reason)
	require.Zero(t, premiumCalls.Load())
	cheapURL := buildOpenAIEndpointURL(account.GetOpenAIBaseURL(), "/v1/chat/completions")
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, cheapURL, nil)
	require.NoError(t, err)
	resp, err = svc.doOpenAIUpstream(req, "", latest)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.EqualValues(t, 1, businessCalls.Load())
	require.Equal(t, server.URL+"/legacy", latest.GetCredential("base_url"))
	require.Equal(t, 0.4, latest.BillingRateMultiplier())
}

func TestOpenAIWSBillingBindingUsesLatestGate(t *testing.T) {
	for _, target := range []string{"wss://fixture.invalid/cheap/v1/responses", "wss://fixture.invalid/premium/v1/responses", ""} {
		t.Run(target, func(t *testing.T) {
			account := profitControlSyncedTestAccount(time.Now().UTC())
			account.Credentials = map[string]any{"base_url": "https://fixture.invalid/cheap", "api_key": "turn-fixture"}
			svc := &OpenAIGatewayService{}
			binding := svc.bindOpenAIWSHandshake(account, "gpt-fixture", make(http.Header))
			binding.billingHandshake = newOpenAIWSBillingHandshake(WithHTTPUpstreamRedirectsDisabled(t.Context()), target, http.Header{"Authorization": {"never-retain"}})
			require.Empty(t, binding.billingHandshake.headers, "binding must not duplicate credentials")
			require.NoError(t, svc.checkOpenAIWSBinding(t.Context(), account, "gpt-fixture", binding))
			ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, threshold: 0.5})
			err := svc.checkOpenAIWSBinding(ctx, account, "gpt-fixture", binding)
			if strings.Contains(target, "/cheap/") {
				require.NoError(t, err)
			} else {
				var denied *OpenAITurnAdmissionError
				require.ErrorAs(t, err, &denied)
				require.Equal(t, openAIProfitFilterReasonUpstreamRateUnavailable, denied.Reason)
			}
		})
	}
}

func TestStrictProviderProfitAdmissionReadFailure(t *testing.T) {
	for _, platform := range strictTurnAdmissionPlatformsForTest()[1:] {
		for _, mode := range []string{"deleted", "read error", "missing reader", "missing repository"} {
			t.Run(platform+"/"+mode, func(t *testing.T) {
				selected := profitControlSyncedTestAccount(time.Now().UTC())
				selected.Platform, selected.Status, selected.Schedulable = platform, StatusActive, true
				selected.GroupIDs = []int64{9}
				repo := &turnAdmissionRepo{}
				svc := &OpenAIGatewayService{accountRepo: repo, requireLatestTurnAdmission: true}
				switch mode {
				case "read error":
					repo.err = errors.New("fixture database unavailable")
				case "missing reader":
					svc.accountRepo = &upstreamBillingProbeAccountRepo{}
				case "missing repository":
					svc.accountRepo = nil
				}
				ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: platform, threshold: 0.5})
				req := httptest.NewRequest(http.MethodPost, "https://fixture.invalid/v1/responses", nil).WithContext(ctx)
				response, err := svc.doOpenAIUpstream(req, "", selected)
				var denied *OpenAITurnAdmissionError
				require.ErrorAs(t, err, &denied)
				require.Equal(t, "latest_state_unavailable", denied.Reason)
				require.Nil(t, response, "no transport is installed: rejection must precede all sends")
				previousReads := repo.reads
				ungated, err := svc.admitOpenAITurn(t.Context(), nil, selected, "grok-4.5")
				require.NoError(t, err)
				require.Same(t, selected, ungated)
				require.Equal(t, previousReads, repo.reads)
			})
		}
	}
}

func TestGrokStrictProfitAdmissionPreservesOAuthRefresh(t *testing.T) {
	selected := profitControlSyncedTestAccount(time.Now().UTC())
	selected.Platform, selected.Type = PlatformGrok, AccountTypeOAuth
	selected.Status, selected.Schedulable = StatusActive, true
	selected.Credentials = map[string]any{"base_url": "https://fixture.invalid", "access_token": "old-fixture"}
	latest := *selected
	latest.Credentials = maps.Clone(selected.Credentials)
	latest.Credentials["access_token"] = "refreshed-fixture"
	repo := &turnAdmissionRepo{account: &latest}
	svc := &OpenAIGatewayService{accountRepo: repo, requireLatestTurnAdmission: true}
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: PlatformGrok, threshold: 0.5})
	admitted, err := svc.admitOpenAITurn(ctx, nil, selected, "grok-4.5")
	require.NoError(t, err)
	require.Same(t, &latest, admitted)
	require.Equal(t, "refreshed-fixture", admitted.GetCredential("access_token"))
	require.Equal(t, 1, repo.reads)
}

func TestOpenAITurnAdmissionLatestState(t *testing.T) {
	for _, change := range []string{"disabled", "paused", "expired", "removed_group", "deleted", "db_error", "binding", "credentials", "credential_route", "platform", "cooldown", "model_block", "persisted_model_limit", "quality_isolation", "fingerprint", "ws_mode", "proxy_endpoint"} {
		t.Run(change, func(t *testing.T) {
			selected := ticketTestAccount(901)
			selected.GroupIDs = []int64{9}
			latest := *selected
			latest.Extra = maps.Clone(selected.Extra)
			if latest.Extra == nil {
				latest.Extra = make(map[string]any)
			}
			repo := &turnAdmissionRepo{account: &latest}
			s := &OpenAIGatewayService{accountRepo: repo}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			group := int64(9)
			c.Set("api_key", &APIKey{GroupID: &group})
			switch change {
			case "disabled":
				latest.Status = "disabled"
			case "paused":
				latest.Schedulable = false
			case "expired":
				past := time.Now().Add(-time.Second)
				latest.ExpiresAt, latest.AutoPauseOnExpired = &past, true
			case "removed_group":
				latest.GroupIDs = []int64{10}
			case "deleted":
				repo.account = nil
			case "db_error":
				repo.err = errors.New("unavailable")
			case "binding":
				latest.ParentAccountID = func() *int64 {
					id := int64(902)
					return &id
				}()
			case "credentials":
				latest.Credentials = maps.Clone(selected.Credentials)
				latest.Credentials["access_token"] = "different"
				latest.Credentials["_token_version"] = int64(2)
			case "credential_route":
				latest.Credentials = map[string]any{"base_url": "https://changed.example.invalid"}
			case "platform":
				latest.Platform = PlatformKimi
			case "cooldown":
				s.openaiAccountRuntimeBlockUntil.Store(latest.ID, time.Now().Add(time.Minute))
			case "model_block":
				s.recordOpenAIAccountModelTransientFailure(&latest, "gpt-6-astra", time.Now())
				s.recordOpenAIAccountModelTransientFailure(&latest, "gpt-6-astra", time.Now())
			case "persisted_model_limit":
				latest.Extra[modelRateLimitsKey] = map[string]any{
					"gpt-6-astra": map[string]any{"rate_limit_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)},
				}
			case "quality_isolation":
				latest.Extra[QualityModelIsolationKey] = map[string]any{"gpt-6-astra": map[string]any{"reason": "quality_rule:7"}}
			case "fingerprint":
				latest.Extra[codexFingerprintSeedExtraKey] = "changed-synthetic-seed"
			case "ws_mode":
				latest.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeOff
			case "proxy_endpoint":
				id := int64(22)
				latest.ProxyID, selected.ProxyID = &id, &id
				selected.Proxy = &Proxy{ID: id, Protocol: "http", Host: "old.invalid", Port: 80}
				latest.Proxy = &Proxy{ID: id, Protocol: "http", Host: "new.invalid", Port: 80}
			}
			_, err := s.admitOpenAITurn(context.Background(), c, selected, "gpt-6-astra")
			if change == "credentials" {
				require.NoError(t, err)
			} else {
				require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
			}
			require.Equal(t, 1, repo.reads)
			require.True(t, selected.Schedulable)
			require.Nil(t, selected.TempUnschedulableUntil)
			if err != nil {
				require.Same(t, err, s.handleOpenAIUpstreamTransportError(context.Background(), c, selected, err, false))
			}
		})
	}
}

func TestOpenAITurnAdmissionLocalCache(t *testing.T) {
	selected := ticketTestAccount(980)
	selected.GroupIDs = []int64{9}
	latest := *selected
	latest.Credentials = maps.Clone(selected.Credentials)
	latest.Extra = maps.Clone(selected.Extra)
	repo := &turnAdmissionRepo{account: &latest}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.OpenAITurnAdmissionCacheTTLSeconds = 300
	svc := &OpenAIGatewayService{accountRepo: repo, cfg: cfg}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	group := int64(9)
	c.Set("api_key", &APIKey{GroupID: &group})

	first, err := svc.admitOpenAITurn(context.Background(), c, selected, "gpt-6-astra")
	require.NoError(t, err)
	require.Equal(t, 1, repo.reads)
	first.Credentials["base_url"] = "https://mutated.example.invalid"

	second, err := svc.admitOpenAITurn(context.Background(), c, selected, "gpt-6-astra")
	require.NoError(t, err)
	require.Equal(t, 1, repo.reads, "cache hit must avoid another authoritative read")
	require.NotEqual(t, "https://mutated.example.invalid", second.Credentials["base_url"])
}

func TestOpenAITurnAdmissionExplicitGroupRejectsRemovedMembership(t *testing.T) {
	selected := ticketTestAccount(905)
	selected.GroupIDs = []int64{9}
	latest := *selected
	latest.GroupIDs = []int64{10}
	repo := &turnAdmissionRepo{account: &latest}
	s := &OpenAIGatewayService{accountRepo: repo}

	_, err := s.admitOpenAITurnForGroup(context.Background(), 9, true, selected, "gpt-5.5")

	require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
	var denied *OpenAITurnAdmissionError
	require.ErrorAs(t, err, &denied)
	require.Equal(t, "group_membership_changed", denied.Reason)
	require.Equal(t, 1, repo.reads)
}

func TestOpenAITurnAdmissionSimpleModeDoesNotRequireGroupMembership(t *testing.T) {
	selected := ticketTestAccount(906)
	selected.GroupIDs = []int64{9}
	latest := *selected
	latest.GroupIDs = nil
	repo := &turnAdmissionRepo{account: &latest}
	s := &OpenAIGatewayService{
		accountRepo: repo,
		cfg:         &config.Config{RunMode: config.RunModeSimple},
	}

	groupID := int64(9)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("api_key", &APIKey{GroupID: &groupID})

	got, err := s.admitOpenAITurn(context.Background(), c, selected, "gpt-5.5")

	require.NoError(t, err)
	require.Same(t, &latest, got)
	require.Equal(t, 1, repo.reads)
}

type turnAdmissionDialerFunc func(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error)

func (f turnAdmissionDialerFunc) Dial(ctx context.Context, url string, headers http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
	return f(ctx, url, headers, proxy)
}

func TestOpenAITurnAdmissionAfterDialBeforeLease(t *testing.T) {
	selected := ticketTestAccount(7)
	latest := *selected
	repo := &turnAdmissionRepo{account: &latest}
	s := &OpenAIGatewayService{accountRepo: repo}
	pool := newOpenAIWSConnPool(&config.Config{})
	defer pool.Close()
	upstream := newStagedPassthroughConn()
	pool.setClientDialerForTest(turnAdmissionDialerFunc(func(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
		// Simulate an admin update after the pre-dial read, not an actual DB mutation.
		latest.Schedulable = false
		return upstream, http.StatusSwitchingProtocols, http.Header{}, nil
	}))
	lease, err := pool.Acquire(context.Background(), openAIWSAcquireRequest{
		Account: selected, WSURL: "ws://loopback.invalid/responses",
		HeadersFactory: func(ctx context.Context, headers http.Header) (http.Header, error) {
			_, err := s.admitOpenAITurn(ctx, nil, selected, "gpt-5.5")
			return headers, err
		},
		BindHandshake: func(h http.Header) *openAIWSTurnBinding {
			return s.bindOpenAIWSHandshake(selected, "gpt-5.5", h)
		},
		CheckBinding: func(ctx context.Context, b *openAIWSTurnBinding) error {
			a, err := s.admitOpenAITurn(ctx, nil, selected, "gpt-5.5")
			if err != nil {
				return err
			}
			return s.checkOpenAIWSBinding(t.Context(), a, "gpt-5.5", b)
		},
	})
	require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
	require.Nil(t, lease)
	require.Empty(t, upstream.writes)
	select {
	case <-upstream.closed:
	default:
		t.Fatal("rejected lease was not released/closed")
	}
}

func TestOpenAITurnAdmissionRequiresPrimaryReader(t *testing.T) {
	s := &OpenAIGatewayService{requireLatestTurnAdmission: true}
	_, err := s.admitOpenAITurn(context.Background(), nil, ticketTestAccount(1), "gpt-5.5")
	require.True(t, IsOpenAITurnAdmissionError(err))
	s.accountRepo = &turnAdmissionRepo{account: ticketTestAccount(1)}
	_, err = s.admitOpenAITurn(context.Background(), nil, ticketTestAccount(1), "gpt-5.5")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.admitOpenAITurn(ctx, nil, ticketTestAccount(1), "gpt-5.5")
	require.ErrorIs(t, err, context.Canceled)
}

func TestOpenAITurnAdmissionReaderChecksLifecycle(t *testing.T) {
	for _, name := range []string{"disabled", "paused", "expired", "cooldown"} {
		t.Run(name, func(t *testing.T) {
			account := ticketTestAccount(17)
			switch name {
			case "disabled":
				account.Status = StatusDisabled
			case "paused":
				account.Schedulable = false
			case "expired":
				expired := time.Now().Add(-time.Second)
				account.AutoPauseOnExpired = true
				account.ExpiresAt = &expired
			case "cooldown":
				until := time.Now().Add(time.Minute)
				account.TempUnschedulableUntil = &until
			}
			s := &OpenAIGatewayService{accountRepo: &turnAdmissionRepo{account: account}}
			_, err := s.AdmitOpenAITurn(context.Background(), nil, account, "gpt-5.5")
			require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
		})
	}
}

func TestOpenAITurnAdmissionFailureInvalidatesOnlyCurrentSessionState(t *testing.T) {
	const groupID int64 = 41
	store := NewOpenAIWSStateStore(nil)
	s := &OpenAIGatewayService{openaiWSStateStore: store}

	require.NoError(t, store.BindResponseAccount(context.Background(), groupID, "resp_rejected", 901, time.Hour))
	store.BindResponseConn("resp_rejected", "conn-rejected", time.Hour)
	store.BindSessionTurnState(groupID, "session-rejected", "turn-state", time.Hour)
	store.BindSessionConn(groupID, "session-rejected", "conn-rejected", time.Hour)

	require.NoError(t, store.BindResponseAccount(context.Background(), groupID, "resp-unrelated", 902, time.Hour))
	store.BindResponseConn("resp-unrelated", "conn-unrelated", time.Hour)
	store.BindSessionTurnState(groupID, "session-unrelated", "turn-state", time.Hour)
	store.BindSessionConn(groupID, "session-unrelated", "conn-unrelated", time.Hour)

	s.invalidateOpenAIWSTurnStateAfterAdmissionFailure(
		context.Background(),
		groupID,
		"session-rejected",
		"resp_rejected",
		901,
		&OpenAITurnAdmissionError{Reason: "account_ineligible"},
	)

	accountID, err := store.GetResponseAccount(context.Background(), groupID, "resp_rejected")
	require.NoError(t, err)
	require.Zero(t, accountID)
	_, responseConnExists := store.GetResponseConn("resp_rejected")
	require.False(t, responseConnExists)
	_, turnStateExists := store.GetSessionTurnState(groupID, "session-rejected")
	require.False(t, turnStateExists)
	_, sessionConnExists := store.GetSessionConn(groupID, "session-rejected")
	require.False(t, sessionConnExists)

	unrelatedAccountID, err := store.GetResponseAccount(context.Background(), groupID, "resp-unrelated")
	require.NoError(t, err)
	require.Equal(t, int64(902), unrelatedAccountID)
	unrelatedConn, ok := store.GetResponseConn("resp-unrelated")
	require.True(t, ok)
	require.Equal(t, "conn-unrelated", unrelatedConn)
	unrelatedState, ok := store.GetSessionTurnState(groupID, "session-unrelated")
	require.True(t, ok)
	require.Equal(t, "turn-state", unrelatedState)
	unrelatedSessionConn, ok := store.GetSessionConn(groupID, "session-unrelated")
	require.True(t, ok)
	require.Equal(t, "conn-unrelated", unrelatedSessionConn)
}

func TestOpenAITurnAdmissionFailureDoesNotClearStateOnControlPlaneReadError(t *testing.T) {
	const groupID int64 = 42
	store := NewOpenAIWSStateStore(nil)
	s := &OpenAIGatewayService{openaiWSStateStore: store}
	require.NoError(t, store.BindResponseAccount(context.Background(), groupID, "resp-read-error", 903, time.Hour))
	store.BindSessionConn(groupID, "session-read-error", "conn-read-error", time.Hour)

	s.invalidateOpenAIWSTurnStateAfterAdmissionFailure(
		context.Background(),
		groupID,
		"session-read-error",
		"resp-read-error",
		903,
		&OpenAITurnAdmissionError{Reason: "latest_state_unavailable"},
	)

	accountID, err := store.GetResponseAccount(context.Background(), groupID, "resp-read-error")
	require.NoError(t, err)
	require.Equal(t, int64(903), accountID)
	connID, ok := store.GetSessionConn(groupID, "session-read-error")
	require.True(t, ok)
	require.Equal(t, "conn-read-error", connID)
}

func TestOpenAITurnAdmissionFailureCleanupUnwrapsClientCloseError(t *testing.T) {
	const groupID int64 = 43
	const sessionHash = "session-wrapped-admission"
	const responseID = "resp-wrapped-admission"
	store := NewOpenAIWSStateStore(nil)
	s := &OpenAIGatewayService{openaiWSStateStore: store}

	require.NoError(t, store.BindResponseAccount(context.Background(), groupID, responseID, 904, time.Hour))
	store.BindResponseConn(responseID, "conn-wrapped-admission", time.Hour)
	store.BindSessionTurnState(groupID, sessionHash, "turn-state-wrapped", time.Hour)
	store.BindSessionConn(groupID, sessionHash, "conn-wrapped-admission", time.Hour)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(openAIWSIngressSessionHashContextKey, sessionHash)
	group := groupID
	c.Set("api_key", &APIKey{GroupID: &group})
	wrapped := NewOpenAIWSClientCloseError(
		coderws.StatusTryAgainLater,
		"account eligibility changed",
		&OpenAITurnAdmissionError{Reason: "account_ineligible"},
	)
	s.invalidateOpenAIWSTurnStateAfterAdmissionFailureForRequest(
		context.Background(),
		c,
		[]byte(`{"type":"response.create","previous_response_id":"resp-wrapped-admission"}`),
		904,
		wrapped,
	)

	accountID, err := store.GetResponseAccount(context.Background(), groupID, responseID)
	require.NoError(t, err)
	require.Zero(t, accountID)
	_, responseConnExists := store.GetResponseConn(responseID)
	require.False(t, responseConnExists)
	_, turnStateExists := store.GetSessionTurnState(groupID, sessionHash)
	require.False(t, turnStateExists)
	_, sessionConnExists := store.GetSessionConn(groupID, sessionHash)
	require.False(t, sessionConnExists)
}

func TestOpenAITurnAdmissionShadowParent(t *testing.T) {
	a := ticketTestAccount(1)
	parent := ticketTestAccount(2)
	a.ParentAccountID = &parent.ID
	parent.Schedulable = false
	future := time.Now().Add(time.Hour)
	parent.RateLimitResetAt = &future
	parent.OverloadUntil = &future
	repo := &turnAdmissionRepo{account: a, parent: parent}
	s := &OpenAIGatewayService{accountRepo: repo}
	_, err := s.admitOpenAITurn(context.Background(), nil, a, "gpt-5.3-codex-spark")
	require.NoError(t, err, "global parent quota must not pause the independent shadow")
	parent.TempUnschedulableUntil = &future
	_, err = s.admitOpenAITurn(context.Background(), nil, a, "gpt-5.3-codex-spark")
	require.True(t, IsOpenAITurnAdmissionError(err))
}

func TestOpenAITurnAdmissionModelAndHandshakeBinding(t *testing.T) {
	a := ticketTestAccount(1)
	attachReadyCodexTicket(a, "gpt-5.6-sol")
	s := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true}, nil)
	h := http.Header{}
	require.NoError(t, s.applyOpenAICodexTicket(context.Background(), a, "gpt-5.6-sol", h))
	b := s.bindOpenAIWSHandshake(a, "gpt-5.6-sol", h)
	_, err := s.admitOpenAITurn(context.Background(), nil, a, "gpt-6-astra")
	require.ErrorIs(t, err, ErrOpenAICodexTicketUnavailable)
	_, err = s.admitOpenAITurn(context.Background(), nil, a, "gpt-5.6-sol")
	require.NoError(t, err)
	require.NoError(t, s.checkOpenAIWSBinding(t.Context(), a, "gpt-5.6-sol", b))
	primary := *s.lookupOpenAICodexTicket(a, "gpt-5.6-sol")
	standby := primary
	standby.State = fakeCodexTicketState(292)[:291] + "A"
	primary.Standby = &standby
	s.openaiCodexTickets.Store(openAICodexTicketKey(a.ID, "gpt-5.6-sol"), &primary)
	require.NoError(t, s.checkOpenAIWSBinding(t.Context(), a, "gpt-5.6-sol", b), "standby refresh must not force reconnection")
	// A valid inventory does not extend a connection's original handshake.
	b.ticket.ExpiresAt = time.Now().Add(-time.Second)
	require.Error(t, s.checkOpenAIWSBinding(t.Context(), a, "gpt-5.6-sol", b))
	require.True(t, a.Schedulable)
	require.Nil(t, a.TempUnschedulableUntil)
}

func TestOpenAITurnAdmissionMissingHandshakeTicketFailsClosed(t *testing.T) {
	a := ticketTestAccount(2)
	s := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:    true,
		FailClosed: true,
		Models:     []string{"gpt-6-astra"},
	}, nil)
	binding := &openAIWSTurnBinding{
		model:       "gpt-6-astra",
		fingerprint: AccountRouteFingerprint(a),
		createdAt:   time.Now(),
	}

	err := s.checkOpenAIWSBinding(t.Context(), a, "gpt-6-astra", binding)

	var denied *OpenAITurnAdmissionError
	require.ErrorAs(t, err, &denied)
	require.Equal(t, "connection_ticket_missing", denied.Reason)
}

func TestOpenAITurnAdmissionBindingUsesFinalHeaders(t *testing.T) {
	a := ticketTestAccount(1)
	attachReadyCodexTicket(a, "gpt-6-astra")
	a.Extra[codexFingerprintSeedExtraKey] = testCodexFingerprintSeed
	a.Extra[codexFingerprintModeExtraKey] = string(codexFingerprintFull)
	s := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true}, nil)
	pool := newOpenAIWSConnPool(&config.Config{})
	defer pool.Close()
	upstream := newStagedPassthroughConn()
	pool.setClientDialerForTest(&stagedPassthroughDialer{conn: upstream})
	req := openAIWSAcquireRequest{
		Account: a, WSURL: "ws://loopback.invalid/responses", Headers: http.Header{},
		HeadersFactory: func(ctx context.Context, h http.Header) (http.Header, error) {
			h.Set("session_id", "factory-session")
			return h, s.applyOpenAICodexTicket(ctx, a, "gpt-6-astra", h)
		},
		BindHandshake: func(h http.Header) *openAIWSTurnBinding {
			return s.bindOpenAIWSHandshake(a, "gpt-6-astra", h)
		},
	}
	conn, err := pool.dialConn(context.Background(), req)
	require.NoError(t, err)
	defer conn.close()
	require.NotNil(t, conn.turnBinding.ticket)
	require.Equal(t, "factory-session", conn.handshakeCompatibility.sessionIDUnderscore)
	require.NoError(t, s.checkOpenAIWSBinding(t.Context(), a, "gpt-6-astra", conn.turnBinding))
}

func TestQualityIsolationAllowsOnlyInternalRecoveryProbeAdmission(t *testing.T) {
	selected := ticketTestAccount(901)
	latest := *selected
	latest.Extra = maps.Clone(selected.Extra)
	if latest.Extra == nil {
		latest.Extra = map[string]any{}
	}
	latest.Extra[QualityModelIsolationKey] = map[string]any{"gpt-6-astra": map[string]any{"reason": "quality_rule:7"}}
	repo := &turnAdmissionRepo{account: &latest}
	s := &OpenAIGatewayService{accountRepo: repo}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	_, err := s.admitOpenAITurn(context.Background(), c, selected, "gpt-6-astra")
	require.True(t, IsOpenAITurnAdmissionError(err))
	ctx := context.WithValue(context.Background(), qualityProbeContextKey{}, true)
	_, err = s.admitOpenAITurn(ctx, c, selected, "gpt-6-astra")
	require.NoError(t, err, "the model must remain testable for independent recovery")
}
