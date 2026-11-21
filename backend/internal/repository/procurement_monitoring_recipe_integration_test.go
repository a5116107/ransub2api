//go:build integration

package repository_test

import (
	"bytes"
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
	protected.POST("/upstream-procurement/products/:id/monitoring-recipe", h.ApplyProcurementRecipe)
	protected.POST("/upstream-procurement/monitoring-preview", h.ProcurementSharedMonitoringPreview)
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
	require.Equal(t, 3, r.Version)
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
	t.Run("shared products compile one native model and cron union with original budget and no writes", func(t *testing.T) {
		var second, secondSource, secondPreset int64
		for name, id := range map[string]*int64{"Second sales product": &second, "Second approved inventory": &secondSource} {
			require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,rate_multiplier) VALUES($1,'openai',.3) RETURNING id`, name).Scan(id))
		}
		secondPolicy := fmt.Sprintf(`{"version":6,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"not_required","availability":"not_required","source_group_ids":[%d]}`, secondSource)
		_, err := db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$1 WHERE id=$2`, secondPolicy, second)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2),($3,$4)`, accounts[0], secondSource, accounts[2], second)
		require.NoError(t, err)
		secondConfig := strings.Replace(cfg, `["model-a","model-b"]`, `["model-b","model-c"]`, 1)
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO quality_rule_presets(name,model_id,cron_expression,enabled,max_results,pelican_config) VALUES('Second preset','model-b','*/30 * * * *',true,100,$1) RETURNING id`, secondConfig).Scan(&secondPreset))
		selection := &service.ProcurementSharedMonitoringCommand{Version: 1, Products: []service.ProcurementMonitoringProductSelection{{ProductID: second, PresetID: secondPreset}, {ProductID: product, PresetID: preset}}}
		hashes := func() map[string]string {
			t.Helper()
			out := map[string]string{}
			for _, table := range []string{"groups", "accounts", "account_groups", "quality_rule_presets", "scheduled_test_plans", "monitoring_probe_budget_counters", "monitoring_probe_budget_leases", "scheduled_test_results", "scheduler_outbox", "usage_logs"} {
				var digest string
				require.NoError(t, db.QueryRowContext(ctx, `SELECT md5(COALESCE(string_agg(to_jsonb(t)::text,E'\n' ORDER BY to_jsonb(t)::text),'')) FROM `+table+` t`).Scan(&digest))
				out[table] = digest
			}
			return out
		}
		call := func(role string, raw []byte) *httptest.ResponseRecorder {
			t.Helper()
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/upstream-procurement/monitoring-preview", bytes.NewReader(raw))
			if tokens[role] != "" {
				req.Header.Set("Authorization", "Bearer "+tokens[role])
			}
			router.ServeHTTP(w, req)
			for _, secret := range []string{"private-", "fixture-private-recipe-key", "pq:", "password_hash"} {
				require.NotContains(t, w.Body.String(), secret)
			}
			return w
		}
		readShared := func() service.ProcurementSharedMonitoringPreview {
			t.Helper()
			before := hashes()
			raw, err := json.Marshal(selection)
			require.NoError(t, err)
			w := call("admin", raw)
			require.Equal(t, 200, w.Code, w.Body.String())
			require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			require.Equal(t, before, hashes())
			var reply struct {
				Data service.ProcurementSharedMonitoringPreview `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &reply))
			return reply.Data
		}
		raw, err := json.Marshal(selection)
		require.NoError(t, err)
		for role, status := range map[string]int{"": 401, "user": 403, "observer": 403} {
			require.Equal(t, status, call(role, raw).Code)
		}
		for _, raw := range [][]byte{[]byte(`null`), []byte(`{"version":1,"products":[]}`), []byte(`{"version":1,"version":1,"products":[{"product_id":1,"preset_id":1}]}`), bytes.Repeat([]byte(" "), 8193)} {
			require.Equal(t, 400, call("admin", raw).Code)
		}
		got := readShared()
		require.Equal(t, "compatible", got.State)
		require.Equal(t, 3, got.Requirements)
		require.Equal(t, 1, got.SharedAccounts)
		require.Len(t, got.Accounts, 2)
		require.Equal(t, accounts[0], got.Accounts[0].AccountID)
		require.Equal(t, []string{"model-a", "model-b", "model-c"}, got.Accounts[0].Models)
		require.Len(t, got.Accounts[0].ProductIDs, 2)
		require.Nil(t, got.Accounts[0].ExistingPlanID)
		require.EqualValues(t, 24, *got.CurrentMinimumRequestsPerDay)
		require.EqualValues(t, 768, *got.DeltaMinimumRequestsPerDay)
		require.EqualValues(t, 792, got.Coverage.MinimumTotalRequestsPerDay)
		require.EqualValues(t, 3, got.Coverage.AccountPlans, "one original connectivity plus two distinct account quality plans, not three product-account rules")
		require.True(t, got.Coverage.CanSave)
		settings.RequestsPerDay = 500
		settings, err = budget.SaveSettings(ctx, settings)
		require.NoError(t, err)
		got = readShared()
		require.Equal(t, "compatible", got.State, "compatibility and budget are different conclusions")
		require.False(t, got.Coverage.CanSave)
		require.NotEmpty(t, got.Coverage.BlockingReasons)
		settings.RequestsPerDay = 1000
		settings, err = budget.SaveSettings(ctx, settings)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `UPDATE quality_rule_presets SET cron_expression='0 * * * *' WHERE id=$1`, secondPreset)
		require.NoError(t, err)
		got = readShared()
		require.EqualValues(t, 480, *got.DeltaMinimumRequestsPerDay, "shared overlap is measured once, not summed across products")
		var nativeConfig service.PelicanTestConfig
		require.NoError(t, json.Unmarshal([]byte(cfg), &nativeConfig))
		old, err := native.CreatePlan(ctx, &service.ScheduledTestPlan{AccountID: accounts[0], ModelID: "model-a", CronExpression: "0 * * * *", Enabled: false, MaxResults: 100, PelicanConfig: &nativeConfig})
		require.NoError(t, err)
		got = readShared()
		require.Equal(t, "conflict", got.State)
		require.Equal(t, "existing_observation_plan", got.Accounts[0].Reason)
		require.Equal(t, old.ID, *got.Accounts[0].ExistingPlanID)
		require.False(t, *got.Accounts[0].ExistingEnabled)
		require.NotNil(t, got.Coverage, "native conflict never grants permission even when budget can be compiled")
		require.NoError(t, native.DeletePlan(ctx, old.ID))
		_, err = db.ExecContext(ctx, `UPDATE quality_rule_presets SET pelican_config=jsonb_set(pelican_config,'{reasoning_effort}','"high"') WHERE id=$1`, secondPreset)
		require.NoError(t, err)
		got = readShared()
		require.Equal(t, "conflict", got.State)
		require.Equal(t, "observation_settings_conflict", got.Accounts[0].Reason)
		require.Nil(t, got.Coverage)
		require.Nil(t, got.DeltaMinimumRequestsPerDay)
		_, err = db.ExecContext(ctx, `UPDATE quality_rule_presets SET pelican_config=$2 WHERE id=$1`, secondPreset, secondConfig)
		require.NoError(t, err)
		t.Run("one repeatable read snapshot across all products", func(t *testing.T) {
			lock, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = lock.Rollback() }()
			_, err = lock.ExecContext(ctx, `LOCK TABLE quality_rule_presets IN ACCESS EXCLUSIVE MODE`)
			require.NoError(t, err)
			type reply struct {
				out *service.ProcurementSharedMonitoringPreview
				err error
			}
			done := make(chan reply, 1)
			repo := repository.NewChannelRepository(db).(service.ProcurementSharedMonitoringRepository)
			go func() { out, err := repo.PreviewProcurementSharedMonitoring(ctx, selection); done <- reply{out, err} }()
			require.Eventually(t, func() bool {
				var waiting bool
				err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE NOT granted AND relation='quality_rule_presets'::regclass)`).Scan(&waiting)
				return err == nil && waiting
			}, 3*time.Second, 10*time.Millisecond)
			_, err = db.ExecContext(ctx, `DELETE FROM account_groups WHERE group_id=$1`, secondSource)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, accounts[2], secondSource)
			require.NoError(t, err)
			require.NoError(t, lock.Commit())
			select {
			case result := <-done:
				require.NoError(t, result.err)
				require.Equal(t, 1, result.out.SharedAccounts)
				require.Len(t, result.out.Accounts, 2)
				require.Equal(t, accounts[0], result.out.Accounts[0].AccountID)
			case <-time.After(5 * time.Second):
				t.Fatal("snapshot preview did not finish")
			}
			_, err = db.ExecContext(ctx, `DELETE FROM account_groups WHERE group_id=$1`, secondSource)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, accounts[0], secondSource)
			require.NoError(t, err)
		})
		_, err = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=NULL WHERE id=$1`, second)
		require.NoError(t, err)
		got = readShared()
		require.Equal(t, "incomplete", got.State)
		require.Nil(t, got.Coverage)
		require.Len(t, got.Accounts, 2, "unknown source must not use the product's old bound extra account")
		_, err = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$2,status='inactive' WHERE id=$1`, second, secondPolicy)
		require.NoError(t, err)
		got = readShared()
		require.Equal(t, "incomplete", got.State)
		require.Nil(t, got.Coverage)
		_, err = db.ExecContext(ctx, `UPDATE groups SET status='active' WHERE id=$1`, second)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `DELETE FROM account_groups WHERE group_id=$1`, secondSource)
		require.NoError(t, err)
		got = readShared()
		require.Equal(t, "incomplete", got.State)
		require.Nil(t, got.Coverage)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = repository.NewChannelRepository(db).(service.ProcurementSharedMonitoringRepository).PreviewProcurementSharedMonitoring(canceled, selection)
		require.ErrorIs(t, err, context.Canceled)
		_, err = db.ExecContext(ctx, `ALTER TABLE quality_rule_presets RENAME TO shared_fault_presets`)
		require.NoError(t, err)
		require.Equal(t, 503, call("admin", raw).Code)
		_, err = db.ExecContext(ctx, `ALTER TABLE shared_fault_presets RENAME TO quality_rule_presets`)
		require.NoError(t, err)
		require.Equal(t, 200, call("admin", raw).Code)
	})
	apply := func(role string, raw []byte) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/admin/upstream-procurement/products/%d/monitoring-recipe", product), bytes.NewReader(raw))
		if tokens[role] != "" {
			req.Header.Set("Authorization", "Bearer "+tokens[role])
		}
		router.ServeHTTP(w, req)
		require.NotContains(t, w.Body.String(), "private-")
		require.NotContains(t, w.Body.String(), "pq:")
		return w
	}
	command := func() []byte {
		t.Helper()
		r := read().MonitoringRecipe
		require.True(t, r.ApplicationReview.CanConfirm)
		raw, err := json.Marshal(service.ProcurementRecipeCommand{Version: 1, PresetID: preset, ObservedAt: r.ApplicationReview.ObservedAt, Fingerprint: r.ApplicationReview.Fingerprint})
		require.NoError(t, err)
		return raw
	}
	t.Run("first additions use original plans and are independently readable without adopting old rules", func(t *testing.T) {
		raw := command()
		for role, status := range map[string]int{"": 401, "user": 403, "observer": 403} {
			require.Equal(t, status, apply(role, raw).Code)
		}
		for _, bad := range [][]byte{[]byte(`null`), []byte(`{}`), []byte(`{"version":1,"version":1,"preset_id":1,"observed_at":"2026-11-21T00:00:00Z","fingerprint":"` + string(bytes.Repeat([]byte("a"), 64)) + `"}`), append(raw[:len(raw)-1:len(raw)-1], []byte(`,"account_ids":[1]}`)...), bytes.Repeat([]byte(" "), 8193)} {
			require.Equal(t, 400, apply("admin", bad).Code)
		}
		w := apply("admin", raw)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		var saved struct {
			Data service.ProcurementRecipeApplication `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &saved))
		require.Equal(t, product, saved.Data.GroupID)
		require.Equal(t, preset, saved.Data.PresetID)
		require.Len(t, saved.Data.Plans, 2)
		for _, row := range saved.Data.Plans {
			plan, err := native.GetPlan(ctx, row.PlanID)
			require.NoError(t, err)
			require.Equal(t, row.AccountID, plan.AccountID)
			require.Contains(t, accounts[:2], plan.AccountID)
			require.True(t, plan.Enabled)
			require.NotNil(t, plan.NextRunAt)
			require.Equal(t, service.QualityActionObserveOnly, plan.PelicanConfig.Quality.Action)
			require.Equal(t, []string{"model-a", "model-b"}, plan.PelicanConfig.ModelIDs)
			// Use the original runner consumer, including the unbound and
			// scheduling-paused approved account. No paid request is issued.
			runner := repository.NewScheduledTestPlanRepository(db)
			due, err := runner.ListDue(ctx, *plan.NextRunAt)
			require.NoError(t, err)
			require.Contains(t, func() []int64 {
				ids := []int64{}
				for _, p := range due {
					ids = append(ids, p.ID)
				}
				return ids
			}(), plan.ID)
			until := plan.NextRunAt.Add(time.Minute)
			claimed, err := runner.ClaimPelican(ctx, plan, *plan.NextRunAt, until, plan.NextRunAt.Add(time.Hour))
			require.NoError(t, err)
			require.True(t, claimed)
			require.NoError(t, runner.FinishPelican(ctx, plan.ID, until, *plan.NextRunAt))
			finished, err := runner.GetByID(ctx, plan.ID)
			require.NoError(t, err)
			require.Nil(t, finished.RunningUntil)
			require.NotNil(t, finished.LastRunAt)
			t.Cleanup(func() { require.NoError(t, native.DeletePlan(ctx, row.PlanID)) })
		}
		fresh := read().MonitoringRecipe
		require.Equal(t, "conflict", fresh.DistributionReview.State)
		require.False(t, fresh.ApplicationReview.CanConfirm)
		require.Len(t, fresh.DistributionReview.Conflicts, 2)
		require.Equal(t, 409, apply("admin", raw).Code, "replay must not add duplicate plans")
		for _, row := range saved.Data.Plans {
			require.NoError(t, native.DeletePlan(ctx, row.PlanID))
		}
	})
	t.Run("confirmation waits for the original budget lock and sees the committed preceding writer", func(t *testing.T) {
		raw := command()
		var cmd service.ProcurementRecipeCommand
		require.NoError(t, json.Unmarshal(raw, &cmd))
		lock, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = lock.Rollback() }()
		_, err = lock.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x42505350524f4245))
		require.NoError(t, err)
		pending := make(chan error, 1)
		go func() {
			_, err := repository.NewChannelRepository(db).(service.ProcurementRecipeRepository).ApplyProcurementRecipe(ctx, product, &cmd)
			pending <- err
		}()
		require.Eventually(t, func() bool {
			var n int
			err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock%'`).Scan(&n)
			return err == nil && n > 0
		}, time.Second, 10*time.Millisecond)
		changed := settings
		changed.RequestsPerDay = 100
		changed.Revision++
		encoded, err := json.Marshal(changed)
		require.NoError(t, err)
		_, err = lock.ExecContext(ctx, `UPDATE settings SET value=$1 WHERE key=$2`, string(encoded), service.MonitoringProbeBudgetSettingKey)
		require.NoError(t, err)
		require.NoError(t, lock.Commit())
		require.ErrorIs(t, <-pending, service.ErrProcurementRecipeConflict)
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_plans`).Scan(&n))
		require.Equal(t, 1, n)
		settings.Revision = changed.Revision
		settings, err = budget.SaveSettings(ctx, settings)
		require.NoError(t, err)
	})
	t.Run("final native coverage failure rolls back every inserted plan", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `CREATE FUNCTION recipe_final_coverage_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.cron_expression='* * * * *'; RETURN NEW; END $$; CREATE TRIGGER recipe_final_coverage_fault BEFORE INSERT ON scheduled_test_plans FOR EACH ROW EXECUTE FUNCTION recipe_final_coverage_fault()`)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := db.ExecContext(ctx, `DROP TRIGGER IF EXISTS recipe_final_coverage_fault ON scheduled_test_plans; DROP FUNCTION IF EXISTS recipe_final_coverage_fault()`)
			require.NoError(t, err)
		})
		require.Equal(t, 503, apply("admin", command()).Code)
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_plans`).Scan(&n))
		require.Equal(t, 1, n)
		_, err = db.ExecContext(ctx, `DROP TRIGGER recipe_final_coverage_fault ON scheduled_test_plans; DROP FUNCTION recipe_final_coverage_fault()`)
		require.NoError(t, err)
	})
	t.Run("source removal committed while waiting for an account lock withdraws the whole confirmation", func(t *testing.T) {
		var cmd service.ProcurementRecipeCommand
		require.NoError(t, json.Unmarshal(command(), &cmd))
		lock, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = lock.Rollback() }()
		_, err = lock.ExecContext(ctx, `SELECT id FROM accounts WHERE id=$1 FOR NO KEY UPDATE`, accounts[0])
		require.NoError(t, err)
		pending := make(chan error, 1)
		go func() {
			_, err := repository.NewChannelRepository(db).(service.ProcurementRecipeRepository).ApplyProcurementRecipe(ctx, product, &cmd)
			pending <- err
		}()
		require.Eventually(t, func() bool {
			var n int
			err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FROM accounts%FOR NO KEY UPDATE%'`).Scan(&n)
			return err == nil && n > 0
		}, time.Second, 10*time.Millisecond)
		_, err = lock.ExecContext(ctx, `DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, accounts[0], source)
		require.NoError(t, err)
		require.NoError(t, lock.Commit())
		require.ErrorIs(t, <-pending, service.ErrProcurementRecipeConflict)
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_plans`).Scan(&n))
		require.Equal(t, 1, n)
		_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, accounts[0], source)
		require.NoError(t, err)
	})
	t.Run("two products sharing an account cannot create a second observation plan", func(t *testing.T) {
		var other int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,rate_multiplier,procurement_policy) VALUES('Another recipe product','openai',.2,$1) RETURNING id`, policy).Scan(&other))
		repo := repository.NewChannelRepository(db)
		reviews := map[int64]*service.ProcurementRecipeCommand{}
		for _, id := range []int64{product, other} {
			out, err := repo.(service.ProcurementCatalogRepository).PreviewProcurementProductImpact(ctx, id, preset)
			require.NoError(t, err)
			r := out.MonitoringRecipe.ApplicationReview
			require.True(t, r.CanConfirm)
			reviews[id] = &service.ProcurementRecipeCommand{Version: 1, PresetID: preset, ObservedAt: r.ObservedAt, Fingerprint: r.Fingerprint}
		}
		pending := make(chan error, 2)
		for id, cmd := range reviews {
			go func() {
				_, err := repo.(service.ProcurementRecipeRepository).ApplyProcurementRecipe(ctx, id, cmd)
				pending <- err
			}()
		}
		var success, conflict int
		for range 2 {
			err := <-pending
			if err == nil {
				success++
			} else {
				require.ErrorIs(t, err, service.ErrProcurementRecipeConflict)
				conflict++
			}
		}
		require.Equal(t, 1, success)
		require.Equal(t, 1, conflict)
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_plans`).Scan(&n))
		require.Equal(t, 3, n)
		_, err := db.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE pelican_config#>>'{quality,action}'='observe_only'; DELETE FROM groups WHERE name='Another recipe product'`)
		require.NoError(t, err)
	})
	t.Run("changed preset source or budget invalidates confirmation before any insert", func(t *testing.T) {
		for _, kind := range []string{"preset", "source", "budget", "expired", "future", "forged"} {
			raw := command()
			switch kind {
			case "preset":
				_, err = db.ExecContext(ctx, `UPDATE quality_rule_presets SET max_results=101 WHERE id=$1`, preset)
			case "source":
				_, err = db.ExecContext(ctx, `UPDATE accounts SET name='Changed approved supply' WHERE id=$1`, accounts[0])
			case "budget":
				settings.RequestsPerDay = 1100
				settings, err = budget.SaveSettings(ctx, settings)
			default:
				var cmd service.ProcurementRecipeCommand
				require.NoError(t, json.Unmarshal(raw, &cmd))
				if kind == "expired" {
					cmd.ObservedAt = cmd.ObservedAt.Add(-3 * time.Minute)
				}
				if kind == "future" {
					cmd.ObservedAt = cmd.ObservedAt.Add(time.Minute)
				}
				if kind == "forged" {
					cmd.Fingerprint = string(bytes.Repeat([]byte("a"), 64))
				}
				raw, err = json.Marshal(cmd)
			}
			require.NoError(t, err)
			require.Equal(t, 409, apply("admin", raw).Code, kind)
			_, err = db.ExecContext(ctx, `UPDATE quality_rule_presets SET max_results=100 WHERE id=$1`, preset)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `UPDATE accounts SET name='Recipe supply 0' WHERE id=$1`, accounts[0])
			require.NoError(t, err)
			if kind == "budget" {
				settings.RequestsPerDay = 1000
				settings, err = budget.SaveSettings(ctx, settings)
				require.NoError(t, err)
			}
			var n int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_plans`).Scan(&n))
			require.Equal(t, 1, n)
		}
	})
	t.Run("a failure on the second native row rolls back the complete batch", func(t *testing.T) {
		_, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION recipe_fail_second() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.account_id=%d THEN RAISE EXCEPTION 'private-batch-fault'; END IF; RETURN NEW; END $$; CREATE TRIGGER recipe_fail_second BEFORE INSERT ON scheduled_test_plans FOR EACH ROW EXECUTE FUNCTION recipe_fail_second()`, accounts[1]))
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := db.ExecContext(ctx, `DROP TRIGGER IF EXISTS recipe_fail_second ON scheduled_test_plans; DROP FUNCTION IF EXISTS recipe_fail_second()`)
			require.NoError(t, err)
		})
		require.Equal(t, 503, apply("admin", command()).Code)
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_plans`).Scan(&n))
		require.Equal(t, 1, n)
		_, err = db.ExecContext(ctx, `DROP TRIGGER recipe_fail_second ON scheduled_test_plans; DROP FUNCTION recipe_fail_second()`)
		require.NoError(t, err)
	})
	t.Run("concurrent confirmations create a single complete set", func(t *testing.T) {
		raw := command()
		results := make(chan *httptest.ResponseRecorder, 2)
		for i := 0; i < 2; i++ {
			go func() { results <- apply("admin", raw) }()
		}
		statuses := map[int]int{}
		for i := 0; i < 2; i++ {
			w := <-results
			statuses[w.Code]++
			if w.Code == 200 {
				var saved struct {
					Data service.ProcurementRecipeApplication `json:"data"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &saved))
				for _, row := range saved.Data.Plans {
					t.Cleanup(func() { require.NoError(t, native.DeletePlan(ctx, row.PlanID)) })
				}
			}
		}
		require.Equal(t, map[int]int{200: 1, 409: 1}, statuses)
		_, err := db.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE pelican_config#>>'{quality,action}'='observe_only'`)
		require.NoError(t, err)
	})
	t.Run("a first distribution needs a short-lived input review distinct from budget", func(t *testing.T) {
		raw, err := json.Marshal(read().MonitoringRecipe)
		require.NoError(t, err)
		var projection struct {
			ApplicationReview *struct {
				CanConfirm  bool      `json:"can_confirm"`
				Fingerprint string    `json:"fingerprint"`
				ObservedAt  time.Time `json:"observed_at"`
				ExpiresAt   time.Time `json:"expires_at"`
			} `json:"application_review"`
		}
		require.NoError(t, json.Unmarshal(raw, &projection))
		require.NotNil(t, projection.ApplicationReview, "budget and uniqueness alone are not a write review")
		require.True(t, projection.ApplicationReview.CanConfirm)
		require.Len(t, projection.ApplicationReview.Fingerprint, 64)
		require.Equal(t, 2*time.Minute, projection.ApplicationReview.ExpiresAt.Sub(projection.ApplicationReview.ObservedAt))
	})
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
