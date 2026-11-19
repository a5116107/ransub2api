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

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestProcurementPolicyQualityPostgresHTTP(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	groups := repository.NewAdminGroupRepository(client, db)
	management := service.NewAdminService(nil, nil, groups, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, client, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h := admin.NewGroupHandler(management, nil, nil)
	router := gin.New()
	router.POST("/groups", h.Create)
	router.PUT("/groups/:id", h.Update)
	router.GET("/groups/:id", h.GetByID)
	run := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, request)
		return response
	}
	policy := `{"version":4,"revision":0,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"current_answer"}`
	response := run(http.MethodPost, "/groups", `{"name":"answer-quality-product","platform":"openai","rate_multiplier":0.15,"profit_control_enabled":true,"procurement_policy":`+policy+`}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var envelope struct {
		Data service.Group `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	id := envelope.Data.ID
	path := fmt.Sprintf("/groups/%d", id)
	read := func() *service.Group {
		t.Helper()
		group, err := groups.GetByIDLite(t.Context(), id)
		require.NoError(t, err)
		return group
	}
	require.Equal(t, "current_answer", read().ProcurementPolicy.Quality)
	response = run(http.MethodPut, path, `{"name":"ordinary-answer-edit"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, "current_answer", read().ProcurementPolicy.Quality)
	for _, invalid := range []string{
		`{"version":3,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null}}`,
		`{"version":4,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null}}`,
		`{"version":4,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":null}`,
	} {
		response = run(http.MethodPut, path, `{"name":"must-not-write","procurement_policy":`+invalid+`}`)
		require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		require.Equal(t, "ordinary-answer-edit", read().Name)
		require.Equal(t, "current_answer", read().ProcurementPolicy.Quality)
	}
	response = run(http.MethodPut, path, `{"procurement_policy":`+strings.ReplaceAll(strings.ReplaceAll(policy, `"revision":0`, `"revision":1`), "current_answer", "not_required")+`}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, int64(2), read().ProcurementPolicy.Revision)
	require.Equal(t, "not_required", read().ProcurementPolicy.Quality)
	response = run(http.MethodPut, path, `{"procurement_policy":`+strings.ReplaceAll(policy, `"revision":0`, `"revision":1`)+`}`)
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.Equal(t, "not_required", read().ProcurementPolicy.Quality)
	require.Equal(t, .15, read().RateMultiplier)
}

func TestProcurementPolicyStoredJSONPostgresRejectsMissingBounds(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	max := "0.03"
	policy := &domain.ProcurementPolicy{Version: 1, Revision: 1, MaxRate: &max}
	group, err := client.Group.Create().SetName("strict-policy-json").SetPlatform(service.PlatformOpenAI).SetRateMultiplier(.15).SetProfitControlEnabled(true).SetProcurementPolicy(policy).Save(ctx)
	require.NoError(t, err)
	repo := repository.NewAdminGroupRepository(client, db)
	for name, broken := range map[string]string{
		"missing maximum": `{"version":1,"revision":1,"min_rate":null}`,
		"missing minimum": `{"version":1,"revision":1,"max_rate":"0.03"}`,
		"null revision":   `{"version":1,"revision":null,"min_rate":null,"max_rate":"0.03"}`,
		"wrong case":      `{"Version":1,"revision":1,"min_rate":null,"max_rate":"0.03"}`,
		"unknown field":   `{"version":1,"revision":1,"min_rate":null,"max_rate":"0.03","owner":"unknown"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$2::jsonb WHERE id=$1`, group.ID, broken)
			require.NoError(t, err)
			loaded, err := repo.GetByIDLite(ctx, group.ID)
			require.Error(t, err, "SQL JSONB reads must not turn damaged constraints into unbounded admission")
			require.Nil(t, loaded)
			saved, err := json.Marshal(policy)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$2::jsonb WHERE id=$1`, group.ID, string(saved))
			require.NoError(t, err)
			loaded, err = repo.GetByIDLite(ctx, group.ID)
			require.NoError(t, err)
			require.Equal(t, "0.03", *loaded.ProcurementPolicy.MaxRate)
			require.Equal(t, .15, loaded.RateMultiplier)
			require.EqualValues(t, 1, loaded.ProcurementPolicy.Revision)
		})
	}
}

