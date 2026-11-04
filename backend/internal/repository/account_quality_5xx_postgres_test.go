//go:build unit

package repository

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// An explicit local test DSN is required; each test owns only its fresh schema.
func quality5xxPostgresDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("SUB2API_QUALITY_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated SUB2API_QUALITY_TEST_DSN")
	}
	base, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, base.Close()) })
	schema := pq.QuoteIdentifier(fmt.Sprintf("quality_5xx_test_%d", time.Now().UnixNano()))
	_, err = base.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := base.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		require.NoError(t, err)
	})
	db, err := sql.Open("postgres", dsn+" search_path="+schema)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	// Repository fixture, not a migration test: only columns read by this flow.
	_, err = db.Exec(`CREATE TABLE accounts (
 id BIGSERIAL PRIMARY KEY, platform TEXT NOT NULL DEFAULT 'openai', type TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'active', schedulable BOOLEAN NOT NULL DEFAULT true,
 concurrency INT NOT NULL DEFAULT 17, credentials JSONB NOT NULL DEFAULT '{}',
 extra JSONB NOT NULL DEFAULT '{"unrelated":"kept"}', proxy_id BIGINT, parent_account_id BIGINT,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now(), deleted_at TIMESTAMPTZ);
CREATE TABLE proxies (id BIGINT PRIMARY KEY, protocol TEXT, host TEXT, port INT, username TEXT, password TEXT, deleted_at TIMESTAMPTZ);
CREATE TABLE scheduled_test_plans (
 id BIGSERIAL PRIMARY KEY, account_id BIGINT NOT NULL REFERENCES accounts(id),
 model_id TEXT NOT NULL, cron_expression TEXT NOT NULL, enabled BOOLEAN NOT NULL,
 max_results INT NOT NULL, auto_recover BOOLEAN NOT NULL, last_run_at TIMESTAMPTZ,
 next_run_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
 pelican_config JSONB, running_until TIMESTAMPTZ);
CREATE TABLE account_quality_states (plan_id BIGINT PRIMARY KEY REFERENCES scheduled_test_plans(id), state JSONB NOT NULL);
CREATE TABLE scheduler_outbox (id BIGSERIAL PRIMARY KEY, event_type TEXT NOT NULL, account_id BIGINT, payload JSONB);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT);`)
	require.NoError(t, err)
	return db
}

func quality5xxPostgresPlan(t *testing.T, db *sql.DB, accountType string) (*scheduledTestPlanRepository, *service.ScheduledTestPlan) {
	t.Helper()
	var accountID int64
	require.NoError(t, db.QueryRow(`INSERT INTO accounts(type) VALUES($1) RETURNING id`, accountType).Scan(&accountID))
	next := time.Now().UTC().Add(time.Hour)
	repo := &scheduledTestPlanRepository{db: db}
	plan, err := repo.Create(t.Context(), &service.ScheduledTestPlan{
		AccountID: accountID, ModelID: "target", CronExpression: "0 * * * *", Enabled: true,
		MaxResults: 10, NextRunAt: &next,
		PelicanConfig: &service.PelicanTestConfig{ParallelCount: 1, Quality: &service.QualityPolicy{
			Action: service.QualityActionRemoveModel, RemoveModels: []string{"target"},
			TriggerOnUpstream5xx: true, AutoRestore: true, RecoveryConcurrency: 4,
		}},
	})
	require.NoError(t, err)
	return repo, plan
}

