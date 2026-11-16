package service

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func billingManualAccount() *Account {
	rate := 0.4
	return &Account{ID: 92, Type: AccountTypeAPIKey, Platform: PlatformOpenAI, RateMultiplier: &rate,
		Credentials: map[string]any{"base_url": "https://relay.example", "api_key": "fixture-key"}, Extra: map[string]any{}}
}

func billingManualCommand(t *testing.T, account *Account, at time.Time, entries ...map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"source_version": "manual-fixture-1", "reason": "supplier contract reviewed",
		"expires_at": at.Add(time.Hour), "confirm_current_binding": true, "expected_revision": manualApplicabilityRevision(account.Extra), "entries": entries})
	require.NoError(t, err)
	return raw
}

func TestBillingManualApplicabilityLifecycle(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	account := billingManualAccount()
	_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at,
		billingApplicabilityEntry("responses_http", "unspecified", true), billingApplicabilityEntry("responses_http", "priority", false)), at)
	require.NoError(t, err)
	scope := &AvailabilityRequestScope{Protocol: "responses_http", Model: "gpt-fixture", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}
	status, reason := upstreamBillingScopeVerdictAt(account, scope, at.Add(time.Second))
	require.Equal(t, "covered", status)
	require.Empty(t, reason)
	scope.ServiceTier = "priority"
	status, _ = upstreamBillingScopeVerdictAt(account, scope, at.Add(time.Second))
	require.Equal(t, "excluded", status)
	status, _ = upstreamBillingScopeVerdictAt(account, scope, at.Add(time.Hour))
	require.Equal(t, "expired", status)
	view := UpstreamBillingManualApplicabilityView(account, at.Add(time.Second))
	require.Equal(t, "active", view["status"])
	raw, err := json.Marshal(view)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "fixture-key")
	require.NotContains(t, view, "binding")
	// Repeated reads, refreshes and ordinary edits never extend a manual TTL.
	before, _ := json.Marshal(account.Extra)
	_ = UpstreamBillingManualApplicabilityView(account, at.Add(2*time.Hour))
	after, _ := json.Marshal(account.Extra)
	require.JSONEq(t, string(before), string(after))
}

func TestBillingManualApplicabilityResponsesOutputLifecycle(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	account := billingManualAccount()
	entry := billingApplicabilityEntry("responses_http", "unspecified", true)
	entry["reasoning_summary"], entry["include_encrypted_content"] = "auto", true
	_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
	require.NoError(t, err)
	view := UpstreamBillingManualApplicabilityView(account, at.Add(time.Second))
	require.Equal(t, 2, view["schema_version"])
	entries := view["entries"].([]upstreamBillingApplicabilityEntry)
	require.Equal(t, "auto", entries[0].ReasoningSummary)
	require.True(t, entries[0].IncludeEncryptedContent)
	require.Equal(t, "covered", entries[0].Conclusion)
	// Persistence through JSON must preserve the exact output contract.
	raw, err := json.Marshal(account.Extra)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &account.Extra))
	require.Equal(t, "active", UpstreamBillingManualApplicabilityView(account, at.Add(time.Second))["status"])
	account.Credentials["api_key"] = "changed-fixture-key"
	require.Equal(t, "binding_changed", UpstreamBillingManualApplicabilityView(account, at.Add(time.Second))["status"])
	require.Equal(t, "expired", UpstreamBillingManualApplicabilityView(account, at.Add(2*time.Hour))["status"])
}

