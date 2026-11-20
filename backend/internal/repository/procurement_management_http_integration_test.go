//go:build integration

package repository_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestProcurementManagementAdminHTTP(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	var product, source, account int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform) VALUES('Review HTTP product','openai') RETURNING id`).Scan(&product))
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform) VALUES('Review HTTP source','openai') RETURNING id`).Scan(&source))
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,credentials,extra,status,schedulable) VALUES('Review HTTP account','openai','apikey','{"api_key":"private-http-management-key"}','{}','active',true) RETURNING id`).Scan(&account))
	_, err := db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2),($1,$3)`, account, product, source)
	require.NoError(t, err)
	policy := fmt.Sprintf(`{"version":6,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"not_required","availability":"not_required","source_group_ids":[%d]}`, source)
	_, err = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$2::jsonb WHERE id=$1`, product, policy)
	require.NoError(t, err)
	users := repository.NewUserRepository(client, db)
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "management-local-fixture", ExpireHour: 1}}
	auth := service.NewAuthService(client, users, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	tokens := map[string]string{}
	for _, role := range []string{"admin", "user", "observer"} {
		var id int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,role) VALUES($1,'private-management-password-hash',$2) RETURNING id`, "management-"+role+"@test.invalid", role).Scan(&id))
		user, err := users.GetByID(ctx, id)
		require.NoError(t, err)
		tokens[role], err = auth.GenerateToken(ctx, user)
		require.NoError(t, err)
	}
	h := admin.NewChannelHandler(service.NewChannelService(repository.NewChannelRepository(db), nil, nil, nil, nil), nil, nil)
	router := gin.New()
	protected := router.Group("/api/v1/admin")
	protected.Use(gin.HandlerFunc(middleware.NewAdminAuthMiddleware(auth, service.NewUserService(users, nil, nil, nil), nil, nil)))
	protected.Use((&admin.AccountHandler{}).AuthorizeObserver)
	base := "/upstream-procurement/products/:id/management"
	protected.GET(base, h.ProcurementManagement)
	protected.POST(base+"/preview", h.ProcurementManagement)
	protected.PUT(base, h.ProcurementManagement)
	run := func(method, suffix, role, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, fmt.Sprintf("/api/v1/admin/upstream-procurement/products/%d/management%s", product, suffix), strings.NewReader(body))
		if tokens[role] != "" {
			req.Header.Set("Authorization", "Bearer "+tokens[role])
		}
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		for _, private := range []string{"private-http-management-key", "private-management-password-hash", "credentials", "password_hash", "pq:"} {
			require.NotContains(t, w.Body.String(), private)
		}
		return w
	}
	ids := fmt.Sprintf(`{"account_ids":[%d]}`, account)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
		suffix, body := "", ""
		if method == http.MethodPost {
			suffix = "/preview"
			body = ids
		}
		if method == http.MethodPut {
			body = `{"version":1,"mode":"manual","expected_revision":0}`
		}
		require.Equal(t, 401, run(method, suffix, "", body).Code)
		require.Equal(t, 403, run(method, suffix, "user", body).Code)
		require.Equal(t, 403, run(method, suffix, "observer", body).Code)
	}
	w := run(http.MethodGet, "", "admin", "")
	require.Equal(t, 200, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	for _, body := range []string{`{}`, `{"account_ids":[1,1]}`, `{"account_ids":[0]}`, `{"account_ids":[1],"future":1}`, strings.Repeat("x", 65537)} {
		require.Equal(t, 400, run(http.MethodPost, "/preview", "admin", body).Code)
	}
	w = run(http.MethodPost, "/preview", "admin", ids)
	require.Equal(t, 200, w.Code, w.Body.String())
	var preview struct {
		Data service.ProcurementManagementPreview `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &preview))
	require.True(t, preview.Data.CanConfirm)
	command := service.ProcurementManagementCommand{Version: 1, Mode: "review", ExpectedRevision: 0, AccountIDs: []int64{account}, ObservedAt: preview.Data.ObservedAt, Fingerprint: preview.Data.Fingerprint}
	raw, err := json.Marshal(command)
	require.NoError(t, err)
	w = run(http.MethodPut, "", "admin", string(raw))
	require.Equal(t, 200, w.Code, w.Body.String())
	var saved struct {
		Data service.ProcurementManagementState `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &saved))
	require.Equal(t, int64(1), saved.Data.Revision)
	require.Equal(t, 1, saved.Data.CurrentOwned)
	w = run(http.MethodGet, "", "admin", "")
	require.Equal(t, 200, w.Code)
	var read struct {
		Data service.ProcurementManagementState `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &read))
	require.Equal(t, saved.Data, read.Data)
	require.Equal(t, 409, run(http.MethodPut, "", "admin", string(raw)).Code)
	require.Equal(t, 400, run(http.MethodPut, "", "admin", `{"version":1,"mode":"automatic","expected_revision":1}`).Code)
	_, err = db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,'broken') ON CONFLICT(key) DO UPDATE SET value='broken'`, service.ProcurementCatalogSettingKey)
	require.NoError(t, err)
	w = run(http.MethodPost, "/preview", "admin", ids)
	require.Equal(t, 503, w.Code, w.Body.String())
	w = run(http.MethodGet, "", "admin", "")
	require.Equal(t, 200, w.Code)
	w = run(http.MethodPut, "", "admin", `{"version":1,"mode":"manual","expected_revision":1}`)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &saved))
	require.Equal(t, "manual", saved.Data.Mode)
	require.Zero(t, saved.Data.CurrentOwned)
	require.Equal(t, 1, saved.Data.RetainedOwned)
	_, err = db.ExecContext(ctx, `ALTER TABLE groups DROP CONSTRAINT groups_procurement_management_contract`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE groups SET procurement_management='{"version":2,"mode":"automatic","revision":3}' WHERE id=$1`, product)
	require.NoError(t, err)
	require.Equal(t, 503, run(http.MethodGet, "", "admin", "").Code)
	require.Equal(t, 503, run(http.MethodPut, "", "admin", `{"version":1,"mode":"manual","expected_revision":3}`).Code)
}
