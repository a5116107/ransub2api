//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUpstreamGroupCatalogIntegrationCollectReadFailureRecoveryAndIsolation(t *testing.T) {
	ctx := t.Context()
	db, client := UpstreamChannelsTestDatabase(t)
	defer func() { _ = client.Close(); _ = db.Close() }()
	var phase, calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer fixture-management" || r.Header.Get("New-Api-User") != "7" {
			w.WriteHeader(401)
			return
		}
		body := "{}"
		switch r.URL.Path {
		case "/api/status":
			body = `{"success":true,"data":{"quota_per_unit":500000}}`
		case "/api/user/self":
			body = `{"success":true,"data":{"id":7,"quota":1000000}}`
		case "/api/pricing":
			body = `{"success":true,"group_ratio":{"vip":0.8,"other":0.9,"public-only":1}}`
			if phase.Load() >= 3 {
				at := time.Now().UTC().Add(-time.Minute)
				body = fmt.Sprintf(`{"success":true,"pricing_version":"catalog-v1","group_ratio":{"vip":0,"other":0.9},"rate_applicability":{"vip":{"schema_version":1,"source_version":"catalog-v1","group_rate_multiplier":0,"observed_at":%q,"expires_at":%q,"entries":[{"model":"gpt-fixture","protocol":"responses_http","service_tier":"unspecified","reasoning_effort":"high","context":"all","group_rate_applies":true},{"model":"gpt-fixture","protocol":"responses_http","service_tier":"priority","reasoning_effort":"high","context":"all","group_rate_applies":false}]}}}`, at.Format(time.RFC3339Nano), at.Add(time.Hour).Format(time.RFC3339Nano))
			}
		case "/api/user/self/groups":
			body = `{"success":true,"data":{"vip":{"ratio":0.1,"desc":"Same name"},"other":{"ratio":0.2,"desc":"Same name"}}}`
			if phase.Load() == 1 {
				body = `{"success":true,"data":{"vip":{"ratio":0,"desc":"Renamed"},"other":{"ratio":0.2,"desc":"Same name"}}}`
			}
			if phase.Load() == 2 {
				w.WriteHeader(503)
				body = `{"message":"private failure body must not persist"}`
			}
			if phase.Load() == 3 {
				body = `{"success":true,"data":{"vip":{"ratio":0,"desc":"Renamed"}}}`
			}
		case "/api/token/":
			body = `{"success":true,"data":{"items":[{"id":3,"key":"fixture-call-first","group":"vip"}],"total":1,"p":0,"page_size":100}}`
		default:
			w.WriteHeader(404)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	source, id := seedProcurementSourceMappingRoute(t, db, "catalog-source", "openai")
	_, err := db.ExecContext(ctx, `UPDATE accounts SET credentials=$2::jsonb WHERE id=$1`, id, fmt.Sprintf(`{"base_url":%q,"api_key":"sk-fixture-call-first"}`, server.URL+"/v1"))
	require.NoError(t, err)
	mappings := NewChannelRepository(db).(service.ProcurementSourceMappingRepository)
	mapping, err := mappings.CreateProcurementSourceMapping(ctx, domain.ProcurementSourceMappingCommand{Version: 1, SiteCode: "catalog-supplier", SiteName: "Supplier", Platform: "openai", ExternalGroupID: "vip", LocalGroupID: source, AccountID: id, Confirm: true})
	require.NoError(t, err)
	product, _ := seedProcurementSourceMappingRoute(t, db, "catalog-product", "openai")
	_, err = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$2::jsonb WHERE id=$1`, product, fmt.Sprintf(`{"version":6,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"not_required","availability":"not_required","source_group_ids":[%d]}`, source))
	require.NoError(t, err)
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	upstream := NewHTTPUpstream(cfg).(*httpUpstreamService)
	defer func() {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()
		for _, entry := range upstream.clients {
			entry.client.CloseIdleConnections()
		}
	}()
	encryptor, err := NewAESEncryptor(&config.Config{Totp: config.TotpConfig{EncryptionKey: strings.Repeat("42", 32)}})
	require.NoError(t, err)
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	var originalRate float64
	var originalMemberships int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT rate_multiplier::float8,(SELECT count(*) FROM account_groups WHERE account_id=$1) FROM accounts WHERE id=$1`, id).Scan(&originalRate, &originalMemberships))
	tester := service.NewAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil)
	runner := service.NewUpstreamBillingProbeService(accounts, tester, nil)
	auth := NewNewAPIAuthorizationRepository(db)
	runner.SetNewAPIAuthorization(auth, encryptor, true)
	_, err = runner.SaveNewAPIConfig(ctx, id, service.NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-management", AccountIDs: []int64{id}})
	require.NoError(t, err)
	probe := func() *service.UpstreamGroupCatalogView {
		t.Helper()
		_, err := runner.ProbeAccount(ctx, id)
		require.NoError(t, err)
		before := calls.Load()
		// Reconstruct repositories/runners to prove the result is durable, not
		// a collector-local cache, and that reading does not call the supplier.
		reader := service.NewUpstreamBillingProbeService(newAccountRepositoryWithSQL(client, db, nil), nil, nil)
		reader.SetNewAPIAuthorization(NewNewAPIAuthorizationRepository(db), encryptor, true)
		view, err := reader.GetUpstreamGroupCatalog(ctx, id)
		require.NoError(t, err)
		require.Equal(t, before, calls.Load())
		return view
	}
	first := probe()
	require.Equal(t, "ok", first.Catalog.Status)
	require.Len(t, first.Catalog.Groups, 2, "same names do not collapse stable group IDs")
	require.Empty(t, first.Catalog.Changes)
	firstSource, err := mappings.GetProcurementSourceMapping(ctx, mapping.ID)
	require.NoError(t, err)
	require.Equal(t, "supplier_declared", firstSource.Evidence.Selection.Status)
	require.Equal(t, "not_declared", firstSource.Evidence.Models.Status, "a pricing catalog alone must not invent model/tier coverage")
	require.Contains(t, first.Impact.Sources, service.UpstreamCatalogRelated{ID: mapping.ID, Name: "Supplier / vip"})
	require.Contains(t, first.Impact.Products, service.UpstreamCatalogRelated{ID: product, Name: "catalog-product-group"})
	phase.Store(1)
	changed := probe()
	require.Len(t, changed.Catalog.Changes, 1)
	require.Equal(t, "vip", changed.Catalog.Changes[0].GroupID)
	require.Equal(t, 0.0, *changed.Catalog.Changes[0].After.PersonalRate)
	phase.Store(2)
	failed := probe()
	require.Equal(t, "failed", failed.Catalog.Status)
	require.Equal(t, changed.Catalog.ReceivedAt, failed.Catalog.ReceivedAt)
	require.Equal(t, changed.Catalog.FreshUntil, failed.Catalog.FreshUntil)
	require.Len(t, failed.Catalog.Changes, 1)
	require.Len(t, failed.Catalog.Groups, 2)
	failedSource, err := mappings.GetProcurementSourceMapping(ctx, mapping.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", failedSource.Evidence.Selection.Status)
	require.Empty(t, failedSource.Evidence.Models.Entries)
	phase.Store(3)
	recovered := probe()
	require.Equal(t, "ok", recovered.Catalog.Status)
	require.Equal(t, "group_removed", recovered.Catalog.Changes[0].Kind)
	require.Equal(t, "other", recovered.Catalog.Changes[0].GroupID)
	validSource, err := mappings.GetProcurementSourceMapping(ctx, mapping.ID)
	require.NoError(t, err)
	require.Equal(t, "supplier_declared", validSource.Evidence.Selection.Status)
	require.Equal(t, "declared", validSource.Evidence.Models.Status)
	require.Len(t, validSource.Evidence.Models.Entries, 2)
	require.Equal(t, "covered", validSource.Evidence.Models.Entries[0].Conclusion)
	require.Equal(t, "excluded", validSource.Evidence.Models.Entries[1].Conclusion)
	require.False(t, validSource.Dispatchable)
	_, err = db.ExecContext(ctx, `UPDATE procurement_supplier_groups SET external_group_id='wrong-supplier-group' WHERE id=$1`, mapping.SourceGroupID)
	require.NoError(t, err)
	mismatch, err := mappings.GetProcurementSourceMapping(ctx, mapping.ID)
	require.NoError(t, err)
	require.Equal(t, "mismatch", mismatch.Evidence.Selection.Status)
	require.Empty(t, mismatch.Evidence.Models.Entries)
	_, err = db.ExecContext(ctx, `UPDATE procurement_supplier_groups SET external_group_id='vip' WHERE id=$1`, mapping.SourceGroupID)
	require.NoError(t, err)
	var raw, accountExtra string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT group_catalog::text FROM new_api_site_authorizations WHERE site_url=$1`, server.URL).Scan(&raw))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT extra::text FROM accounts WHERE id=$1`, id).Scan(&accountExtra))
	for _, secret := range []string{"fixture-management", "fixture-call-first", "private failure body"} {
		require.NotContains(t, raw, secret)
		require.NotContains(t, accountExtra, secret)
	}
	require.NotContains(t, accountExtra, "group_catalog", "shared directories must be stored once, not duplicated on each account")
	declared, err := mappings.GetProcurementSourceMapping(ctx, mapping.ID)
	require.NoError(t, err)
	require.False(t, declared.Dispatchable, "a successful catalog does not qualify an account")
	require.Equal(t, mapping.Revision, declared.Revision)
	var currentRate float64
	var currentMemberships int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT rate_multiplier::float8,(SELECT count(*) FROM account_groups WHERE account_id=$1) FROM accounts WHERE id=$1`, id).Scan(&currentRate, &currentMemberships))
	require.Equal(t, originalRate, currentRate)
	require.Equal(t, originalMemberships, currentMemberships)
	_, err = runner.GetUpstreamGroupCatalog(service.WithObserverScope(ctx, []int64{source}), id)
	require.ErrorIs(t, err, service.ErrObserverScope)
	// Rotating management authorization makes the old live quote explicitly
	// stale. A losing old probe cannot replace it, even with a newer timestamp.
	oldBinding, err := auth.GetBinding(ctx, id)
	require.NoError(t, err)
	oldAccount, err := accounts.GetByID(ctx, id)
	require.NoError(t, err)
	_, err = runner.SaveNewAPIConfig(ctx, id, service.NewAPIConfigRequest{UserID: 7, AccountIDs: []int64{id}})
	require.NoError(t, err)
	rotated, err := runner.GetUpstreamGroupCatalog(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "authorization_changed", rotated.Catalog.Status)
	rotatedSource, err := mappings.GetProcurementSourceMapping(ctx, mapping.ID)
	require.NoError(t, err)
	require.Equal(t, "not_observed", rotatedSource.Evidence.Selection.Status)
	require.Empty(t, rotatedSource.Evidence.Models.Entries)
	require.ErrorIs(t, auth.WriteSnapshot(ctx, oldAccount, oldBinding, &service.UpstreamBillingProbeSnapshot{CatalogObservation: recovered.Catalog}), service.ErrUpstreamBillingProbeIdentityChanged)
	// Old application reads do not depend on the additive field. The local
	// migration rollback rehearsal is transactional and preserves observations.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, func() error {
		_, e := tx.ExecContext(ctx, `ALTER TABLE new_api_site_authorizations DROP COLUMN group_catalog`)
		return e
	}())
	var revision int64
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT revision FROM new_api_site_authorizations WHERE site_url=$1`, server.URL).Scan(&revision))
	require.NoError(t, tx.Rollback())
	var restored string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT group_catalog::text FROM new_api_site_authorizations WHERE site_url=$1`, server.URL).Scan(&restored))
	require.Equal(t, raw, restored)
	// A future catalog is neither downgraded nor used as a new empty baseline.
	// The established independent billing refresh must still complete.
	_, err = db.ExecContext(ctx, `UPDATE new_api_site_authorizations SET group_catalog='{"version":99}'::jsonb WHERE site_url=$1`, server.URL)
	require.NoError(t, err)
	snap, err := runner.ProbeAccount(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "ok", snap.Status)
	_, err = runner.GetUpstreamGroupCatalog(ctx, id)
	require.Error(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT group_catalog::text FROM new_api_site_authorizations WHERE site_url=$1`, server.URL).Scan(&restored))
	require.JSONEq(t, `{"version":99}`, restored)
}