func TestBillingManualApplicabilityBindingAndSourcePrecedence(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	for _, change := range []string{"key", "base", "rate", "mapping", "sync", "account"} {
		t.Run(change, func(t *testing.T) {
			account := billingManualAccount()
			_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, billingApplicabilityEntry("responses_http", "unspecified", true)), at)
			require.NoError(t, err)
			switch change {
			case "key":
				account.Credentials["api_key"] = "other-fixture-key"
			case "base":
				account.Credentials["base_url"] = "https://other.example"
			case "rate":
				rate := 0.5
				account.RateMultiplier = &rate
			case "mapping":
				account.Credentials["model_mapping"] = map[string]any{"alias": "other"}
			case "sync":
				account.Extra[UpstreamBillingRateSyncEnabledExtraKey] = true
				account.Extra[UpstreamBillingProbeEnabledExtraKey] = true
			case "account":
				account.ID++
			}
			_, status := manualBillingApplicabilityAt(account, at.Add(time.Second))
			require.Contains(t, []string{"binding_changed", "unavailable"}, status)
		})
	}
	account := billingApplicabilityAccount(t, at, billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", false)))
	_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, billingApplicabilityEntry("responses_http", "unspecified", true)), at)
	require.NoError(t, err)
	scope := &AvailabilityRequestScope{Protocol: "responses_http", Model: "gpt-fixture", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}
	status, _ := upstreamBillingScopeVerdictAt(account, scope, at.Add(time.Second))
	require.Equal(t, "excluded", status, "manual approval does not override supplier exclusion")
	require.Equal(t, "superseded", UpstreamBillingManualApplicabilityView(account, at.Add(time.Second))["status"])
	snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra)
	delete(snapshot.Data, upstreamBillingApplicabilityKey)
	account.Extra[UpstreamBillingProbeExtraKey] = snapshot
	status, _ = upstreamBillingScopeVerdictAt(account, scope, at.Add(time.Second))
	require.Equal(t, "covered", status, "an explicit manual declaration can fill absent legacy support")
	snapshot.Status = "failed"
	account.Extra[UpstreamBillingProbeExtraKey] = snapshot
	status, _ = upstreamBillingScopeVerdictAt(account, scope, at.Add(time.Second))
	require.Equal(t, "unavailable", status, "manual scope never replaces failed automatic price evidence")
}

func TestBillingManualApplicabilityRejectsUnsafeCommandsAndStaleEdits(t *testing.T) {
	at := time.Now().UTC()
	account := billingManualAccount()
	good := billingManualCommand(t, account, at, billingApplicabilityEntry("responses_http", "unspecified", true))
	for _, mutate := range []func(map[string]any){
		func(m map[string]any) { m["expires_at"] = at.Add(25 * time.Hour) },
		func(m map[string]any) { m["expires_at"] = at },
		func(m map[string]any) { m["confirm_current_binding"] = false },
		func(m map[string]any) { m["reason"] = "" },
		func(m map[string]any) { m["source"] = "supplier" },
		func(m map[string]any) { m["SOURCE_VERSION"] = "alias" },
		func(m map[string]any) { m["entries"] = []any{} },
	} {
		var command map[string]any
		require.NoError(t, json.Unmarshal(good, &command))
		mutate(command)
		raw, err := json.Marshal(command)
		require.NoError(t, err)
		_, err = prepareUpstreamBillingManualApplicability(t.Context(), account, raw, at)
		require.Error(t, err)
		require.NotContains(t, account.Extra, UpstreamBillingManualApplicabilityExtraKey)
	}
	ctx, err := prepareUpstreamBillingManualApplicability(t.Context(), account, good, at)
	require.NoError(t, err)
	first := maps.Clone(account.Extra)
	_, err = MergeUpstreamBillingManualApplicability(ctx, first, account.Extra)
	require.ErrorIs(t, err, ErrUpstreamBillingManualApplicabilityConflict, "row-lock CAS must reject a second concurrent creation")
	_, err = prepareUpstreamBillingManualApplicability(t.Context(), account, good, at)
	require.ErrorIs(t, err, ErrUpstreamBillingManualApplicabilityConflict)
	merged, err := MergeUpstreamBillingManualApplicability(context.Background(), first, map[string]any{UpstreamBillingManualApplicabilityExtraKey: "forged"})
	require.NoError(t, err)
	require.Equal(t, first, merged, "generic edits preserve current server-owned state")
	revoke, err := json.Marshal(map[string]any{"revoke": true, "expected_revision": manualApplicabilityRevision(account.Extra)})
	require.NoError(t, err)
	ctx, err = prepareUpstreamBillingManualApplicability(t.Context(), account, revoke, at)
	require.NoError(t, err)
	merged, err = MergeUpstreamBillingManualApplicability(ctx, first, account.Extra)
	require.NoError(t, err)
	require.True(t, manualApplicabilityRevoked(merged))
	require.NotEqual(t, manualApplicabilityRevision(first), manualApplicabilityRevision(merged))
	_, err = prepareUpstreamBillingManualApplicability(t.Context(), account, good, at)
	require.ErrorIs(t, err, ErrUpstreamBillingManualApplicabilityConflict, "a pre-creation form must not resurrect a revoked declaration")
	require.False(t, upstreamBillingHasApplicability(account), "revocation removes strict evidence, not legacy compatibility")
}

