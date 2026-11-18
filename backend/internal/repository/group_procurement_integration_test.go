//go:build integration

package repository_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

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