func TestProcurementPolicySourcesPostgresHTTP(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	groups := repository.NewAdminGroupRepository(client, db)
	management := service.NewAdminService(nil, nil, groups, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, client, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h := admin.NewGroupHandler(management, nil, nil)
	router := gin.New()
	router.POST("/groups", h.Create)
	router.PUT("/groups/:id", h.Update)
	router.GET("/groups/:id", h.GetByID)
	run := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, req)
		return recorder
	}
	response := run(http.MethodPost, "/groups", `{"name":"approved-source-product","platform":"openai","rate_multiplier":0.15,"profit_control_enabled":true,"procurement_policy":{"version":2,"revision":0,"min_rate":null,"max_rate":null,"sources":{"account_ids":[]}}}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var envelope struct {
		Data service.Group `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	id := envelope.Data.ID
	require.Positive(t, id)
	path := fmt.Sprintf("/groups/%d", id)
	read := func() *service.Group {
		t.Helper()
		loaded, err := repository.NewAdminGroupRepository(client, db).GetByIDLite(ctx, id)
		require.NoError(t, err)
		return loaded
	}
	loaded := read()
	require.NotNil(t, loaded.ProcurementPolicy.Sources.AccountIDs)
	require.Empty(t, loaded.ProcurementPolicy.Sources.AccountIDs)
	response = run(http.MethodPut, path, `{"name":"ordinary-legacy-edit"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, loaded.ProcurementPolicy, read().ProcurementPolicy)
	for _, invalid := range []string{
		`{"version":1,"revision":1,"min_rate":null,"max_rate":null}`,
		`{"version":2,"revision":1,"min_rate":null,"max_rate":null,"sources":{}}`,
		`{"version":2,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null,"account_ids":null}}`,
		`{"version":2,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":[]},"sources":{"account_ids":[]}}`,
	} {
		response = run(http.MethodPut, path, `{"name":"must-not-write","procurement_policy":`+invalid+`}`)
		require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		require.Equal(t, "ordinary-legacy-edit", read().Name)
		require.Equal(t, loaded.ProcurementPolicy, read().ProcurementPolicy)
	}
	response = run(http.MethodPut, path, `{"procurement_policy":{"version":2,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":[902,901]}}}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, []int64{901, 902}, read().ProcurementPolicy.Sources.AccountIDs)
	response = run(http.MethodPut, path, `{"name":"must-not-write","procurement_policy":{"version":1,"revision":1,"min_rate":null,"max_rate":null}}`)
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "GROUP_PROCUREMENT_POLICY_CONFLICT")
	response = run(http.MethodPut, path, `{"name":"must-not-write","procurement_policy":{"version":1,"revision":2,"min_rate":null,"max_rate":null}}`)
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	require.Equal(t, "ordinary-legacy-edit", read().Name)
	legacy, first, second := read(), read(), read()
	start, outcomes := make(chan struct{}), make(chan error, 2)
	var workers sync.WaitGroup
	for i, input := range []*service.Group{first, second} {
		expected := int64(2)
		input.ProcurementPolicyExpectedRevision = &expected
		input.ProcurementPolicy = input.ProcurementPolicy.WithRevision(3)
		input.ProcurementPolicy.Sources.AccountIDs = []int64{int64(903 + i)}
		input.Name = fmt.Sprintf("approved-%d", i)
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			outcomes <- repository.NewAdminGroupRepository(client, db).Update(ctx, input)
		}()
	}
	close(start)
	workers.Wait()
	close(outcomes)
	successes := 0
	for err := range outcomes {
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, service.ErrProcurementPolicyConflict)
		}
	}
	require.Equal(t, 1, successes)
	winner := read().ProcurementPolicy
	require.EqualValues(t, 3, winner.Revision)
	legacy.Name = "legacy-after-approved-change"
	require.NoError(t, groups.Update(ctx, legacy))
	require.Equal(t, winner, legacy.ProcurementPolicy)
	duplicate, err := management.DuplicateGroup(ctx, id, "fixture-admin", "source-copy")
	require.NoError(t, err)
	require.Equal(t, winner.Sources, duplicate.ProcurementPolicy.Sources)
	require.EqualValues(t, 1, duplicate.ProcurementPolicy.Revision)
	duplicate.ProcurementPolicy.Sources.AccountIDs[0] = 1
	require.Equal(t, winner, read().ProcurementPolicy)
	response = run(http.MethodPut, path, `{"procurement_policy":{"version":2,"revision":2,"min_rate":null,"max_rate":null,"sources":{"account_ids":null}}}`)
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	response = run(http.MethodPut, path, `{"procurement_policy":{"version":2,"revision":3,"min_rate":null,"max_rate":null,"sources":{"account_ids":null}}}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	loaded = read()
	require.Nil(t, loaded.ProcurementPolicy.Sources.AccountIDs)
	require.Equal(t, 0.15, loaded.RateMultiplier)
	userDTO, err := json.Marshal(dto.GroupFromService(loaded))
	require.NoError(t, err)
	require.NotContains(t, string(userDTO), "procurement_policy")
	require.NotContains(t, string(userDTO), "account_ids")
	// Explicitly empty remains empty after another independent SQL read.
	expected := int64(4)
	loaded.ProcurementPolicyExpectedRevision = &expected
	loaded.ProcurementPolicy = &service.ProcurementPolicy{Version: 2, Revision: 5, Sources: &domain.ProcurementSources{AccountIDs: []int64{}}}
	require.NoError(t, groups.Update(ctx, loaded))
	require.NotNil(t, read().ProcurementPolicy.Sources.AccountIDs)
	// A malformed persisted object must fail the real Ent projection, rather
	// than silently interpreting a missing account_ids as an unrestricted pool.
	backup, err := json.Marshal(loaded.ProcurementPolicy)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=jsonb_set(procurement_policy,'{sources}','{}'::jsonb) WHERE id=$1`, id)
	require.NoError(t, err)
	_, err = groups.GetByIDLite(ctx, id)
	require.Error(t, err)
	_, err = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$2::jsonb WHERE id=$1`, id, string(backup))
	require.NoError(t, err)
	require.NotNil(t, read().ProcurementPolicy.Sources.AccountIDs)
	for _, table := range []string{"usage_logs", "monitoring_probe_budget_counters", "monitoring_probe_budget_leases", "account_groups"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count)
	}
}

