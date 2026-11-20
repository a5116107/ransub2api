//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestProcurementManagementLegacyState(t *testing.T) {
	f := newMembershipFixture(t)
	var raw []byte
	err := integrationDB.QueryRowContext(t.Context(), `SELECT procurement_management FROM groups WHERE id=$1`, f.groups[0]).Scan(&raw)
	require.NoError(t, err, "review authorization must be persisted with the original Group")
	require.Empty(t, raw, "existing products must remain manual, not automatically claimed")
}

type managementFixture struct {
	db              *sql.DB
	client          *dbent.Client
	repo            *channelRepository
	product, source int64
	ids             []int64
}

func newManagementFixture(t *testing.T) managementFixture {
	t.Helper()
	db, client := UpstreamChannelsTestDatabase(t)
	f := managementFixture{db: db, client: client, repo: NewChannelRepository(db).(*channelRepository)}
	ctx := t.Context()
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform) VALUES('review product','openai') RETURNING id`).Scan(&f.product))
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform) VALUES('approved supply','openai') RETURNING id`).Scan(&f.source))
	policy := fmt.Sprintf(`{"version":6,"revision":1,"min_rate":"0","max_rate":"1","sources":{"account_ids":null},"buckets":{"codes":null},"quality":"not_required","availability":"not_required","source_group_ids":[%d]}`, f.source)
	_, err := db.ExecContext(ctx, `UPDATE groups SET procurement_policy=$2::jsonb WHERE id=$1`, f.product, policy)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		var id int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,credentials,extra,status,schedulable)
 VALUES($1,'openai','apikey','{"api_key":"review-local-secret","base_url":"https://review.invalid"}','{}','active',true) RETURNING id`, fmt.Sprint("supplier-", i)).Scan(&id))
		f.ids = append(f.ids, id)
		_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id,priority,allowed_models) VALUES($1,$2,7,'["sol"]'),($1,$3,3,NULL)`, id, f.product, f.source)
		require.NoError(t, err)
	}
	return f
}

func (f managementFixture) preview(t *testing.T, ids ...int64) *service.ProcurementManagementPreview {
	t.Helper()
	p, err := f.repo.PreviewProcurementManagement(t.Context(), f.product, ids)
	require.NoError(t, err)
	return p
}

func managementCommand(p *service.ProcurementManagementPreview) *service.ProcurementManagementCommand {
	ids := make([]int64, 0, len(p.Memberships))
	for _, m := range p.Memberships {
		ids = append(ids, m.AccountID)
	}
	return &service.ProcurementManagementCommand{Version: 1, Mode: "review", ExpectedRevision: p.State.Revision, AccountIDs: ids, ObservedAt: p.ObservedAt, Fingerprint: p.Fingerprint}
}

