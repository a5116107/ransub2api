package repository

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Wei-Shaw/sub2api/migrations"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Requires a disposable PostgreSQL server: supplied by the integration harness,
// or explicitly for standalone runs. Each path gets its own database; this must
// never point to an application database/server.
func TestAPIKeyConcurrencyUpgradePaths(t *testing.T) {
	dsn := os.Getenv("SUB2API_UPGRADE_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set SUB2API_UPGRADE_TEST_POSTGRES_URL to a disposable PostgreSQL server")
	}
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	ctx := context.Background()
	const filename = "237_add_api_key_concurrency_limit.sql"
	const checksum = "64659971d796ca6f80bca02642a5c54939943d40d071e1bee8b3661ca33631c2"
	for _, deployed := range []bool{false, true} {
		t.Run(fmt.Sprintf("deployed_pr=%t", deployed), func(t *testing.T) {
			name := fmt.Sprintf("key_upgrade_%d", time.Now().UnixNano())
			_, err := admin.ExecContext(ctx, "CREATE DATABASE "+name)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := admin.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
				require.NoError(t, err)
			})
			u, err := url.Parse(dsn)
			require.NoError(t, err)
			u.Path = "/" + name
			db, err := sql.Open("postgres", u.String())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			baseline := fstest.MapFS{}
			files, err := fs.Glob(migrations.FS, "*.sql")
			require.NoError(t, err)
			for _, file := range files {
				if file == filename && !deployed {
					continue
				}
				data, err := fs.ReadFile(migrations.FS, file)
				require.NoError(t, err)
				baseline[file] = &fstest.MapFile{Data: data}
			}
			require.NoError(t, applyMigrationsFS(ctx, db, baseline))
			var userID, keyID int64
			require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO users(email,password_hash) VALUES ('upgrade@example.test','test') RETURNING id").Scan(&userID))
			require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO api_keys(user_id,key,name) VALUES ($1,'upgrade-key','upgrade') RETURNING id", userID).Scan(&keyID))
			want := 0
			if deployed {
				want = 9
				_, err := db.ExecContext(ctx, "UPDATE api_keys SET concurrency_limit=9 WHERE id=$1", keyID)
				require.NoError(t, err)
			}
			for repeat := 0; repeat < 2; repeat++ {
				require.NoError(t, applyMigrationsFS(ctx, db, migrations.FS))
				var got int
				require.NoError(t, db.QueryRowContext(ctx, "SELECT concurrency_limit FROM api_keys WHERE id=$1", keyID).Scan(&got))
				require.Equal(t, want, got)
				var recorded string
				require.NoError(t, db.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE filename=$1", filename).Scan(&recorded))
				require.Equal(t, checksum, recorded)
				var count int
				require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE filename IN ($1,'237_add_minimax_platform.sql','240_affiliate_ledger_operation_id.sql')", filename).Scan(&count))
				require.Equal(t, 3, count)
			}
			_, err = db.ExecContext(ctx, "UPDATE api_keys SET concurrency_limit=-1 WHERE id=$1", keyID)
			require.ErrorContains(t, err, "api_keys_concurrency_limit_nonnegative")
			var defaultLimit int
			require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO api_keys(user_id,key,name) VALUES ($1,'new-upgrade-key','new') RETURNING concurrency_limit", userID).Scan(&defaultLimit))
			require.Zero(t, defaultLimit)
		})
	}
}

