//go:build integration

package repository_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestProcurementProductLegacyWritersPostgres(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	repo := repository.NewChannelRepository(db).(service.ProcurementCatalogRepository)
	var productID, sourceID, otherID int64
	for i, id := range []*int64{&productID, &sourceID, &otherID} {
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform) VALUES($1,'openai') RETURNING id`, fmt.Sprintf("writer product %d", i)).Scan(id))
	}
	create := func(name string, target int64) int64 {
		t.Helper()
		var id int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,credentials,extra)
 VALUES($1,'openai','oauth','{"api_key":"writer-synthetic-secret"}',jsonb_build_object('openai_excel_bps',true,'openai_excel_bps_auto_move_on_403',true,'openai_excel_bps_403_target_group_id',$2::bigint)) RETURNING id`, name, target).Scan(&id))
		return id
	}
	current, unbound, outside, unrelated := create("Current writer", otherID), create("Unbound approved writer", 0), create("External target writer", productID), create("Unrelated writer", otherID)
	_, err := db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2),($1,$3),($4,$3)`, current, productID, sourceID, unbound)
	require.NoError(t, err)
	policy := fmt.Sprintf(`{"version":6,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"not_required","availability":"not_required","source_group_ids":[%d]}`, sourceID)
	_, err = db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$2::jsonb WHERE id=$1`, productID, policy)
	require.NoError(t, err)
	read := func() *service.ProcurementProductImpact {
		t.Helper()
		out, err := repo.PreviewProcurementProductImpact(ctx, productID)
		require.NoError(t, err)
		require.NotNil(t, out.ManagementReview.WriterReview)
		encoded, err := json.Marshal(out)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), "writer-synthetic-secret")
		return out
	}
	find := func(out *service.ProcurementProductImpact, kind string, id int64) service.ProcurementWriter {
		t.Helper()
		for _, row := range out.ManagementReview.WriterReview.Writers {
			if row.Kind == kind && row.AccountID != nil && *row.AccountID == id {
				return row
			}
		}
		t.Fatalf("missing writer %s for account %d", kind, id)
		return service.ProcurementWriter{}
	}
	saveSetting := func(key string, value any) {
		t.Helper()
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, key, string(raw))
		require.NoError(t, err)
	}
	t.Run("current, approved-unbound and external targets stay distinct", func(t *testing.T) {
		out := read()
		review := out.ManagementReview.WriterReview
		require.Equal(t, 3, review.AccountsExamined)
		require.Equal(t, 3, review.MembershipConflicts)
		require.Zero(t, review.Unknown)
		require.True(t, review.InventoryComplete)
		require.Len(t, out.ManagementReview.SourceInventory.Accounts, 2)
		require.Equal(t, 2, *out.ManagementReview.SourceInventory.ApprovedAccounts)
		require.Equal(t, 1, out.BoundAccounts)
		require.True(t, find(out, "excel_bps_403", unbound).WritesProduct)
		require.Equal(t, "External target writer", find(out, "excel_bps_403", outside).AccountName)
		for _, row := range review.Writers {
			require.NotEqual(t, unrelated, *row.AccountID)
		}
	})
	t.Run("reported 403 writer really mutates memberships through the existing executor", func(t *testing.T) {
		accounts := repository.NewAdminAccountRepository(client, db, nil)
		account, err := accounts.GetByID(ctx, current)
		require.NoError(t, err)
		require.True(t, find(read(), "excel_bps_403", current).WritesProduct)
		changed, err := accounts.(service.AccountExcelBPSGroupRepository).MoveExcelBPSOn403(ctx, account)
		require.NoError(t, err)
		require.True(t, changed)
		var groups string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_agg(group_id ORDER BY group_id)::text FROM account_groups WHERE account_id=$1`, current).Scan(&groups))
		require.JSONEq(t, fmt.Sprintf("[%d]", otherID), groups)
		require.Zero(t, read().BoundAccounts)
		// Restore only this fixture's memberships; the production executor and
		// its audit/outbox are not modified by this read-only feature.
		_, err = db.ExecContext(ctx, `DELETE FROM account_groups WHERE account_id=$1`, current)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2),($1,$3)`, current, productID, sourceID)
		require.NoError(t, err)
		require.Equal(t, 1, read().BoundAccounts)
	})
	t.Run("global controls, saved ownership and stopped switches", func(t *testing.T) {
		c := service.DefaultOAuthAutoConfig()
		threshold := 50.0
		c.DegradationAction.Enabled, c.DegradationAction.ThresholdPercent, c.DegradationAction.LimitGroups, c.DegradationAction.GroupIDs = true, &threshold, true, []int64{productID}
		saveSetting(service.SettingKeyOAuthAutoConfig, c)
		out := read()
		require.Equal(t, "membership_conflict", find(out, "oauth_degradation", current).Risk)
		c.DegradationAction.GroupIDs = []int64{otherID}
		saveSetting(service.SettingKeyOAuthAutoConfig, c)
		require.Equal(t, "account_protection", find(read(), "oauth_degradation", unbound).Risk)
		a := config.AstraRoutingSettings{AccountScheduling: true, SchedulingMode: "groups", SchedulingGroupIDs: []int64{productID}, CookiePool: config.CodexGatewayPinConfig{TargetAccountIDs: []int64{unrelated}}}
		require.NoError(t, a.Validate())
		saveSetting("astra_routing_experiment_v1", a)
		out = read()
		require.Equal(t, 4, out.ManagementReview.WriterReview.AccountsExamined)
		require.True(t, find(out, "astra_scheduling", unrelated).WritesProduct)
		_, err = db.ExecContext(ctx, `INSERT INTO astra_scheduling_states(account_id,state) VALUES($1,jsonb_build_object('mode','groups','group_ids',jsonb_build_array($2::bigint)))`, outside, productID)
		require.NoError(t, err)
		a.AccountScheduling = false
		saveSetting("astra_routing_experiment_v1", a)
		row := find(read(), "astra_scheduling", outside)
		require.Equal(t, "membership_conflict", row.Risk)
		require.True(t, row.RetainedOwnership)
		require.False(t, *row.Enabled)
		_, err = db.ExecContext(ctx, `UPDATE astra_scheduling_states SET state=jsonb_set(state,'{mode}','"model"') WHERE account_id=$1`, outside)
		require.NoError(t, err)
		row = find(read(), "astra_scheduling", outside)
		require.Equal(t, "account_protection", row.Risk, "a retained selection in model mode is not membership ownership")
		require.True(t, row.RetainedOwnership)
		require.False(t, row.WritesProduct)
		_, err = db.ExecContext(ctx, `UPDATE accounts SET extra=extra||'{"oauth_degradation_action":{"revision":"old","native_controls":true}}'::jsonb WHERE id=$1`, unbound)
		require.NoError(t, err)
		row = find(read(), "oauth_degradation", unbound)
		require.Equal(t, "historical_only", row.Risk)
		require.False(t, row.RetainedOwnership)
		_, err = db.ExecContext(ctx, `UPDATE accounts SET extra=extra-'oauth_degradation_action' WHERE id=$1`, unbound)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `DELETE FROM settings WHERE key IN ($1,'astra_routing_experiment_v1')`, service.SettingKeyOAuthAutoConfig)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `DELETE FROM astra_scheduling_states`)
		require.NoError(t, err)
	})
	t.Run("2FA enrollment matches the saved plan, not global defaults", func(t *testing.T) {
		p := service.DefaultOpenAITwoFAAdmissionConfig()
		p.Enabled, p.IsolationGroupID = true, productID
		plan, err := json.Marshal(&service.PelicanTestConfig{Quality: &service.QualityPolicy{Action: service.QualityActionOpenAITwoFAAdmission, Admission: &p}})
		require.NoError(t, err)
		var planID int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,pelican_config) VALUES($1,'gpt-6.1-sol','* * * * *',$2::jsonb) RETURNING id`, unrelated, string(plan)).Scan(&planID))
		state := service.OpenAITwoFAAdmissionState{PlanID: planID, Policy: p, Phase: "pending"}
		raw, err := json.Marshal(state)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{openai_two_fa_admission}',$2::jsonb) WHERE id=$1`, unrelated, string(raw))
		require.NoError(t, err)
		row := find(read(), "two_fa_admission", unrelated)
		require.True(t, row.WritesProduct)
		_, err = db.ExecContext(ctx, `UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, planID)
		require.NoError(t, err)
		row = find(read(), "two_fa_admission", unrelated)
		require.Equal(t, "account_protection", row.Risk)
		require.False(t, *row.Enabled)
		_, err = db.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{openai_two_fa_admission,phase}','"isolated"') WHERE id=$1`, unrelated)
		require.NoError(t, err)
		require.Equal(t, "historical_only", find(read(), "two_fa_admission", unrelated).Risk)
		_, err = db.ExecContext(ctx, `UPDATE scheduled_test_plans SET pelican_config='{}' WHERE id=$1`, planID)
		require.NoError(t, err)
		require.Equal(t, "unknown", find(read(), "two_fa_admission", unrelated).Risk)
		_, err = db.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE id=$1`, planID)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `UPDATE accounts SET extra=extra-'openai_two_fa_admission' WHERE id=$1`, unrelated)
		require.NoError(t, err)
	})
	t.Run("unknown settings and saved state do not become safe", func(t *testing.T) {
		for _, key := range []string{service.SettingKeyOAuthAutoConfig, "astra_routing_experiment_v1"} {
			saveSetting(key, map[string]any{"future_secret": "writer-synthetic-secret"})
		}
		out := read()
		require.Equal(t, 2, out.ManagementReview.WriterReview.Unknown)
		require.False(t, out.ManagementReview.WriterReview.InventoryComplete)
		_, err = db.ExecContext(ctx, `INSERT INTO astra_scheduling_states(account_id,state) VALUES($1,'{"mode":"future"}')`, current)
		require.NoError(t, err)
		require.Equal(t, "unknown", find(read(), "astra_scheduling", current).Risk)
		_, err = db.ExecContext(ctx, `DELETE FROM settings WHERE key IN ($1,'astra_routing_experiment_v1')`, service.SettingKeyOAuthAutoConfig)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `DELETE FROM astra_scheduling_states`)
		require.NoError(t, err)
		require.Zero(t, read().ManagementReview.WriterReview.Unknown)
	})
	t.Run("same snapshot while configuration changes, cancellation and recovery", func(t *testing.T) {
		lock, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = lock.Rollback() }()
		_, err = lock.ExecContext(ctx, `LOCK TABLE astra_scheduling_states IN ACCESS EXCLUSIVE MODE`)
		require.NoError(t, err)
		type result struct {
			out *service.ProcurementProductImpact
			err error
		}
		pending := make(chan result, 1)
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		go func() {
			out, err := repo.PreviewProcurementProductImpact(readCtx, productID)
			pending <- result{out, err}
		}()
		require.Eventually(t, func() bool {
			var n int
			err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%astra_scheduling_states%'`).Scan(&n)
			return err == nil && n > 0
		}, 5*time.Second, 20*time.Millisecond)
		_, err = db.ExecContext(ctx, `UPDATE accounts SET extra=extra||'{"openai_excel_bps_auto_move_on_403":false}'::jsonb WHERE id=$1`, current)
		require.NoError(t, err)
		require.NoError(t, lock.Commit())
		out := <-pending
		require.NoError(t, out.err)
		require.True(t, find(out.out, "excel_bps_403", current).WritesProduct, "one repeatable-read snapshot, not mixed account config")
		require.Equal(t, 2, read().ManagementReview.WriterReview.MembershipConflicts)
		canceled, stop := context.WithCancel(ctx)
		stop()
		_, err = repo.PreviewProcurementProductImpact(canceled, productID)
		require.Error(t, err)
		_, err = db.ExecContext(ctx, `UPDATE accounts SET extra=extra||'{"openai_excel_bps_auto_move_on_403":true}'::jsonb WHERE id=$1`, current)
		require.NoError(t, err)
		require.Equal(t, 3, read().ManagementReview.WriterReview.MembershipConflicts)
	})
	t.Run("preview preserves bindings, controls, state, plans, usage and outbox", func(t *testing.T) {
		capture := func() string {
			t.Helper()
			var value string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_build_object(
 'groups',(SELECT jsonb_agg(to_jsonb(g) ORDER BY id) FROM groups g),
 'accounts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM accounts a),
 'bindings',(SELECT jsonb_agg(to_jsonb(b) ORDER BY account_id,group_id) FROM account_groups b),
 'settings',(SELECT jsonb_agg(to_jsonb(s) ORDER BY key) FROM settings s),
 'astra',(SELECT jsonb_agg(to_jsonb(s) ORDER BY account_id) FROM astra_scheduling_states s),
 'plans',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM scheduled_test_plans p),
 'outbox',(SELECT count(*) FROM scheduler_outbox), 'usage',(SELECT count(*) FROM usage_logs))::text`).Scan(&value))
			return value
		}
		before := capture()
		_ = read()
		require.Equal(t, before, capture())
	})
	t.Run("exact account boundary and overflow never truncate as success", func(t *testing.T) {
		known := read().ManagementReview.WriterReview.AccountsExamined
		_, err = db.ExecContext(ctx, `INSERT INTO accounts(name,platform,type,credentials,extra)
 SELECT 'bounded external writer','openai','apikey','{}',jsonb_build_object('openai_excel_bps_403_target_group_id',$1::bigint) FROM generate_series(1,$2::int)`, productID, 1000-known)
		require.NoError(t, err)
		require.Equal(t, 1000, read().ManagementReview.WriterReview.AccountsExamined)
		_ = create("over-limit external writer", productID)
		out, err := repo.PreviewProcurementProductImpact(ctx, productID)
		require.ErrorIs(t, err, service.ErrProcurementProductImpactTooLarge)
		require.Nil(t, out)
	})
}
