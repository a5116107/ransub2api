package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func billingApplicabilityFixture(at time.Time, entries ...map[string]any) map[string]any {
	return map[string]any{
		"schema_version": 1, "source_version": "fixture-1",
		"observed_at": at.Format(time.RFC3339Nano), "expires_at": at.Add(10 * time.Minute).Format(time.RFC3339Nano),
		"entries": entries,
	}
}

func billingApplicabilityEntry(protocol, tier string, applies any) map[string]any {
	return map[string]any{"model": "gpt-fixture", "protocol": protocol, "service_tier": tier,
		"reasoning_effort": "unspecified", "context": "all", "group_rate_applies": applies}
}

func billingApplicabilityBody(t *testing.T, at time.Time, declaration any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"object": "sub2api.key_billing", "schema_version": 1, "billing_scope": "token",
		"group_rate_multiplier": 0.4, "resolved_rate_multiplier": 0.4, "effective_rate_multiplier": 0.4,
		"peak_rate_enabled": false, "observed_at": at.Format(time.RFC3339Nano), "rate_applicability": declaration,
	})
	require.NoError(t, err)
	return body
}

func billingApplicabilityAccount(t *testing.T, at time.Time, declaration any) *Account {
	t.Helper()
	account := profitControlSyncedTestAccount(at)
	account.Credentials = map[string]any{"api_key": "fixture-key", "base_url": "https://relay.example"}
	snapshot, rate := upstreamBillingRateSnapshotFromResult(account, 30, at, upstreamBillingProbeHTTPResult{
		statusCode: 200, body: billingApplicabilityBody(t, at, declaration),
	}, 0.4)
	require.Equal(t, UpstreamBillingProbeStatusOK, snapshot.Status)
	require.NotNil(t, rate)
	account.Extra[UpstreamBillingProbeExtraKey] = snapshot
	return account
}

func TestBillingManualApplicabilityInvalidEvidenceStopsFinalSend(t *testing.T) {
	account := &Account{ID: 91, Type: AccountTypeAPIKey, Platform: PlatformOpenAI,
		Credentials: map[string]any{"base_url": "https://relay.example", "api_key": "fixture-key"},
		Extra:       map[string]any{"upstream_billing_manual_applicability": map[string]any{"expires_at": "2020-01-01T00:00:00Z"}}}
	req, err := http.NewRequest(http.MethodPost, "https://relay.example/v1/responses", strings.NewReader(`{"model":"gpt-fixture","input":"hello"}`))
	require.NoError(t, err)
	_, err = guardUpstreamBillingRequest(t.Context(), account, req)
	require.True(t, IsOpenAITurnAdmissionError(err), "an explicit invalid manual declaration must not silently become legacy admission")
}