func TestProcurementPolicyPostgresHTTPRevisionAndLegacyClient(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	groups := repository.NewAdminGroupRepository(client, db)
	management := service.NewAdminService(nil, nil, groups, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, client, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h := admin.NewGroupHandler(management, nil, nil)
	router := gin.New()
	router.POST("/groups", h.Create)
	router.PUT("/groups/:id", h.Update)
	router.GET("/groups/:id", h.GetByID)
	run := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, req)
		return recorder
	}
	created := run(http.MethodPost, "/groups", `{"name":"procurement-http","platform":"openai","rate_multiplier":0.15,"profit_control_enabled":true,"procurement_policy":{"version":1,"revision":0,"min_rate":"0.02","max_rate":"0.03"}}`)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	var envelope struct {
		Data struct {
			ID     int64                      `json:"id"`
			Policy *service.ProcurementPolicy `json:"procurement_policy"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &envelope))
	id := envelope.Data.ID
	require.Positive(t, id)
	require.EqualValues(t, 1, envelope.Data.Policy.Revision)
	path := fmt.Sprintf("/groups/%d", id)
	response := run(http.MethodPut, path, `{"name":"renamed","rate_multiplier":0.15}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	loaded, err := groups.GetByIDLite(ctx, id)
	require.NoError(t, err)
	require.EqualValues(t, 1, loaded.ProcurementPolicy.Revision)
	require.Equal(t, "0.03", *loaded.ProcurementPolicy.MaxRate)
	userDTO, err := json.Marshal(dto.GroupFromService(loaded))
	require.NoError(t, err)
	require.NotContains(t, string(userDTO), "procurement_policy", "user-facing sales groups must not expose procurement data")
	response = run(http.MethodPut, path, `{"name":"must-not-write","procurement_policy":{"version":1,"revision":0,"min_rate":"0.01","max_rate":"0.04"}}`)
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "GROUP_PROCUREMENT_POLICY_CONFLICT")
	for _, invalid := range []string{`null`, `{"version":1,"revision":1,"min_rate":"0.03","max_rate":"0.02"}`, `{"version":2,"revision":1,"min_rate":null,"max_rate":null}`} {
		response = run(http.MethodPut, path, `{"name":"must-not-write","procurement_policy":`+invalid+`}`)
		require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	}
	loaded, err = groups.GetByIDLite(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "renamed", loaded.Name)
	require.Equal(t, 0.15, loaded.RateMultiplier)
	response = run(http.MethodPut, path, `{"procurement_policy":{"version":1,"revision":1,"min_rate":"0.03","max_rate":"0.05"}}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	response = run(http.MethodGet, path, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	require.EqualValues(t, 2, envelope.Data.Policy.Revision)
	require.Equal(t, "0.03", *envelope.Data.Policy.MinRate)

	// Two repositories read revision 2. Exactly one policy/name edit can commit;
	// neither a rejected CAS nor an old client's unrelated edit can erase it.
	first, err := groups.GetByIDLite(ctx, id)
	require.NoError(t, err)
	second, err := groups.GetByIDLite(ctx, id)
	require.NoError(t, err)
	legacy, err := groups.GetByIDLite(ctx, id)
	require.NoError(t, err)
	start, outcomes := make(chan struct{}), make(chan error, 2)
	var workers sync.WaitGroup
	for index, input := range []*service.Group{first, second} {
		input.Name = fmt.Sprintf("concurrent-%d", index)
		expected := int64(2)
		input.ProcurementPolicyExpectedRevision = &expected
		input.ProcurementPolicy = input.ProcurementPolicy.WithRevision(3)
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			outcomes <- repository.NewAdminGroupRepository(client, db).Update(ctx, input)
		}()
	}
	close(start)
	workers.Wait()
	close(outcomes)
	successes := 0
	for err := range outcomes {
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, service.ErrProcurementPolicyConflict)
		}
	}
	require.Equal(t, 1, successes)
	legacy.Name = "legacy-after-concurrent"
	require.NoError(t, groups.Update(ctx, legacy))
	require.EqualValues(t, 3, legacy.ProcurementPolicy.Revision, "ordinary update response reloads the winning policy")
	response = run(http.MethodPut, path, `{"procurement_policy":{"version":1,"revision":3,"min_rate":null,"max_rate":null}}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	duplicate, err := management.DuplicateGroup(ctx, id, "fixture-admin", "procurement-copy")
	require.NoError(t, err)
	require.EqualValues(t, 1, duplicate.ProcurementPolicy.Revision)
	require.Nil(t, duplicate.ProcurementPolicy.MinRate)
	require.Nil(t, duplicate.ProcurementPolicy.MaxRate)
	loaded, err = groups.GetByIDLite(ctx, id)
	require.NoError(t, err)
	require.EqualValues(t, 4, loaded.ProcurementPolicy.Revision)
	require.Equal(t, 0.15, loaded.RateMultiplier)
	for _, table := range []string{"usage_logs", "monitoring_probe_budget_counters", "monitoring_probe_budget_leases"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count)
	}
}