func TestQuality5xxPostgresTriggeredClaimGuards(t *testing.T) {
	db := quality5xxPostgresDB(t)
	for _, tc := range []struct {
		name, accountType, edit string
		want                    bool
	}{
		{"apikey", "apikey", "", true},
		{"oauth", "oauth", "", true},
		{"setup_token", "setup-token", "", false},
		{"upstream", "upstream", "", false},
		{"trigger_disabled", "apikey", `UPDATE scheduled_test_plans SET pelican_config=jsonb_set(pelican_config,'{quality,trigger_on_upstream_5xx}','false') WHERE id=$1`, false},
		{"paused", "apikey", `UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, false},
		{"edited", "apikey", `UPDATE scheduled_test_plans SET updated_at=updated_at+interval '1 second' WHERE id=$1`, false},
		{"rescheduled", "apikey", `UPDATE scheduled_test_plans SET next_run_at=next_run_at+interval '1 hour' WHERE id=$1`, false},
		{"deleted_account", "apikey", `UPDATE accounts SET deleted_at=now() WHERE id=(SELECT account_id FROM scheduled_test_plans WHERE id=$1)`, false},
		{"running", "apikey", `UPDATE scheduled_test_plans SET running_until=now()+interval '10 minutes' WHERE id=$1`, false},
		{"peer_running", "apikey", `INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,max_results,auto_recover,next_run_at,created_at,updated_at,pelican_config,running_until) SELECT account_id,model_id,cron_expression,enabled,max_results,auto_recover,next_run_at,created_at,updated_at,pelican_config,now()+interval '10 minutes' FROM scheduled_test_plans WHERE id=$1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, plan := quality5xxPostgresPlan(t, db, tc.accountType)
			now := time.Now().UTC().Truncate(time.Microsecond)
			until := now.Add(time.Minute)
			ok, err := repo.ClaimPelican(t.Context(), plan, now, until, *plan.NextRunAt)
			require.NoError(t, err)
			require.False(t, ok, "future timer must not claim")
			if tc.edit != "" {
				_, err = db.Exec(tc.edit, plan.ID)
				require.NoError(t, err)
			}
			plan.TriggerSource = "upstream_5xx"
			ok, err = repo.ClaimPelican(t.Context(), plan, now, until, *plan.NextRunAt)
			require.NoError(t, err)
			require.Equal(t, tc.want, ok)
			if !ok {
				return
			}
			stored, err := repo.GetByID(t.Context(), plan.ID)
			require.NoError(t, err)
			require.True(t, stored.NextRunAt.Equal(*plan.NextRunAt), "trigger preserves cron")
			ok, err = repo.ClaimPelican(t.Context(), plan, now, until, *plan.NextRunAt)
			require.NoError(t, err)
			require.False(t, ok, "running lease must not duplicate")
			require.NoError(t, repo.FinishTriggeredQuality(t.Context(), plan, until, now, *plan.NextRunAt))
		})
	}
}