func TestBillingApplicabilityFinalHTTP(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	decl := billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", true),
		billingApplicabilityEntry("responses_http", "priority", false))
	account := billingApplicabilityAccount(t, at, decl)
	for _, tc := range []struct {
		name, payload string
		allowed       bool
	}{
		{"covered standard", `{"model":"gpt-fixture","input":"hello"}`, true},
		{"fast exception", `{"model":"gpt-fixture","input":"hello","service_tier":"priority"}`, false},
		{"other model", `{"model":"other","input":"hello"}`, false},
		{"other effort", `{"model":"gpt-fixture","input":"hello","reasoning":{"effort":"high"}}`, false},
		{"tools not covered", `{"model":"gpt-fixture","input":"hello","tools":[{"type":"web_search"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://relay.example/v1/responses", bytes.NewBufferString(tc.payload))
			require.NoError(t, err)
			ctx, err := guardUpstreamBillingRequest(req.Context(), account, req)
			if tc.allowed {
				require.NoError(t, err)
				require.True(t, HTTPUpstreamRedirectsDisabled(ctx))
			} else {
				require.True(t, IsOpenAITurnAdmissionError(err), "uncovered physical scope must be rejected before send")
			}
			body, readErr := io.ReadAll(req.Body)
			require.NoError(t, readErr)
			require.Equal(t, tc.payload, string(body), "admission must preserve the physical request body")
		})
	}
}

func TestBillingApplicabilityFinalWS(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	account := billingApplicabilityAccount(t, at, billingApplicabilityFixture(at,
		billingApplicabilityEntry("responses_ws_v2", "unspecified", true),
		billingApplicabilityEntry("responses_ws_v2", "priority", false)))
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{threshold: 1})
	ctx = WithHTTPUpstreamRedirectsDisabled(ctx)
	headers := http.Header{"Authorization": {"Bearer fixture-key"}}
	svc := &OpenAIGatewayService{}
	b := svc.bindOpenAIWSHandshake(account, "gpt-fixture", headers, "wss://relay.example/v1/responses")
	b.billingHandshake = newOpenAIWSBillingHandshake(ctx, "wss://relay.example/v1/responses", headers)
	require.NoError(t, checkOpenAIWSBillingHandshake(ctx, account, b.billingHandshake), "handshake is not evidence of a priced turn")
	require.NoError(t, svc.checkOpenAIWSBinding(ctx, account, "gpt-fixture", b,
		[]byte(`{"type":"response.create","model":"gpt-fixture","input":"hello"}`)))
	err := svc.checkOpenAIWSBinding(ctx, account, "gpt-fixture", b,
		[]byte(`{"type":"response.create","model":"gpt-fixture","input":"hello","service_tier":"priority"}`))
	require.True(t, IsOpenAITurnAdmissionError(err), "an open standard socket must not authorize an uncovered fast turn")
}

func TestBillingApplicabilityContract(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"covered", func(map[string]any) {}, "covered"},
		{"explicit exception", func(d map[string]any) { d["entries"].([]map[string]any)[0]["group_rate_applies"] = false }, "excluded"},
		{"explicit unknown", func(d map[string]any) { d["entries"].([]map[string]any)[0]["group_rate_applies"] = nil }, "unknown"},
		{"missing is not false", func(d map[string]any) { delete(d["entries"].([]map[string]any)[0], "group_rate_applies") }, "unknown"},
		{"limited context not measured", func(d map[string]any) { d["entries"].([]map[string]any)[0]["context"] = "long" }, "unknown"},
		{"context exception blocks generic borrowing", func(d map[string]any) {
			exception := billingApplicabilityEntry("responses_http", "unspecified", false)
			exception["context"] = "long"
			d["entries"] = append(d["entries"].([]map[string]any), exception)
		}, "unknown"},
		{"protocol not inherited", func(d map[string]any) { d["entries"].([]map[string]any)[0]["protocol"] = "chat_http" }, "unknown"},
		{"tier not normalized", func(d map[string]any) { d["entries"].([]map[string]any)[0]["service_tier"] = "default" }, "unknown"},
		{"effort not inherited", func(d map[string]any) { d["entries"].([]map[string]any)[0]["reasoning_effort"] = "high" }, "unknown"},
		{"expired exact boundary", func(d map[string]any) { d["expires_at"] = at.Add(time.Second).Format(time.RFC3339Nano) }, "expired"},
		{"version unsupported", func(d map[string]any) { d["schema_version"] = 2 }, "invalid"},
		{"source version absent", func(d map[string]any) { delete(d, "source_version") }, "invalid"},
		{"observation mismatch", func(d map[string]any) { d["observed_at"] = at.Add(-time.Second).Format(time.RFC3339Nano) }, "invalid"},
		{"expiry missing", func(d map[string]any) { delete(d, "expires_at") }, "invalid"},
		{"ttl exceeds cap", func(d map[string]any) { d["expires_at"] = at.Add(24*time.Hour + time.Second).Format(time.RFC3339Nano) }, "invalid"},
		{"empty entries", func(d map[string]any) { d["entries"] = []map[string]any{} }, "invalid"},
		{"wildcard not a proof", func(d map[string]any) { d["entries"].([]map[string]any)[0]["model"] = "*" }, "invalid"},
		{"conflicting entries", func(d map[string]any) {
			d["entries"] = append(d["entries"].([]map[string]any), billingApplicabilityEntry("responses_http", "unspecified", false))
		}, "conflict"},
		{"unknown conflicts with pass", func(d map[string]any) {
			d["entries"] = append(d["entries"].([]map[string]any), billingApplicabilityEntry("responses_http", "unspecified", nil))
		}, "conflict"},
		{"unrelated exception local only", func(d map[string]any) {
			d["entries"] = append(d["entries"].([]map[string]any), billingApplicabilityEntry("responses_http", "priority", false))
		}, "covered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", true))
			tc.mutate(fixture)
			data, err := parseUpstreamBillingProbeResponse(billingApplicabilityBody(t, at, fixture))
			require.NoError(t, err, "bad scope does not erase a valid group-level observation")
			declaration, present := upstreamBillingApplicabilityFromData(data)
			require.True(t, present, "invalid evidence must stay distinct from absent legacy support")
			scope := &AvailabilityRequestScope{Protocol: "responses_http", Model: "gpt-fixture", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}
			require.Equal(t, tc.want, declaration.verdict(scope, at.Add(time.Second)))
		})
	}
}

func TestBillingApplicabilitySanitizesAndDoesNotRejuvenate(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	fixture := billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", true))
	fixture["source"], fixture["api_key"] = "https://private.example/TOKEN", "secret-field-fixture"
	fixture["entries"].([]map[string]any)[0]["api_key"] = "secret-entry-fixture"
	data, err := parseUpstreamBillingProbeResponse(billingApplicabilityBody(t, at, fixture))
	require.NoError(t, err)
	encoded, err := json.Marshal(data)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private.example")
	require.NotContains(t, string(encoded), "secret-")
	declaration, present := upstreamBillingApplicabilityFromData(data)
	require.True(t, present)
	require.Equal(t, "sub2api.key_billing", declaration.Source)
	require.Equal(t, "fixture-1", declaration.SourceVersion)
	require.True(t, declaration.ExpiresAt.Equal(at.Add(10*time.Minute)))
	account := billingApplicabilityAccount(t, at, fixture)
	// Receiving the same old result later never extends the declared TTL.
	snapshot, _ := upstreamBillingRateSnapshotFromResult(account, 30, at.Add(11*time.Minute), upstreamBillingProbeHTTPResult{statusCode: 200, body: billingApplicabilityBody(t, at, fixture)}, 0.4)
	declaration, _ = upstreamBillingApplicabilityFromData(snapshot.Data)
	require.Equal(t, "expired", declaration.verdict(nil, at.Add(11*time.Minute)))
	// A previous snapshot in JSON-map form is semantically identical.
	var roundTrip map[string]any
	require.NoError(t, json.Unmarshal(encoded, &roundTrip))
	require.False(t, upstreamBillingApplicabilityReplay(roundTrip, data))
}

func TestBillingApplicabilityCanonicalFieldsAndBounds(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	for _, tc := range []struct {
		name, replace, with string
		valid               bool
	}{
		{"case alias cannot change model", `"model":"gpt-fixture"`, `"model":"gpt-fixture","MODEL":"other"`, true},
		{"case alias cannot change boolean", `"group_rate_applies":true`, `"group_rate_applies":true,"GROUP_RATE_APPLIES":false`, true},
		{"case alias cannot change version", `"schema_version":1`, `"schema_version":1,"SCHEMA_VERSION":2`, true},
		{"supplied conclusion is not trusted", `"group_rate_applies":true`, `"group_rate_applies":true,"conclusion":"excluded"`, true},
		{"case alias alone is missing", `"model":"gpt-fixture"`, `"MODEL":"gpt-fixture"`, false},
		{"conflicting repeated field", `"model":"gpt-fixture"`, `"model":"gpt-fixture","model":"other"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", true)))
			require.NoError(t, err)
			modified := strings.Replace(string(raw), tc.replace, tc.with, 1)
			require.NotEqual(t, string(raw), modified)
			declaration := decodeUpstreamBillingApplicability([]byte(modified), at)
			if !tc.valid {
				require.Nil(t, declaration)
				return
			}
			require.NotNil(t, declaration)
			require.Equal(t, "gpt-fixture", declaration.Entries[0].Model)
			require.Empty(t, declaration.Entries[0].Conclusion, "supplier-provided conclusion must be dropped")
			require.Equal(t, "covered", declaration.verdict(&AvailabilityRequestScope{
				Model: "gpt-fixture", Protocol: "responses_http", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text",
			}, at))
		})
	}
	entries := make([]map[string]any, 129)
	for i := range entries {
		entries[i] = billingApplicabilityEntry("responses_http", "unspecified", true)
	}
	raw, err := json.Marshal(billingApplicabilityFixture(at, entries...))
	require.NoError(t, err)
	require.Nil(t, decodeUpstreamBillingApplicability(raw, at), "bounded contract must reject oversized entry lists")
	decl, err := json.Marshal(billingApplicabilityFixture(at, entries[0]))
	require.NoError(t, err)
	data := make(map[string]any)
	parseUpstreamBillingApplicabilityResponse([]byte(`{"rate_applicability":`+string(decl)+`,"rate_applicability":null}`), data, at)
	value, present := data[upstreamBillingApplicabilityKey]
	require.True(t, present)
	require.Nil(t, value, "an ambiguous envelope must not silently select either declaration")
}

