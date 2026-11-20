//go:build integration

package repository_test

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestMonitoringProbePlanDraftHTTPPostgres(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	var account int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,credentials,extra) VALUES('Draft budget account','openai','apikey','{"api_key":"private-draft-key"}','{}') RETURNING id`).Scan(&account))
	budget := repository.NewMonitoringProbeBudgetRepository(db)
	policy := service.DefaultMonitoringProbeBudgetSettings()
	policy.Enabled, policy.RequestsPerDay, policy.RecoveryReservePercent = true, 100, 0
	policy, err := budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	probe := service.NewUpstreamBillingProbeService(nil, nil, nil)
	probe.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(budget))
	h := &admin.AccountHandler{}
	h.SetUpstreamBillingProbeService(probe)
	users := repository.NewUserRepository(client, db)
	auth := service.NewAuthService(client, users, nil, nil, &config.Config{JWT: config.JWTConfig{Secret: "draft-budget-fixture", ExpireHour: 1}}, nil, nil, nil, nil, nil, nil, nil, nil)
	tokens := map[string]string{}
	for _, role := range []string{"admin", "user", "observer"} {
		var id int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,role) VALUES($1,'fixture',$2) RETURNING id`, "draft-budget-"+role+"@test.invalid", role).Scan(&id))
		user, err := users.GetByID(ctx, id)
		require.NoError(t, err)
		tokens[role], err = auth.GenerateToken(ctx, user)
		require.NoError(t, err)
	}
	router := gin.New()
	protected := router.Group("/api/v1/admin")
	protected.Use(gin.HandlerFunc(middleware.NewAdminAuthMiddleware(auth, service.NewUserService(users, nil, nil, nil), nil, nil)))
	protected.Use(h.AuthorizeObserver)
	protected.POST("/accounts/upstream-billing-probe/budget/preview", h.PreviewMonitoringProbeCoverage)
	run := func(role, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/upstream-billing-probe/budget/preview", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if tokens[role] != "" {
			req.Header.Set("Authorization", "Bearer "+tokens[role])
		}
		router.ServeHTTP(w, req)
		for _, secret := range []string{"private-draft-key", `"password_hash":`, `"credentials":`, "pq:"} {
			require.NotContains(t, w.Body.String(), secret)
		}
		return w
	}
	raw, err := json.Marshal(policy)
	require.NoError(t, err)
	var command map[string]any
	require.NoError(t, json.Unmarshal(raw, &command))
	draft := map[string]any{"plan_id": 0, "account_id": account, "model_id": "model-a", "cron_expression": "0 * * * *", "enabled": true, "auto_recover": false, "max_results": 50}
	command["plan_draft"] = draft
	raw, err = json.Marshal(command)
	require.NoError(t, err)
	require.Equal(t, 401, run("", string(raw)).Code)
	require.Equal(t, 403, run("user", string(raw)).Code)
	require.Equal(t, 403, run("observer", string(raw)).Code)
	w := run("admin", string(raw))
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	var envelope struct {
		Data struct {
			Minimum int64           `json:"minimum_total_requests_per_day"`
			Change  json.RawMessage `json:"plan_change"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.NotEmpty(t, envelope.Data.Change, "a draft must be evaluated, not silently ignored as a settings-only preview")
	require.EqualValues(t, 24, envelope.Data.Minimum)
	var plans int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_plans`).Scan(&plans))
	require.Zero(t, plans, "preview must never create the proposed plan")
	read := func() *service.MonitoringProbeCoverage {
		t.Helper()
		body, err := json.Marshal(command)
		require.NoError(t, err)
		w := run("admin", string(body))
		require.Equal(t, 200, w.Code, w.Body.String())
		var out struct {
			Data service.MonitoringProbeCoverage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		return &out.Data
	}
	native := service.NewScheduledTestService(repository.NewScheduledTestPlanRepository(db), nil)
	saved, err := native.CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: account, ModelID: "model-a", CronExpression: "0 * * * *", Enabled: true, MaxResults: 50})
	require.NoError(t, err)
	draft["plan_id"] = saved.ID
	out := read()
	require.EqualValues(t, 24, out.PlanChange.CurrentMinimumRequestsPerDay)
	require.Zero(t, out.PlanChange.DeltaMinimumRequestsPerDay, "replacement must not double-count the saved plan")
	_, offset := out.ObservedAt.Zone()
	require.Zero(t, offset)
	draft["cron_expression"] = "0 */2 * * *"
	out = read()
	require.EqualValues(t, -12, out.PlanChange.DeltaMinimumRequestsPerDay)
	require.EqualValues(t, 12, out.MinimumTotalRequestsPerDay)
	draft["enabled"] = false
	out = read()
	require.EqualValues(t, -24, out.PlanChange.DeltaMinimumRequestsPerDay)
	require.Zero(t, out.MinimumTotalRequestsPerDay)
	draft["enabled"], draft["cron_expression"] = true, "* * * * *"
	out = read()
	require.False(t, out.CanSave)
	require.Contains(t, out.BlockingReasons, "day_capacity")
	require.EqualValues(t, 1416, out.PlanChange.DeltaMinimumRequestsPerDay)
	saved.CronExpression = "* * * * *"
	_, err = native.UpdatePlan(ctx, saved)
	require.ErrorContains(t, err, "day_capacity", "actual save must independently reject the same over-budget proposal")
	reloaded, err := native.GetPlan(ctx, saved.ID)
	require.NoError(t, err)
	require.Equal(t, "0 * * * *", reloaded.CronExpression, "failed save must roll back")
	// Recompute after another plan is saved; a preview is never a reservation.
	_, err = native.CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: account, ModelID: "other", CronExpression: "0 */3 * * *", Enabled: true, MaxResults: 50})
	require.NoError(t, err)
	draft["cron_expression"] = "0 * * * *"
	out = read()
	require.EqualValues(t, 32, out.PlanChange.CurrentMinimumRequestsPerDay)
	require.Zero(t, out.PlanChange.DeltaMinimumRequestsPerDay)
	// Enabling a saved paused plan adds demand, rather than losing its identity.
	saved.Enabled = false
	_, err = native.UpdatePlan(ctx, saved)
	require.NoError(t, err)
	out = read()
	require.EqualValues(t, 8, out.PlanChange.CurrentMinimumRequestsPerDay)
	require.EqualValues(t, 24, out.PlanChange.DeltaMinimumRequestsPerDay)
	for name, id := range map[string]int64{"missing": 9007199254740990, "another account": account + 1000} {
		t.Run(name, func(t *testing.T) {
			original := draft["account_id"]
			draft["account_id"] = id
			defer func() { draft["account_id"] = original }()
			body, err := json.Marshal(command)
			require.NoError(t, err)
			require.Equal(t, 404, run("admin", string(body)).Code)
		})
	}
	var otherAccount int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type) VALUES('Other draft account','openai','apikey') RETURNING id`).Scan(&otherAccount))
	draft["account_id"] = otherAccount
	body, err := json.Marshal(command)
	require.NoError(t, err)
	require.Equal(t, 400, run("admin", string(body)).Code, "plan identity is scoped to its saved account")
	draft["account_id"] = account
	command["revision"] = policy.Revision - 1
	body, err = json.Marshal(command)
	require.NoError(t, err)
	require.Equal(t, 409, run("admin", string(body)).Code)
	command["revision"] = policy.Revision
	for _, body := range []string{`{"plan_draft":null}`, `{"plan_draft":{}}`, strings.Repeat("x", 65537)} {
		require.Equal(t, 400, run("admin", body).Code)
	}
	// Settings-only clients remain valid and receive no manufactured delta.
	body, err = json.Marshal(policy)
	require.NoError(t, err)
	w = run("admin", string(body))
	require.Equal(t, 200, w.Code)
	require.NotContains(t, w.Body.String(), "plan_change")
	d := &service.MonitoringProbePlanDraft{PlanID: saved.ID, AccountID: account, ModelID: "model-a", CronExpression: "0 * * * *", MaxResults: 50, Enabled: true}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = budget.PreviewCoverage(canceled, policy, d)
	require.ErrorIs(t, err, context.Canceled)
	// Group-plan IDs are a separate namespace and must remain in both sides
	// of an account-plan replacement, even when the numeric IDs are equal.
	var group int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform) VALUES('Draft group control','openai') RETURNING id`).Scan(&group))
	_, err = db.ExecContext(ctx, `INSERT INTO pelican_group_test_plans(id,group_id,model_id,cron_expression,enabled,pelican_config,next_run_at)
 VALUES($1,$2,'group-model','0 * * * *',true,'{"parallel_count":2}',NOW())`, saved.ID, group)
	require.NoError(t, err)
	out = read()
	require.EqualValues(t, 56, out.PlanChange.CurrentMinimumRequestsPerDay)
	require.EqualValues(t, 24, out.PlanChange.DeltaMinimumRequestsPerDay)
	require.EqualValues(t, 80, out.MinimumTotalRequestsPerDay)
	// Database faults must not look like zero demand or expose SQL internals;
	// repair the same fixture and verify the unchanged draft can be retried.
	_, err = db.ExecContext(ctx, `ALTER TABLE scheduled_test_plans RENAME TO draft_fault_plans`)
	require.NoError(t, err)
	body, err = json.Marshal(command)
	require.NoError(t, err)
	w = run("admin", string(body))
	require.Equal(t, 503, w.Code)
	require.NotContains(t, w.Body.String(), "draft_fault_plans")
	require.NotContains(t, w.Body.String(), "does not exist")
	_, err = db.ExecContext(ctx, `ALTER TABLE draft_fault_plans RENAME TO scheduled_test_plans`)
	require.NoError(t, err)
	out = read()
	require.EqualValues(t, 80, out.MinimumTotalRequestsPerDay)
	persisted, err := native.GetPlan(ctx, saved.ID)
	require.NoError(t, err)
	// No preview creates usage, leases, counters, results or scheduling events.
	for _, table := range []string{"monitoring_probe_budget_counters", "monitoring_probe_budget_leases", "scheduled_test_results", "scheduler_outbox", "usage_logs"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s", table)).Scan(&count))
		require.Zero(t, count, table)
	}
	var next time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT next_run_at FROM scheduled_test_plans WHERE id=$1`, saved.ID).Scan(&next))
	require.Equal(t, persisted.NextRunAt.UTC().Truncate(time.Microsecond), next.UTC())
}