func (f managementFixture) preserved(t *testing.T) string {
	t.Helper()
	var s string
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT jsonb_build_object(
 'group',(SELECT to_jsonb(g)-'procurement_management'-'updated_at' FROM groups g WHERE id=$1),
 'memberships',(SELECT jsonb_agg(to_jsonb(g)-'ownership' ORDER BY account_id,group_id) FROM account_groups g),
 'accounts',(SELECT jsonb_agg(to_jsonb(a)-'ai_ops_revision' ORDER BY id) FROM accounts a),
 'settings',(SELECT jsonb_agg(to_jsonb(s) ORDER BY key) FROM settings s),
 'plans',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM scheduled_test_plans p),
 'states',(SELECT jsonb_agg(to_jsonb(s) ORDER BY plan_id) FROM account_quality_states s),
 'usage',(SELECT count(*) FROM usage_logs),
 'leases',(SELECT count(*) FROM monitoring_probe_budget_leases))::text`, f.product).Scan(&s))
	return s
}

func TestProcurementManagementPostgresLifecycle(t *testing.T) {
	f := newManagementFixture(t)
	before := f.preserved(t)
	p := f.preview(t, f.ids[0])
	require.True(t, p.CanConfirm)
	require.Equal(t, "manual", p.State.Mode)
	require.Zero(t, p.State.Revision)
	require.Nil(t, p.Memberships[0].Ownership)
	require.Equal(t, before, f.preserved(t), "preview must have zero side effects")
	var revisionBefore, revisionAfter int64
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT ai_ops_revision FROM accounts WHERE id=$1`, f.ids[0]).Scan(&revisionBefore))
	s, err := f.repo.SaveProcurementManagement(t.Context(), f.product, managementCommand(p))
	require.NoError(t, err)
	require.Equal(t, int64(1), s.Revision)
	require.Equal(t, "review", s.Mode)
	require.Equal(t, 1, s.CurrentOwned)
	require.Equal(t, 2, s.ManualMemberships)
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT ai_ops_revision FROM accounts WHERE id=$1`, f.ids[0]).Scan(&revisionAfter))
	require.Equal(t, revisionBefore+1, revisionAfter, "original membership trigger must advance configuration revision")
	require.Equal(t, before, f.preserved(t), "claim changes ownership only, never protections, plans or prices")
	read, err := f.repo.ReadProcurementManagement(t.Context(), f.product)
	require.NoError(t, err)
	require.Equal(t, s, read)
	_, err = f.repo.SaveProcurementManagement(t.Context(), f.product, managementCommand(p))
	require.ErrorIs(t, err, service.ErrProcurementManagementConflict)
	p = f.preview(t, f.ids[1])
	s, err = f.repo.SaveProcurementManagement(t.Context(), f.product, managementCommand(p))
	require.NoError(t, err)
	require.Equal(t, 1, s.CurrentOwned)
	require.Equal(t, 1, s.RetainedOwned)
	// Ordinary edits retain identity/ownership; remove and manually re-add does not.
	a := newAccountRepositoryWithSQL(f.client, f.db, nil)
	require.NoError(t, a.SetGroupAllowedModels(t.Context(), f.ids[1], map[int64][]string{f.product: {"sol"}}))
	require.NoError(t, a.RemoveFromGroup(t.Context(), f.ids[1], f.product))
	require.NoError(t, a.AddToGroup(t.Context(), f.ids[1], f.product, 7))
	s, err = f.repo.ReadProcurementManagement(t.Context(), f.product)
	require.NoError(t, err)
	require.Zero(t, s.CurrentOwned)
	require.Equal(t, 1, s.RetainedOwned)
	require.Equal(t, 2, s.ManualMemberships)
	// State read and return to manual do not depend on a healthy catalog/policy.
	_, err = f.db.ExecContext(t.Context(), `WITH saved AS (INSERT INTO settings(key,value) VALUES($1,'broken') ON CONFLICT(key) DO UPDATE SET value='broken' RETURNING key) UPDATE groups SET procurement_policy='{"future":true}' WHERE id=$2`, service.ProcurementCatalogSettingKey, f.product)
	require.NoError(t, err)
	s, err = f.repo.SaveProcurementManagement(t.Context(), f.product, &service.ProcurementManagementCommand{Version: 1, Mode: "manual", ExpectedRevision: s.Revision})
	require.NoError(t, err)
	require.Equal(t, "manual", s.Mode)
	require.Zero(t, s.CurrentOwned)
	require.Equal(t, 1, s.RetainedOwned)
	copyGroup := &service.Group{Name: "manual-copy", Platform: service.PlatformOpenAI}
	// Broken price policy is not copied for this independent schema check.
	_, err = f.db.ExecContext(t.Context(), `UPDATE groups SET procurement_policy=NULL WHERE id=$1`, f.product)
	require.NoError(t, err)
	require.NoError(t, newGroupRepositoryWithSQL(f.client, f.db).CreateFromSource(t.Context(), copyGroup, f.product))
	copyState, err := f.repo.ReadProcurementManagement(t.Context(), copyGroup.ID)
	require.NoError(t, err)
	require.Zero(t, copyState.Revision)
	require.Equal(t, "manual", copyState.Mode)
	require.Zero(t, copyState.RetainedOwned)
}

func TestProcurementManagementPostgresCAS(t *testing.T) {
	for _, change := range []string{"same_revision_policy", "same_revision_catalog", "membership_models", "remove_readd", "account_protection", "source_removed", "rule_added", "tampered_time", "expired", "future_time", "cancelled", "notification", "concurrent"} {
		t.Run(change, func(t *testing.T) {
			f := newManagementFixture(t)
			p := f.preview(t, f.ids[0])
			require.True(t, p.CanConfirm)
			cmd := managementCommand(p)
			switch change {
			case "same_revision_policy":
				_, err := f.db.ExecContext(t.Context(), `UPDATE groups SET procurement_policy=jsonb_set(procurement_policy,'{max_rate}','"0.8"') WHERE id=$1`, f.product)
				require.NoError(t, err)
			case "same_revision_catalog":
				_, err := f.db.ExecContext(t.Context(), `INSERT INTO settings(key,value) VALUES($1,'{"version":1,"revision":0,"buckets":[]}')`, service.ProcurementCatalogSettingKey)
				require.NoError(t, err)
			case "membership_models":
				_, err := f.db.ExecContext(t.Context(), `UPDATE account_groups SET allowed_models='["astra"]' WHERE account_id=$1 AND group_id=$2`, f.ids[0], f.product)
				require.NoError(t, err)
			case "remove_readd":
				_, err := f.db.ExecContext(t.Context(), `WITH removed AS (DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2 RETURNING account_id) INSERT INTO account_groups(account_id,group_id,priority,allowed_models) SELECT account_id,$2,7,'["sol"]'::jsonb FROM removed`, f.ids[0], f.product)
				require.NoError(t, err)
			case "account_protection":
				_, err := f.db.ExecContext(t.Context(), `UPDATE accounts SET schedulable=false WHERE id=$1`, f.ids[0])
				require.NoError(t, err)
			case "source_removed":
				_, err := f.db.ExecContext(t.Context(), `DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, f.ids[0], f.source)
				require.NoError(t, err)
			case "rule_added":
				_, err := f.db.ExecContext(t.Context(), `INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,max_results,auto_recover,pelican_config,next_run_at) VALUES($1,'sol','0 * * * *',true,10,false,'{"quality":{"action":"disable_scheduling"}}',now())`, f.ids[0])
				require.NoError(t, err)
			case "tampered_time":
				cmd.ObservedAt = cmd.ObservedAt.Add(time.Second)
			case "expired", "future_time":
				tx, err := f.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
				require.NoError(t, err)
				at := time.Now().Add(-3 * time.Minute)
				if change == "future_time" {
					at = time.Now().Add(time.Minute)
				}
				p, err = buildProcurementManagementPreview(t.Context(), tx, f.product, cmd.AccountIDs, at)
				require.NoError(t, err)
				require.NoError(t, tx.Rollback())
				cmd = managementCommand(p)
			case "notification":
				_, err := f.db.ExecContext(t.Context(), `CREATE FUNCTION reject_review_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'review outbox failed'; END $$;
 CREATE TRIGGER reject_review_event BEFORE INSERT ON scheduler_outbox FOR EACH ROW EXECUTE FUNCTION reject_review_event()`)
				require.NoError(t, err)
			case "concurrent":
				var wg sync.WaitGroup
				results := make(chan error, 2)
				for range 2 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						_, err := f.repo.SaveProcurementManagement(t.Context(), f.product, cmd)
						results <- err
					}()
				}
				wg.Wait()
				close(results)
				success, conflicts := 0, 0
				for err := range results {
					if err == nil {
						success++
					} else {
						require.ErrorIs(t, err, service.ErrProcurementManagementConflict)
						conflicts++
					}
				}
				require.Equal(t, 1, success)
				require.Equal(t, 1, conflicts)
				return
			}
			before := f.preserved(t)
			ctx := t.Context()
			if change == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_, err := f.repo.SaveProcurementManagement(ctx, f.product, cmd)
			if change == "notification" {
				require.ErrorContains(t, err, "review outbox failed")
			} else if change == "cancelled" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, service.ErrProcurementManagementConflict)
			}
			require.Equal(t, before, f.preserved(t))
			s, err := f.repo.ReadProcurementManagement(t.Context(), f.product)
			require.NoError(t, err)
			require.Zero(t, s.Revision)
		})
	}
}

