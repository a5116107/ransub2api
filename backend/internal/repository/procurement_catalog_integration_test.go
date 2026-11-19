//go:build integration

package repository_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestProcurementCatalogPostgresHTTPAndSnapshot(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	repo := repository.NewChannelRepository(db).(service.ProcurementCatalogRepository)
	h := admin.NewChannelHandler(service.NewChannelService(repository.NewChannelRepository(db), nil, nil, nil, nil), nil, nil)
	router := gin.New()
	router.GET("/catalog", h.ProcurementCatalog)
	router.PUT("/catalog", h.ProcurementCatalog)
	router.POST("/preview", h.ProcurementClassificationPreview)
	run := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, request)
		return w
	}
	bound := func(v string) *string { return &v }
	catalog := &domain.ProcurementCatalog{Version: 1, Buckets: []domain.ProcurementBucket{
		{Code: "low", Name: "Low", MinRate: bound("0.02"), MaxRate: bound("0.03")},
		{Code: "next", Name: "Next", MinRate: bound("0.03"), MaxRate: bound("0.05")},
	}}
	encode := func(value any) string {
		t.Helper()
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		return string(raw)
	}

	t.Run("default and first concurrent creation", func(t *testing.T) {
		out := run(http.MethodGet, "/catalog", "")
		require.Equal(t, 200, out.Code, out.Body.String())
		require.Contains(t, out.Body.String(), `"revision":0`)
		require.Contains(t, out.Body.String(), `"buckets":[]`)
		start := make(chan struct{})
		outcomes := make(chan error, 2)
		var wait sync.WaitGroup
		for range 2 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				_, err := repo.SaveProcurementCatalog(ctx, catalog)
				outcomes <- err
			}()
		}
		close(start)
		wait.Wait()
		close(outcomes)
		successes := 0
		for err := range outcomes {
			if err == nil {
				successes++
			} else {
				require.ErrorIs(t, err, service.ErrProcurementCatalogConflict)
			}
		}
		require.Equal(t, 1, successes)
	})

	t.Run("HTTP invalid and stale commands preserve saved state", func(t *testing.T) {
		for _, body := range []string{`{}`, `null`, `{"version":1,"revision":1,"buckets":null}`, `{"version":2,"revision":1,"buckets":[]}`, strings.Replace(encode(catalog), `"revision":0`, `"revision":1,"revision":1`, 1), strings.Repeat("x", 65537)} {
			out := run(http.MethodPut, "/catalog", body)
			require.Equal(t, 400, out.Code, out.Body.String())
		}
		out := run(http.MethodPut, "/catalog", encode(catalog))
		require.Equal(t, 409, out.Code)
		require.Contains(t, out.Body.String(), "PROCUREMENT_CATALOG_CONFLICT")
		loaded, err := repo.ReadProcurementCatalog(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, loaded.Revision)
		require.Equal(t, "0.03", *loaded.Buckets[0].MaxRate)
		loaded.Buckets[0].Name = "Reviewed low"
		out = run(http.MethodPut, "/catalog", encode(loaded))
		require.Equal(t, 200, out.Code, out.Body.String())
		loaded, err = repo.ReadProcurementCatalog(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 2, loaded.Revision)
		require.Equal(t, "Reviewed low", loaded.Buckets[0].Name)
	})

	var accountID, groupID int64
	creds := `{"base_url":"https://relay.invalid","api_key":"catalog-fixture-secret","model_mapping":{"public-model":"wire-model"}}`
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,credentials,extra,rate_multiplier,group_rate_multiplier,status,schedulable) VALUES('catalog account','openai','apikey',$1::jsonb,'{"cost_multiplier":0.99}',0.025,1,'active',false) RETURNING id`, creds).Scan(&accountID))
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,rate_multiplier) VALUES('manual sales product','openai',0.15) RETURNING id`).Scan(&groupID))
	_, err := db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id,priority) VALUES($1,$2,7)`, accountID, groupID)
	require.NoError(t, err)
	management := service.NewAdminService(&config.Config{}, nil, repository.NewAdminGroupRepository(client, db), repository.NewAdminAccountRepository(client, db, nil), nil, nil, nil, nil, nil, nil, nil, nil, nil, client, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	attest := func() {
		t.Helper()
		current, err := repository.NewAccountRepository(client, db, nil).GetByID(ctx, accountID)
		require.NoError(t, err)
		revision := ""
		if stored, ok := current.Extra[service.UpstreamBillingManualApplicabilityExtraKey].(map[string]any); ok {
			revision, _ = stored["revision"].(string)
		}
		command := map[string]any{"source_version": "reviewed-fixture-v1", "reason": "local contract fixture", "expires_at": time.Now().UTC().Add(time.Hour), "confirm_current_binding": true, "expected_revision": revision, "entries": []any{map[string]any{"model": "wire-model", "protocol": "responses_http", "service_tier": "unspecified", "reasoning_effort": "unspecified", "context": "all", "group_rate_applies": true}}}
		_, err = management.UpdateAccount(ctx, accountID, &service.UpdateAccountInput{ManualRateApplicability: json.RawMessage(encode(command))})
		require.NoError(t, err)
	}
	request := service.ProfitPreviewRequestScope{AccountID: accountID, PublicModel: "public-model", Scope: service.AvailabilityRequestScope{Version: 2, Protocol: "responses_http", Model: "wire-model", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}}
	preview := func(scopes ...service.ProfitPreviewRequestScope) *service.ProcurementClassificationPreview {
		t.Helper()
		out := run(http.MethodPost, "/preview", encode(map[string]any{"requests": scopes}))
		require.Equal(t, 200, out.Code, out.Body.String())
		require.NotContains(t, out.Body.String(), "catalog-fixture-secret")
		require.NotContains(t, out.Body.String(), "relay.invalid")
		require.NotContains(t, out.Body.String(), "credentials")
		var response struct {
			Data service.ProcurementClassificationPreview `json:"data"`
		}
		require.NoError(t, json.Unmarshal(out.Body.Bytes(), &response))
		return &response.Data
	}

	t.Run("scope without declaration is not classified", func(t *testing.T) {
		out := preview(request)
		require.Equal(t, "scope_unconfirmed", out.Results[0].State)
		require.Nil(t, out.Results[0].Bucket)
		require.Equal(t, "not_evaluated", out.Results[0].Qualification)
	})
	attest()
	t.Run("product policy is evaluated in the original scope preview", func(t *testing.T) {
		t.Cleanup(func() {
			_, err := db.ExecContext(ctx, `UPDATE groups SET profit_control_enabled=false,procurement_policy=null WHERE id=$1`, groupID)
			require.NoError(t, err)
		})
		policy := &domain.ProcurementPolicy{Version: 2, Revision: 1, Sources: &domain.ProcurementSources{AccountIDs: []int64{}}}
		savePolicy := func(enabled bool) {
			t.Helper()
			_, err := db.ExecContext(ctx, `UPDATE groups SET profit_control_enabled=$2,procurement_policy=$3::jsonb WHERE id=$1`, groupID, enabled, encode(policy))
			require.NoError(t, err)
		}
		readProduct := func(id int64) *service.ProcurementClassificationPreview {
			t.Helper()
			out := run(http.MethodPost, "/preview", encode(map[string]any{"product_group_id": id, "requests": []service.ProfitPreviewRequestScope{request}}))
			require.Equal(t, 200, out.Code, out.Body.String())
			require.NotContains(t, out.Body.String(), "catalog-fixture-secret")
			require.NotContains(t, out.Body.String(), "relay.invalid")
			var response struct {
				Data service.ProcurementClassificationPreview `json:"data"`
			}
			require.NoError(t, json.Unmarshal(out.Body.Bytes(), &response))
			return &response.Data
		}
		savePolicy(true)
		out := readProduct(groupID)
		require.Equal(t, "classified", out.Results[0].State)
		require.Equal(t, "blocked", out.Results[0].Product.State)
		require.Equal(t, service.ProfitPreviewClassRejectedSource, out.Results[0].Product.PriceClass)
		require.Equal(t, "fail", out.Results[0].Product.EvaluatedGates.State)
		policy.Sources.AccountIDs = []int64{accountID}
		policy.Revision++
		savePolicy(true)
		out = readProduct(groupID)
		require.Equal(t, "constraints_passed", out.Results[0].Product.State)
		require.Equal(t, .15, out.Results[0].Product.DefaultD)
		require.EqualValues(t, 2, out.Results[0].Product.PolicyRevision)
		require.Equal(t, "group_default", out.Results[0].Product.PriceBasis)
		require.Equal(t, "pass", out.Results[0].Product.EvaluatedGates.State)
		require.Equal(t, 1, out.Results[0].Product.EvaluatedGates.RequiredGates)
		require.Equal(t, 1, out.Results[0].Product.EvaluatedGates.PassedGates)
		require.Equal(t, out.ObservedAt, out.Results[0].Product.EvaluatedGates.EvaluatedAt)
		require.Nil(t, out.Results[0].Product.EvaluatedGates.ValidUntil)
		require.Equal(t, "not_evaluated", out.Results[0].Qualification, "manual schedulable=false is not silently upgraded to full eligibility")

		policy.Version = 3
		policy.Buckets = &domain.ProcurementBuckets{Codes: []string{"next"}}
		policy.Revision++
		savePolicy(true)
		out = readProduct(groupID)
		require.Equal(t, "low", out.Results[0].Bucket.Code)
		require.Equal(t, "blocked", out.Results[0].Product.State)
		require.Equal(t, service.ProfitPreviewClassRejectedBucket, out.Results[0].Product.PriceClass)
		require.Equal(t, out.Catalog.Revision, *out.Results[0].Product.CatalogRevision)
		policy.Buckets.Codes = []string{"low"}
		policy.Revision++
		savePolicy(true)
		require.Equal(t, "constraints_passed", readProduct(groupID).Results[0].Product.State)
		policy.Buckets.Codes = []string{"low", "removed"}
		policy.Revision++
		savePolicy(true)
		out = readProduct(groupID)
		require.Equal(t, service.ProfitPreviewClassUnknownCatalog, out.Results[0].Product.PriceClass)
		require.Equal(t, "unknown", out.Results[0].Product.EvaluatedGates.State)
		policy.Buckets.Codes = nil
		policy.Revision++
		savePolicy(true)
		require.Equal(t, "constraints_passed", readProduct(groupID).Results[0].Product.State)
		policy.Version, policy.Quality, policy.Availability = 5, "current_answer", "current_state"
		policy.Revision++
		savePolicy(true)
		out = readProduct(groupID)
		require.Equal(t, "constraints_passed", out.Results[0].Product.State)
		require.Equal(t, "unknown", out.Results[0].Product.EvaluatedGates.State)
		require.Equal(t, "answer_quality_unavailable", out.Results[0].Product.EvaluatedGates.Reason)
		require.Equal(t, 3, out.Results[0].Product.EvaluatedGates.RequiredGates)
		require.Equal(t, 1, out.Results[0].Product.EvaluatedGates.PassedGates)
		require.Equal(t, "not_evaluated", out.Results[0].Qualification)
		policy.Quality, policy.Availability = "not_required", "not_required"
		policy.MinRate, policy.MaxRate = bound("0.03"), bound("0.05")
		policy.Revision++
		savePolicy(true)
		out = readProduct(groupID)
		require.Equal(t, service.ProfitPreviewClassRejectedProcurement, out.Results[0].Product.PriceClass)
		savePolicy(false)
		require.Equal(t, "disabled", readProduct(groupID).Results[0].Product.State)
		savePolicy(true)
		_, err := db.ExecContext(ctx, `DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, accountID, groupID)
		require.NoError(t, err)
		require.Equal(t, "not_associated", readProduct(groupID).Results[0].Product.State)
		_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id,priority) VALUES($1,$2,7)`, accountID, groupID)
		require.NoError(t, err)
		missing := run(http.MethodPost, "/preview", encode(map[string]any{"product_group_id": 99999999, "requests": []service.ProfitPreviewRequestScope{request}}))
		require.Equal(t, 503, missing.Code)
		require.Contains(t, missing.Body.String(), "PROCUREMENT_CATALOG_UNAVAILABLE")
		require.NotContains(t, missing.Body.String(), "SELECT")
		for _, table := range []string{"usage_logs", "monitoring_probe_budget_counters", "monitoring_probe_budget_leases"} {
			var count int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
			require.Zero(t, count)
		}
	})
	t.Run("current authority precise scope and unchanged manual relations", func(t *testing.T) {
		other := request
		other.Scope.ServiceTier = "priority"
		missing := request
		missing.AccountID = 99999999
		out := preview(request, other, missing)
		require.Len(t, out.Results, 3)
		require.Equal(t, "low", out.Results[0].Bucket.Code)
		require.Equal(t, "0.025", *out.Results[0].EffectiveRate)
		require.Equal(t, "manual", out.Results[0].RateSource)
		require.Equal(t, "scope_unconfirmed", out.Results[1].State)
		require.Nil(t, out.Results[1].Bucket)
		require.Equal(t, "account_unavailable", out.Results[2].State)
		var schedulable bool
		var rate, sale float64
		var priority int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT a.schedulable,a.rate_multiplier,g.rate_multiplier,ag.priority FROM accounts a JOIN account_groups ag ON ag.account_id=a.id JOIN groups g ON g.id=ag.group_id WHERE a.id=$1 AND g.id=$2`, accountID, groupID).Scan(&schedulable, &rate, &sale, &priority))
		require.False(t, schedulable, "classification grants no scheduling access")
		require.Equal(t, .025, rate)
		require.Equal(t, .15, sale)
		require.Equal(t, 7, priority)
	})
	t.Run("price change invalidates the previous scope before new classification", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `UPDATE accounts SET rate_multiplier=0.03 WHERE id=$1`, accountID)
		require.NoError(t, err)
		out := preview(request)
		require.Equal(t, "scope_unconfirmed", out.Results[0].State)
		require.Nil(t, out.Results[0].Bucket)
		attest()
		out = preview(request)
		require.Equal(t, "next", out.Results[0].Bucket.Code, "exclusive boundary belongs to the next bucket")
		_, err = db.ExecContext(ctx, `UPDATE accounts SET rate_multiplier=0.025 WHERE id=$1`, accountID)
		require.NoError(t, err)
		attest()
	})
	t.Run("bounded 100 scope preview and malformed input", func(t *testing.T) {
		batch := make([]service.ProfitPreviewRequestScope, 100)
		for i := range batch {
			batch[i] = request
		}
		out := preview(batch...)
		require.Len(t, out.Results, 100)
		for _, body := range []string{`{"requests":[]}`, `{"requests":null}`, encode(map[string]any{"requests": append(batch, request)}), encode(map[string]any{"requests": batch, "rate": .001})} {
			response := run(http.MethodPost, "/preview", body)
			require.Equal(t, 400, response.Code, response.Body.String())
		}
	})
	t.Run("catalog and account are read from one snapshot", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `UPDATE groups SET profit_control_enabled=true,procurement_policy=$2::jsonb WHERE id=$1`, groupID, encode(&domain.ProcurementPolicy{Version: 2, Revision: 7, Sources: &domain.ProcurementSources{AccountIDs: []int64{accountID}}}))
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := db.ExecContext(ctx, `UPDATE groups SET profit_control_enabled=false,procurement_policy=null WHERE id=$1`, groupID)
			require.NoError(t, err)
		})
		before, err := repo.ReadProcurementCatalog(ctx)
		require.NoError(t, err)
		lock, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = lock.Rollback() }()
		_, err = lock.ExecContext(ctx, `LOCK TABLE accounts IN ACCESS EXCLUSIVE MODE`)
		require.NoError(t, err)
		type outcome struct {
			out *service.ProcurementClassificationPreview
			err error
		}
		pending := make(chan outcome, 1)
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		go func() {
			out, err := repo.PreviewProcurementClassification(readCtx, &service.ProcurementPreviewCommand{Requests: []service.ProfitPreviewRequestScope{request}, ProductGroupID: &groupID})
			pending <- outcome{out, err}
		}()
		require.Eventually(t, func() bool {
			var count int
			err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%accounts%'`).Scan(&count)
			return err == nil && count > 0
		}, 5*time.Second, 20*time.Millisecond, "preview should wait on the live account read")
		next := before.WithRevision(before.Revision)
		next.Buckets = next.Buckets[1:]
		_, err = repo.SaveProcurementCatalog(ctx, next)
		require.NoError(t, err)
		_, err = lock.ExecContext(ctx, `UPDATE groups SET procurement_policy=$2::jsonb WHERE id=$1`, groupID, encode(&domain.ProcurementPolicy{Version: 2, Revision: 8, Sources: &domain.ProcurementSources{AccountIDs: []int64{}}}))
		require.NoError(t, err)
		_, err = lock.ExecContext(ctx, `UPDATE accounts SET rate_multiplier=0.04 WHERE id=$1`, accountID)
		require.NoError(t, err)
		require.NoError(t, lock.Commit())
		result := <-pending
		require.NoError(t, result.err)
		require.Equal(t, before.Revision, result.out.Catalog.Revision)
		require.Equal(t, "0.025", *result.out.Results[0].EffectiveRate)
		require.Equal(t, "low", result.out.Results[0].Bucket.Code)
		require.EqualValues(t, 7, result.out.Results[0].Product.PolicyRevision)
		require.Equal(t, "constraints_passed", result.out.Results[0].Product.State)
		fresh, err := repo.PreviewProcurementClassification(ctx, &service.ProcurementPreviewCommand{Requests: []service.ProfitPreviewRequestScope{request}, ProductGroupID: &groupID})
		require.NoError(t, err)
		require.EqualValues(t, 8, fresh.Results[0].Product.PolicyRevision)
		require.Equal(t, "blocked", fresh.Results[0].Product.State)
		latest := preview(request)
		require.Greater(t, latest.Catalog.Revision, before.Revision)
		require.Equal(t, "scope_unconfirmed", latest.Results[0].State, "new price must not reuse the old attestation")
	})
	t.Run("corrupt authority and canceled reads fail closed then recover", func(t *testing.T) {
		current, err := repo.ReadProcurementCatalog(ctx)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `UPDATE settings SET value='{"version":99,"revision":88,"buckets":[]}' WHERE key=$1`, service.ProcurementCatalogSettingKey)
		require.NoError(t, err)
		for _, route := range []struct{ method, path, body string }{{"GET", "/catalog", ""}, {"PUT", "/catalog", encode(current)}, {"POST", "/preview", encode(map[string]any{"requests": []service.ProfitPreviewRequestScope{request}})}} {
			out := run(route.method, route.path, route.body)
			require.Equal(t, 503, out.Code, out.Body.String())
			require.NotContains(t, out.Body.String(), "SELECT")
		}
		_, err = db.ExecContext(ctx, `UPDATE settings SET value=$2 WHERE key=$1`, service.ProcurementCatalogSettingKey, encode(current))
		require.NoError(t, err)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = repo.ReadProcurementCatalog(canceled)
		require.Error(t, err)
		out := run("GET", "/catalog", "")
		require.Equal(t, 200, out.Code, out.Body.String())
		empty := &domain.ProcurementCatalog{Version: 1, Revision: current.Revision, Buckets: []domain.ProcurementBucket{}}
		out = run("PUT", "/catalog", encode(empty))
		require.Equal(t, 200, out.Code, out.Body.String())
		require.Contains(t, out.Body.String(), `"buckets":[]`)
		result := preview(request)
		require.Equal(t, "not_configured", result.Results[0].State)
	})
	t.Log(fmt.Sprintf("isolated PostgreSQL catalog/HTTP/100 scopes/CAS/expiry binding/snapshot/failure recovery verified for account %d", accountID))
}