func TestBillingApplicabilityReplayAndDowngrade(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	for _, tc := range []struct {
		name           string
		offset         time.Duration
		remove, change bool
		allowed        bool
	}{
		{"same observation", 0, false, false, true},
		{"new observation", time.Second, false, true, true},
		{"late observation", -time.Second, false, false, false},
		{"same-time conflict", 0, false, true, false},
		{"silent support downgrade", time.Second, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := billingApplicabilityAccount(t, at, billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", true)))
			nextAt := at.Add(tc.offset)
			fixture := billingApplicabilityFixture(nextAt, billingApplicabilityEntry("responses_http", "unspecified", !tc.change))
			body := billingApplicabilityBody(t, nextAt, fixture)
			if tc.remove {
				var raw map[string]any
				require.NoError(t, json.Unmarshal(body, &raw))
				delete(raw, "rate_applicability")
				var err error
				body, err = json.Marshal(raw)
				require.NoError(t, err)
			}
			snapshot, rate := upstreamBillingRateSnapshotFromResult(account, 30, at.Add(time.Minute), upstreamBillingProbeHTTPResult{statusCode: 200, body: body}, 0.4)
			if tc.allowed {
				require.Equal(t, "ok", snapshot.Status)
				require.NotNil(t, rate)
			} else {
				require.Equal(t, "failed", snapshot.Status)
				require.Equal(t, "applicability_declaration_replayed", snapshot.LastError)
				require.Nil(t, rate)
				require.Equal(t, "fixture-1", stringMapValue(snapshot.Data[upstreamBillingApplicabilityKey].(map[string]any), "source_version"))
			}
		})
	}
}

