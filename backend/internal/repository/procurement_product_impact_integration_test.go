//go:build integration

package repository_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestProcurementProductImpactPostgresAdminHTTP(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	repo := repository.NewChannelRepository(db).(service.ProcurementCatalogRepository)
	var groupID, accountID, userID int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,rate_multiplier,profit_min_margin,profit_safety_buffer)
	 VALUES('Fixed sale product','openai',.2,.1,.05) RETURNING id`).Scan(&groupID))
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,credentials,extra,rate_multiplier,status,schedulable)
	 VALUES('Primary supplier','openai','apikey','{"api_key":"synthetic-impact-private-key","base_url":"https://private-impact.invalid"}',
	 '{"cost_multiplier":0.1}',1,'active',true) RETURNING id`).Scan(&accountID))
	_, err := db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, accountID, groupID)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,role) VALUES('impact-private-user@test.invalid','fixture','user') RETURNING id`).Scan(&userID))
	_, err = db.ExecContext(ctx, `INSERT INTO user_group_rate_multipliers(user_id,group_id,rate_multiplier) VALUES($1,$2,.1)`, userID, groupID)
	require.NoError(t, err)
	users := repository.NewUserRepository(client, db)
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "impact-local-fixture", ExpireHour: 1}}
	auth := service.NewAuthService(client, users, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	tokens := map[string]string{}
	for _, role := range []string{"admin", "user", "observer"} {
		var id int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,role) VALUES($1,'fixture',$2) RETURNING id`, "impact-"+role+"@test.invalid", role).Scan(&id))
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
	protected.GET("/upstream-procurement/products/:id/impact", h.ProcurementProductImpact)
	run := func(id, role string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/upstream-procurement/products/"+id+"/impact", nil)
		if tokens[role] != "" {
			req.Header.Set("Authorization", "Bearer "+tokens[role])
		}
		router.ServeHTTP(w, req)
		return w
	}
	id := fmt.Sprint(groupID)
	require.Equal(t, 401, run(id, "").Code)
	require.Equal(t, 403, run(id, "user").Code)
	require.Equal(t, 403, run(id, "observer").Code)
	for _, bad := range []string{"0", "-1", "invalid", "9007199254740992"} {
		require.Equal(t, 400, run(bad, "admin").Code)
	}
	require.Equal(t, 404, run("9007199254740990", "admin").Code)
	read := func() service.ProcurementProductImpact {
		t.Helper()
		w := run(id, "admin")
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		for _, private := range []string{"synthetic-impact-private-key", "private-impact.invalid", "impact-private-user", "password_hash", "credentials", "user_id"} {
			require.NotContains(t, w.Body.String(), private)
		}
		var envelope struct {
			Data service.ProcurementProductImpact `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		return envelope.Data
	}
	var eventsBefore int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduler_outbox`).Scan(&eventsBefore))
	out := read()
	require.Equal(t, 1, out.Version)
	require.False(t, out.GateEnabled)
	require.True(t, out.Report.AssumedEnabled)
	require.Equal(t, "account_price_only", out.Report.ModelCountsScope)
	require.Equal(t, 1, out.BoundAccounts)
	require.Equal(t, 1, out.ConsideredAccounts)
	require.Equal(t, 1, out.UserOverrideCount)
	require.InDelta(t, .2, out.Report.DefaultD, 1e-12)
	require.InDelta(t, .1, out.Report.MinEffectiveD, 1e-12)
	require.InDelta(t, .17, out.Report.ThresholdDefault, 1e-12)
	require.InDelta(t, .085, out.Report.ThresholdMinD, 1e-12)
	require.Len(t, out.Report.Verdicts, 1)
	require.Equal(t, 1.0, *out.Report.Verdicts[0].EffectiveRate)
	require.Equal(t, .1, out.Report.Verdicts[0].EstimatedCostRate)
	require.Equal(t, service.ProfitPreviewClassRejectedThreshold, out.Report.Verdicts[0].Class)
	var enabled bool
	var eventsAfter int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT profit_control_enabled FROM groups WHERE id=$1`, groupID).Scan(&enabled))
	require.False(t, enabled, "preview never enables a saved gate")
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduler_outbox`).Scan(&eventsAfter))
	require.Equal(t, eventsBefore, eventsAfter, "preview must not publish scheduling mutations")

	t.Run("zero, disabled accounts, empty and unsupported product", func(t *testing.T) {
		t.Cleanup(func() {
			_, err := db.ExecContext(ctx, `UPDATE groups SET platform='openai',status='active' WHERE id=$1`, groupID)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `UPDATE accounts SET rate_multiplier=1,schedulable=true,extra='{"cost_multiplier":0.1}' WHERE id=$1`, accountID)
			require.NoError(t, err)
		})
		_, err := db.ExecContext(ctx, `UPDATE accounts SET rate_multiplier=0,extra='{"cost_multiplier":0}' WHERE id=$1`, accountID)
		require.NoError(t, err)
		out := read()
		require.Equal(t, 0.0, *out.Report.Verdicts[0].EffectiveRate)
		require.Equal(t, 0.0, out.Report.Verdicts[0].EstimatedCostRate)
		require.Equal(t, service.ProfitPreviewClassAdmitted, out.Report.Verdicts[0].Class)
		_, err = db.ExecContext(ctx, `UPDATE accounts SET schedulable=false WHERE id=$1`, accountID)
		require.NoError(t, err)
		out = read()
		require.Equal(t, 1, out.BoundAccounts)
		require.Zero(t, out.ConsideredAccounts)
		require.NotNil(t, out.Report.Verdicts)
		_, err = db.ExecContext(ctx, `UPDATE groups SET platform='composite',status='inactive' WHERE id=$1`, groupID)
		require.NoError(t, err)
		out = read()
		require.False(t, out.Report.EffectiveGate)
		require.Equal(t, "inactive", out.GroupStatus)
	})
	t.Run("one snapshot, final evaluation clock and cancellation recovery", func(t *testing.T) {
		lock, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = lock.Rollback() }()
		_, err = lock.ExecContext(ctx, `LOCK TABLE accounts IN ACCESS EXCLUSIVE MODE`)
		require.NoError(t, err)
		type result struct {
			out *service.ProcurementProductImpact
			err error
		}
		pending := make(chan result, 1)
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		go func() {
			out, err := repo.PreviewProcurementProductImpact(readCtx, groupID)
			pending <- result{out, err}
		}()
		require.Eventually(t, func() bool {
			var count int
			err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%accounts%'`).Scan(&count)
			return err == nil && count > 0
		}, 5*time.Second, 20*time.Millisecond)
		_, err = lock.ExecContext(ctx, `UPDATE groups SET rate_multiplier=.3 WHERE id=$1`, groupID)
		require.NoError(t, err)
		_, err = lock.ExecContext(ctx, `UPDATE accounts SET rate_multiplier=.025 WHERE id=$1`, accountID)
		require.NoError(t, err)
		_, err = lock.ExecContext(ctx, `UPDATE user_group_rate_multipliers SET rate_multiplier=.25 WHERE group_id=$1`, groupID)
		require.NoError(t, err)
		beforeRelease := time.Now()
		require.NoError(t, lock.Commit())
		old := <-pending
		require.NoError(t, old.err)
		require.Len(t, old.out.Report.Verdicts, 1)
		require.GreaterOrEqual(t, old.out.ObservedAt.UnixMilli(), beforeRelease.UnixMilli())
		require.Equal(t, .2, old.out.Report.DefaultD)
		require.Equal(t, .1, old.out.Report.MinEffectiveD)
		require.Equal(t, 1.0, *old.out.Report.Verdicts[0].EffectiveRate)
		fresh := read()
		require.Len(t, fresh.Report.Verdicts, 1)
		require.Equal(t, .3, fresh.Report.DefaultD)
		require.Equal(t, .25, fresh.Report.MinEffectiveD)
		require.Equal(t, .025, *fresh.Report.Verdicts[0].EffectiveRate)
		canceled, stop := context.WithCancel(ctx)
		stop()
		_, err = repo.PreviewProcurementProductImpact(canceled, groupID)
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 200, run(id, "admin").Code)
	})
	t.Run("corrupt catalog is unavailable, not an empty passing result", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,'{"invalid":"private-impact-marker"}')`, service.ProcurementCatalogSettingKey)
		require.NoError(t, err)
		w := run(id, "admin")
		require.Equal(t, 503, w.Code)
		require.NotContains(t, w.Body.String(), "private-impact-marker")
		_, err = db.ExecContext(ctx, `DELETE FROM settings WHERE key=$1`, service.ProcurementCatalogSettingKey)
		require.NoError(t, err)
		require.Equal(t, 200, run(id, "admin").Code)
	})
	t.Run("bounded inventory never silently truncates", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `WITH added AS (INSERT INTO accounts(name,platform,type,credentials,status,schedulable)
		 SELECT 'bounded-impact-'||n,'openai','apikey','{}','active',true FROM generate_series(1,$2::int) n RETURNING id)
		 INSERT INTO account_groups(account_id,group_id) SELECT id,$1 FROM added`, groupID, service.ProcurementProductImpactAccountLimit)
		require.NoError(t, err)
		w := run(id, "admin")
		require.Equal(t, 400, w.Code)
		require.Contains(t, w.Body.String(), "PROCUREMENT_PRODUCT_IMPACT_TOO_LARGE")
	})
}