func TestProcurementQualityEvidenceHTTPReadOnlyAndRecovery(t *testing.T) {
	db, plan, sample := repository.ProcurementQualityTestRound(t)
	handler := admin.NewChannelHandler(service.NewChannelService(repository.NewChannelRepository(db), nil, nil, nil, nil), nil, nil)
	router := gin.New()
	router.POST("/preview", handler.ProcurementClassificationPreview)
	command := &service.ProcurementPreviewCommand{Requests: []service.ProfitPreviewRequestScope{{AccountID: plan.AccountID, PublicModel: plan.ModelID, Scope: *sample.PelicanConfig.QualityMeasurement.Scope}}}
	before, err := db.Query(`SELECT count(*) FROM scheduled_test_results WHERE plan_id=$1`, plan.ID)
	require.NoError(t, err)
	require.True(t, before.Next())
	var samples int
	require.NoError(t, before.Scan(&samples))
	require.NoError(t, before.Close())
	preview := func() *service.ProcurementClassificationPreview {
		t.Helper()
		raw, err := json.Marshal(command)
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, "/preview", strings.NewReader(string(raw)))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		var envelope struct {
			Data *service.ProcurementClassificationPreview `json:"data"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
		require.NotNil(t, envelope.Data)
		for _, secret := range []string{"private-route-fixture", "measurement_hash", "response_text", "_quality_", "route.invalid", sample.QualityRouteEvidence.Fingerprint} {
			require.NotContains(t, recorder.Body.String(), secret)
		}
		return envelope.Data
	}
	require.Equal(t, "pass", preview().Results[0].Quality.Status)
	_, err = db.Exec(`UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, plan.ID)
	require.NoError(t, err)
	require.Equal(t, "unknown", preview().Results[0].Quality.Status)
	_, err = db.Exec(`UPDATE scheduled_test_plans SET enabled=true WHERE id=$1`, plan.ID)
	require.NoError(t, err)
	require.Equal(t, "pass", preview().Results[0].Quality.Status)
	var after int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM scheduled_test_results WHERE plan_id=$1`, plan.ID).Scan(&after))
	require.Equal(t, samples, after, "preview must never start a probe or save a generated sample")
}