func TestBillingApplicabilitySameObservationPriceConflict(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	fixture := billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", true))
	account := billingApplicabilityAccount(t, at, fixture)
	body := billingApplicabilityBody(t, at, fixture)
	var changed map[string]any
	require.NoError(t, json.Unmarshal(body, &changed))
	for _, key := range []string{"group_rate_multiplier", "resolved_rate_multiplier", "effective_rate_multiplier"} {
		changed[key] = 0.2
	}
	body, err := json.Marshal(changed)
	require.NoError(t, err)
	snapshot, rate := upstreamBillingRateSnapshotFromResult(account, 30, at.Add(time.Second), upstreamBillingProbeHTTPResult{statusCode: 200, body: body}, 0.4)
	require.Nil(t, rate)
	require.Equal(t, "applicability_declaration_replayed", snapshot.LastError)
	require.Equal(t, 0.4, snapshot.Data["resolved_rate_multiplier"])
}

func TestBillingApplicabilityPhysicalHTTP(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"model":"gpt-fixture","input":"hello"}`, string(body))
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)
	at := time.Now().UTC().Add(-time.Second)
	account := billingApplicabilityAccount(t, at, billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", true)))
	account.Credentials["base_url"] = server.URL
	account.Status, account.Schedulable, account.GroupIDs = StatusActive, true, []int64{41}
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 41, threshold: 0.5})
	for _, payload := range []string{`{"model":"gpt-fixture","input":"hello"}`, `{"model":"gpt-fixture","input":"hello","service_tier":"priority"}`} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", bytes.NewBufferString(payload))
		require.NoError(t, err)
		guarded, err := guardGatewayUpstreamRequest(req, account, repo, nil)
		if strings.Contains(payload, "priority") {
			require.True(t, IsOpenAITurnAdmissionError(err))
			require.False(t, IsOpenAIInitialAdmissionRejection(err), "a final-send denial does not authorize replay after earlier work")
			continue
		}
		require.NoError(t, err)
		resp, err := server.Client().Do(guarded)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
	require.EqualValues(t, 1, sends.Load(), "uncovered fast request must never reach the physical upstream")
}

func TestBillingApplicabilityReplayBodyAndLegacy(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	account := billingApplicabilityAccount(t, at, billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", true)))
	for _, tc := range []string{"mismatched replay", "missing replay", "encoded body", "legacy", "manual"} {
		t.Run(tc, func(t *testing.T) {
			candidate := *account
			candidate.Extra = mergeMap(nil, account.Extra)
			const body = `{"model":"gpt-fixture","input":"hello"}`
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://relay.example/v1/responses", bytes.NewBufferString(body))
			require.NoError(t, err)
			switch tc {
			case "mismatched replay":
				req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{"model":"other"}`)), nil }
			case "missing replay":
				req.GetBody = nil
			case "encoded body":
				req.Header.Set("Content-Encoding", "gzip")
			case "legacy":
				snapshot := decodeUpstreamBillingProbeSnapshot(candidate.Extra)
				delete(snapshot.Data, upstreamBillingApplicabilityKey)
				candidate.Extra[UpstreamBillingProbeExtraKey] = snapshot
				req.GetBody = nil
			case "manual":
				candidate.Extra[UpstreamBillingRateSyncEnabledExtraKey] = false
				req.GetBody = nil
			}
			_, err = guardUpstreamBillingRequest(t.Context(), &candidate, req)
			if tc == "legacy" || tc == "manual" {
				require.NoError(t, err)
			} else {
				require.True(t, IsOpenAITurnAdmissionError(err))
			}
			actual, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.Equal(t, body, string(actual))
		})
	}
}

