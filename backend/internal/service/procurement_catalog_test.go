package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

const catalogCommandFixture = `{"version":1,"revision":0,"buckets":[{"code":"b1","name":"First","min_rate":"0.2","max_rate":"0.5"},{"code":"b2","name":"Second","min_rate":"0.5","max_rate":"0.6"}]}`

func TestProcurementCatalogStrictCommand(t *testing.T) {
	c, err := ParseProcurementCatalogCommand([]byte(catalogCommandFixture))
	require.NoError(t, err)
	require.Len(t, c.Buckets, 2)
	for _, raw := range []string{
		`null`, `{}`, `[]`, strings.Replace(catalogCommandFixture, `"revision":0`, `"revision":null`, 1),
		strings.Replace(catalogCommandFixture, `"revision":0`, `"Revision":0`, 1),
		strings.Replace(catalogCommandFixture, `"revision":0`, `"revision":0,"revision":1`, 1),
		strings.Replace(catalogCommandFixture, `"revision":0`, `"revision":0,"revision":0`, 1),
		strings.Replace(catalogCommandFixture, `"code":"b1"`, `"code":"b1","code":"b2"`, 1),
		strings.Replace(catalogCommandFixture, `"code":"b1"`, `"code":"b1","code":"b1"`, 1),
		strings.Replace(catalogCommandFixture, `"min_rate":"0.2",`, ``, 1),
		strings.Replace(catalogCommandFixture, `"max_rate":"0.5"`, `"max_rate":"0.5","credential":"private"`, 1),
		`{"version":1,"revision":0,"buckets":null}`,
		catalogCommandFixture + ` {}`,
	} {
		_, err := ParseProcurementCatalogCommand([]byte(raw))
		require.ErrorIs(t, err, ErrProcurementCatalogInvalid, raw)
	}
}

func TestProcurementCatalogPreviewScopeAndPriceAuthority(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	catalog, err := ParseProcurementCatalogCommand([]byte(catalogCommandFixture))
	require.NoError(t, err)
	catalog = catalog.WithRevision(1)
	request := ProfitPreviewRequestScope{AccountID: 1, PublicModel: "gpt-fixture", Scope: AvailabilityRequestScope{Version: 2, Protocol: "responses_http", Model: "gpt-fixture", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}}
	for _, tc := range []struct {
		name        string
		mutate      func(*Account, *ProfitPreviewRequestScope)
		state, code string
	}{
		{"covered", func(*Account, *ProfitPreviewRequestScope) {}, "classified", "b1"},
		{"price rise", func(a *Account, _ *ProfitPreviewRequestScope) { v := .5; a.RateMultiplier = &v }, "classified", "b2"},
		{"gap", func(a *Account, _ *ProfitPreviewRequestScope) { v := .6; a.RateMultiplier = &v }, "unclassified", ""},
		{"wrong model", func(_ *Account, r *ProfitPreviewRequestScope) { r.Scope.Model = "other" }, "scope_unconfirmed", ""},
		{"unsupported public", func(_ *Account, r *ProfitPreviewRequestScope) { r.PublicModel = "other" }, "scope_unconfirmed", ""},
		{"priority", func(_ *Account, r *ProfitPreviewRequestScope) { r.Scope.ServiceTier = "priority" }, "scope_unconfirmed", ""},
		{"unknown scope", func(_ *Account, r *ProfitPreviewRequestScope) { r.Scope.Version = 99 }, "scope_unconfirmed", ""},
		{"manual no declaration", func(a *Account, _ *ProfitPreviewRequestScope) { a.Extra = map[string]any{} }, "scope_unconfirmed", ""},
		{"missing price", func(a *Account, _ *ProfitPreviewRequestScope) { a.RateMultiplier = nil }, "rate_unavailable", ""},
		{"stale", func(a *Account, _ *ProfitPreviewRequestScope) {
			s := decodeUpstreamBillingProbeSnapshot(a.Extra)
			until := at
			s.FreshUntil = &until
			a.Extra[UpstreamBillingProbeExtraKey] = s
		}, "rate_unavailable", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := billingApplicabilityAccount(t, at, billingApplicabilityFixture(at, billingApplicabilityEntry("responses_http", "unspecified", true)))
			a.ID = 1
			a.Credentials["model_mapping"] = map[string]any{"gpt-fixture": "gpt-fixture"}
			r := request
			tc.mutate(a, &r)
			result := ClassifyProcurementScope(catalog, a, r, at.Add(time.Second))
			require.Equal(t, tc.state, result.State)
			require.Equal(t, "not_evaluated", result.Qualification)
			if tc.code == "" {
				require.Nil(t, result.Bucket)
			} else {
				require.Equal(t, tc.code, result.Bucket.Code)
				require.NotNil(t, result.EffectiveRate)
			}
			raw, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "credentials")
			require.NotContains(t, string(raw), "test-api-key")
		})
	}
	result := ClassifyProcurementScope(catalog, nil, request, at)
	require.Equal(t, "account_unavailable", result.State)
	result = ClassifyProcurementScope(&domain.ProcurementCatalog{Version: 1, Buckets: []domain.ProcurementBucket{}}, nil, request, at)
	require.Equal(t, "not_configured", result.State)
}

func TestProcurementPreviewStrictInputAndResponsesVerbosity(t *testing.T) {
	raw := `{"requests":[{"account_id":1,"public_model":"gpt-fixture","scope":{"version":4,"protocol":"responses_http","stream":false,"model":"gpt-fixture","service_tier":"unspecified","reasoning_effort":"unspecified","capability":"text","text_verbosity":"medium"}}]}`
	r, err := ParseProcurementPreviewCommand([]byte(raw))
	require.NoError(t, err)
	require.Equal(t, "medium", r[0].Scope.TextVerbosity)
	for _, invalid := range []string{`{}`, `{"requests":null}`, `{"requests":[]}`, strings.Replace(raw, `"account_id":1`, `"account_id":0`, 1), strings.Replace(raw, `"public_model":"gpt-fixture"`, `"public_model":null`, 1), strings.Replace(raw, `"text_verbosity":"medium"`, `"text_verbosity":"future"`, 1), strings.Replace(raw, `"requests":`, `"confirm":true,"requests":`, 1), strings.Replace(raw, `"model":"gpt-fixture"`, `"model":"gpt-fixture","model":"other"`, 1), strings.Replace(raw, `"account_id":1`, `"account_id":1,"rate":0.02`, 1),
		strings.Replace(raw, `"account_id":1`, `"account_id":1,"account_id":1`, 1),
		strings.Replace(raw, `"model":"gpt-fixture"`, `"model":"gpt-fixture","model":"gpt-fixture"`, 1), strings.Replace(raw, `"requests":[`, `"requests":[],"requests":[`, 1)} {
		_, err := ParseProcurementPreviewCommand([]byte(invalid))
		require.ErrorIs(t, err, ErrProcurementCatalogInvalid, invalid)
	}
}
