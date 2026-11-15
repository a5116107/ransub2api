package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type newAPIAccounts struct {
	*upstreamBillingProbeAccountRepo
}

func (r *newAPIAccounts) ListAllWithFilters(context.Context, string, string, string, string, int64, string) ([]Account, error) {
	out := []Account{}
	for _, a := range r.accounts {
		out = append(out, *a)
	}
	return out, nil
}

type newAPIAuthRepoFake struct {
	NewAPIAuthorizationRepository
	profile    *NewAPISiteAuthorization
	bindings   map[int64]*NewAPIAccountBinding
	saves      int
	writeCheck func(context.Context)
}

func (r *newAPIAuthRepoFake) GetProfile(context.Context, string, int64) (*NewAPISiteAuthorization, error) {
	return r.profile, nil
}
func (r *newAPIAuthRepoFake) GetBinding(_ context.Context, id int64) (*NewAPIAccountBinding, error) {
	return r.bindings[id], nil
}
func (r *newAPIAuthRepoFake) Save(_ context.Context, p *NewAPISiteAuthorization, b []NewAPIBindingSave) error {
	r.saves++
	r.profile = p
	for _, v := range b {
		r.bindings[v.Account.ID] = &NewAPIAccountBinding{Profile: *p, AccountID: v.Account.ID, TokenID: v.TokenID, Fingerprint: NewAPIAccountFingerprint(v.Account)}
	}
	return nil
}

type newAPIEncryptorFake struct{}

func (newAPIEncryptorFake) Encrypt(s string) (string, error) { return "encrypted-" + s, nil }
func (newAPIEncryptorFake) Decrypt(s string) (string, error) {
	return strings.TrimPrefix(s, "encrypted-"), nil
}
func newAPIConfigFixture() (*UpstreamBillingProbeService, *newAPIAuthRepoFake, *newAPIHTTP, *newAPIAccounts) {
	accounts := &newAPIAccounts{&upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{1: {ID: 1, Name: "first", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://example.com/v1", "api_key": "sk-abcd123456789wxyz"}}, 2: {ID: 2, Name: "peer", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://example.com", "api_key": "sk-nope123456789xxxx"}}, 3: {ID: 3, Name: "foreign", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://example.com/tenant", "api_key": "sk-abcd123456789wxyz"}}}}}
	h := &newAPIHTTP{bodies: map[string]string{"GET /api/status": `{"success":true,"data":{"quota_per_unit":500000}}`, "GET /api/user/self": `{"success":true,"data":{"id":7,"quota":1250000}}`, "GET /api/pricing": `{"success":true,"group_ratio":{"vip":0.5}}`, "GET /api/token/?p=0&page_size=100": `{"success":true,"data":{"items":[{"id":3,"key":"abcd**********wxyz","group":"vip","name":"mine"}],"total":1,"page":1,"page_size":100}}`}}
	h.bodies["POST /api/token/3/key"] = `{"success":true,"data":{"key":"sk-abcd123456789wxyz"}}`
	h.bodies["POST /api/token/4/key"] = `{"success":true,"data":{"key":"sk-nope123456789xxxx"}}`
	s := newUpstreamBillingProbeTestService(accounts, h, nil)
	r := &newAPIAuthRepoFake{bindings: map[int64]*NewAPIAccountBinding{}}
	s.SetNewAPIAuthorization(r, newAPIEncryptorFake{}, true)
	return s, r, h, accounts
}
func TestNewAPIUniqueMaskedKeyRequiresFullVerification(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		verified       bool
	}{
		{"matching raw key", `{"success":true,"data":{"key":"sk-abcd123456789wxyz"}}`, true},
		{"single colliding mask", `{"success":true,"data":{"key":"sk-abcdDIFFERENTwxyz"}}`, false},
		{"endpoint still masked", `{"success":true,"data":{"key":"abcd**********wxyz"}}`, false},
		{"empty key", `{"success":true,"data":{"key":""}}`, false},
		{"failed endpoint", `{"success":false,"data":{"key":"sk-abcd123456789wxyz"}}`, false},
		{"missing endpoint", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, upstream, _ := newAPIConfigFixture()
			if tc.response != "" {
				upstream.bodies["POST /api/token/3/key"] = tc.response
			} else {
				delete(upstream.bodies, "POST /api/token/3/key")
			}
			req := NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}}
			preview, err := s.PreviewNewAPIConfig(t.Context(), 1, req)
			require.NoError(t, err)
			require.Equal(t, tc.verified, preview.Accounts[0].Matched)
			_, err = s.SaveNewAPIConfig(t.Context(), 1, req)
			if tc.verified {
				require.NoError(t, err)
				require.Equal(t, 1, repo.saves)
			} else {
				require.Error(t, err)
				require.Zero(t, repo.saves)
				require.Nil(t, preview.Accounts[0].Rate)
			}
		})
	}
}

func TestNewAPICompleteInventoryKeyProof(t *testing.T) {
	for _, tc := range []struct {
		name, credential, inventory string
		verified                    bool
	}{
		{"exact key", "sk-abcd123456789wxyz", "sk-abcd123456789wxyz", true},
		{"optional prefix and whitespace", "  sk-abcd123456789wxyz  ", " abcd123456789wxyz ", true},
		{"case is significant", "sk-abcd123456789wxyz", "sk-abcd123456789WXYZ", false},
		{"masked credential is not proof", "sk-abcd**********wxyz", "abcd**********wxyz", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, upstream, accounts := newAPIConfigFixture()
			accounts.accounts[1].Credentials["api_key"] = tc.credential
			upstream.bodies["GET /api/token/?p=0&page_size=100"] = fmt.Sprintf(`{"success":true,"data":{"items":[{"id":3,"key":%q,"group":"vip"}],"total":1,"page":1,"page_size":100}}`, tc.inventory)
			upstream.check = func(req *http.Request) {
				require.False(t, strings.HasSuffix(req.URL.Path, "/key"), "complete inventory needs no key reveal")
			}
			req := NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}}
			preview, err := s.PreviewNewAPIConfig(t.Context(), 1, req)
			require.NoError(t, err)
			require.Equal(t, tc.verified, preview.Accounts[0].Matched)
			encoded, err := json.Marshal(preview)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "abcd123456789wxyz")
			require.NotContains(t, string(encoded), "fixture-secret")
			_, err = s.SaveNewAPIConfig(t.Context(), 1, req)
			if tc.verified {
				require.NoError(t, err)
				probe, err := s.probeNewAPIAccount(t.Context(), accounts.accounts[1], repo.bindings[1], 30)
				require.NoError(t, err)
				require.Equal(t, UpstreamBillingProbeStatusOK, probe.Status)
			} else {
				require.Error(t, err)
				require.Zero(t, repo.saves)
			}
		})
	}
}