func TestBillingApplicabilityZeroIsNotMissing(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	body := billingApplicabilityBody(t, at, billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", true)))
	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	for _, key := range []string{"group_rate_multiplier", "resolved_rate_multiplier", "effective_rate_multiplier"} {
		raw[key] = 0
	}
	body, err := json.Marshal(raw)
	require.NoError(t, err)
	account := profitControlSyncedTestAccount(at)
	snapshot, rate := upstreamBillingRateSnapshotFromResult(account, 30, at, upstreamBillingProbeHTTPResult{statusCode: 200, body: body}, 0.4)
	require.Equal(t, "ok", snapshot.Status)
	require.Equal(t, 0.0, snapshot.Data["resolved_rate_multiplier"])
	require.Nil(t, rate, "explicit zero remains visible but does not silently disable quota cost accounting")
	account.Extra[UpstreamBillingProbeExtraKey] = snapshot
	require.True(t, IsOpenAITurnAdmissionError(guardUpstreamBillingApplicability(account, &AvailabilityRequestScope{
		Model: "gpt-fixture", Protocol: "responses_http", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text",
	})))
	preview := previewAccountProfitAdmission(account, true, 1, 1, at)
	require.Equal(t, ProfitPreviewClassRejectedUpstreamRate, preview.Class)
	require.Contains(t, preview.Warnings, ProfitPreviewWarningScopeNotEvaluated)
}

func TestBillingApplicabilityPhysicalRetryRechecksExpiry(t *testing.T) {
	for _, source := range []string{"automatic", "manual"} {
		t.Run(source, func(t *testing.T) {
			at := time.Now().UTC().Add(-time.Minute)
			account := billingApplicabilityAccount(t, at, billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", true)))
			if source == "manual" {
				account = billingManualAccount()
				_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, billingApplicabilityEntry("responses_http", "unspecified", true)), at)
				require.NoError(t, err)
			}
			account.GroupIDs = []int64{41}
			ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 41, threshold: 0.5})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://relay.example/v1/responses", bytes.NewBufferString(`{"model":"gpt-fixture","input":"hello"}`))
			require.NoError(t, err)
			svc := &OpenAIGatewayService{}
			req, err = svc.guardOpenAIProfitRequest(req, account)
			require.NoError(t, err, "entry admission has valid evidence")
			if source == "manual" {
				stored := account.Extra[UpstreamBillingManualApplicabilityExtraKey].(upstreamBillingManualApplicability)
				stored.ExpiresAt = time.Now().Add(-time.Second)
				account.Extra[UpstreamBillingManualApplicabilityExtraKey] = stored
			} else {
				snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra)
				snapshot.Data[upstreamBillingApplicabilityKey].(map[string]any)["expires_at"] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
				account.Extra[UpstreamBillingProbeExtraKey] = snapshot
			}
			// No HTTPUpstream is installed: a stale physical attempt must stop before
			// transport, submission accounting, or supplier-fault attribution.
			response, err := svc.doOpenAIProxyAttempt(req, account, runtimeProxyEgress{})
			require.Nil(t, response)
			var denied *OpenAITurnAdmissionError
			require.ErrorAs(t, err, &denied)
			require.Equal(t, "profit_upstream_scope_expired", denied.Reason)
			require.False(t, IsOpenAIInitialAdmissionRejection(err))
		})
	}
}