func TestBillingManualApplicabilityRequiresCurrentAPIKey(t *testing.T) {
	for _, key := range []any{nil, "", " \t\n", true} {
		account := billingManualAccount()
		account.Credentials["api_key"] = key
		at := time.Now().UTC()
		_, err := prepareUpstreamBillingManualApplicability(t.Context(), account,
			billingManualCommand(t, account, at, billingApplicabilityEntry("responses_http", "unspecified", true)), at)
		require.ErrorIs(t, err, ErrUpstreamBillingManualApplicability)
		require.NotContains(t, account.Extra, UpstreamBillingManualApplicabilityExtraKey)
	}
}

func TestBillingManualApplicabilityRealHTTPAndWSBoundaries(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sends.Add(1); w.WriteHeader(200) }))
	defer server.Close()
	account := billingManualAccount()
	account.Credentials["base_url"] = server.URL
	at := time.Now().UTC().Add(-time.Second)
	_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at,
		billingApplicabilityEntry("responses_http", "unspecified", true), billingApplicabilityEntry("responses_ws_v2", "unspecified", true)), at)
	require.NoError(t, err)
	for _, tier := range []string{"unspecified", "priority"} {
		payload := `{"model":"gpt-fixture","input":"hello"}`
		if tier == "priority" {
			payload = `{"model":"gpt-fixture","input":"hello","service_tier":"priority"}`
		}
		req, e := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/responses", strings.NewReader(payload))
		require.NoError(t, e)
		ctx, e := guardUpstreamBillingRequest(req.Context(), account, req)
		if tier == "priority" {
			require.True(t, IsOpenAITurnAdmissionError(e))
			continue
		}
		require.NoError(t, e)
		resp, e := server.Client().Do(req.WithContext(ctx))
		require.NoError(t, e)
		require.NoError(t, resp.Body.Close())
	}
	require.EqualValues(t, 1, sends.Load(), "uncovered scope never reaches the local upstream")
	// A socket handshake does not authorize subsequent fast turns.
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{threshold: 1})
	ctx = WithHTTPUpstreamRedirectsDisabled(ctx)
	target := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	headers := http.Header{"Authorization": {"Bearer fixture-key"}}
	svc := &OpenAIGatewayService{}
	binding := svc.bindOpenAIWSHandshake(account, "gpt-fixture", headers, target)
	binding.billingHandshake = newOpenAIWSBillingHandshake(ctx, target, headers)
	require.NoError(t, checkOpenAIWSBillingHandshake(ctx, account, binding.billingHandshake))
	require.NoError(t, svc.checkOpenAIWSBinding(ctx, account, "gpt-fixture", binding, []byte(`{"type":"response.create","model":"gpt-fixture","input":"hello"}`)))
	require.True(t, IsOpenAITurnAdmissionError(svc.checkOpenAIWSBinding(ctx, account, "gpt-fixture", binding, []byte(`{"type":"response.create","model":"gpt-fixture","input":"hello","service_tier":"priority"}`))))
}