func TestProcurementPolicyMigrationLegacyPreservation(t *testing.T) {
	db, _ := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	// The exact embedded migration resolves to this transaction-owned temporary
	// table, never the application's groups table. Rollback removes the fixture.
	_, err = tx.ExecContext(ctx, `CREATE TEMP TABLE groups (id BIGINT PRIMARY KEY, name TEXT NOT NULL, rate_multiplier NUMERIC NOT NULL, status TEXT NOT NULL);
		INSERT INTO groups VALUES (1, 'legacy', 0.15, 'active'), (2, 'paused', 0.18, 'inactive')`)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("348_group_procurement_policy.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = tx.ExecContext(ctx, string(migration))
		require.NoError(t, err)
	}
	var legacy, paused int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM groups WHERE procurement_policy IS NULL`).Scan(&legacy))
	require.Equal(t, 2, legacy, "old groups are not silently enrolled")
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM groups WHERE id=2 AND status='inactive' AND rate_multiplier=0.18`).Scan(&paused))
	require.Equal(t, 1, paused)
	_, err = tx.ExecContext(ctx, `UPDATE groups SET procurement_policy='{"version":1,"revision":1,"min_rate":"0.02","max_rate":"0.03"}'::jsonb WHERE id=1`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	var revision int
	var salesRate float64
	var dataType string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT (procurement_policy->>'revision')::int, rate_multiplier, pg_typeof(procurement_policy)::text FROM groups WHERE id=1`).Scan(&revision, &salesRate, &dataType))
	require.Equal(t, 1, revision)
	require.Equal(t, 0.15, salesRate, "old-binary sales queries still read the original price; they do not enforce procurement")
	require.Equal(t, "jsonb", dataType)
	require.NoError(t, tx.Rollback())
}

func TestProcurementPolicyBucketsPostgresHTTPAndAtomicCatalog(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	groups := repository.NewAdminGroupRepository(client, db)
	management := service.NewAdminService(nil, nil, groups, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, client, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	handler := admin.NewGroupHandler(management, nil, nil)
	router := gin.New()
	router.POST("/groups", handler.Create)
	router.PUT("/groups/:id", handler.Update)
	router.GET("/groups/:id", handler.GetByID)
	run := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		out := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(out, req)
		return out
	}
	command := `{"version":3,"revision":0,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":["low"]}}`
	response := run("POST", "/groups", `{"name":"bucket-product","platform":"openai","rate_multiplier":0.15,"profit_control_enabled":true,"procurement_policy":`+command+`}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	var envelope struct {
		Data service.Group `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	id := envelope.Data.ID
	require.Positive(t, id)
	path := fmt.Sprintf("/groups/%d", id)
	read := func() *service.Group {
		t.Helper()
		g, err := groups.GetByIDLite(ctx, id)
		require.NoError(t, err)
		return g
	}
	require.Equal(t, []string{"low"}, read().ProcurementPolicy.Buckets.Codes)
	require.Empty(t, read().ProcurementCatalog.Buckets, "missing settings is an empty directory, never an invented allowed bucket")
	catalogs := repository.NewChannelRepository(db).(service.ProcurementCatalogRepository)
	catalog, err := service.ParseProcurementCatalogCommand([]byte(`{"version":1,"revision":0,"buckets":[{"code":"low","name":"Low","min_rate":"0.02","max_rate":"0.03"},{"code":"next","name":"Next","min_rate":"0.03","max_rate":"0.05"}]}`))
	require.NoError(t, err)
	catalog, err = catalogs.SaveProcurementCatalog(ctx, catalog)
	require.NoError(t, err)
	require.EqualValues(t, 1, read().ProcurementCatalog.Revision)
	require.NotContains(t, run("GET", path, "").Body.String(), "procurement_catalog", "the projection must not leak through admin group DTOs")
	for _, bad := range []string{`{"version":3,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{}}`, strings.Replace(command, `"revision":0`, `"revision":null`, 1), strings.Replace(command, `"codes":["low"]`, `"codes":["low","low"]`, 1)} {
		response = run("PUT", path, `{"name":"must-not-write","procurement_policy":`+bad+`}`)
		require.Equal(t, 400, response.Code, response.Body.String())
		require.Equal(t, "bucket-product", read().Name)
	}
	for _, version := range []int{1, 2} {
		legacy := fmt.Sprintf(`{"version":%d,"revision":0,"min_rate":null,"max_rate":null`, version)
		if version == 2 {
			legacy += `,"sources":{"account_ids":null}`
		}
		legacy += `}`
		response = run("PUT", path, `{"procurement_policy":`+legacy+`}`)
		require.Equal(t, 409, response.Code, response.Body.String())
		response = run("PUT", path, `{"procurement_policy":`+strings.Replace(legacy, `"revision":0`, `"revision":1`, 1)+`}`)
		require.Equal(t, 400, response.Code, response.Body.String())
	}
	copy, err := management.DuplicateGroup(ctx, id, "fixture-admin", "bucket-copy")
	require.NoError(t, err)
	require.EqualValues(t, 1, copy.ProcurementPolicy.Revision)
	require.Equal(t, []string{"low"}, copy.ProcurementPolicy.Buckets.Codes)
	copy.ProcurementPolicy.Buckets.Codes[0] = "next"
	require.Equal(t, []string{"low"}, read().ProcurementPolicy.Buckets.Codes)
	t.Run("group and catalog share a repeatable read snapshot", func(t *testing.T) {
		lock, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = lock.Rollback() }()
		_, err = lock.ExecContext(ctx, `LOCK TABLE settings IN ACCESS EXCLUSIVE MODE`)
		require.NoError(t, err)
		type result struct {
			group *service.Group
			err   error
		}
		pending := make(chan result, 1)
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		go func() { g, err := groups.GetByIDLite(readCtx, id); pending <- result{g, err} }()
		require.Eventually(t, func() bool {
			var count int
			err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT value FROM settings%'`).Scan(&count)
			return err == nil && count > 0
		}, 5*time.Second, 20*time.Millisecond)
		next := catalog.WithRevision(2)
		next.Buckets = next.Buckets[1:]
		raw, err := json.Marshal(next)
		require.NoError(t, err)
		_, err = lock.ExecContext(ctx, `UPDATE settings SET value=$2 WHERE key=$1`, service.ProcurementCatalogSettingKey, string(raw))
		require.NoError(t, err)
		_, err = lock.ExecContext(ctx, `UPDATE groups SET procurement_policy=jsonb_set(jsonb_set(procurement_policy,'{revision}','2'::jsonb),'{buckets,codes}','["next"]'::jsonb) WHERE id=$1`, id)
		require.NoError(t, err)
		require.NoError(t, lock.Commit())
		previous := <-pending
		require.NoError(t, previous.err)
		require.EqualValues(t, 1, previous.group.ProcurementPolicy.Revision)
		require.EqualValues(t, 1, previous.group.ProcurementCatalog.Revision)
		require.Equal(t, "low", previous.group.ProcurementCatalog.Buckets[0].Code)
		fresh := read()
		require.EqualValues(t, 2, fresh.ProcurementPolicy.Revision)
		require.EqualValues(t, 2, fresh.ProcurementCatalog.Revision)
		require.Equal(t, []string{"next"}, fresh.ProcurementPolicy.Buckets.Codes)
		catalog = next
	})

	t.Run("regional publication uses the transaction catalog without a primary reread", func(t *testing.T) {
		authority := repository.NewRegionalSchedulerAuthority(db)
		region := "procurement-fixture"
		_, err := authority.Restore(ctx, region, true)
		require.NoError(t, err)
		original := read().ProcurementPolicy
		saved, err := json.Marshal(original)
		require.NoError(t, err)
		raw, err := json.Marshal(catalog)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$2::jsonb WHERE id=$1`, id, string(saved))
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `UPDATE settings SET value=$2 WHERE key=$1`, service.ProcurementCatalogSettingKey, string(raw))
			require.NoError(t, err)
		})
		_, err = authority.Publish(ctx, region, func(txCtx context.Context, revision uint64, _ service.AccountRepository, txGroups service.GroupRepository) ([]byte, error) {
			before, err := txGroups.GetByIDLite(txCtx, id)
			require.NoError(t, err)
			require.EqualValues(t, 2, before.ProcurementPolicy.Revision)
			require.EqualValues(t, 2, before.ProcurementCatalog.Revision)
			nextCatalog := catalog.WithRevision(3)
			nextRaw, err := json.Marshal(nextCatalog)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `UPDATE settings SET value=$2 WHERE key=$1`, service.ProcurementCatalogSettingKey, string(nextRaw))
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=jsonb_set(procurement_policy,'{revision}','3'::jsonb) WHERE id=$1`, id)
			require.NoError(t, err)
			after, err := txGroups.GetByIDLite(txCtx, id)
			require.NoError(t, err)
			require.EqualValues(t, 2, after.ProcurementPolicy.Revision)
			require.EqualValues(t, 2, after.ProcurementCatalog.Revision)
			return []byte("consistent-v3-product"), nil
		})
		require.NoError(t, err)
		_, err = authority.Publish(ctx, region, func(txCtx context.Context, revision uint64, _ service.AccountRepository, txGroups service.GroupRepository) ([]byte, error) {
			g, err := txGroups.GetByIDLite(txCtx, id)
			require.NoError(t, err)
			require.EqualValues(t, 3, g.ProcurementPolicy.Revision)
			require.EqualValues(t, 3, g.ProcurementCatalog.Revision)
			return []byte("new-v3-product"), nil
		})
		require.NoError(t, err)
	})
	response = run("PUT", path, `{"name":"ordinary-update"}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.EqualValues(t, 2, read().ProcurementPolicy.Revision)
	_, err = db.ExecContext(ctx, `UPDATE settings SET value='{"version":99,"revision":77,"buckets":[]}' WHERE key=$1`, service.ProcurementCatalogSettingKey)
	require.NoError(t, err)
	require.Nil(t, read().ProcurementCatalog, "bad catalog remains unknown without erasing the product policy")
	raw, err := json.Marshal(catalog)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE settings SET value=$2 WHERE key=$1`, service.ProcurementCatalogSettingKey, string(raw))
	require.NoError(t, err)
	require.EqualValues(t, 2, read().ProcurementCatalog.Revision)
	require.Equal(t, .15, read().RateMultiplier)
	response = run("PUT", path, `{"procurement_policy":{"version":3,"revision":2,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":[]}}}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.NotNil(t, read().ProcurementPolicy.Buckets.Codes)
	require.Empty(t, read().ProcurementPolicy.Buckets.Codes)
	response = run("PUT", path, `{"procurement_policy":{"version":3,"revision":3,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null}}}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.Nil(t, read().ProcurementPolicy.Buckets.Codes)
}