func TestV2211UpgradePreservesCustomPolicies(t *testing.T) {
	dsn := os.Getenv("SUB2API_UPGRADE_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set SUB2API_UPGRADE_TEST_POSTGRES_URL to a disposable PostgreSQL server")
	}
	ctx := t.Context()
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	name := fmt.Sprintf("v2211_upgrade_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		require.NoError(t, err)
	})
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.Path = "/" + name
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	added := map[string]bool{
		"341_api_key_context_token_limit.sql": true,
		"342_key_risk_per_key_ip.sql":         true,
		"342_payment_provider_fee_rates.sql":  true,
	}
	baseline := fstest.MapFS{}
	files, err := fs.Glob(migrations.FS, "*.sql")
	require.NoError(t, err)
	for _, file := range files {
		if !added[file] {
			data, err := fs.ReadFile(migrations.FS, file)
			require.NoError(t, err)
			baseline[file] = &fstest.MapFile{Data: data}
		}
	}
	require.NoError(t, applyMigrationsFS(ctx, db, baseline))
	_, err = db.ExecContext(ctx, `
		INSERT INTO users(email,password_hash) VALUES ('v2211@example.test','fixture');
		INSERT INTO groups(name,rate_multiplier) VALUES ('fixed-product',0.15);
		INSERT INTO api_keys(user_id,key,name,group_id,allow_long_context,concurrency_limit)
		 SELECT u.id,'v2211-fixture','original',g.id,false,7 FROM users u CROSS JOIN groups g
		 WHERE u.email='v2211@example.test' AND g.name='fixed-product';
		INSERT INTO accounts(name,platform,type,credentials,extra,rate_multiplier)
		 VALUES ('custom-route','openai','apikey','{"api_key":"synthetic","base_url":"https://fixture.invalid"}',
		 '{"upstream_key_concurrency_limit":9,"custom_marker":"preserve","openai_long_context_billing_enabled":true}',0.03);
		INSERT INTO settings(key,value) VALUES ('v2211_upgrade_fixture','{"enabled":true}');
		INSERT INTO payment_provider_instances(provider_key,name,config) VALUES ('easypay','legacy','{}');
		UPDATE key_risk_control SET config='{"ips_per_key":5,"users_per_ip":4,"enabled":true}';
		INSERT INTO key_risk_events(rule,subject,evidence,first_seen,last_seen)
		 VALUES ('cross_user_ip','old','{}',now(),now()),('key_ip_spread','current','{}',now(),now());
		INSERT INTO key_risk_outbox(user_id,kind,bucket,state)
		 SELECT id,'risk',bucket,'pending' FROM users CROSS JOIN generate_series(1,2) bucket
		 WHERE email='v2211@example.test';
		INSERT INTO key_risk_outbox(user_id,kind,bucket,state)
		 SELECT id,'risk',3,'sending' FROM users WHERE email='v2211@example.test';
		INSERT INTO key_risk_outbox_items(outbox_id,event_id,generation,key_id,evidence)
		 SELECT o.id,e.id,e.generation,k.id,'{}' FROM key_risk_outbox o CROSS JOIN key_risk_events e CROSS JOIN api_keys k
		 WHERE (o.bucket=1 AND e.rule='key_ip_spread') OR (o.bucket=2 AND e.rule='cross_user_ip');`)
	require.NoError(t, err)
	checksums := map[string]string{}
	rows, err := db.QueryContext(ctx, "SELECT filename,checksum FROM schema_migrations")
	require.NoError(t, err)
	for rows.Next() {
		var file, checksum string
		require.NoError(t, rows.Scan(&file, &checksum))
		checksums[file] = checksum
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	for stage := 0; stage < 3; stage++ {
		if stage == 1 {
			// Older applications leave additive columns and migration records intact.
			require.NoError(t, applyMigrationsFS(ctx, db, baseline))
			_, err = db.ExecContext(ctx, `UPDATE api_keys SET name='old-writer' WHERE key='v2211-fixture';
			 UPDATE payment_provider_instances SET name='old-writer' WHERE provider_key='easypay'`)
			require.NoError(t, err)
		}
		require.NoError(t, applyMigrationsFS(ctx, db, migrations.FS))
		for file, checksum := range checksums {
			var got string
			require.NoError(t, db.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE filename=$1", file).Scan(&got))
			require.Equal(t, checksum, got, file)
		}
		var count, limit, concurrency int
		var allow bool
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count))
		require.Equal(t, len(checksums)+len(added), count)
		require.NoError(t, db.QueryRowContext(ctx, "SELECT context_token_limit,allow_long_context,concurrency_limit FROM api_keys WHERE key='v2211-fixture'").Scan(&limit, &allow, &concurrency))
		require.Equal(t, stage*1234, limit)
		require.False(t, allow)
		require.Equal(t, 7, concurrency)
		var multiplier float64
		var credentials, extra, config, value, state string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT rate_multiplier FROM groups WHERE name='fixed-product'").Scan(&multiplier))
		require.Equal(t, 0.15, multiplier)
		require.NoError(t, db.QueryRowContext(ctx, "SELECT credentials::text,extra::text,rate_multiplier FROM accounts WHERE name='custom-route'").Scan(&credentials, &extra, &multiplier))
		require.Equal(t, 0.03, multiplier)
		require.JSONEq(t, `{"api_key":"synthetic","base_url":"https://fixture.invalid"}`, credentials)
		require.JSONEq(t, `{"upstream_key_concurrency_limit":9,"custom_marker":"preserve","openai_long_context_billing_enabled":true}`, extra)
		require.NoError(t, db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='v2211_upgrade_fixture'").Scan(&value))
		require.JSONEq(t, `{"enabled":true}`, value)
		require.NoError(t, db.QueryRowContext(ctx, "SELECT config::text FROM key_risk_control").Scan(&config))
		require.JSONEq(t, `{"ips_per_key":2,"enabled":true}`, config)
		for bucket, expected := range map[int]string{1: "pending", 2: "skipped", 3: "sending"} {
			require.NoError(t, db.QueryRowContext(ctx, "SELECT state FROM key_risk_outbox WHERE bucket=$1", bucket).Scan(&state))
			require.Equal(t, expected, state)
		}
		var rate sql.NullFloat64
		require.NoError(t, db.QueryRowContext(ctx, "SELECT fee_rate,method_fee_rates::text FROM payment_provider_instances").Scan(&rate, &config))
		if stage == 0 {
			require.False(t, rate.Valid)
			require.JSONEq(t, `{}`, config)
		} else {
			require.True(t, rate.Valid)
			require.Zero(t, rate.Float64)
			require.JSONEq(t, `{"alipay":1.25}`, config)
		}
		_, err = db.ExecContext(ctx, "UPDATE api_keys SET context_token_limit=$1", (stage+1)*1234)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `UPDATE payment_provider_instances SET fee_rate=0,method_fee_rates='{"alipay":1.25}'`)
		require.NoError(t, err)
		t.Logf("stage=%d old_checksums=%d migrations=%d preserved", stage, len(checksums), count)
	}
}
