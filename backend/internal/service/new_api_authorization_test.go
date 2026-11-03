package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"net/http"
	"strings"
	"testing"
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
	profile  *NewAPISiteAuthorization
	bindings map[int64]*NewAPIAccountBinding
	saves    int
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
			require.Equal(t, "new_api_bound_key_not_owned", snapshot.LastError)
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

func TestNewAPIPreviewAndProbeUseSameFixedGroupRule(t *testing.T) {
	for _, tc := range []struct {
		name, group, routes string
		cross, eligible     bool
	}{
		{"legacy fixed group", "vip", `[]`, false, true},
		{"matching route", "vip", `["vip"]`, false, true},
		{"different route", "vip", `["default"]`, false, false},
		{"empty route", "vip", `[""]`, false, false},
		{"multiple routes", "vip", `["vip","default"]`, false, false},
		{"cross group retry", "vip", `["vip"]`, true, false},
		{"auto declared group", "auto", `[]`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, upstream, accounts := newAPIConfigFixture()
			upstream.bodies["GET /api/pricing"] = `{"success":true,"group_ratio":{"vip":0.5,"auto":0.1}}`
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
func (r *newAPIAuthRepoFake) WriteSnapshot(_ context.Context, a *Account, _ *NewAPIAccountBinding, s *UpstreamBillingProbeSnapshot) error {
	a.Extra = map[string]any{UpstreamBillingProbeExtraKey: s}
	return nil
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