func TestNewAPIProbeRechecksUniqueMaskedKeyAndRecovers(t *testing.T) {
	s, repo, upstream, accounts := newAPIConfigFixture()
	upstream.bodies["POST /api/token/3/key"] = `{"success":true,"data":{"key":"sk-abcd123456789wxyz"}}`
	_, err := s.SaveNewAPIConfig(t.Context(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
	require.NoError(t, err)
	for _, tc := range []struct{ key, status string }{
		{"sk-abcd123456789wxyz", UpstreamBillingProbeStatusOK},
		{"sk-abcdDIFFERENTwxyz", UpstreamBillingProbeStatusFailed},
		{"sk-abcd123456789wxyz", UpstreamBillingProbeStatusOK},
		{"", UpstreamBillingProbeStatusFailed},
		{"sk-abcd123456789wxyz", UpstreamBillingProbeStatusOK},
	} {
		upstream.bodies["POST /api/token/3/key"] = fmt.Sprintf(`{"success":true,"data":{"key":%q}}`, tc.key)
		if tc.key == "" {
			delete(upstream.bodies, "POST /api/token/3/key")
		}
		snapshot, err := s.probeNewAPIAccount(t.Context(), accounts.accounts[1], repo.bindings[1], 30)
		require.NoError(t, err)
		require.Equal(t, tc.status, snapshot.Status)
		if tc.status == UpstreamBillingProbeStatusFailed {
			if tc.key == "" {
				require.Equal(t, "new_api_request_failed", snapshot.LastError)
				require.Equal(t, http.StatusNotFound, snapshot.HTTPStatus)
			} else {
				require.Equal(t, "new_api_bound_key_not_owned", snapshot.LastError)
				require.Zero(t, snapshot.HTTPStatus)
			}
			require.Nil(t, snapshot.Data)
			require.Nil(t, snapshot.FreshUntil)
			require.Nil(t, snapshot.ReceivedAt)
		}
		stored := decodeUpstreamBillingProbeSnapshot(accounts.accounts[1].Extra)
		require.NotNil(t, stored)
		require.Equal(t, tc.status, stored.Status)
		if tc.status == UpstreamBillingProbeStatusFailed {
			require.Nil(t, stored.Data)
			require.Nil(t, stored.FreshUntil)
		}
	}
}

func newAPIPricingApplicabilityFixture(t *testing.T, at time.Time) map[string]any {
	t.Helper()
	declaration := billingApplicabilityFixture(at,
		billingApplicabilityEntry("responses_http", "unspecified", true),
		billingApplicabilityEntry("responses_http", "priority", false))
	declaration["group_rate_multiplier"] = 0.5
	return map[string]any{"success": true, "group_ratio": map[string]any{"vip": 0.5},
		"pricing_version": "fixture-1", "rate_applicability": map[string]any{"vip": declaration}}
}

func TestNewAPIBillingApplicabilityExactSourceAndFinalSend(t *testing.T) {
	s, repo, upstream, accounts := newAPIConfigFixture()
	at := time.Now().UTC().Add(-time.Minute)
	pricing := newAPIPricingApplicabilityFixture(t, at)
	body, err := json.Marshal(pricing)
	require.NoError(t, err)
	upstream.bodies["GET /api/pricing"] = string(body)
	_, err = s.SaveNewAPIConfig(t.Context(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
	require.NoError(t, err)
	account := accounts.accounts[1]
	rate := 0.5
	account.RateMultiplier = &rate
	account.Extra = map[string]any{UpstreamBillingProbeEnabledExtraKey: true, UpstreamBillingRateSyncEnabledExtraKey: true}
	snapshot, err := s.probeNewAPIAccount(t.Context(), account, repo.bindings[1], 30)
	require.NoError(t, err)
	require.Equal(t, "ok", snapshot.Status)
	declaration, present := upstreamBillingApplicabilityFromData(snapshot.Data)
	require.True(t, present, "the verified fixed group must retain its own explicit supplier contract")
	require.NotNil(t, declaration)
	require.Equal(t, "new_api.pricing", declaration.Source)
	require.Equal(t, "fixture-1", declaration.SourceVersion)
	require.Equal(t, "vip", snapshot.Data["group"])
	for _, tc := range []struct {
		payload, verdict string
	}{
		{`{"model":"gpt-fixture","input":"hello"}`, "covered"},
		{`{"model":"gpt-fixture","input":"hello","service_tier":"priority"}`, "excluded"},
		{`{"model":"gpt-fixture","input":"hello","reasoning":{"effort":"high"}}`, "unknown"},
	} {
		request, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", strings.NewReader(tc.payload))
		require.NoError(t, err)
		scope := availabilityScopeForRequest(request, []byte(tc.payload))
		verdict, _ := upstreamBillingScopeVerdictAt(account, scope, time.Now())
		require.Equal(t, tc.verdict, verdict)
		_, err = guardUpstreamBillingRequest(t.Context(), account, request)
		if tc.verdict == "covered" {
			require.NoError(t, err)
		} else {
			require.True(t, IsOpenAITurnAdmissionError(err))
		}
	}
	// Full snapshot JSON is an admin-facing projection, not management access.
	projection, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NotContains(t, string(projection), "fixture-secret")
	require.NotContains(t, string(projection), "abcd123456789wxyz")
}

func TestNewAPIBillingApplicabilityBindingValidation(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"matching source", func(map[string]any) {}, "covered"},
		{"other group is not borrowed", func(p map[string]any) {
			p["rate_applicability"] = map[string]any{"other": p["rate_applicability"].(map[string]any)["vip"]}
		}, "invalid"},
		{"explicit null", func(p map[string]any) { p["rate_applicability"] = nil }, "invalid"},
		{"source version required", func(p map[string]any) { delete(p, "pricing_version") }, "invalid"},
		{"source version mismatch", func(p map[string]any) { p["pricing_version"] = "changed" }, "invalid"},
		{"rate changed", func(p map[string]any) { p["group_ratio"] = map[string]any{"vip": 0.6} }, "invalid"},
		{"future observation", func(p map[string]any) {
			d := p["rate_applicability"].(map[string]any)["vip"].(map[string]any)
			d["observed_at"] = at.Add(time.Hour)
			d["expires_at"] = at.Add(2 * time.Hour)
		}, "invalid"},
		{"expired observation", func(p map[string]any) {
			p["rate_applicability"].(map[string]any)["vip"].(map[string]any)["expires_at"] = at.Add(time.Second)
		}, "expired"},
		{"untrusted source stripped", func(p map[string]any) {
			p["rate_applicability"].(map[string]any)["vip"].(map[string]any)["source"] = "https://credential.example/TOKEN"
		}, "covered"},
		{"native catalog is not a declaration", func(p map[string]any) {
			delete(p, "rate_applicability")
			p["data"] = []any{map[string]any{"model_name": "gpt-fixture", "quota_type": 0, "enable_groups": []string{"vip"}}}
		}, "not_declared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, upstream, accounts := newAPIConfigFixture()
			pricing := newAPIPricingApplicabilityFixture(t, at)
			tc.mutate(pricing)
			body, err := json.Marshal(pricing)
			require.NoError(t, err)
			upstream.bodies["GET /api/pricing"] = string(body)
			_, err = s.SaveNewAPIConfig(t.Context(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
			require.NoError(t, err)
			account := accounts.accounts[1]
			rate := 0.5
			account.RateMultiplier = &rate
			account.Extra = map[string]any{UpstreamBillingProbeEnabledExtraKey: true, UpstreamBillingRateSyncEnabledExtraKey: true}
			snapshot, err := s.probeNewAPIAccount(t.Context(), account, repo.bindings[1], 30)
			require.NoError(t, err)
			declaration, present := upstreamBillingApplicabilityFromData(snapshot.Data)
			if tc.want == "not_declared" {
				require.False(t, present)
				require.Nil(t, guardUpstreamBillingApplicability(account, nil), "native catalog keeps legacy compatibility, not strict eligibility")
				return
			}
			require.True(t, present)
			scope := &AvailabilityRequestScope{Model: "gpt-fixture", Protocol: "responses_http", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}
			require.Equal(t, tc.want, declaration.verdict(scope, time.Now()))
			encoded, err := json.Marshal(snapshot)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "credential.example")
		})
	}
}

func TestNewAPIBillingApplicabilityReplayFailureAndRecovery(t *testing.T) {
	s, repo, upstream, accounts := newAPIConfigFixture()
	at := time.Now().UTC().Add(-time.Minute)
	setPricing := func(when time.Time, mutate func(map[string]any)) {
		t.Helper()
		pricing := newAPIPricingApplicabilityFixture(t, when)
		if mutate != nil {
			mutate(pricing)
		}
		body, err := json.Marshal(pricing)
		require.NoError(t, err)
		upstream.bodies["GET /api/pricing"] = string(body)
	}
	setPricing(at, nil)
	_, err := s.SaveNewAPIConfig(t.Context(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
	require.NoError(t, err)
	probe := func(want string) *UpstreamBillingProbeSnapshot {
		t.Helper()
		snapshot, err := s.probeNewAPIAccount(t.Context(), accounts.accounts[1], repo.bindings[1], 30)
		require.NoError(t, err)
		require.Equal(t, want, snapshot.Status)
		return snapshot
	}
	probe("ok")
	probe("ok") // An unchanged observation may be read again; its TTL is not renewed.
	setPricing(at.Add(-time.Second), nil)
	for range 2 {
		snapshot := probe("failed")
		require.Equal(t, "applicability_declaration_replayed", snapshot.LastError)
		require.Nil(t, snapshot.FreshUntil)
		require.Equal(t, UpstreamBillingProbeStatusOK, snapshot.Balance.Status)
	}
	setPricing(at, func(p map[string]any) { delete(p, "rate_applicability") })
	probe("failed")
	setPricing(at.Add(time.Second), nil)
	probe("ok")
	setPricing(at.Add(time.Second), func(p map[string]any) {
		p["group_ratio"] = map[string]any{"vip": 0.6}
		p["rate_applicability"].(map[string]any)["vip"].(map[string]any)["group_rate_multiplier"] = 0.6
	})
	probe("failed") // Same observation cannot acquire another price.
	setPricing(at.Add(2*time.Second), nil)
	probe("ok")
	delete(upstream.bodies, "GET /api/pricing")
	probe("failed") // HTTP/pricing loss cannot erase the declaration floor.
	setPricing(at.Add(time.Second), nil)
	probe("failed")
	setPricing(at.Add(3*time.Second), nil)
	probe("ok")
}

func TestNewAPIBillingApplicabilityObservationDuringFetch(t *testing.T) {
	s, repo, upstream, accounts := newAPIConfigFixture()
	_, err := s.SaveNewAPIConfig(t.Context(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
	require.NoError(t, err)
	start := time.Now().UTC().Add(-time.Minute)
	now := start
	s.now = func() time.Time { return now }
	pricing := newAPIPricingApplicabilityFixture(t, start.Add(time.Second))
	body, err := json.Marshal(pricing)
	require.NoError(t, err)
	upstream.bodies["GET /api/pricing"] = string(body)
	upstream.check = func(request *http.Request) {
		if request.URL.Path == "/api/pricing" {
			now = start.Add(2 * time.Second)
		}
	}
	snapshot, err := s.probeNewAPIAccount(t.Context(), accounts.accounts[1], repo.bindings[1], 30)
	require.NoError(t, err)
	declaration, present := upstreamBillingApplicabilityFromData(snapshot.Data)
	require.True(t, present)
	require.NotNil(t, declaration, "the supplier may observe after the request starts but before the verified response completes")
	require.Equal(t, now, *snapshot.ReceivedAt)
}

func TestNewAPIBillingApplicabilityRejectsDuplicateSelectors(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	for _, mutate := range []func(string) string{
		func(raw string) string {
			return strings.Replace(raw, `"pricing_version":"fixture-1"`, `"pricing_version":"fixture-1","pricing_version":"other"`, 1)
		},
		func(raw string) string { return strings.Replace(raw, `"vip":0.5`, `"vip":0.6,"vip":0.5`, 1) },
		func(raw string) string {
			return strings.Replace(raw, `"group_rate_applies":true`, `"group_rate_applies":false,"group_rate_applies":true`, 1)
		},
	} {
		s, repo, upstream, accounts := newAPIConfigFixture()
		_, err := s.SaveNewAPIConfig(t.Context(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
		require.NoError(t, err)
		body, err := json.Marshal(newAPIPricingApplicabilityFixture(t, at))
		require.NoError(t, err)
		upstream.bodies["GET /api/pricing"] = mutate(string(body))
		account := accounts.accounts[1]
		rate := 0.5
		account.RateMultiplier = &rate
		account.Extra = map[string]any{UpstreamBillingProbeEnabledExtraKey: true, UpstreamBillingRateSyncEnabledExtraKey: true}
		_, err = s.probeNewAPIAccount(t.Context(), account, repo.bindings[1], 30)
		require.NoError(t, err)
		scope := &AvailabilityRequestScope{Model: "gpt-fixture", Protocol: "responses_http", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}
		verdict, reason := upstreamBillingScopeVerdictAt(account, scope, time.Now())
		require.NotEqual(t, "covered", verdict)
		require.NotEmpty(t, reason)
	}
}

func TestNewAPIBillingApplicabilityPhysicalSendAndWS(t *testing.T) {
	var sends atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	s, repo, upstream, accounts := newAPIConfigFixture()
	account := accounts.accounts[1]
	account.Credentials["base_url"] = server.URL + "/v1"
	at := time.Now().UTC().Add(-time.Minute)
	pricing := newAPIPricingApplicabilityFixture(t, at)
	declaration := pricing["rate_applicability"].(map[string]any)["vip"].(map[string]any)
	declaration["entries"] = append(declaration["entries"].([]map[string]any),
		billingApplicabilityEntry("responses_ws_v2", "unspecified", true),
		billingApplicabilityEntry("responses_ws_v2", "priority", false))
	body, err := json.Marshal(pricing)
	require.NoError(t, err)
	upstream.bodies["GET /api/pricing"] = string(body)
	_, err = s.SaveNewAPIConfig(t.Context(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
	require.NoError(t, err)
	rate := 0.5
	account.RateMultiplier = &rate
	account.GroupIDs = []int64{41}
	account.Extra = map[string]any{UpstreamBillingProbeEnabledExtraKey: true, UpstreamBillingRateSyncEnabledExtraKey: true}
	_, err = s.probeNewAPIAccount(t.Context(), account, repo.bindings[1], 30)
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 41, threshold: 1})
	for _, payload := range []string{`{"model":"gpt-fixture","input":"hello"}`, `{"model":"gpt-fixture","input":"hello","service_tier":"priority"}`} {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(payload))
		require.NoError(t, err)
		guarded, err := (&OpenAIGatewayService{}).guardOpenAIProfitRequest(request, account)
		if strings.Contains(payload, "priority") {
			require.True(t, IsOpenAITurnAdmissionError(err))
			continue
		}
		require.NoError(t, err)
		response, err := server.Client().Do(guarded)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
	}
	require.EqualValues(t, 1, sends.Load(), "only the covered physical request reaches the upstream")
	svc := &OpenAIGatewayService{}
	target := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	headers := http.Header{"Authorization": {"Bearer " + account.GetCredential("api_key")}}
	binding := svc.bindOpenAIWSHandshake(account, "gpt-fixture", headers, target)
	guardedCtx, err := guardOpenAIWSBillingHandshake(ctx, account, target, headers)
	require.NoError(t, err)
	binding.billingHandshake = newOpenAIWSBillingHandshake(guardedCtx, target, headers)
	require.NoError(t, svc.checkOpenAIWSBinding(guardedCtx, account, "gpt-fixture", binding,
		[]byte(`{"type":"response.create","model":"gpt-fixture","input":"hello"}`)))
	require.True(t, IsOpenAITurnAdmissionError(svc.checkOpenAIWSBinding(guardedCtx, account, "gpt-fixture", binding,
		[]byte(`{"type":"response.create","model":"gpt-fixture","input":"hello","service_tier":"priority"}`))))
}

func TestNewAPIPreviewAndProbeUseSameFixedGroupRule(t *testing.T) {
	for _, tc := range []struct {
		name, group, routes, pricing string
		cross, eligible              bool
	}{
		{"legacy fixed group", "vip", `[]`, `{"vip":0.5,"auto":0.1}`, false, true},
		{"matching route", "vip", `["vip"]`, `{"vip":0.5}`, false, true},
		{"different route", "vip", `["default"]`, `{"vip":0.5}`, false, false},
		{"empty route", "vip", `[""]`, `{"vip":0.5}`, false, false},
		{"multiple routes", "vip", `["vip","default"]`, `{"vip":0.5}`, false, false},
		{"cross group retry", "vip", `["vip"]`, `{"vip":0.5}`, true, false},
		{"auto declared group", "auto", `[]`, `{"auto":0.1}`, false, false},
		{"null group rate is unknown", "vip", `["vip"]`, `{"vip":null,"default":0.2}`, false, false},
		{"missing group rate is unknown", "vip", `["vip"]`, `{"default":0.2}`, false, false},
		{"null ratio table is unknown", "vip", `["vip"]`, `null`, false, false},
		{"explicit zero remains valid", "vip", `["vip"]`, `{"vip":0}`, false, true},
		{"unrelated null does not erase numeric group", "vip", `["vip"]`, `{"vip":0.5,"default":null}`, false, true},
		{"numeric string is not a rate", "vip", `["vip"]`, `{"vip":"0.5"}`, false, false},
		{"boolean is not a rate", "vip", `["vip"]`, `{"vip":false}`, false, false},
		{"object is not a rate", "vip", `["vip"]`, `{"vip":{}}`, false, false},
		{"negative rate", "vip", `["vip"]`, `{"vip":-0.5}`, false, false},
		{"excessive rate", "vip", `["vip"]`, `{"vip":1000001}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, upstream, accounts := newAPIConfigFixture()
			upstream.bodies["GET /api/pricing"] = fmt.Sprintf(`{"success":true,"group_ratio":%s}`, tc.pricing)
			upstream.bodies["POST /api/token/3/key"] = `{"success":true,"data":{"key":"sk-abcd123456789wxyz"}}`
			upstream.bodies["GET /api/token/?p=0&page_size=100"] = fmt.Sprintf(`{"success":true,"data":{"items":[{"id":3,"key":"abcd**********wxyz","group":%q,"route_groups":%s,"cross_group_retry":%t}],"total":1,"page":1,"page_size":100}}`, tc.group, tc.routes, tc.cross)
			preview, err := s.SaveNewAPIConfig(t.Context(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
			require.NoError(t, err, "ownership and fixed-group suitability are separate")
			require.True(t, preview.Accounts[0].Matched)
			require.Equal(t, tc.eligible, preview.Accounts[0].Rate != nil)
			snapshot, err := s.probeNewAPIAccount(t.Context(), accounts.accounts[1], repo.bindings[1], 30)
			require.NoError(t, err)
			require.Equal(t, tc.eligible, snapshot.Status == UpstreamBillingProbeStatusOK)
			require.Equal(t, UpstreamBillingProbeStatusOK, snapshot.Balance.Status)
			require.Equal(t, 2.5, snapshot.Balance.Data["wallet_balance"])
			if tc.eligible {
				require.Equal(t, *preview.Accounts[0].Rate, snapshot.Data["resolved_rate_multiplier"])
				require.NotNil(t, snapshot.ReceivedAt)
				require.NotNil(t, snapshot.FreshUntil)
			} else {
				require.Equal(t, "unsupported_group", snapshot.LastError)
				require.Nil(t, snapshot.ReceivedAt)
				require.Nil(t, snapshot.FreshUntil)
				for _, name := range []string{"group_rate_multiplier", "resolved_rate_multiplier", "effective_rate_multiplier"} {
					require.NotContains(t, snapshot.Data, name)
				}
			}
		})
	}
}

func TestNewAPIConfigNoSaveForForeignOrUnmatchedAccounts(t *testing.T) {
	for _, ids := range [][]int64{{1, 2}, {1, 3}, {2}, {1, 1}} {
		s, r, _, _ := newAPIConfigFixture()
		_, e := s.SaveNewAPIConfig(context.Background(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "secret", AccountIDs: ids})
		require.Error(t, e)
		require.Zero(t, r.saves)
		require.Nil(t, r.profile)
		require.Empty(t, r.bindings)
	}
}
func TestNewAPIConfigSharedEncryptedAuthAndSafeRead(t *testing.T) {
	s, r, h, accounts := newAPIConfigFixture()
	h.bodies["GET /api/token/?p=0&page_size=100"] = `{"success":true,"data":{"items":[{"id":3,"key":"abcd**********wxyz","group":"vip"},{"id":4,"key":"nope**********xxxx","group":"auto"}],"total":2,"page":1,"page_size":100}}`
	out, e := s.SaveNewAPIConfig(context.Background(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "private-management-secret", AccountIDs: []int64{1, 2}})
	require.NoError(t, e)
	require.Len(t, out.Accounts, 2)
	require.Equal(t, "encrypted-private-management-secret", r.profile.Ciphertext)
	require.Equal(t, r.bindings[1].Profile, r.bindings[2].Profile)
	cfg, e := s.GetNewAPIConfig(context.Background(), 1)
	require.NoError(t, e)
	require.Len(t, cfg.Accounts, 2)
	b, e := json.Marshal(cfg)
	require.NoError(t, e)
	require.NotContains(t, string(b), "secret")
	require.NotContains(t, string(b), "cipher")
	_, e = s.PreviewNewAPIConfig(context.Background(), 1, NewAPIConfigRequest{UserID: 7, AccountIDs: []int64{1}})
	require.NoError(t, e)
	accounts.accounts[1].Credentials["api_key"] = "changed"
	cfg, e = s.GetNewAPIConfig(context.Background(), 1)
	require.NoError(t, e)
	require.False(t, cfg.Configured)
}
func TestNewAPIAmbiguousMaskRequiresVerifiedManualSelection(t *testing.T) {
	s, r, h, _ := newAPIConfigFixture()
	h.bodies["GET /api/token/?p=0&page_size=100"] = `{"success":true,"data":{"items":[{"id":3,"key":"abcd**********wxyz","group":"vip"},{"id":4,"key":"abcd**********wxyz","group":"vip"}],"total":2,"page":1,"page_size":100}}`
	req := NewAPIConfigRequest{UserID: 7, AccessToken: "secret", AccountIDs: []int64{1}}
	out, e := s.PreviewNewAPIConfig(context.Background(), 1, req)
	require.NoError(t, e)
	require.False(t, out.Accounts[0].Matched)
	require.Len(t, out.Accounts[0].TokenOptions, 2)
	_, e = s.SaveNewAPIConfig(context.Background(), 1, req)
	require.Error(t, e)
	require.Zero(t, r.saves)
	req.TokenSelections = map[string]int64{"1": 4}
	h.bodies["POST /api/token/4/key"] = `{"success":true,"data":{"key":"sk-abcdFOREIGNwxyz"}}`
	_, e = s.SaveNewAPIConfig(context.Background(), 1, req)
	require.Error(t, e)
	require.Zero(t, r.saves)
	h.bodies["POST /api/token/4/key"] = `{"success":true,"data":{"key":"sk-abcd123456789wxyz"}}`
	_, e = s.SaveNewAPIConfig(context.Background(), 1, req)
	require.NoError(t, e)
	require.Equal(t, int64(4), r.bindings[1].TokenID)
}
func TestNewAPIConfigRequiresFixedEncryptionKey(t *testing.T) {
	s, r, _, _ := newAPIConfigFixture()
	s.newAPIFixedKey = false
	_, e := s.SaveNewAPIConfig(context.Background(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "secret", AccountIDs: []int64{1}})
	require.ErrorIs(t, e, ErrNewAPIEncryptionKey)
	require.Zero(t, r.saves)
}
func TestNewAPIChangedKeyBindingReturnsToLegacyProbeWithoutUserCredential(t *testing.T) {
	s, _, h, accounts := newAPIConfigFixture()
	_, e := s.SaveNewAPIConfig(context.Background(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "private-management-secret", AccountIDs: []int64{1}})
	require.NoError(t, e)
	accounts.accounts[1].Credentials["api_key"] = "sk-changed"
	h.check = func(r *http.Request) {
		require.NotEqual(t, "Bearer private-management-secret", r.Header.Get("Authorization"))
		require.Equal(t, "", r.Header.Get("New-Api-User"))
	}
	snap, e := s.ProbeAccount(context.Background(), 1)
	require.NoError(t, e)
	require.Equal(t, UpstreamBillingProbeStatusUnsupported, snap.Status)
}
func TestNewAPIFingerprintIgnoresModelMappingButGuardsTransport(t *testing.T) {
	_, _, _, r := newAPIConfigFixture()
	a := r.accounts[1]
	first := NewAPIAccountFingerprint(a)
	a.Credentials["model_mapping"] = map[string]any{"old": "new"}
	require.Equal(t, first, NewAPIAccountFingerprint(a))
	a.Credentials["base_url"] = "https://foreign.example"
	require.NotEqual(t, first, NewAPIAccountFingerprint(a))
}
func (r *newAPIAuthRepoFake) WriteSnapshot(ctx context.Context, a *Account, _ *NewAPIAccountBinding, s *UpstreamBillingProbeSnapshot) error {
	if r.writeCheck != nil {
		r.writeCheck(ctx)
	}
	if a.Extra == nil {
		a.Extra = map[string]any{}
	}
	a.Extra[UpstreamBillingProbeExtraKey] = s
	return nil
}

func TestNewAPIProbeHonorsRetryAfterAndRecovers(t *testing.T) {
	for _, stage := range []string{"GET /api/status", "GET /api/user/self", "GET /api/pricing", "GET /api/token/?p=0&page_size=100", "POST /api/token/3/key"} {
		for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
			t.Run(fmt.Sprintf("%s/%d", stage, code), func(t *testing.T) {
				s, repo, upstream, accounts := newAPIConfigFixture()
				ctx := t.Context()
				_, err := s.SaveNewAPIConfig(ctx, 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
				require.NoError(t, err)
				now := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
				s.now = func() time.Time { return now }
				account := accounts.accounts[1]
				initial, err := s.probeNewAPIAccount(ctx, account, repo.bindings[1], 30)
				require.NoError(t, err)
				require.Equal(t, UpstreamBillingProbeStatusOK, initial.Status)
				upstream.statusCodes = map[string]int{stage: code}
				upstream.headers = map[string]http.Header{stage: {"Retry-After": []string{"14400"}}}
				var requests []string
				upstream.check = func(req *http.Request) { requests = append(requests, req.Method+" "+req.URL.RequestURI()) }

				snapshot, err := s.probeNewAPIAccount(ctx, account, repo.bindings[1], 30)
				require.NoError(t, err)
				require.Equal(t, UpstreamBillingProbeStatusFailed, snapshot.Status)
				require.Equal(t, "new_api_request_failed", snapshot.LastError)
				require.Equal(t, code, snapshot.HTTPStatus)
				require.Equal(t, now.Add(4*time.Hour), snapshot.NextProbeAt)
				require.Equal(t, stage, requests[len(requests)-1], "a wait response must stop the remaining management requests")
				require.Nil(t, snapshot.Data, "a failed ownership/rate observation must not authorize a stale rate")
				require.Nil(t, snapshot.ReceivedAt)
				require.Nil(t, snapshot.FreshUntil)
				require.Equal(t, UpstreamBillingProbeStatusFailed, snapshot.Balance.Status)
				stored := decodeUpstreamBillingProbeSnapshot(account.Extra)
				require.Equal(t, snapshot.NextProbeAt, stored.NextProbeAt)
				require.Equal(t, code, stored.HTTPStatus)
				require.Equal(t, 1, stored.FailureCount)
				encoded, err := json.Marshal(stored)
				require.NoError(t, err)
				require.NotContains(t, string(encoded), "fixture-secret")
				require.NotContains(t, string(encoded), "abcd123456789wxyz")

				upstream.statusCodes = nil
				upstream.headers = nil
				now = snapshot.NextProbeAt
				recovered, err := s.probeNewAPIAccount(ctx, account, repo.bindings[1], 30)
				require.NoError(t, err)
				require.Equal(t, UpstreamBillingProbeStatusOK, recovered.Status)
				require.Zero(t, recovered.FailureCount)
				require.Zero(t, recovered.HTTPStatus)
				require.Equal(t, now, *recovered.ReceivedAt)
				require.Equal(t, 0.5, recovered.Data["resolved_rate_multiplier"])
				require.Equal(t, UpstreamBillingProbeStatusOK, recovered.Balance.Status)
			})
		}
	}
}
func TestNewAPIProbeRetryAfterFormats(t *testing.T) {
	now := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, header string
		wait         time.Duration
	}{
		{"seconds", "14400", 4 * time.Hour},
		{"http date", now.Add(4 * time.Hour).Format(http.TimeFormat), 4 * time.Hour},
		{"capped seconds", "172800", upstreamBillingProbeMaxDelay},
		{"overflow seconds", "9223372036854775807", upstreamBillingProbeMaxDelay},
		{"overflow integer", "999999999999999999999999999999999999", upstreamBillingProbeMaxDelay},
		{"capped date", now.Add(48 * time.Hour).Format(http.TimeFormat), upstreamBillingProbeMaxDelay},
		{"zero", "0", 0},
		{"negative", "-1", 0},
		{"invalid", "later", 0},
		{"expired date", now.Add(-time.Hour).Format(http.TimeFormat), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, upstream, accounts := newAPIConfigFixture()
			_, err := s.SaveNewAPIConfig(t.Context(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
			require.NoError(t, err)
			s.now = func() time.Time { return now }
			upstream.statusCodes = map[string]int{"GET /api/status": http.StatusServiceUnavailable}
			upstream.headers = map[string]http.Header{"GET /api/status": {"Retry-After": []string{tc.header}}}
			snapshot, err := s.probeNewAPIAccount(t.Context(), accounts.accounts[1], repo.bindings[1], 30)
			require.NoError(t, err)
			require.Equal(t, http.StatusServiceUnavailable, snapshot.HTTPStatus)
			if tc.wait > 0 {
				require.Equal(t, now.Add(tc.wait), snapshot.NextProbeAt)
			} else {
				require.GreaterOrEqual(t, snapshot.NextProbeAt.Sub(now), 24*time.Minute)
				require.LessOrEqual(t, snapshot.NextProbeAt.Sub(now), 36*time.Minute)
			}
		})
	}
}

func TestNewAPIProbeWaitStartsAtResponse(t *testing.T) {
	s, repo, upstream, accounts := newAPIConfigFixture()
	_, err := s.SaveNewAPIConfig(t.Context(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
	require.NoError(t, err)
	start := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	now := start
	s.now = func() time.Time { return now }
	upstream.statusCodes = map[string]int{"GET /api/status": http.StatusServiceUnavailable}
	upstream.headers = map[string]http.Header{"GET /api/status": {"Retry-After": []string{"14400"}}}
	upstream.check = func(*http.Request) { now = start.Add(10 * time.Second) }
	snapshot, err := s.probeNewAPIAccount(t.Context(), accounts.accounts[1], repo.bindings[1], 30)
	require.NoError(t, err)
	require.Equal(t, start, snapshot.LastAttemptAt)
	require.Equal(t, now.Add(4*time.Hour), snapshot.NextProbeAt)
}

func TestNewAPIProbePersistsWithIndependentDeadline(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			s, repo, upstream, accounts := newAPIConfigFixture()
			_, err := s.SaveNewAPIConfig(t.Context(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			upstream.statusCodes = map[string]int{"GET /api/status": code}
			var requestDeadline time.Time
			var requestContext context.Context
			upstream.check = func(req *http.Request) {
				requestContext = req.Context()
				var ok bool
				requestDeadline, ok = req.Context().Deadline()
				require.True(t, ok)
				require.True(t, requestDeadline.Before(deadline))
			}
			writes := 0
			repo.writeCheck = func(writeCtx context.Context) {
				writes++
				writeDeadline, ok := writeCtx.Deadline()
				require.True(t, ok)
				require.True(t, writeDeadline.Before(deadline), "snapshot persistence must retain the caller's deadline")
				require.False(t, writeDeadline.Before(requestDeadline), "persistence must not shorten the last request's budget")
				require.ErrorIs(t, requestContext.Err(), context.Canceled, "the network context is already released before the independent write")
				require.Positive(t, time.Until(writeDeadline))
				require.LessOrEqual(t, time.Until(writeDeadline), upstreamBillingProbeRequestTimeout, "snapshot persistence must remain bounded")
				require.NoError(t, writeCtx.Err())
			}
			_, err = s.probeNewAPIAccount(ctx, accounts.accounts[1], repo.bindings[1], 30)
			require.NoError(t, err)
			require.Equal(t, 1, writes)
		})
	}
}

func TestNewAPIPreviewStopsOnKeyVerificationWait(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		for _, save := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/save_%t", code, save), func(t *testing.T) {
				s, repo, upstream, _ := newAPIConfigFixture()
				upstream.statusCodes = map[string]int{"POST /api/token/3/key": code}
				upstream.headers = map[string]http.Header{"POST /api/token/3/key": {"Retry-After": []string{"14400"}}}
				var requests []string
				upstream.check = func(req *http.Request) { requests = append(requests, req.Method+" "+req.URL.RequestURI()) }
				req := NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1, 2}}
				var err error
				if save {
					_, err = s.SaveNewAPIConfig(t.Context(), 1, req)
				} else {
					_, err = s.PreviewNewAPIConfig(t.Context(), 1, req)
				}
				require.ErrorIs(t, err, newAPIError("http_error"))
				require.NotContains(t, err.Error(), "fixture-secret")
				require.Len(t, requests, 5, "the next selected account must not send requests after a wait")
				require.Equal(t, "POST /api/token/3/key", requests[len(requests)-1])
				require.Zero(t, repo.saves)
				require.Empty(t, repo.bindings)
			})
		}
	}
}

func TestNewAPIBoundKeyDisappearsPersistsFailureWithBackoff(t *testing.T) {
	s, r, h, accounts := newAPIConfigFixture()
	_, e := s.SaveNewAPIConfig(context.Background(), 1, NewAPIConfigRequest{UserID: 7, AccessToken: "secret", AccountIDs: []int64{1}})
	require.NoError(t, e)
	h.bodies["GET /api/token/?p=0&page_size=100"] = `{"success":true,"data":{"items":[],"total":0,"page":1,"page_size":100}}`
	snap, e := s.probeNewAPIAccount(context.Background(), accounts.accounts[1], r.bindings[1], 30)
	require.NoError(t, e)
	require.Equal(t, UpstreamBillingProbeStatusFailed, snap.Status)
	require.Equal(t, "new_api_bound_key_not_owned", snap.LastError)
	require.True(t, snap.NextProbeAt.After(snap.LastAttemptAt))
	require.Nil(t, snap.Data)
	require.Equal(t, UpstreamBillingProbeStatusFailed, snap.Balance.Status)
}

func TestNewAPIBoundKeyRouteChangeInvalidatesRateAndRecovers(t *testing.T) {
	s, repo, http, accounts := newAPIConfigFixture()
	ctx := context.Background()
	_, err := s.SaveNewAPIConfig(ctx, 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
	require.NoError(t, err)
	account := accounts.accounts[1]
	rate := 0.7
	account.RateMultiplier = &rate
	fingerprint := NewAPIAccountFingerprint(account)
	for _, phase := range []struct {
		name, routes, status string
	}{
		{"initial matching group", `["vip"]`, UpstreamBillingProbeStatusOK},
		{"different upstream route", `["default"]`, UpstreamBillingProbeStatusUnsupported},
		{"multiple upstream routes", `["vip","default"]`, UpstreamBillingProbeStatusUnsupported},
		{"matching group restored", `["vip"]`, UpstreamBillingProbeStatusOK},
	} {
		t.Run(phase.name, func(t *testing.T) {
			http.bodies["GET /api/token/?p=0&page_size=100"] = `{"success":true,"data":{"items":[{"id":3,"key":"abcd**********wxyz","group":"vip","route_groups":` + phase.routes + `}],"total":1,"page":1,"page_size":100}}`
			snapshot, err := s.probeLoadedAccount(ctx, account, 30)
			require.NoError(t, err)
			require.Equal(t, phase.status, snapshot.Status)
			stored := decodeUpstreamBillingProbeSnapshot(account.Extra)
			require.NotNil(t, stored)
			require.Equal(t, phase.status, stored.Status)
			require.Equal(t, UpstreamBillingProbeStatusOK, stored.Balance.Status)
			require.Equal(t, 2.5, stored.Balance.Data["wallet_balance"])
			require.Equal(t, 0.7, *account.RateMultiplier, "New API group evidence must not overwrite token account rates")
			require.Equal(t, fingerprint, repo.bindings[1].Fingerprint)
			require.Equal(t, fingerprint, NewAPIAccountFingerprint(account))
			if phase.status == UpstreamBillingProbeStatusOK {
				require.Equal(t, 0.5, stored.Data["resolved_rate_multiplier"])
			} else {
				require.NotContains(t, stored.Data, "resolved_rate_multiplier", "route changes must discard the previously valid rate")
				require.Nil(t, stored.FreshUntil)
			}
		})
	}
}

func TestNewAPIBindingRejectsChangedProtocolIdentity(t *testing.T) {
	for _, change := range []struct {
		key   string
		value any
	}{
		{"api_base_urls", map[string]any{APIProtocolChatCompletions: "https://new.example/v1"}},
		{"api_protocol", APIProtocolChatCompletions},
		{"account_mode", AccountModeCoding},
		{"protocol_rules", []any{map[string]any{"pattern": "gpt-*", "protocol": APIProtocolResponses}}},
	} {
		t.Run(change.key, func(t *testing.T) {
			s, repo, _, accounts := newAPIConfigFixture()
			account := accounts.accounts[1]
			account.Platform = PlatformDeepseek
			account.Credentials["api_protocol"] = APIProtocolAdaptive
			account.Credentials["account_mode"] = "payg"
			account.Credentials["api_base_urls"] = map[string]any{APIProtocolChatCompletions: "https://example.com/v1"}
			ctx := context.Background()
			_, err := s.SaveNewAPIConfig(ctx, 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
			require.NoError(t, err)
			config, err := s.GetNewAPIConfig(ctx, 1)
			require.NoError(t, err)
			require.True(t, config.Configured)
			binding := repo.bindings[1]
			account.Credentials[change.key] = change.value
			config, err = s.GetNewAPIConfig(ctx, 1)
			require.NoError(t, err)
			require.False(t, config.Configured, "old binding must require verification after route identity changes")
			snapshot, err := s.probeNewAPIAccount(ctx, account, binding, 30)
			require.ErrorIs(t, err, ErrUpstreamBillingProbeIdentityChanged)
			require.Nil(t, snapshot)
		})
	}
}

func TestNewAPIUsesEffectiveProtocolSite(t *testing.T) {
	for _, platform := range []string{PlatformKimi, PlatformOpenCodeGo} {
		t.Run(platform, func(t *testing.T) {
			s, repo, upstream, accounts := newAPIConfigFixture()
			for _, id := range []int64{1, 2} {
				account := accounts.accounts[id]
				account.Platform = platform
				account.Credentials["base_url"] = "https://stale.example/v1"
				account.Credentials["api_protocol"] = APIProtocolAdaptive
				account.Credentials["api_base_urls"] = map[string]any{APIProtocolChatCompletions: "https://example.com/v1"}
			}
			foreign := *accounts.accounts[2]
			foreign.ID = 4
			foreign.Credentials = mergeMap(accounts.accounts[2].Credentials, map[string]any{
				"base_url":      "https://example.com/v1",
				"api_base_urls": map[string]any{APIProtocolChatCompletions: "https://foreign.example/v1"},
			})
			accounts.accounts[4] = &foreign
			requests := 0
			upstream.check = func(req *http.Request) {
				requests++
				require.Equal(t, "example.com", req.URL.Host, "management credentials must follow the effective site")
			}
			ctx := context.Background()
			preview, err := s.SaveNewAPIConfig(ctx, 1, NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-secret", AccountIDs: []int64{1}})
			require.NoError(t, err)
			require.Equal(t, "https://example.com", preview.SiteURL)
			require.Equal(t, "https://example.com", repo.profile.SiteURL)
			config, err := s.GetNewAPIConfig(ctx, 1)
			require.NoError(t, err)
			require.True(t, config.Configured)
			ids := make([]int64, 0, len(config.Accounts))
			for _, account := range config.Accounts {
				ids = append(ids, account.AccountID)
			}
			require.Equal(t, []int64{1, 2}, ids, "same-site candidates follow the effective base, not legacy base_url")
			snapshot, err := s.ProbeAccount(ctx, 1)
			require.NoError(t, err)
			require.Equal(t, UpstreamBillingProbeStatusOK, snapshot.Status)
			require.Greater(t, requests, 4, "exercise save verification and the bound-key probe")
		})
	}
}
