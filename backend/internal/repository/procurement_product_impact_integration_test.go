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
		var sourceID, unboundID int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform) VALUES('Snapshot source before','openai') RETURNING id`).Scan(&sourceID))
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,credentials) VALUES('Snapshot unbound before','openai','apikey','{}') RETURNING id`).Scan(&unboundID))
		_, err := db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, unboundID, sourceID)
		require.NoError(t, err)
		policy := fmt.Sprintf(`{"version":6,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"not_required","availability":"not_required","source_group_ids":[%d]}`, sourceID)
		_, err = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$2::jsonb WHERE id=$1`, groupID, policy)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, e := db.ExecContext(ctx, `UPDATE groups SET procurement_policy=NULL WHERE id=$1`, groupID)
			require.NoError(t, e)
		})
		budget := service.DefaultMonitoringProbeBudgetSettings()
		budget.Revision = 7
		raw, err := json.Marshal(budget)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,$2)`, service.MonitoringProbeBudgetSettingKey, string(raw))
		require.NoError(t, err)
		t.Cleanup(func() {
			_, e := db.ExecContext(ctx, `DELETE FROM settings WHERE key=$1`, service.MonitoringProbeBudgetSettingKey)
			require.NoError(t, e)
		})
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
		_, err = lock.ExecContext(ctx, `UPDATE groups SET name='Snapshot source after' WHERE id=$1`, sourceID)
		require.NoError(t, err)
		_, err = lock.ExecContext(ctx, `UPDATE accounts SET name='Snapshot unbound after' WHERE id=$1`, unboundID)
		require.NoError(t, err)
		budget.Revision = 8
		raw, err = json.Marshal(budget)
		require.NoError(t, err)
		_, err = lock.ExecContext(ctx, `UPDATE settings SET value=$2 WHERE key=$1`, service.MonitoringProbeBudgetSettingKey, string(raw))
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
		require.Equal(t, int64(7), old.out.ManagementReview.Budget.Revision)
		require.Equal(t, "Snapshot source before", old.out.ManagementReview.SourceInventory.Groups[0].Name)
		require.Equal(t, "Snapshot unbound before", old.out.ManagementReview.SourceInventory.Accounts[1].Name)
		fresh := read()
		require.Len(t, fresh.Report.Verdicts, 1)
		require.Equal(t, .3, fresh.Report.DefaultD)
		require.Equal(t, .25, fresh.Report.MinEffectiveD)
		require.Equal(t, .025, *fresh.Report.Verdicts[0].EffectiveRate)
		require.Equal(t, int64(8), fresh.ManagementReview.Budget.Revision)
		require.Equal(t, "Snapshot source after", fresh.ManagementReview.SourceInventory.Groups[0].Name)
		require.Equal(t, "Snapshot unbound after", fresh.ManagementReview.SourceInventory.Accounts[1].Name)
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
	t.Run("management review is returned without taking over the product", func(t *testing.T) {
		w := run(id, "admin")
		require.Equal(t, 200, w.Code)
		var envelope struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.Contains(t, envelope.Data, "management_review", "price-only success must not silently imply safe automatic management")
	})
	t.Run("saved rule conflicts, paused owners and shared budget remain separate", func(t *testing.T) {
		var otherID int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,credentials) VALUES('Former member','openai','apikey','{}') RETURNING id`).Scan(&otherID))
		planIDs := []int64{}
		t.Cleanup(func() {
			for _, pid := range planIDs {
				_, e := db.ExecContext(ctx, `DELETE FROM account_quality_states WHERE plan_id=$1`, pid)
				require.NoError(t, e)
				_, e = db.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE id=$1`, pid)
				require.NoError(t, e)
			}
			_, e := db.ExecContext(ctx, `DELETE FROM settings WHERE key=$1`, service.MonitoringProbeBudgetSettingKey)
			require.NoError(t, e)
		})
		add := func(aid int64, enabled, recover bool, config, state string) int64 {
			t.Helper()
			var pid int64
			require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,max_results,auto_recover,pelican_config,next_run_at) VALUES($1,'sol','* * * * *',$2,10,$3,$4::jsonb,now()) RETURNING id`, aid, enabled, recover, config).Scan(&pid))
			planIDs = append(planIDs, pid)
			if state != "" {
				_, e := db.ExecContext(ctx, `INSERT INTO account_quality_states(plan_id,state) VALUES($1,$2::jsonb)`, pid, state)
				require.NoError(t, e)
			}
			return pid
		}
		remove := fmt.Sprintf(`{"question_kind":"candy","parallel_count":1,"quality":{"action":"remove_groups","remove_group_ids":[%d],"expected_answer":"private-review-prompt"}}`, groupID)
		live := add(accountID, true, false, remove, "")
		paused := add(otherID, false, false, `{"quality":{"action":"observe_only"}}`, fmt.Sprintf(`{"action":"remove_groups","removed":[{"group_id":%d}]}`, groupID))
		composite := add(otherID, false, false, `null`, fmt.Sprintf(`{"action":"isolate_models","groups":{"removed":[{"group_id":%d}]} }`, groupID))
		unknown := add(accountID, false, false, `null`, `{"future_private_owner":"private-review-state"}`)
		judge := add(accountID, true, false, fmt.Sprintf(`{"question_kind":"candy","parallel_count":1,"quality":{"action":"observe_only","judge":{"kind":"llm","group_id":%d,"model_id":"sol","prompt":"private-judge-prompt"}}}`, groupID), "")
		protection := add(accountID, true, true, `null`, "")
		bps := add(accountID, false, false, `null`, `{"bps_applied":{"bps_enabled":true}}`)
		ignored := add(otherID, true, false, `{"quality":{"action":"remove_groups","remove_group_ids":[999999]}}`, "")
		pausedEmpty := add(accountID, false, false, `null`, "")
		out := read()
		review := out.ManagementReview
		require.NotNil(t, review)
		require.Equal(t, "manual", review.Mode)
		require.Equal(t, "current_bindings_and_saved_rule_targets", review.Scope)
		require.True(t, review.InventoryComplete)
		require.Equal(t, 7, review.RulesExamined)
		require.Equal(t, 3, review.ConflictingRules)
		require.Equal(t, 2, review.ProtectionRules)
		require.Equal(t, 1, review.UnknownRules)
		require.Equal(t, 1, review.JudgeDependencyRules)
		rows := map[int64]service.ProcurementManagementRule{}
		for _, r := range review.Rules {
			rows[r.PlanID] = r
		}
		require.True(t, rows[live].WritesProduct)
		require.False(t, rows[live].RetainedOwnership)
		for _, pid := range []int64{paused, composite} {
			require.False(t, rows[pid].Enabled)
			require.True(t, rows[pid].RetainedOwnership)
			require.True(t, rows[pid].SavedProductOwnership)
			require.Equal(t, "membership_conflict", rows[pid].Risk)
		}
		require.Equal(t, "unknown", rows[unknown].Risk)
		require.True(t, rows[judge].JudgeDependsOnProduct)
		require.Equal(t, "observation", rows[judge].Risk)
		require.Equal(t, "account_protection", rows[protection].Risk)
		require.True(t, rows[bps].RetainedOwnership)
		require.Equal(t, "account_protection", rows[bps].Risk)
		require.NotContains(t, rows, ignored)
		require.NotContains(t, rows, pausedEmpty)
		for _, marker := range []string{"private-review-prompt", "private-review-state", "private-judge-prompt"} {
			require.NotContains(t, run(id, "admin").Body.String(), marker)
		}
		require.Equal(t, "not_enforced", review.Budget.State)
		settings := service.DefaultMonitoringProbeBudgetSettings()
		settings.Enabled = true
		settings.RequestsPerDay = 1
		settings.Revision = 2
		raw, e := json.Marshal(settings)
		require.NoError(t, e)
		_, e = db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,$2)`, service.MonitoringProbeBudgetSettingKey, string(raw))
		require.NoError(t, e)
		budget := read().ManagementReview.Budget
		require.True(t, budget.Enforced)
		require.Equal(t, int64(2), budget.Revision)
		require.Equal(t, "insufficient", budget.State)
		require.Contains(t, budget.Reasons, "day_capacity")
		require.Greater(t, budget.MinimumRequestsPerDay, budget.RegularDayCapacity)
		settings.RequestsPerDay = 100000
		settings.Revision++
		raw, e = json.Marshal(settings)
		require.NoError(t, e)
		_, e = db.ExecContext(ctx, `UPDATE settings SET value=$2 WHERE key=$1`, service.MonitoringProbeBudgetSettingKey, string(raw))
		require.NoError(t, e)
		budget = read().ManagementReview.Budget
		require.Equal(t, "partial", budget.State)
		require.Greater(t, budget.UnestimatedSources, 0)
		require.True(t, budget.WindowEnd.After(budget.WindowStart))
		require.Equal(t, 24*time.Hour, budget.WindowEnd.Sub(budget.WindowStart))
		var before string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_build_object('states',(SELECT jsonb_agg(state ORDER BY plan_id) FROM account_quality_states),'events',(SELECT count(*) FROM scheduler_outbox),'bindings',(SELECT jsonb_agg(to_jsonb(g) ORDER BY account_id,group_id) FROM account_groups g))::text`).Scan(&before))
		read()
		read()
		var after string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_build_object('states',(SELECT jsonb_agg(state ORDER BY plan_id) FROM account_quality_states),'events',(SELECT count(*) FROM scheduler_outbox),'bindings',(SELECT jsonb_agg(to_jsonb(g) ORDER BY account_id,group_id) FROM account_groups g))::text`).Scan(&after))
		require.JSONEq(t, before, after, "review cannot consume ownership or change scheduling")
		_, e = db.ExecContext(ctx, `UPDATE settings SET value='{"revision":"private-invalid"}' WHERE key=$1`, service.MonitoringProbeBudgetSettingKey)
		require.NoError(t, e)
		w := run(id, "admin")
		require.Equal(t, 503, w.Code)
		require.NotContains(t, w.Body.String(), "private-invalid")
		_, e = db.ExecContext(ctx, `DELETE FROM settings WHERE key=$1`, service.MonitoringProbeBudgetSettingKey)
		require.NoError(t, e)
		require.Equal(t, 200, run(id, "admin").Code)
	})
	t.Run("rule inventory cap stays incomplete instead of implying safe takeover", func(t *testing.T) {
		_, e := db.ExecContext(ctx, `WITH added AS (INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,max_results,auto_recover) SELECT $1,'sol','0 * * * *',false,10,false FROM generate_series(1,1001) RETURNING id) INSERT INTO account_quality_states(plan_id,state) SELECT id,'{}'::jsonb FROM added`, accountID)
		require.NoError(t, e)
		t.Cleanup(func() {
			_, e := db.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE account_id=$1`, accountID)
			require.NoError(t, e)
		})
		review := read().ManagementReview
		require.False(t, review.InventoryComplete)
		require.Equal(t, 1000, review.RulesExamined)
		require.Len(t, review.Rules, 1000)
		require.Zero(t, review.ConflictingRules)
		require.Equal(t, "manual", review.Mode)
	})
	t.Run("approved source inventory includes unbound accounts and their protections", func(t *testing.T) {
		var sourceID, secondID, newID, excludedID, ordinaryID int64
		for i, name := range []string{"Approved source Alpha", "Approved source Beta"} {
			var id int64
			require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform) VALUES($1,'openai') RETURNING id`, name).Scan(&id))
			if i == 0 {
				sourceID = id
			} else {
				secondID = id
			}
		}
		for i, name := range []string{"Not yet in product", "Excluded by account filter", "Ordinary private supplier"} {
			var id int64
			require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,credentials) VALUES($1,'openai','apikey','{}') RETURNING id`, name).Scan(&id))
			switch i {
			case 0:
				newID = id
			case 1:
				excludedID = id
			case 2:
				ordinaryID = id
			}
		}
		_, e := db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$3),($1,$4),($2,$3)`, newID, excludedID, sourceID, secondID)
		require.NoError(t, e)
		var planID, ignoredPlanID int64
		for _, entry := range []struct {
			id  int64
			dst *int64
		}{{newID, &planID}, {ordinaryID, &ignoredPlanID}} {
			require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,max_results,auto_recover,pelican_config,next_run_at) VALUES($1,'sol','0 * * * *',true,10,false,'{"quality":{"action":"disable_scheduling"}}',now()) RETURNING id`, entry.id).Scan(entry.dst))
		}
		policy := fmt.Sprintf(`{"version":6,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":[%d]},"buckets":{"codes":null},"quality":"not_required","availability":"not_required","source_group_ids":[%d,%d]}`, newID, sourceID, secondID)
		_, e = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$2::jsonb WHERE id=$1`, groupID, policy)
		require.NoError(t, e)
		t.Cleanup(func() {
			_, e := db.ExecContext(ctx, `UPDATE groups SET procurement_policy=NULL WHERE id=$1`, groupID)
			require.NoError(t, e)
		})
		get := func() map[string]any {
			t.Helper()
			w := run(id, "admin")
			require.Equal(t, 200, w.Code, w.Body.String())
			var body struct {
				Data struct {
					Review map[string]any `json:"management_review"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			return body.Data.Review
		}
		review := get()
		require.Contains(t, review, "source_inventory", "approved but unbound supply must not disappear from the handover check")
		inventory := review["source_inventory"].(map[string]any)
		require.Equal(t, "approved_sources", inventory["state"])
		require.Equal(t, float64(1), inventory["approved_accounts"])
		require.Equal(t, float64(1), inventory["unbound_approved_accounts"])
		require.Equal(t, "current_bindings_and_approved_sources_and_saved_rule_targets", review["scope"])
		entries := inventory["accounts"].([]any)
		require.Len(t, entries, 2, "union must retain the existing member, deduplicate sources and exclude unapproved accounts")
		seen := map[int64]map[string]any{}
		for _, row := range entries {
			r := row.(map[string]any)
			seen[int64(r["account_id"].(float64))] = r
		}
		require.Equal(t, false, seen[newID]["in_product"])
		require.Equal(t, true, seen[newID]["in_approved_sources"])
		require.Equal(t, true, seen[accountID]["in_product"])
		require.Equal(t, false, seen[accountID]["in_approved_sources"])
		planFound, ignoredFound := false, false
		for _, row := range review["rules"].([]any) {
			r := row.(map[string]any)
			if r["plan_id"] == float64(planID) {
				planFound = true
				require.Equal(t, "account_protection", r["risk"])
			}
			ignoredFound = ignoredFound || r["plan_id"] == float64(ignoredPlanID)
		}
		require.True(t, planFound)
		require.False(t, ignoredFound)
		require.Contains(t, run(id, "admin").Body.String(), "Approved source Alpha")
		// A selected source losing authority invalidates the whole approval. Never
		// silently fall back to ordinary accounts or report zero as a valid pool.
		_, e = db.ExecContext(ctx, `UPDATE groups SET status='inactive' WHERE id=$1`, sourceID)
		require.NoError(t, e)
		review = get()
		inventory = review["source_inventory"].(map[string]any)
		require.Equal(t, "unavailable", inventory["state"])
		require.Nil(t, inventory["approved_accounts"])
		require.Nil(t, inventory["unbound_approved_accounts"])
		require.Equal(t, "current_bindings_and_saved_rule_targets", review["scope"])
		require.Len(t, inventory["accounts"], 1)
		// Explicitly empty approval differs from an unconfigured/original pool.
		_, e = db.ExecContext(ctx, `UPDATE groups SET status='active' WHERE id=$1`, sourceID)
		require.NoError(t, e)
		_, e = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=jsonb_set(procurement_policy,'{source_group_ids}','[]'::jsonb) WHERE id=$1`, groupID)
		require.NoError(t, e)
		inventory = get()["source_inventory"].(map[string]any)
		require.Equal(t, "approved_sources", inventory["state"])
		require.Equal(t, float64(0), inventory["approved_accounts"])
		_, e = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=jsonb_set(procurement_policy,'{source_group_ids}','null'::jsonb) WHERE id=$1`, groupID)
		require.NoError(t, e)
		inventory = get()["source_inventory"].(map[string]any)
		require.Equal(t, "original_pool", inventory["state"])
		require.Nil(t, inventory["approved_accounts"])
		// All review reads, including source faults, leave membership, plans and
		// saved ownership untouched. Ordinary accounts are never imported.
		var snapshot string
		unchanged := func() string {
			t.Helper()
			var value string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_build_object('accounts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM accounts a),'bindings',(SELECT jsonb_agg(to_jsonb(g) ORDER BY account_id,group_id) FROM account_groups g),'plans',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM scheduled_test_plans p),'states',(SELECT jsonb_agg(to_jsonb(s) ORDER BY plan_id) FROM account_quality_states s),'outbox',(SELECT count(*) FROM scheduler_outbox))::text`).Scan(&value))
			return value
		}
		snapshot = unchanged()
		get()
		get()
		require.JSONEq(t, snapshot, unchanged())
		// A malformed persisted policy fails the whole read, not an empty or
		// permissive inventory. The HTTP error never echoes private stored data.
		_, e = db.ExecContext(ctx, `UPDATE groups SET procurement_policy='{"private-broken-policy":"do-not-leak"}'::jsonb WHERE id=$1`, groupID)
		require.NoError(t, e)
		w := run(id, "admin")
		require.Equal(t, 503, w.Code)
		require.NotContains(t, w.Body.String(), "do-not-leak")
		_, e = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$2::jsonb WHERE id=$1`, groupID, policy)
		require.NoError(t, e)
		require.Equal(t, 200, run(id, "admin").Code)
	})
	t.Run("approved union cap rejects excess, preserves exact boundary and account filters", func(t *testing.T) {
		var sourceID int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform) VALUES('Bounded approved source','openai') RETURNING id`).Scan(&sourceID))
		_, e := db.ExecContext(ctx, `WITH added AS (INSERT INTO accounts(name,platform,type,credentials) SELECT 'source-cap-'||n,'openai','apikey','{}' FROM generate_series(1,1000) n RETURNING id) INSERT INTO account_groups(account_id,group_id) SELECT id,$1 FROM added`, sourceID)
		require.NoError(t, e)
		policy := fmt.Sprintf(`{"version":6,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"not_required","availability":"not_required","source_group_ids":[%d]}`, sourceID)
		_, e = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$2::jsonb WHERE id=$1`, groupID, policy)
		require.NoError(t, e)
		t.Cleanup(func() {
			_, e := db.ExecContext(ctx, `UPDATE groups SET procurement_policy=NULL WHERE id=$1`, groupID)
			require.NoError(t, e)
		})
		w := run(id, "admin")
		require.Equal(t, 400, w.Code)
		require.Contains(t, w.Body.String(), "PROCUREMENT_PRODUCT_IMPACT_TOO_LARGE")
		_, e = db.ExecContext(ctx, `DELETE FROM account_groups WHERE group_id=$1 AND account_id=(SELECT max(account_id) FROM account_groups WHERE group_id=$1)`, sourceID)
		require.NoError(t, e)
		out := read()
		require.Len(t, out.ManagementReview.SourceInventory.Accounts, 1000)
		require.Equal(t, 999, *out.ManagementReview.SourceInventory.ApprovedAccounts)
		_, e = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=jsonb_set(procurement_policy,'{sources,account_ids}','[]'::jsonb) WHERE id=$1`, groupID)
		require.NoError(t, e)
		out = read()
		require.Len(t, out.ManagementReview.SourceInventory.Accounts, 1)
		require.Zero(t, *out.ManagementReview.SourceInventory.ApprovedAccounts)
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
