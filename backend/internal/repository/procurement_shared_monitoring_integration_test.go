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
	"github.com/stretchr/testify/require"
)

func TestProcurementSharedMonitoringPersistencePostgresHTTP(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	var first, second, source, preset, otherPreset int64
	for name, id := range map[string]*int64{"Shared first": &first, "Shared second": &second, "Approved source": &source} {
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,rate_multiplier) VALUES($1,'openai',.2) RETURNING id`, name).Scan(id))
	}
	policy := fmt.Sprintf(`{"version":6,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"not_required","availability":"not_required","source_group_ids":[%d]}`, source)
	_, err := db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$1 WHERE id IN ($2,$3)`, policy, first, second)
	require.NoError(t, err)
	accounts := []int64{}
	for i := 0; i < 2; i++ {
		var id int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,schedulable,credentials,extra) VALUES($1,'openai','apikey','active',false,'{"api_key":"shared-private-fixture-key"}','{}') RETURNING id`, fmt.Sprintf("Shared supply %d", i)).Scan(&id))
		accounts = append(accounts, id)
		_, err := db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, id, source)
		require.NoError(t, err)
	}
	cfg := `{"question_kind":"candy","prompt":"shared-private-prompt","parallel_count":1,"reasoning_effort":"medium","quality":{"action":"observe_only","expected_answer":"shared-private-answer"}}`
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO quality_rule_presets(name,model_id,cron_expression,enabled,max_results,pelican_config) VALUES('Shared A','model-a','0 * * * *',true,100,$1) RETURNING id`, cfg).Scan(&preset))
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO quality_rule_presets(name,model_id,cron_expression,enabled,max_results,pelican_config) VALUES('Shared B','model-b','*/30 * * * *',true,100,$1) RETURNING id`, cfg).Scan(&otherPreset))
	budget := repository.NewMonitoringProbeBudgetRepository(db)
	settings := service.DefaultMonitoringProbeBudgetSettings()
	settings.Enabled, settings.RequestsPerDay, settings.RecoveryReservePercent = true, 10000, 0
	settings, err = budget.SaveSettings(ctx, settings)
	require.NoError(t, err)
	repo := repository.NewChannelRepository(db).(service.ProcurementSharedMonitoringRepository)
	native := repository.NewScheduledTestPlanRepository(db)
	users := repository.NewUserRepository(client, db)
	auth := service.NewAuthService(client, users, nil, nil, &config.Config{JWT: config.JWTConfig{Secret: "shared-local-fixture", ExpireHour: 1}}, nil, nil, nil, nil, nil, nil, nil, nil)
	tokens := map[string]string{}
	for _, role := range []string{"admin", "user", "observer"} {
		var id int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,role) VALUES($1,'fixture',$2) RETURNING id`, "shared-"+role+"@test.invalid", role).Scan(&id))
		u, err := users.GetByID(ctx, id)
		require.NoError(t, err)
		tokens[role], err = auth.GenerateToken(ctx, u)
		require.NoError(t, err)
	}
	h := admin.NewChannelHandler(service.NewChannelService(repository.NewChannelRepository(db), nil, nil, nil, nil), nil, nil)
	router := gin.New()
	routes := router.Group("/api/v1/admin")
	routes.Use(gin.HandlerFunc(middleware.NewAdminAuthMiddleware(auth, service.NewUserService(users, nil, nil, nil), nil, nil)))
	routes.Use((&admin.AccountHandler{}).AuthorizeObserver)
	routes.POST("/upstream-procurement/monitoring-preview", h.ProcurementSharedMonitoringPreview)
	routes.GET("/upstream-procurement/monitoring", h.ProcurementSharedMonitoring)
	routes.POST("/upstream-procurement/monitoring", h.ProcurementSharedMonitoring)
	call := func(role, method, path string, raw []byte) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/v1/admin/upstream-procurement/"+path, bytes.NewReader(raw))
		if tokens[role] != "" {
			req.Header.Set("Authorization", "Bearer "+tokens[role])
		}
		router.ServeHTTP(w, req)
		if role == "admin" {
			require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		}
		for _, secret := range []string{"shared-private-", "password_hash", "pq:", "definition"} {
			require.NotContains(t, w.Body.String(), secret)
		}
		return w
	}
	read := func() *service.ProcurementSharedMonitoringState {
		t.Helper()
		w := call("admin", http.MethodGet, "monitoring", nil)
		require.Equal(t, 200, w.Code, w.Body.String())
		var r struct {
			Data service.ProcurementSharedMonitoringState `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &r))
		return &r.Data
	}
	snapshot := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, table := range []string{"settings", "accounts", "groups", "account_groups", "quality_rule_presets", "scheduled_test_plans", "scheduled_test_results", "monitoring_probe_budget_counters", "monitoring_probe_budget_leases", "scheduler_outbox", "usage_logs"} {
			var v string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT md5(COALESCE(string_agg(to_jsonb(t)::text,E'\n' ORDER BY to_jsonb(t)::text),'')) FROM `+table+` t`).Scan(&v))
			out[table] = v
		}
		return out
	}
	command := service.ProcurementSharedMonitoringCommand{Version: 2, Products: []service.ProcurementMonitoringProductSelection{{ProductID: first, PresetID: preset}, {ProductID: second, PresetID: otherPreset}}}
	preview := func(c service.ProcurementSharedMonitoringCommand) *service.ProcurementSharedMonitoringPreview {
		t.Helper()
		before := snapshot()
		raw, err := json.Marshal(c)
		require.NoError(t, err)
		w := call("admin", http.MethodPost, "monitoring-preview", raw)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Equal(t, before, snapshot(), "preview must not write any table")
		var r struct {
			Data service.ProcurementSharedMonitoringPreview `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &r))
		require.Equal(t, 2, r.Data.Version)
		require.NotNil(t, r.Data.ApplicationReview)
		return &r.Data
	}
	confirmation := func(c service.ProcurementSharedMonitoringCommand, p *service.ProcurementSharedMonitoringPreview) service.ProcurementSharedMonitoringApplyCommand {
		return service.ProcurementSharedMonitoringApplyCommand{ProcurementSharedMonitoringCommand: c, Revision: p.ApplicationReview.Revision, ObservedAt: p.ApplicationReview.ObservedAt, Fingerprint: p.ApplicationReview.Fingerprint, Confirm: true}
	}
	apply := func(c service.ProcurementSharedMonitoringApplyCommand, status int) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(c)
		require.NoError(t, err)
		w := call("admin", http.MethodPost, "monitoring", raw)
		require.Equal(t, status, w.Code, w.Body.String())
		return w
	}
	state := read()
	require.Zero(t, state.Revision)
	require.Empty(t, state.Products)
	require.Empty(t, state.Plans)
	p := preview(command)
	require.True(t, p.ApplicationReview.CanConfirm)
	require.Len(t, p.ApplicationReview.Changes, 2)
	require.Equal(t, 2, p.SharedAccounts)
	for _, role := range []string{"", "user", "observer"} {
		status := 403
		if role == "" {
			status = 401
		}
		raw, err := json.Marshal(confirmation(command, p))
		require.NoError(t, err)
		for _, path := range []string{"monitoring", "monitoring-preview"} {
			w := call(role, http.MethodPost, path, raw)
			require.Equal(t, status, w.Code)
		}
		require.Equal(t, status, call(role, http.MethodGet, "monitoring", nil).Code)
	}
	for _, bad := range []string{`null`, `{}`, `{"version":2,"products":[],"confirm":true}`, strings.Repeat(" ", 8193)} {
		require.Equal(t, 400, call("admin", http.MethodPost, "monitoring", []byte(bad)).Code)
	}
	apply(confirmation(command, p), 200)
	state = read()
	require.EqualValues(t, 1, state.Revision)
	require.Len(t, state.Products, 2)
	require.Len(t, state.Plans, 2)
	ids := []int64{state.Plans[0].PlanID, state.Plans[1].PlanID}
	for _, row := range state.Plans {
		require.Equal(t, "managed", row.State)
		require.True(t, row.Enabled)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO scheduled_test_results(plan_id,status,started_at,finished_at,created_at) VALUES($1,'success',clock_timestamp(),clock_timestamp(),clock_timestamp())`, ids[0])
	require.NoError(t, err)

	t.Run("repeat compilation replaces existing demand and native manual edits are guarded", func(t *testing.T) {
		p := preview(command)
		require.True(t, p.ApplicationReview.CanConfirm)
		require.Zero(t, *p.DeltaMinimumRequestsPerDay)
		for _, c := range p.ApplicationReview.Changes {
			require.Equal(t, "retain", c.Operation)
		}
		before := snapshot()
		plan, err := native.GetByID(ctx, ids[0])
		require.NoError(t, err)
		_, err = native.Update(ctx, plan)
		require.ErrorIs(t, err, service.ErrProcurementSharedMonitoringOwned)
		require.ErrorIs(t, native.Delete(ctx, ids[0]), service.ErrProcurementSharedMonitoringOwned)
		_, err = native.(service.QualityOwnershipRepository).QualityOwnership(ctx, ids[0], &service.QualityOwnershipRequest{Delete: true, Confirm: true})
		require.ErrorIs(t, err, service.ErrProcurementSharedMonitoringOwned)
		require.Equal(t, before, snapshot())
	})
	t.Run("scoped native writers authorize before revealing shared ownership", func(t *testing.T) {
		before := snapshot()
		plan, err := native.GetByID(ctx, ids[0])
		require.NoError(t, err)
		outside := service.WithObserverScope(ctx, []int64{})
		_, err = native.Update(outside, plan)
		require.ErrorIs(t, err, service.ErrObserverScope)
		require.ErrorIs(t, native.Delete(outside, plan.ID), service.ErrObserverScope)
		inside := service.WithObserverScope(ctx, []int64{source})
		_, err = native.Update(inside, plan)
		require.ErrorIs(t, err, service.ErrProcurementSharedMonitoringOwned)
		require.ErrorIs(t, native.Delete(inside, plan.ID), service.ErrProcurementSharedMonitoringOwned)
		require.Equal(t, before, snapshot())
	})
	t.Run("configuration update preserves plan IDs and history", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `UPDATE quality_rule_presets SET cron_expression='*/15 * * * *',updated_at=clock_timestamp() WHERE id=$1`, otherPreset)
		require.NoError(t, err)
		p := preview(command)
		require.True(t, p.ApplicationReview.CanConfirm)
		for _, c := range p.ApplicationReview.Changes {
			require.Equal(t, "update", c.Operation)
		}
		apply(confirmation(command, p), 200)
		s := read()
		require.EqualValues(t, 2, s.Revision)
		require.Equal(t, ids[0], s.Plans[0].PlanID)
		require.Equal(t, ids[1], s.Plans[1].PlanID)
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_results WHERE plan_id=$1`, ids[0]).Scan(&n))
		require.Equal(t, 1, n)
	})
	t.Run("stale and expired consent and original budget changes reject the entire edit", func(t *testing.T) {
		p := preview(command)
		old := confirmation(command, p)
		before := snapshot()
		bad := old
		bad.Fingerprint = strings.Repeat("0", 64)
		apply(bad, 409)
		bad = old
		bad.ObservedAt = bad.ObservedAt.Add(-3 * time.Minute)
		apply(bad, 409)
		bad = old
		bad.ObservedAt = bad.ObservedAt.Add(time.Minute)
		apply(bad, 409)
		require.Equal(t, before, snapshot())
		settings.RequestsPerDay = 400
		settings, err = budget.SaveSettings(ctx, settings)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `UPDATE quality_rule_presets SET cron_expression='*/5 * * * *',updated_at=clock_timestamp() WHERE id=$1`, otherPreset)
		require.NoError(t, err)
		before = snapshot()
		apply(old, 409)
		p = preview(command)
		require.False(t, p.ApplicationReview.CanConfirm)
		apply(confirmation(command, p), 409)
		require.Equal(t, before, snapshot())
		settings.RequestsPerDay = 10000
		settings, err = budget.SaveSettings(ctx, settings)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `UPDATE quality_rule_presets SET cron_expression='*/15 * * * *',updated_at=clock_timestamp() WHERE id=$1`, otherPreset)
		require.NoError(t, err)
	})
	t.Run("unknown business fields invalidate ownership but native round markers do not", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `UPDATE scheduled_test_plans SET pelican_config=pelican_config||'{"unrecognized_business":true}'::jsonb WHERE id=$1`, ids[0])
		require.NoError(t, err)
		p := preview(command)
		require.False(t, p.ApplicationReview.CanConfirm)
		require.Equal(t, "managed_plan_changed", p.ApplicationReview.Changes[0].Reason)
		before := snapshot()
		apply(confirmation(command, p), 409)
		require.Equal(t, before, snapshot())
		_, err = db.ExecContext(ctx, `UPDATE scheduled_test_plans SET pelican_config=pelican_config-'unrecognized_business',next_run_at=clock_timestamp()-interval '1 second' WHERE id=$1`, ids[0])
		require.NoError(t, err)
		plan, err := native.GetByID(ctx, ids[0])
		require.NoError(t, err)
		now := time.Now().UTC()
		until := now.Add(time.Minute)
		claimed, err := native.ClaimPelican(ctx, plan, now, until, now.Add(time.Hour))
		require.NoError(t, err)
		require.True(t, claimed)
		p = preview(command)
		require.False(t, p.ApplicationReview.CanConfirm)
		require.Equal(t, "managed_plan_running", p.ApplicationReview.Changes[0].Reason)
		require.NoError(t, native.FinishPelican(ctx, ids[0], until, now.Add(time.Second)))
		p = preview(command)
		require.True(t, p.ApplicationReview.CanConfirm)
		for _, c := range p.ApplicationReview.Changes {
			require.Equal(t, "retain", c.Operation)
		}
	})
	t.Run("waiting for native budget lock reads the newly committed demand", func(t *testing.T) {
		p := preview(command)
		c := confirmation(command, p)
		lock, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = lock.Rollback() }()
		_, err = lock.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x42505350524f4245))
		require.NoError(t, err)
		pending := make(chan error, 1)
		go func() { _, err := repo.ApplyProcurementSharedMonitoring(ctx, &c); pending <- err }()
		require.Eventually(t, func() bool {
			var n int
			err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock%'`).Scan(&n)
			return err == nil && n > 0
		}, 5*time.Second, 20*time.Millisecond)
		_, err = lock.ExecContext(ctx, `UPDATE quality_rule_presets SET updated_at=clock_timestamp(),name='Changed while waiting' WHERE id=$1`, preset)
		require.NoError(t, err)
		require.NoError(t, lock.Commit())
		require.ErrorIs(t, <-pending, service.ErrProcurementSharedMonitoringConflict)
	})
	t.Run("two concurrent administrators commit only one revision", func(t *testing.T) {
		p := preview(command)
		c := confirmation(command, p)
		expected := read().Revision
		start := make(chan struct{})
		results := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() { <-start; _, err := repo.ApplyProcurementSharedMonitoring(ctx, &c); results <- err }()
		}
		close(start)
		success, conflicts := 0, 0
		for i := 0; i < 2; i++ {
			err := <-results
			if err == nil {
				success++
			} else {
				require.ErrorIs(t, err, service.ErrProcurementSharedMonitoringConflict)
				conflicts++
			}
		}
		require.Equal(t, 1, success)
		require.Equal(t, 1, conflicts)
		require.Equal(t, expected+1, read().Revision)
	})
	t.Run("source revoked while waiting for original plan lock rejects all changes", func(t *testing.T) {
		p := preview(command)
		c := confirmation(command, p)
		lock, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = lock.Rollback() }()
		var id int64
		require.NoError(t, lock.QueryRowContext(ctx, `SELECT id FROM scheduled_test_plans WHERE id=$1 FOR UPDATE`, ids[0]).Scan(&id))
		pending := make(chan error, 1)
		go func() { _, err := repo.ApplyProcurementSharedMonitoring(ctx, &c); pending <- err }()
		require.Eventually(t, func() bool {
			var n int
			err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%SELECT id FROM scheduled_test_plans%'`).Scan(&n)
			return err == nil && n > 0
		}, 5*time.Second, 20*time.Millisecond)
		_, err = lock.ExecContext(ctx, `DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, accounts[0], source)
		require.NoError(t, err)
		require.NoError(t, lock.Commit())
		before := snapshot()
		require.ErrorIs(t, <-pending, service.ErrProcurementSharedMonitoringConflict)
		require.Equal(t, before, snapshot())
		_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, accounts[0], source)
		require.NoError(t, err)
	})
	t.Run("final original budget validation rolls back plan and configuration writes", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `UPDATE quality_rule_presets SET cron_expression='*/12 * * * *',updated_at=clock_timestamp() WHERE id=$1`, otherPreset)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `CREATE FUNCTION shared_budget_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE settings SET value=jsonb_set(value::jsonb,'{requests_per_day}','1')::text WHERE key='monitoring_probe_budget'; RETURN NEW; END $$; CREATE TRIGGER shared_budget_fault AFTER UPDATE ON scheduled_test_plans FOR EACH ROW EXECUTE FUNCTION shared_budget_fault()`)
		require.NoError(t, err)
		p := preview(command)
		require.True(t, p.ApplicationReview.CanConfirm)
		before := snapshot()
		apply(confirmation(command, p), 503)
		require.Equal(t, before, snapshot())
		_, err = db.ExecContext(ctx, `DROP TRIGGER shared_budget_fault ON scheduled_test_plans; DROP FUNCTION shared_budget_fault()`)
		require.NoError(t, err)
		apply(confirmation(command, preview(command)), 200)
	})
	t.Run("second native row failure rolls back both rows and participation", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `UPDATE quality_rule_presets SET cron_expression='*/10 * * * *',updated_at=clock_timestamp() WHERE id=$1`, otherPreset)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION shared_plan_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id=%d THEN RAISE EXCEPTION 'shared fixture fault'; END IF; RETURN NEW; END $$`, ids[1]))
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `CREATE TRIGGER shared_plan_fault BEFORE UPDATE ON scheduled_test_plans FOR EACH ROW EXECUTE FUNCTION shared_plan_fault()`)
		require.NoError(t, err)
		p := preview(command)
		before := snapshot()
		apply(confirmation(command, p), 503)
		require.Equal(t, before, snapshot())
		_, err = db.ExecContext(ctx, `DROP TRIGGER shared_plan_fault ON scheduled_test_plans; DROP FUNCTION shared_plan_fault()`)
		require.NoError(t, err)
		apply(confirmation(command, preview(command)), 200)
	})
	t.Run("pause orphan requirements, reuse their IDs, then explicitly release all to manual", func(t *testing.T) {
		empty := service.ProcurementSharedMonitoringCommand{Version: 2, Products: []service.ProcurementMonitoringProductSelection{}}
		settings.Enabled = false
		settings, err = budget.SaveSettings(ctx, settings)
		require.NoError(t, err)
		p := preview(empty)
		require.True(t, p.ApplicationReview.CanConfirm)
		require.Negative(t, *p.DeltaMinimumRequestsPerDay)
		apply(confirmation(empty, p), 200)
		s := read()
		require.Empty(t, s.Products)
		require.Len(t, s.Plans, 2)
		for _, row := range s.Plans {
			require.Equal(t, "paused", row.State)
			require.False(t, row.Enabled)
		}
		settings.Enabled = true
		settings, err = budget.SaveSettings(ctx, settings)
		require.NoError(t, err)
		apply(confirmation(command, preview(command)), 200)
		s = read()
		require.Equal(t, ids[0], s.Plans[0].PlanID)
		require.Equal(t, ids[1], s.Plans[1].PlanID)
		empty.ReleaseOwnership = true
		apply(confirmation(empty, preview(empty)), 200)
		s = read()
		require.Empty(t, s.Products)
		require.Empty(t, s.Plans)
		for _, id := range ids {
			p, err := native.GetByID(ctx, id)
			require.NoError(t, err)
			require.False(t, p.Enabled)
		}
		p = preview(command)
		require.False(t, p.ApplicationReview.CanConfirm)
		require.Equal(t, "existing_observation_plan", p.Accounts[0].Reason)
		plan, err := native.GetByID(ctx, ids[1])
		require.NoError(t, err)
		_, err = native.Update(ctx, plan)
		require.NoError(t, err)
		require.NoError(t, native.Delete(ctx, ids[1]))
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_results WHERE plan_id=$1`, ids[0]).Scan(&n))
		require.Equal(t, 1, n)
	})
	t.Run("missing managed plan blocks without erasing history or adopting another plan", func(t *testing.T) {
		var raw string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='upstream_procurement_shared_observation'`).Scan(&raw))
		defer func() {
			_, restore := db.ExecContext(ctx, `UPDATE settings SET value=$1 WHERE key='upstream_procurement_shared_observation'`, raw)
			require.NoError(t, restore)
		}()
		_, err := db.ExecContext(ctx, `UPDATE settings SET value=jsonb_set(value::jsonb,'{plans}',$1)::text WHERE key='upstream_procurement_shared_observation'`, fmt.Sprintf(`[{"account_id":%d,"plan_id":99999999,"definition":"%s"}]`, accounts[0], strings.Repeat("a", 64)))
		require.NoError(t, err)
		s := read()
		require.Equal(t, "managed_plan_missing", s.Plans[0].State)
		require.False(t, s.Plans[0].Enabled)
		empty := service.ProcurementSharedMonitoringCommand{Version: 2, Products: []service.ProcurementMonitoringProductSelection{}}
		p := preview(empty)
		require.False(t, p.ApplicationReview.CanConfirm)
		require.Equal(t, "managed_plan_missing", p.ApplicationReview.Changes[0].Reason)
		before := snapshot()
		apply(confirmation(empty, p), 409)
		require.Equal(t, before, snapshot())
	})
	t.Run("saved and newly approved accounts share one bounded batch limit", func(t *testing.T) {
		var raw string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='upstream_procurement_shared_observation'`).Scan(&raw))
		defer func() {
			_, restore := db.ExecContext(ctx, `UPDATE settings SET value=$1 WHERE key='upstream_procurement_shared_observation'`, raw)
			require.NoError(t, restore)
		}()
		owners := []map[string]any{}
		for i := 0; i < service.ProcurementProductImpactAccountLimit; i++ {
			owners = append(owners, map[string]any{"account_id": 1000000 + i, "plan_id": 2000000 + i, "definition": strings.Repeat("a", 64)})
		}
		encoded, err := json.Marshal(owners)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `UPDATE settings SET value=jsonb_set(value::jsonb,'{plans}',$1)::text WHERE key='upstream_procurement_shared_observation'`, string(encoded))
		require.NoError(t, err)
		before := snapshot()
		_, err = repo.PreviewProcurementSharedMonitoring(ctx, &command)
		require.ErrorIs(t, err, service.ErrProcurementProductImpactTooLarge)
		require.Equal(t, before, snapshot())
	})
	t.Run("corrupt authority fails closed and recovery preserves manual data", func(t *testing.T) {
		var raw string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='upstream_procurement_shared_observation'`).Scan(&raw))
		for _, bad := range []string{`{"version":1,"version":1,"revision":0,"products":[],"plans":[]}`, `{"version":1,"revision":null,"products":[],"plans":[]}`, `{"version":1,"revision":1,"products":[],"plans":[],"unexpected":true}`, raw + `{}`} {
			_, err := db.ExecContext(ctx, `UPDATE settings SET value=$1 WHERE key='upstream_procurement_shared_observation'`, bad)
			require.NoError(t, err)
			require.Equal(t, 503, call("admin", http.MethodGet, "monitoring", nil).Code)
			p, err := native.GetByID(ctx, ids[0])
			require.NoError(t, err)
			_, err = native.Update(ctx, p)
			require.ErrorIs(t, err, service.ErrProcurementSharedMonitoringUnavailable)
		}
		_, err := db.ExecContext(ctx, `UPDATE settings SET value=$1 WHERE key='upstream_procurement_shared_observation'`, raw)
		require.NoError(t, err)
		require.Empty(t, read().Plans)
	})
	for _, table := range []string{"monitoring_probe_budget_counters", "monitoring_probe_budget_leases", "usage_logs", "scheduler_outbox"} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n))
		require.Zero(t, n, table)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = repo.ReadProcurementSharedMonitoring(canceled)
	require.ErrorIs(t, err, context.Canceled)
}