func TestUpstreamGroupCatalogIntegrationSharedProfileSerializesAndRejectsLateWrites(t *testing.T) {
	ctx := context.Background()
	a, b, site := newAPIIntegrationAccounts(t)
	auth := NewNewAPIAuthorizationRepository(integrationDB)
	require.NoError(t, auth.Save(ctx, &service.NewAPISiteAuthorization{SiteURL: site, UserID: 7, Ciphertext: "fixture-ciphertext"}, []service.NewAPIBindingSave{{Account: a, TokenID: 3, Group: "vip"}, {Account: b, TokenID: 4, Group: "vip"}}))
	accounts := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	loadedA, err := accounts.GetByID(ctx, a.ID)
	require.NoError(t, err)
	loadedB, err := accounts.GetByID(ctx, b.ID)
	require.NoError(t, err)
	bindA, err := auth.GetBinding(ctx, a.ID)
	require.NoError(t, err)
	bindB, err := auth.GetBinding(ctx, b.ID)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	snapshot := func(at time.Time, rate float64) *service.UpstreamBillingProbeSnapshot {
		fresh := at.Add(time.Hour)
		return &service.UpstreamBillingProbeSnapshot{Status: "ok", LastAttemptAt: at, CatalogObservation: &service.UpstreamGroupCatalog{Version: 1, Provider: "new_api", Scope: "management_user", Status: "ok", AuthorizationRevision: bindA.Profile.Revision, AttemptedAt: at, ReceivedAt: &at, FreshUntil: &fresh, Groups: []service.UpstreamGroupQuote{{ID: "vip", PersonalRate: &rate}}}}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, job := range []struct {
		account *service.Account
		binding *service.NewAPIAccountBinding
		snap    *service.UpstreamBillingProbeSnapshot
	}{{loadedA, bindA, snapshot(now, .1)}, {loadedB, bindB, snapshot(now.Add(time.Second), .2)}} {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- auth.WriteSnapshot(ctx, job.account, job.binding, job.snap) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	read := auth.(service.UpstreamGroupCatalogRepository)
	catalog, err := read.ReadNewAPIGroupCatalog(ctx, bindA.Profile.ID, bindA.Profile.Revision)
	require.NoError(t, err)
	require.Equal(t, .2, *catalog.Groups[0].PersonalRate)
	// The same site with another purchasing user has no access to this catalog.
	require.NoError(t, auth.Save(ctx, &service.NewAPISiteAuthorization{SiteURL: site, UserID: 8, Ciphertext: "other-ciphertext"}, []service.NewAPIBindingSave{{Account: loadedA, TokenID: 5, Group: "vip"}}))
	other, err := auth.GetBinding(ctx, a.ID)
	require.NoError(t, err)
	otherCatalog, err := read.ReadNewAPIGroupCatalog(ctx, other.Profile.ID, other.Profile.Revision)
	require.NoError(t, err)
	require.Nil(t, otherCatalog)
	var serialized map[string]any
	raw, err := json.Marshal(catalog)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &serialized))
	require.NotContains(t, serialized, "ciphertext")
}
