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
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestProcurementMonitoringRecipePostgresAdminHTTP(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	var product, source, judge, preset int64
	for name, id := range map[string]*int64{"Recipe product": &product, "Approved supplies": &source, "Independent grader": &judge} {
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,rate_multiplier) VALUES($1,'openai',.2) RETURNING id`, name).Scan(id))
	}
	policy := fmt.Sprintf(`{"version":6,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"not_required","availability":"not_required","source_group_ids":[%d]}`, source)
	_, err := db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$1 WHERE id=$2`, policy, product)
	require.NoError(t, err)
	accounts := []int64{}
	for i := 0; i < 3; i++ {
		var id int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,schedulable,credentials,extra)
 VALUES($1,'openai','apikey','active',$2,'{"api_key":"fixture-private-recipe-key"}','{}') RETURNING id`, fmt.Sprintf("Recipe supply %d", i), i != 1).Scan(&id))
		accounts = append(accounts, id)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$3),($2,$3)`, accounts[0], accounts[1], source)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, accounts[2], product)
	require.NoError(t, err)
	cfg := fmt.Sprintf(`{"question_kind":"candy","prompt":"private-recipe-prompt","reasoning_effort":"medium","parallel_count":2,"model_ids":["model-a","model-b"],"quality":{"action":"observe_only","expected_answer":"private-reference-answer","judge":{"kind":"llm","group_id":%d,"model_id":"grader","prompt":"private-grader-prompt"}}}`, judge)
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO quality_rule_presets(name,model_id,cron_expression,enabled,max_results,pelican_config)
 VALUES('Observe two models','model-a','0 * * * *',true,100,$1) RETURNING id`, cfg).Scan(&preset))
	budget := repository.NewMonitoringProbeBudgetRepository(db)
	settings := service.DefaultMonitoringProbeBudgetSettings()
	settings.Enabled, settings.RequestsPerDay, settings.RecoveryReservePercent = true, 1000, 0
	settings, err = budget.SaveSettings(ctx, settings)
	require.NoError(t, err)
	native := service.NewScheduledTestService(repository.NewScheduledTestPlanRepository(db), nil)
	_, err = native.CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: accounts[0], ModelID: "existing", CronExpression: "0 * * * *", Enabled: true, MaxResults: 50})
	require.NoError(t, err)
	users := repository.NewUserRepository(client, db)
	auth := service.NewAuthService(client, users, nil, nil, &config.Config{JWT: config.JWTConfig{Secret: "recipe-local-fixture", ExpireHour: 1}}, nil, nil, nil, nil, nil, nil, nil, nil)
	tokens := map[string]string{}
	for _, role := range []string{"admin", "user", "observer"} {
		var id int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,role) VALUES($1,'fixture',$2) RETURNING id`, "recipe-"+role+"@test.invalid", role).Scan(&id))
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
	run := func(role, query string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/admin/upstream-procurement/products/%d/impact%s", product, query), nil)
		if tokens[role] != "" {
			req.Header.Set("Authorization", "Bearer "+tokens[role])
		}
		router.ServeHTTP(w, req)
		for _, private := range []string{"private-recipe-prompt", "private-reference-answer", "private-grader-prompt", "fixture-private-recipe-key", "password_hash", "pq:"} {
			require.NotContains(t, w.Body.String(), private)
		}
		return w
	}
	query := fmt.Sprintf("?monitoring_preset_id=%d", preset)
	read := func() service.ProcurementProductImpact {
		t.Helper()
		w := run("admin", query)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		var out struct {
			Data service.ProcurementProductImpact `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		return out.Data
	}
	for role, code := range map[string]int{"": 401, "user": 403, "observer": 403} {
		require.Equal(t, code, run(role, query).Code)
	}
	out := read()
	r := out.MonitoringRecipe
	require.NotNil(t, r)
	require.Equal(t, 2, r.Version)
	require.Equal(t, "ready", r.DistributionReview.State)
	require.Equal(t, 2, r.DistributionReview.ExaminedAccounts)
	require.Empty(t, r.DistributionReview.Conflicts)
	require.Equal(t, "previewed", r.State)
	require.Equal(t, 2, *r.Accounts, "unbound and scheduling-paused approved supply are still detection inventory")
	require.Equal(t, 2, r.Demand.AdditionalPlans)
	require.EqualValues(t, 24, r.Demand.CurrentMinimumRequestsPerDay)
	require.EqualValues(t, 384, r.Demand.DeltaMinimumRequestsPerDay, "2 accounts x 2 models x 2 samples x 24 rounds x sample+judge")
	require.EqualValues(t, 408, r.Demand.Coverage.MinimumTotalRequestsPerDay)
	require.True(t, r.Demand.Coverage.CanSave)
	require.EqualValues(t, 1, r.SourcePolicyRevision)
	require.False(t, r.PresetUpdatedAt.IsZero())
	t.Run("budget capacity does not override the original observation-plan uniqueness", func(t *testing.T) {
		for _, test := range []struct {
			enabled bool
			channel string
		}{{true, "account"}, {false, "account"}, {true, "bps"}, {false, "bps"}} {
			var planID int64
			oldConfig := fmt.Sprintf(`{"question_kind":"candy","test_channel":%q,"prompt":"old-private-prompt","quality":{"action":"observe_only","expected_answer":"old-private-answer"}}`, test.channel)
			require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,max_results,pelican_config)
 VALUES($1,'old-observer','0 * * * *',$2,50,$3) RETURNING id`, accounts[0], test.enabled, oldConfig).Scan(&planID))
			t.Cleanup(func() {
				_, err := db.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE id=$1`, planID)
				require.NoError(t, err)
			})
			recipe := read().MonitoringRecipe
			require.True(t, recipe.Demand.Coverage.CanSave, "this fixture has sufficient global lower-bound capacity")
			// Compare the read-only explanation to the real, unchanged write path.
			var config service.PelicanTestConfig
			require.NoError(t, json.Unmarshal([]byte(cfg), &config))
			plan, err := service.ProcurementMonitoringRecipePlan(&service.QualityRulePreset{Enabled: true, ModelID: "model-a", CronExpression: "0 * * * *", MaxResults: 100, PelicanConfig: &config}, time.Now())
			require.NoError(t, err)
			plan.AccountID = accounts[0]
			_, err = native.CreatePlan(ctx, plan)
			var unique *pq.Error
			require.ErrorAs(t, err, &unique)
			require.Equal(t, pq.ErrorCode("23505"), unique.Code)
			require.Equal(t, "scheduled_test_quality_account_scope_unique", unique.Constraint)
			raw, err := json.Marshal(recipe)
			require.NoError(t, err)
			require.NotNil(t, recipe.DistributionReview, "a budget pass must not hide the real plan-uniqueness blocker")
			require.Equal(t, "conflict", recipe.DistributionReview.State)
			require.Equal(t, 2, recipe.DistributionReview.ExaminedAccounts)
			require.Equal(t, []service.ProcurementRecipeDistributionConflict{{AccountID: accounts[0], PlanID: planID, Enabled: test.enabled}}, recipe.DistributionReview.Conflicts)
			require.NotContains(t, string(raw), "old-private")
			_, err = db.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE id=$1`, planID)
			require.NoError(t, err)
			require.Equal(t, "ready", read().MonitoringRecipe.DistributionReview.State, "removal of the conflicting plan is visible on a new read")
		}
	})
	t.Run("other native scopes and unapproved accounts do not occupy an approved observation slot", func(t *testing.T) {
		for _, action := range []string{"disable_scheduling", "isolate_models", "enable_bps", "observe_only"} {
			id := accounts[0]
			if action == "observe_only" {
				id = accounts[2] // Only bound to the product, not approved as supply.
			}
			var planID int64
			require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,max_results,pelican_config)
 VALUES($1,'existing-rule','0 * * * *',true,50,$2) RETURNING id`, id, fmt.Sprintf(`{"quality":{"action":%q}}`, action)).Scan(&planID))
			t.Cleanup(func() {
				_, err := db.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE id=$1`, planID)
				require.NoError(t, err)
			})
			require.Equal(t, "ready", read().MonitoringRecipe.DistributionReview.State, action)
			var config service.PelicanTestConfig
			require.NoError(t, json.Unmarshal([]byte(cfg), &config))
			plan, err := service.ProcurementMonitoringRecipePlan(&service.QualityRulePreset{Enabled: true, ModelID: "model-a", CronExpression: "0 * * * *", MaxResults: 100, PelicanConfig: &config}, time.Now())
			require.NoError(t, err)
			plan.AccountID = accounts[0]
			created, err := native.CreatePlan(ctx, plan)
			require.NoError(t, err, "the preview matches the original writer for non-conflicting scopes")
			require.NoError(t, native.DeletePlan(ctx, created.ID))
			_, err = db.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE id=$1`, planID)
			require.NoError(t, err)
		}
	})
	t.Run("one snapshot for preset, source inventory and global capacity", func(t *testing.T) {
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
			out, err := repository.NewChannelRepository(db).(service.ProcurementCatalogRepository).PreviewProcurementProductImpact(readCtx, product, preset)
			pending <- result{out, err}
		}()
		require.Eventually(t, func() bool {
			var n int
			err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%accounts%'`).Scan(&n)
			return err == nil && n > 0
		}, 5*time.Second, 20*time.Millisecond)
		_, err = lock.ExecContext(ctx, `UPDATE quality_rule_presets SET name='Changed preset',cron_expression='0 */2 * * *',updated_at=clock_timestamp() WHERE id=$1`, preset)
		require.NoError(t, err)
		_, err = lock.ExecContext(ctx, `DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, accounts[1], source)
		require.NoError(t, err)
		var observationID int64
		require.NoError(t, lock.QueryRowContext(ctx, `INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,max_results,pelican_config)
 VALUES($1,'concurrent-observer','0 * * * *',false,50,'{"quality":{"action":"observe_only"}}') RETURNING id`, accounts[0]).Scan(&observationID))
		t.Cleanup(func() {
			_, err := db.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE id=$1`, observationID)
			require.NoError(t, err)
		})
		changed := settings
		changed.Revision++
		changed.RequestsPerDay = 1200
		raw, err := json.Marshal(changed)
		require.NoError(t, err)
		_, err = lock.ExecContext(ctx, `UPDATE settings SET value=$2 WHERE key=$1`, service.MonitoringProbeBudgetSettingKey, string(raw))
		require.NoError(t, err)
		require.NoError(t, lock.Commit())
		old := <-pending
		require.NoError(t, old.err)
		require.Equal(t, "Observe two models", old.out.MonitoringRecipe.PresetName)
		require.Equal(t, r.PresetUpdatedAt, old.out.MonitoringRecipe.PresetUpdatedAt)
		require.Equal(t, 2, *old.out.MonitoringRecipe.Accounts)
		require.EqualValues(t, 384, old.out.MonitoringRecipe.Demand.DeltaMinimumRequestsPerDay)
		require.Equal(t, settings.Revision, old.out.MonitoringRecipe.Demand.Coverage.BudgetRevision)
		require.EqualValues(t, 1000, old.out.MonitoringRecipe.Demand.Coverage.RegularDayCapacity)
		require.Equal(t, "ready", old.out.MonitoringRecipe.DistributionReview.State)
		require.Equal(t, 2, old.out.MonitoringRecipe.DistributionReview.ExaminedAccounts)
		fresh := read().MonitoringRecipe
		require.Equal(t, "Changed preset", fresh.PresetName)
		require.True(t, fresh.PresetUpdatedAt.After(old.out.MonitoringRecipe.PresetUpdatedAt))
		require.Equal(t, 1, *fresh.Accounts)
		require.EqualValues(t, 96, fresh.Demand.DeltaMinimumRequestsPerDay)
		require.Equal(t, changed.Revision, fresh.Demand.Coverage.BudgetRevision)
		require.EqualValues(t, 1200, fresh.Demand.Coverage.RegularDayCapacity)
		require.Equal(t, "conflict", fresh.DistributionReview.State)
		require.Equal(t, 1, fresh.DistributionReview.ExaminedAccounts)
		require.Equal(t, []service.ProcurementRecipeDistributionConflict{{AccountID: accounts[0], PlanID: observationID, Enabled: false}}, fresh.DistributionReview.Conflicts)
		_, err = db.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE id=$1`, observationID)
		require.NoError(t, err)
		settings.Revision = changed.Revision
		settings, err = budget.SaveSettings(ctx, settings)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `UPDATE quality_rule_presets SET name='Observe two models',cron_expression='0 * * * *' WHERE id=$1`, preset)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, accounts[1], source)
		require.NoError(t, err)
	})
	t.Run("explicit account filter intersects approved source groups", func(t *testing.T) {
		filtered := fmt.Sprintf(`{"version":6,"revision":2,"min_rate":null,"max_rate":null,"sources":{"account_ids":[%d,%d]},"buckets":{"codes":null},"quality":"not_required","availability":"not_required","source_group_ids":[%d]}`, accounts[0], accounts[2], source)
		_, err := db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$1 WHERE id=$2`, filtered, product)
		require.NoError(t, err)
		out := read().MonitoringRecipe
		require.EqualValues(t, 2, out.SourcePolicyRevision)
		require.Equal(t, 1, *out.Accounts)
		require.EqualValues(t, 192, out.Demand.DeltaMinimumRequestsPerDay)
		_, err = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$1 WHERE id=$2`, policy, product)
		require.NoError(t, err)
	})
	t.Run("budget disabled remains measured but unprotected", func(t *testing.T) {
		settings.Enabled = false
		settings, err = budget.SaveSettings(ctx, settings)
		require.NoError(t, err)
		out := read().MonitoringRecipe.Demand.Coverage
		require.False(t, out.Enforced)
		require.EqualValues(t, 408, out.MinimumTotalRequestsPerDay)
		require.NotEmpty(t, out.UnestimatedDemandSources)
		settings.Enabled = true
		settings, err = budget.SaveSettings(ctx, settings)
		require.NoError(t, err)
	})
	// No request silently enables, adopts or distributes the preset.
	for _, table := range []string{"monitoring_probe_budget_counters", "monitoring_probe_budget_leases", "scheduled_test_results", "scheduler_outbox", "usage_logs"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count, table)
	}
	var plans int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_plans`).Scan(&plans))
	require.Equal(t, 1, plans)
	require.NotContains(t, run("admin", "").Body.String(), "monitoring_recipe", "old callers retain saved-only behavior")
	for _, q := range []string{"?monitoring_preset_id=", "?monitoring_preset_id=0", "?monitoring_preset_id=9007199254740992", query + "&monitoring_preset_id=1"} {
		require.Equal(t, 400, run("admin", q).Code)
	}
	require.Equal(t, 404, run("admin", "?monitoring_preset_id=9007199254740990").Code)
	_, err = db.ExecContext(ctx, `UPDATE quality_rule_presets SET cron_expression='*/5 * * * *' WHERE id=$1`, preset)
	require.NoError(t, err)
	out = read()
	require.False(t, out.MonitoringRecipe.Demand.Coverage.CanSave)
	require.Contains(t, out.MonitoringRecipe.Demand.Coverage.BlockingReasons, "day_capacity")
	_, err = db.ExecContext(ctx, `UPDATE quality_rule_presets SET enabled=false WHERE id=$1`, preset)
	require.NoError(t, err)
	require.Equal(t, 400, run("admin", query).Code)
	_, err = db.ExecContext(ctx, `UPDATE quality_rule_presets SET enabled=true,cron_expression='0 * * * *',pelican_config=jsonb_set(pelican_config,'{quality,action}','"remove_groups"') WHERE id=$1`, preset)
	require.NoError(t, err)
	require.Equal(t, 400, run("admin", query).Code)
	_, err = db.ExecContext(ctx, `UPDATE quality_rule_presets SET pelican_config=$2 WHERE id=$1`, preset, cfg)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM account_groups WHERE group_id=$1`, source)
	require.NoError(t, err)
	out = read()
	require.Equal(t, "no_approved_accounts", out.MonitoringRecipe.State)
	require.Zero(t, *out.MonitoringRecipe.Accounts)
	require.Nil(t, out.MonitoringRecipe.Demand)
	require.Equal(t, "unknown", out.MonitoringRecipe.DistributionReview.State)
	require.Zero(t, out.MonitoringRecipe.DistributionReview.ExaminedAccounts)
	require.Empty(t, out.MonitoringRecipe.DistributionReview.Conflicts)
	_, err = db.ExecContext(ctx, `UPDATE groups SET status='inactive' WHERE id=$1`, source)
	require.NoError(t, err)
	out = read()
	require.Equal(t, "sources_unavailable", out.MonitoringRecipe.State)
	require.Equal(t, "unknown", out.MonitoringRecipe.DistributionReview.State)
	require.Nil(t, out.MonitoringRecipe.Accounts)
	require.Nil(t, out.MonitoringRecipe.Demand)
	_, err = db.ExecContext(ctx, `ALTER TABLE quality_rule_presets RENAME TO recipe_fault_presets`)
	require.NoError(t, err)
	require.Equal(t, 503, run("admin", query).Code)
	_, err = db.ExecContext(ctx, `ALTER TABLE recipe_fault_presets RENAME TO quality_rule_presets`)
	require.NoError(t, err)
	require.Equal(t, 200, run("admin", query).Code)
	_, err = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=NULL WHERE id=$1`, product)
	require.NoError(t, err)
	out = read()
	require.Equal(t, "sources_unavailable", out.MonitoringRecipe.State, "no approved source contract must not use the existing product pool")
	require.Nil(t, out.MonitoringRecipe.Accounts)
	require.Nil(t, out.MonitoringRecipe.Demand)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = repository.NewChannelRepository(db).(service.ProcurementCatalogRepository).PreviewProcurementProductImpact(canceled, product, preset)
	require.ErrorIs(t, err, context.Canceled)
}
