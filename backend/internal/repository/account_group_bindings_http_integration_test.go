//go:build integration

package repository_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountGroupBindingsPostgresAdminHTTP(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	users := repository.NewUserRepository(client, db)
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "membership-local-fixture", ExpireHour: 1}}
	var adminID, userID int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,role) VALUES('membership-admin@test.invalid','fixture','admin') RETURNING id`).Scan(&adminID))
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,role) VALUES('membership-user@test.invalid','fixture','user') RETURNING id`).Scan(&userID))
	auth := service.NewAuthService(client, users, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	tokenFor := func(id int64) string {
		t.Helper()
		user, err := users.GetByID(ctx, id)
		require.NoError(t, err)
		token, err := auth.GenerateToken(ctx, user)
		require.NoError(t, err)
		return token
	}
	adminToken, userToken := tokenFor(adminID), tokenFor(userID)
	groups := repository.NewAdminGroupRepository(client, db)
	accounts := repository.NewAdminAccountRepository(client, db, nil)
	settings := service.NewSettingService(repository.NewSettingRepository(client), cfg)
	management := service.NewAdminService(cfg, users, groups, accounts, repository.NewProxyRepository(client, db), nil, nil, nil, nil, nil, nil, nil, nil, client, settings, nil, nil, nil, nil, nil, nil, nil, nil)
	first := &service.Group{Name: "membership-product", Platform: service.PlatformOpenAI, Status: service.StatusActive, RateMultiplier: .15}
	second := &service.Group{Name: "membership-source", Platform: service.PlatformOpenAI, Status: service.StatusActive, RateMultiplier: 1}
	require.NoError(t, groups.Create(ctx, first))
	require.NoError(t, groups.Create(ctx, second))
	account, err := management.CreateAccount(ctx, &service.CreateAccountInput{Name: "membership-source-key", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "synthetic-membership-fixture", "base_url": "http://127.0.0.1:1"}, Concurrency: 7,
		GroupIDs: []int64{first.ID, second.ID}, SkipMixedChannelCheck: true})
	require.NoError(t, err)
	require.NoError(t, accounts.SetGroupAllowedModels(ctx, account.ID, map[int64][]string{first.ID: {"gpt-6-sol"}, second.ID: {"gpt-6-*"}}))
	stamp := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	const owner = `{"version":1,"kind":"procurement","revision":7}`
	_, err = db.ExecContext(ctx, "UPDATE account_groups SET created_at=$2,ownership=$3::jsonb WHERE account_id=$1", account.ID, stamp, owner)
	require.NoError(t, err)
	h := admin.NewAccountHandler(management, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	protected := router.Group("/api/v1/admin")
	protected.Use(gin.HandlerFunc(middleware.NewAdminAuthMiddleware(auth, service.NewUserService(users, nil, nil, nil), nil, nil)))
	protected.Use(h.AuthorizeObserver)
	protected.PUT("/accounts/:id", h.Update)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	httpClient := &http.Client{Timeout: 5 * time.Second}
	update := func(body, token string) int {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("%s/api/v1/admin/accounts/%d", server.URL, account.ID), strings.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Admin-UI", "1")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := httpClient.Do(request)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode
	}
	body := fmt.Sprintf(`{"group_ids":[%d,%d,%d]}`, second.ID, first.ID, second.ID)
	require.Equal(t, http.StatusUnauthorized, update(body, ""))
	require.Equal(t, http.StatusForbidden, update(body, userToken))
	require.Equal(t, http.StatusOK, update(body, adminToken))
	var created time.Time
	var priority int
	var models, ownership string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT created_at,priority,allowed_models::text,ownership::text FROM account_groups WHERE account_id=$1 AND group_id=$2", account.ID, second.ID).Scan(&created, &priority, &models, &ownership))
	require.True(t, stamp.Equal(created))
	require.Equal(t, 1, priority)
	require.JSONEq(t, `["gpt-6-*"]`, models)
	require.JSONEq(t, owner, ownership, "ordinary authenticated edits must preserve the retained relationship owner")
	var originalExtra string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT extra::text FROM accounts WHERE id=$1", account.ID).Scan(&originalExtra))
	require.Equal(t, http.StatusOK, update(fmt.Sprintf(`{"group_ids":[%d]}`, second.ID), adminToken))
	require.Equal(t, http.StatusOK, update(fmt.Sprintf(`{"group_ids":[%d,%d]}`, second.ID, first.ID), adminToken))
	loaded, err := accounts.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{first.ID, second.ID}, loaded.GroupIDs)
	require.Equal(t, []string{"gpt-6-*"}, loaded.GroupAllowedModels(second.ID))
	require.Empty(t, loaded.GroupAllowedModels(first.ID))
	require.True(t, loaded.Schedulable)
	require.Equal(t, 7, loaded.Concurrency)
	raw, err := json.Marshal(loaded.Extra)
	require.NoError(t, err)
	require.JSONEq(t, originalExtra, string(raw), "HTTP removal/re-add creates no persistent exclusion")
	var readded []byte
	require.NoError(t, db.QueryRowContext(ctx, "SELECT ownership FROM account_groups WHERE account_id=$1 AND group_id=$2", account.ID, first.ID).Scan(&readded))
	require.Empty(t, readded, "manual re-add must not resurrect the former owner")
	t.Log("PASS: authenticated real HTTP -> original admin service -> PostgreSQL diff binding; auth denied, identity and owner retained, duplicate IDs normalized, manual remove/re-add without exclusion or former ownership")
}