func TestProcurementManagementPostgresProtections(t *testing.T) {
	for _, kind := range []string{"manual_stop", "quality_stop", "membership_rule", "saved_owner", "unknown_rule", "native_403", "unknown_settings", "outside_source", "unbound"} {
		t.Run(kind, func(t *testing.T) {
			f := newManagementFixture(t)
			exec := func(q string, args ...any) {
				t.Helper()
				_, err := f.db.ExecContext(t.Context(), q, args...)
				require.NoError(t, err)
			}
			switch kind {
			case "manual_stop":
				exec(`UPDATE accounts SET schedulable=false WHERE id=$1`, f.ids[0])
			case "quality_stop", "membership_rule", "saved_owner", "unknown_rule":
				config := `{"quality":{"action":"disable_scheduling"}}`
				if kind == "membership_rule" {
					config = fmt.Sprintf(`{"quality":{"action":"remove_groups","remove_group_ids":[%d]}}`, f.product)
				}
				if kind == "unknown_rule" {
					config = `{"quality":{"action":"future"}}`
				}
				var plan int64
				require.NoError(t, f.db.QueryRowContext(t.Context(), `INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,max_results,auto_recover,pelican_config,next_run_at) VALUES($1,'sol','0 * * * *',true,10,false,$2::jsonb,now()) RETURNING id`, f.ids[0], config).Scan(&plan))
				if kind == "saved_owner" {
					exec(`INSERT INTO account_quality_states(plan_id,state) VALUES($1,$2::jsonb)`, plan, fmt.Sprintf(`{"action":"remove_groups","removed":[{"group_id":%d,"priority":7}]}`, f.product))
					exec(`UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, plan)
				}
			case "native_403":
				exec(`UPDATE accounts SET type='oauth',extra='{"openai_excel_bps":true,"openai_excel_bps_auto_move_on_403":true,"openai_excel_bps_403_target_group_id":0}' WHERE id=$1`, f.ids[0])
			case "unknown_settings":
				exec(`INSERT INTO settings(key,value) VALUES('astra_routing_experiment_v1','{"future":true}')`)
			case "outside_source":
				exec(`DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, f.ids[0], f.source)
			case "unbound":
				exec(`DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, f.ids[0], f.product)
			}
			p := f.preview(t, f.ids[0])
			allowed := kind == "manual_stop" || kind == "quality_stop"
			require.Equal(t, allowed, p.CanConfirm)
			if allowed {
				require.NotEmpty(t, p.Memberships[0].Protections)
				before := f.preserved(t)
				_, err := f.repo.SaveProcurementManagement(t.Context(), f.product, managementCommand(p))
				require.NoError(t, err)
				require.Equal(t, before, f.preserved(t))
			} else {
				_, err := f.repo.SaveProcurementManagement(t.Context(), f.product, managementCommand(p))
				require.ErrorIs(t, err, service.ErrProcurementManagementConflict)
			}
		})
	}
}

func TestProcurementManagementPostgresRuntimeChurn(t *testing.T) {
	f := newManagementFixture(t)
	p := f.preview(t, f.ids[0])
	_, err := f.db.ExecContext(t.Context(), `UPDATE accounts SET last_used_at=now(),updated_at=now(),extra=extra||'{"codex_usage_updated_at":"runtime-refresh"}'::jsonb WHERE id=$1`, f.ids[0])
	require.NoError(t, err)
	_, err = f.db.ExecContext(t.Context(), `INSERT INTO settings(key,value) VALUES('unrelated_ui_preference','changed')`)
	require.NoError(t, err)
	_, err = f.repo.SaveProcurementManagement(t.Context(), f.product, managementCommand(p))
	require.NoError(t, err, "business usage and unrelated preferences must not invalidate a review")
}

func TestProcurementManagementPostgresLockRecheck(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			f := newManagementFixture(t)
			p := f.preview(t, f.ids[0])
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			blocker, err := f.db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = blocker.Rollback() }()
			require.NoError(t, lockLiveAccountForGroupBindings(t.Context(), blocker, f.ids[0]))
			result := make(chan error, 1)
			go func() {
				_, err := f.repo.SaveProcurementManagement(ctx, f.product, managementCommand(p))
				result <- err
			}()
			// The SQL lock is the synchronization evidence; no arbitrary sleep decides
			// whether this confirmation actually reached the in-flight wait.
			require.Eventually(t, func() bool {
				var waiting int
				err := f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FROM accounts WHERE id=%FOR NO KEY UPDATE%'`).Scan(&waiting)
				return err == nil && waiting > 0
			}, 3*time.Second, 10*time.Millisecond)
			if cancelled {
				cancel()
			} else {
				_, err = blocker.ExecContext(t.Context(), `DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, f.ids[0], f.source)
				require.NoError(t, err)
			}
			require.NoError(t, blocker.Commit())
			err = <-result
			if cancelled {
				require.Error(t, err)
			} else {
				require.ErrorIs(t, err, service.ErrProcurementManagementConflict)
			}
			saved, err := f.repo.ReadProcurementManagement(t.Context(), f.product)
			require.NoError(t, err)
			require.Zero(t, saved.Revision)
		})
	}
}

func TestProcurementManagementMigration(t *testing.T) {
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), `CREATE TEMP TABLE groups(id BIGINT,name TEXT) ON COMMIT DROP; INSERT INTO groups VALUES(1,'legacy'); SAVEPOINT before_management`)
	require.NoError(t, err)
	var before string
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT ctid::text||xmin::text FROM groups`).Scan(&before))
	raw, err := migrations.FS.ReadFile("350_group_procurement_management.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = tx.ExecContext(t.Context(), string(raw))
		require.NoError(t, err)
	}
	var after string
	var manual bool
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT ctid::text||xmin::text,procurement_management IS NULL FROM groups`).Scan(&after, &manual))
	require.Equal(t, before, after)
	require.True(t, manual)
	for _, value := range []string{`null`, `{}`, `[]`, `{"version":1,"revision":0,"mode":"review"}`, `{"version":1,"revision":1,"mode":"automatic"}`, `{"version":1,"revision":9007199254740992,"mode":"manual"}`, `{"version":1,"revision":1,"mode":"review","future":true}`} {
		_, err = tx.ExecContext(t.Context(), `SAVEPOINT invalid_management`)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `UPDATE groups SET procurement_management=$1::jsonb`, value)
		require.Error(t, err, value)
		_, err = tx.ExecContext(t.Context(), `ROLLBACK TO SAVEPOINT invalid_management`)
		require.NoError(t, err)
	}
	valid, err := json.Marshal(map[string]any{"version": 1, "revision": 9007199254740991, "mode": "review"})
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE groups SET procurement_management=$1::jsonb`, string(valid))
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `ROLLBACK TO SAVEPOINT before_management`)
	require.NoError(t, err)
	var columns int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_attribute WHERE attrelid='groups'::regclass AND attname='procurement_management' AND NOT attisdropped`).Scan(&columns))
	require.Zero(t, columns)
}