func TestQuality5xxPostgresAPIKeyEpisodeAndRecovery(t *testing.T) {
	db := quality5xxPostgresDB(t)
	repo, plan := quality5xxPostgresPlan(t, db, service.AccountTypeAPIKey)
	now := time.Now().UTC().Truncate(time.Microsecond)
	until := now.Add(5 * time.Minute)
	plan.TriggerSource = "upstream_5xx"
	claim := func() {
		t.Helper()
		var err error
		plan, err = repo.GetByID(t.Context(), plan.ID)
		require.NoError(t, err)
		plan.TriggerSource = "upstream_5xx"
		ok, err := repo.ClaimPelican(t.Context(), plan, now, until, *plan.NextRunAt)
		require.NoError(t, err)
		require.True(t, ok)
	}
	apply := func(verdict, expected string) {
		t.Helper()
		action, err := repo.ApplyQualityOutcome(t.Context(), plan, until, verdict)
		require.NoError(t, err)
		require.Equal(t, expected, action)
	}
	claim()
	for i := 1; i <= 2; i++ {
		changed, err := repo.applyImmediateAccountFault(t.Context(), plan.ID, plan.AccountID, true)
		require.NoError(t, err)
		require.True(t, changed)
		apply("passed", "stale_run")
		require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, now))
		claim()
		require.EqualValues(t, i, plan.Quality5xxEpisode)
	}
	var pendingConcurrency int
	var pendingExtra []byte
	require.NoError(t, db.QueryRow(`SELECT concurrency,extra FROM accounts WHERE id=$1`, plan.AccountID).Scan(&pendingConcurrency, &pendingExtra))
	require.Equal(t, 17, pendingConcurrency, "fault signal alone must not lower manual concurrency")
	var pending map[string]any
	require.NoError(t, json.Unmarshal(pendingExtra, &pending))
	require.NotContains(t, pending, "model_rate_limits", "5xx is not a failed quality answer")
	require.NotContains(t, pending, service.AutoConfigConcurrencyExtraKey)
	apply("failed", "model_cooldown_refreshed")
	apply("inconclusive", "model_cooldown_refreshed")
	var raw []byte
	require.NoError(t, db.QueryRow(`SELECT extra FROM accounts WHERE id=$1`, plan.AccountID).Scan(&raw))
	var extra map[string]any
	require.NoError(t, json.Unmarshal(raw, &extra))
	require.Contains(t, extra["model_rate_limits"], "target", "inconclusive must retain cooldown")
	_, err := db.Exec(`UPDATE accounts SET concurrency=23,extra=jsonb_set(extra,'{model_rate_limits,manual}','{"reason":"manual"}') WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	apply("passed", "restored")
	var concurrency int
	var schedulable bool
	require.NoError(t, db.QueryRow(`SELECT concurrency,schedulable,extra FROM accounts WHERE id=$1`, plan.AccountID).Scan(&concurrency, &schedulable, &raw))
	require.Equal(t, 23, concurrency, "API Key recovery preserves manual concurrency")
	require.True(t, schedulable)
	require.NoError(t, json.Unmarshal(raw, &extra))
	require.Equal(t, "kept", extra["unrelated"])
	require.NotContains(t, extra, service.AutoConfigConcurrencyExtraKey)
	require.NotContains(t, extra["model_rate_limits"], "target")
	require.Contains(t, extra["model_rate_limits"], "manual")
	var states, events int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM account_quality_states`).Scan(&states))
	require.Zero(t, states, "confirmed recovery releases owned state")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM scheduler_outbox WHERE account_id=$1 AND event_type='account_changed'`, plan.AccountID).Scan(&events))
	require.GreaterOrEqual(t, events, 2, "cooldown and recovery invalidate scheduler snapshots")
	require.NoError(t, repo.FinishTriggeredQuality(t.Context(), plan, until, now, now.Add(time.Hour)))
}

func TestQuality5xxPostgresAPIKeyLateResultGuards(t *testing.T) {
	db := quality5xxPostgresDB(t)
	for _, tc := range []struct{ name, edit string }{
		{"paused", `UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`},
		{"edited", `UPDATE scheduled_test_plans SET updated_at=updated_at+interval '1 second' WHERE id=$1`},
		{"lease_replaced", `UPDATE scheduled_test_plans SET running_until=running_until+interval '1 minute' WHERE id=$1`},
		{"lease_expired", `UPDATE scheduled_test_plans SET running_until=now()-interval '1 second' WHERE id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, plan := quality5xxPostgresPlan(t, db, service.AccountTypeAPIKey)
			now := time.Now().UTC().Truncate(time.Microsecond)
			until := now.Add(time.Minute)
			plan.TriggerSource = "upstream_5xx"
			ok, err := repo.ClaimPelican(t.Context(), plan, now, until, *plan.NextRunAt)
			require.NoError(t, err)
			require.True(t, ok)
			if tc.name == "lease_expired" {
				// Keep the saved lease token equal so expiry itself is tested.
				require.NoError(t, db.QueryRow(tc.edit+` RETURNING running_until`, plan.ID).Scan(&until))
			} else {
				_, err = db.Exec(tc.edit, plan.ID)
				require.NoError(t, err)
			}
			action, err := repo.ApplyQualityOutcome(t.Context(), plan, until, "failed")
			require.NoError(t, err)
			require.Equal(t, "stale_run", action)
			var events int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM scheduler_outbox WHERE account_id=$1`, plan.AccountID).Scan(&events))
			require.Zero(t, events, "stale result must not mutate the account")
		})
	}
}

func TestQualityPostgresProbeAccountConfigFence(t *testing.T) {
	db := quality5xxPostgresDB(t)
	for _, verdict := range []string{"failed", "passed"} {
		for _, tc := range []struct {
			name, edit string
			stale      bool
		}{
			{"key", `UPDATE accounts SET credentials=credentials||'{"api_key":"fixture-rotated"}' WHERE id=$1`, true},
			{"endpoint", `UPDATE accounts SET credentials=credentials||'{"base_url":"https://new.example/v1"}' WHERE id=$1`, true},
			{"mapping", `UPDATE accounts SET credentials=credentials||'{"model_mapping":{"target":"other"}}' WHERE id=$1`, true},
			{"protocol", `UPDATE accounts SET credentials=credentials||'{"api_protocol":"chat_completions"}' WHERE id=$1`, true},
			{"protocol_endpoints", `UPDATE accounts SET credentials=credentials||'{"api_base_urls":{"responses":"https://new.example/v1"}}' WHERE id=$1`, true},
			{"protocol_rules", `UPDATE accounts SET credentials=credentials||'{"protocol_rules":[{"pattern":"target","protocol":"responses"}]}' WHERE id=$1`, true},
			{"account_mode", `UPDATE accounts SET credentials=credentials||'{"account_mode":"coding"}' WHERE id=$1`, true},
			{"platform", `UPDATE accounts SET platform='anthropic' WHERE id=$1`, true},
			{"type", `UPDATE accounts SET type='oauth' WHERE id=$1`, true},
			{"bps", `UPDATE accounts SET extra=extra||'{"openai_excel_bps":true}' WHERE id=$1`, true},
			{"proxy", `UPDATE accounts SET proxy_id=1 WHERE id=$1`, true},
			{"proxy_address", `UPDATE proxies SET host=host||'.changed' WHERE id=(SELECT proxy_id FROM accounts WHERE id=$1)`, true},
			{"parent_binding", `UPDATE accounts SET parent_account_id=99 WHERE id=$1`, true},
			{"unrelated_extra", `UPDATE accounts SET extra=extra||'{"unrelated":"edited"}',updated_at=clock_timestamp() WHERE id=$1`, false},
			{"manual_concurrency", `UPDATE accounts SET concurrency=23,updated_at=clock_timestamp() WHERE id=$1`, false},
			{"rotating_token", `UPDATE accounts SET credentials=credentials||'{"access_token":"fixture-refreshed","refresh_token":"fixture-renewed"}' WHERE id=$1`, false},
		} {
			t.Run(verdict+"/"+tc.name, func(t *testing.T) {
				_, err := db.Exec(`INSERT INTO proxies(id,protocol,host,port) VALUES(1,'http','127.0.0.1',12345) ON CONFLICT DO NOTHING`)
				require.NoError(t, err)
				repo, plan := quality5xxPostgresPlan(t, db, service.AccountTypeAPIKey)
				if tc.name == "proxy_address" {
					_, err = db.Exec(`UPDATE accounts SET proxy_id=1 WHERE id=$1`, plan.AccountID)
					require.NoError(t, err)
				}
				now := time.Now().UTC().Truncate(time.Microsecond)
				until := now.Add(time.Minute)
				claim := func() {
					t.Helper()
					plan, err = repo.GetByID(t.Context(), plan.ID)
					require.NoError(t, err)
					plan.TriggerSource = "upstream_5xx"
					ok, claimErr := repo.ClaimPelican(t.Context(), plan, now, until, *plan.NextRunAt)
					require.NoError(t, claimErr)
					require.True(t, ok)
				}
				claim()
				if verdict == "passed" {
					action, applyErr := repo.ApplyQualityOutcome(t.Context(), plan, until, "failed")
					require.NoError(t, applyErr)
					require.Equal(t, "models_cooled", action)
					require.NoError(t, repo.FinishPelican(t.Context(), plan.ID, until, now))
					claim()
				}
				_, err = db.Exec(tc.edit, plan.AccountID)
				require.NoError(t, err)
				snapshot := func() string {
					t.Helper()
					var raw string
					require.NoError(t, db.QueryRow(`SELECT jsonb_build_object(
 'account',(SELECT to_jsonb(a) FROM accounts a WHERE id=$1),
 'state',(SELECT state FROM account_quality_states WHERE plan_id=$2),
 'events',(SELECT count(*) FROM scheduler_outbox WHERE account_id=$1))::text`, plan.AccountID, plan.ID).Scan(&raw))
					return raw
				}
				before := snapshot()
				action, applyErr := repo.ApplyQualityOutcome(t.Context(), plan, until, verdict)
				require.NoError(t, applyErr)
				if tc.stale {
					require.Equal(t, "stale_run", action, "old probe must not mutate the replacement route")
					require.Equal(t, before, snapshot(), "rejected result must retain account, ownership and events")
				} else {
					expected := "models_cooled"
					if verdict == "passed" {
						expected = "restored"
					}
					require.Equal(t, expected, action, "unrelated runtime changes must not starve valid probes")
				}
			})
		}
	}
}

func TestQualityPostgresProbeMissingConfigSnapshotRejectsResult(t *testing.T) {
	db := quality5xxPostgresDB(t)
	repo, plan := quality5xxPostgresPlan(t, db, service.AccountTypeAPIKey)
	now := time.Now().UTC()
	until := now.Add(time.Minute)
	plan.TriggerSource = "upstream_5xx"
	ok, err := repo.ClaimPelican(t.Context(), plan, now, until, *plan.NextRunAt)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, plan.QualityAccountFingerprint)
	plan.QualityAccountFingerprint = nil
	action, err := repo.ApplyQualityOutcome(t.Context(), plan, until, "failed")
	require.NoError(t, err)
	require.Equal(t, "stale_run", action)
	var states, events int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM account_quality_states`).Scan(&states))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM scheduler_outbox`).Scan(&events))
	require.Zero(t, states)
	require.Zero(t, events)
}

func TestQualityPostgresProbeParentCredentialChange(t *testing.T) {
	db := quality5xxPostgresDB(t)
	for _, verdict := range []string{"failed", "passed"} {
		t.Run(verdict, func(t *testing.T) {
			var parentID int64
			require.NoError(t, db.QueryRow(`INSERT INTO accounts(type,credentials) VALUES('apikey','{"api_key":"fixture-parent"}') RETURNING id`).Scan(&parentID))
			repo, plan := quality5xxPostgresPlan(t, db, service.AccountTypeAPIKey)
			_, err := db.Exec(`UPDATE accounts SET parent_account_id=$2 WHERE id=$1`, plan.AccountID, parentID)
			require.NoError(t, err)
			now := time.Now().UTC()
			until := now.Add(time.Minute)
			plan.TriggerSource = "upstream_5xx"
			ok, err := repo.ClaimPelican(t.Context(), plan, now, until, *plan.NextRunAt)
			require.NoError(t, err)
			require.True(t, ok)
			_, err = db.Exec(`UPDATE accounts SET credentials='{"api_key":"fixture-parent-rotated"}' WHERE id=$1`, parentID)
			require.NoError(t, err)
			action, err := repo.ApplyQualityOutcome(t.Context(), plan, until, verdict)
			require.NoError(t, err)
			require.Equal(t, "stale_run", action, "same parent ID must not hide a credential change")
		})
	}
}

func TestQualityPostgresProbeConfigReadFailureRollsBackClaim(t *testing.T) {
	db := quality5xxPostgresDB(t)
	for _, tc := range []struct{ name, edit string }{
		{"missing_proxy", `UPDATE accounts SET proxy_id=99 WHERE id=$1`},
		{"parent_cycle", `UPDATE accounts SET parent_account_id=id WHERE id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, plan := quality5xxPostgresPlan(t, db, service.AccountTypeAPIKey)
			_, err := db.Exec(tc.edit, plan.AccountID)
			require.NoError(t, err)
			now := time.Now().UTC()
			plan.TriggerSource = "upstream_5xx"
			ok, err := repo.ClaimPelican(t.Context(), plan, now, now.Add(time.Minute), *plan.NextRunAt)
			require.Error(t, err)
			require.False(t, ok)
			stored, err := repo.GetByID(t.Context(), plan.ID)
			require.NoError(t, err)
			require.Nil(t, stored.RunningUntil, "snapshot failure must roll back the lease")
			require.True(t, stored.NextRunAt.Equal(*plan.NextRunAt))
		})
	}
}
