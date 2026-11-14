//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestListDueUpstreamBillingProbeAccountsHandlesInvalidCalendarDate(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	_, err := tx.ExecContext(ctx, `
		UPDATE accounts
		SET extra = extra - 'upstream_billing_probe_enabled' - 'upstream_billing_probe'
	`)
	require.NoError(t, err)

	insert := func(name, nextProbeAt string) int64 {
		t.Helper()
		var id int64
		extra := fmt.Sprintf(`{
			"upstream_billing_probe_enabled": true,
			"upstream_billing_probe": {"status": "ok", "next_probe_at": %q}
		}`, nextProbeAt)
		err := scanSingleRow(ctx, tx, `
			INSERT INTO accounts (name, platform, type, status, extra)
			VALUES ($1, 'openai', $2, 'active', $3::jsonb)
			RETURNING id
		`, []any{name, service.AccountTypeAPIKey, extra}, &id)
		require.NoError(t, err)
		return id
	}

	invalidID := insert("probe-invalid-calendar-date", "2026-99-99T12:00:00Z")
	dueID := insert("probe-due", "2026-07-14T11:59:59Z")
	_ = insert("probe-not-due", "2026-07-14T12:00:01Z")

	accounts, err := repo.ListDueUpstreamBillingProbeAccounts(ctx, now, 20)
	require.NoError(t, err)
	require.Len(t, accounts, 2)
	require.Equal(t, invalidID, accounts[0].ID)
	require.Equal(t, dueID, accounts[1].ID)
}

func insertUpstreamBillingProbeAccount(ctx context.Context, t *testing.T, tx sqlQueryer, name, nextProbeAt string) int64 {
	t.Helper()
	var id int64
	extra := fmt.Sprintf(`{
		"upstream_billing_probe_enabled": true,
		"upstream_billing_probe": {"status": "ok", "next_probe_at": %q}
	}`, nextProbeAt)
	err := scanSingleRow(ctx, tx, `
		INSERT INTO accounts (name, platform, type, status, extra)
		VALUES ($1, 'openai', $2, 'active', $3::jsonb)
		RETURNING id
	`, []any{name, service.AccountTypeAPIKey, extra}, &id)
	require.NoError(t, err)
	return id
}

// rfc3339WithFraction renders t in UTC with an explicit fractional-second
// suffix, e.g. "2026-07-25T17:29:00.123456789Z". The probes' Go writer uses
// RFC3339Nano, so stored fractions carry up to 9 digits.
func rfc3339WithFraction(t time.Time, fraction string) string {
	return fmt.Sprintf("%s.%sZ", t.UTC().Format("2006-01-02T15:04:05"), fraction)
}

// Regression for the scheduled-probe starvation bug: Go persists next_probe_at
// via RFC3339Nano (7-9 fractional digits), which jsonpath datetime() cannot
// parse. Before the trim fix every such row was treated as malformed and
// fail-open "due", so the cycle always returned the lowest account IDs and
// higher IDs were never probed.
func TestListDueUpstreamBillingProbeAccountsParsesNanosecondTimestamps(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Date(2026, time.July, 25, 17, 30, 0, 0, time.UTC)
	_, err := tx.ExecContext(ctx, `
		UPDATE accounts
		SET extra = extra - 'upstream_billing_probe_enabled' - 'upstream_billing_probe'
	`)
	require.NoError(t, err)

	// 22 low-ID accounts that are NOT due yet, stored with 9 fractional digits
	// exactly as the legacy writer produced them (no migration).
	notDue := now.Add(time.Hour)
	for i := 0; i < 22; i++ {
		insertUpstreamBillingProbeAccount(ctx, t, tx,
			fmt.Sprintf("probe-nano-not-due-%02d", i),
			rfc3339WithFraction(notDue.Add(time.Duration(i)*time.Second), "123456789"))
	}
	// 3 high-ID accounts that ARE due, with 9/8/7 fractional digits. Their
	// parsed order must follow due time, not insertion/ID order.
	dueThird := insertUpstreamBillingProbeAccount(ctx, t, tx,
		"probe-nano-due-9digits", rfc3339WithFraction(now.Add(-time.Minute), "123456789"))
	dueFirst := insertUpstreamBillingProbeAccount(ctx, t, tx,
		"probe-nano-due-8digits", rfc3339WithFraction(now.Add(-3*time.Minute), "12345678"))
	dueSecond := insertUpstreamBillingProbeAccount(ctx, t, tx,
		"probe-nano-due-7digits", rfc3339WithFraction(now.Add(-2*time.Minute), "1234567"))

	accounts, err := repo.ListDueUpstreamBillingProbeAccounts(ctx, now, 20)
	require.NoError(t, err)
	require.Len(t, accounts, 3)
	require.Equal(t, dueFirst, accounts[0].ID)
	require.Equal(t, dueSecond, accounts[1].ID)
	require.Equal(t, dueThird, accounts[2].ID)
}

// With more due accounts than the cycle limit, selection must follow the
// parsed due time so accounts beyond the first <limit> IDs still get their
// turn; before the fix the same lowest IDs monopolized every cycle.
func TestListDueUpstreamBillingProbeAccountsSelectsEarliestDueAcrossIDs(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Date(2026, time.July, 25, 17, 30, 0, 0, time.UTC)
	_, err := tx.ExecContext(ctx, `
		UPDATE accounts
		SET extra = extra - 'upstream_billing_probe_enabled' - 'upstream_billing_probe'
	`)
	require.NoError(t, err)

	// 25 due accounts; the LOWEST IDs carry the LATEST due times, so a
	// correct query must pick the 20 highest-ID rows here.
	ids := make([]int64, 0, 25)
	for i := 0; i < 25; i++ {
		due := now.Add(-time.Duration(i+1) * time.Minute)
		ids = append(ids, insertUpstreamBillingProbeAccount(ctx, t, tx,
			fmt.Sprintf("probe-nano-order-%02d", i),
			rfc3339WithFraction(due, "123456789")))
	}

	accounts, err := repo.ListDueUpstreamBillingProbeAccounts(ctx, now, 20)
	require.NoError(t, err)
	require.Len(t, accounts, 20)
	got := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		got = append(got, account.ID)
	}
	// Earliest due first == highest index first; the five most recently due
	// (lowest index / lowest IDs) fall outside the limit this cycle.
	want := make([]int64, 0, 20)
	for i := 24; i >= 5; i-- {
		want = append(want, ids[i])
	}
	require.Equal(t, want, got)
}

// 探测资格放宽回归：任何 API-key 平台的启用账号都进入定时探测候选并按
// 到期时间排序；OAuth 与未启用账号仍被排除。
func TestListDueUpstreamBillingProbeAccountsIncludesAllAPIKeyPlatforms(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Date(2026, time.July, 26, 3, 0, 0, 0, time.UTC)
	_, err := tx.ExecContext(ctx, `
		UPDATE accounts
		SET extra = extra - 'upstream_billing_probe_enabled' - 'upstream_billing_probe'
	`)
	require.NoError(t, err)

	insert := func(name, platform, accountType, nextProbeAt string) int64 {
		t.Helper()
		var id int64
		extra := fmt.Sprintf(`{
			"upstream_billing_probe_enabled": true,
			"upstream_billing_probe": {"status": "ok", "next_probe_at": %q}
		}`, nextProbeAt)
		err := scanSingleRow(ctx, tx, `
			INSERT INTO accounts (name, platform, type, status, extra)
			VALUES ($1, $2, $3, 'active', $4::jsonb)
			RETURNING id
		`, []any{name, platform, accountType, extra}, &id)
		require.NoError(t, err)
		return id
	}

	openaiDue := insert("probe-openai-due", "openai", service.AccountTypeAPIKey, "2026-07-26T02:57:00Z")
	anthropicDue := insert("probe-anthropic-due", "anthropic", service.AccountTypeAPIKey, "2026-07-26T02:58:00Z")
	grokDue := insert("probe-grok-due", "grok", service.AccountTypeAPIKey, "2026-07-26T02:59:00Z")
	// OAuth 账号即便误持有启用标记也不得入选。
	_ = insert("probe-grok-oauth-excluded", "grok", service.AccountTypeOAuth, "2026-07-26T02:59:00Z")
	// 未启用探测的 API-key 账号不入选。
	var disabledID int64
	err = scanSingleRow(ctx, tx, `
		INSERT INTO accounts (name, platform, type, status, extra)
		VALUES ('probe-grok-disabled', 'grok', $1, 'active', '{}'::jsonb)
		RETURNING id
	`, []any{service.AccountTypeAPIKey}, &disabledID)
	require.NoError(t, err)

	accounts, err := repo.ListDueUpstreamBillingProbeAccounts(ctx, now, 20)
	require.NoError(t, err)
	require.Len(t, accounts, 3)
	require.Equal(t, openaiDue, accounts[0].ID)
	require.Equal(t, anthropicDue, accounts[1].ID)
	require.Equal(t, grokDue, accounts[2].ID)
}

func TestUpstreamBillingProbeHundredAccountsRoundTripFairness(t *testing.T) {
	ctx := t.Context()
	db, client := newBillingAuditDatabase(t)
	repo := newAccountRepositoryWithSQL(client, db, nil)
	var rateCalls, balanceCalls [103]atomic.Int64
	var active, maximum, unexpected atomic.Int64
	var recovered atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
		}
		// Allow requests from independent probe slots to overlap.
		time.Sleep(3 * time.Millisecond)
		index, err := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer fixture-probe-"))
		if err != nil || index < 0 || index >= len(rateCalls) || r.Method != http.MethodGet {
			unexpected.Add(1)
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/v1/sub2api/billing":
			rateCalls[index].Add(1)
		case "/v1/usage":
			balanceCalls[index].Add(1)
		default:
			unexpected.Add(1)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if index < 20 && !recovered.Load() {
			status := http.StatusServiceUnavailable
			if index < 10 {
				status = http.StatusTooManyRequests
			}
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"fixture unavailable"}`))
			return
		}
		if r.URL.Path == "/v1/usage" {
			_, _ = w.Write([]byte(`{"mode":"unrestricted","isValid":true,"remaining":12.34,"unit":"USD","balance":12.34}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":0.03,"resolved_rate_multiplier":0.03,"peak_rate_enabled":false,"effective_rate_multiplier":0.03,"observed_at":%q}`, time.Now().UTC().Format(time.RFC3339Nano))
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	upstream, ok := NewHTTPUpstream(cfg).(*httpUpstreamService)
	require.True(t, ok)
	t.Cleanup(func() {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()
		for _, entry := range upstream.clients {
			entry.client.CloseIdleConnections()
		}
	})
	settings := service.NewSettingService(NewSettingRepository(client), cfg)
	require.NoError(t, settings.SetUpstreamBillingProbeSettings(ctx, &service.UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}))
	ids := make([]int64, len(rateCalls))
	for i := range ids {
		credentials, err := json.Marshal(map[string]string{"api_key": fmt.Sprintf("fixture-probe-%d", i), "base_url": server.URL})
		require.NoError(t, err)
		accountType, status, enabled := service.AccountTypeAPIKey, service.StatusActive, i != 100
		if i == 101 {
			status = "disabled"
		}
		if i == 102 {
			accountType = service.AccountTypeOAuth
		}
		extra := fmt.Sprintf(`{"upstream_billing_probe_enabled":%t,"upstream_billing_rate_sync_enabled":false}`, enabled)
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra,rate_multiplier) VALUES($1,'openai',$2,$3,$4::jsonb,$5::jsonb,1) RETURNING id`, fmt.Sprintf("probe-fairness-%d", i), accountType, status, string(credentials), extra).Scan(&ids[i]))
	}
	run := func(t *testing.T) {
		t.Helper()
		// A new service and repository on each cycle must use persisted due
		// times, rather than a process-local cursor or the previous object.
		freshRepo := newAccountRepositoryWithSQL(client, db, nil)
		tester := service.NewAccountTestService(freshRepo, nil, nil, nil, nil, upstream, cfg, nil)
		runner := service.NewUpstreamBillingProbeService(freshRepo, tester, service.NewSettingService(NewSettingRepository(client), cfg))
		defer runner.Stop()
		require.NoError(t, runner.RunDue(ctx))
	}
	readSnapshot := func(t *testing.T, index int) service.UpstreamBillingProbeSnapshot {
		t.Helper()
		var raw []byte
		require.NoError(t, db.QueryRowContext(ctx, `SELECT extra->'upstream_billing_probe' FROM accounts WHERE id=$1`, ids[index]).Scan(&raw))
		var snapshot service.UpstreamBillingProbeSnapshot
		require.NoError(t, json.Unmarshal(raw, &snapshot))
		return snapshot
	}
	for cycle := 0; cycle < 5; cycle++ {
		t.Run(fmt.Sprintf("cycle_%d", cycle+1), func(t *testing.T) {
			run(t)
			for i := 0; i < 100; i++ {
				want := int64(0)
				if i < (cycle+1)*20 {
					want = 1
				}
				require.Equal(t, want, rateCalls[i].Load(), "account %d must get exactly one turn", i)
			}
			due, err := repo.ListDueUpstreamBillingProbeAccounts(ctx, time.Now(), 101)
			require.NoError(t, err)
			require.Len(t, due, 100-(cycle+1)*20)
		})
	}
	t.Run("persisted_backoff_and_physical_calls", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			snapshot := readSnapshot(t, i)
			require.True(t, snapshot.NextProbeAt.After(time.Now()))
			if i < 20 {
				require.Equal(t, service.UpstreamBillingProbeStatusFailed, snapshot.Status)
				require.Equal(t, 1, snapshot.FailureCount)
				require.Nil(t, snapshot.FreshUntil)
				require.GreaterOrEqual(t, snapshot.NextProbeAt.Sub(snapshot.LastAttemptAt), 10*time.Minute)
			} else {
				require.Equal(t, service.UpstreamBillingProbeStatusOK, snapshot.Status)
			}
			wantBalance := int64(1)
			if i < 20 {
				wantBalance = 0
			}
			require.Equal(t, wantBalance, balanceCalls[i].Load(), "429 and service Retry-After must not cause another upstream call")
		}
		run(t)
		for i := 0; i < 100; i++ {
			require.Equal(t, int64(1), rateCalls[i].Load(), "no early retry after restarting the runner")
		}
		t.Log("100 distinct accounts covered in five bounded cycles; 180 physical HTTP calls, including balance calls")
	})
	t.Run("expired_failures_recover_without_reprobing_healthy_accounts", func(t *testing.T) {
		recovered.Store(true)
		for i := 0; i < 20; i++ {
			// Use the writer's nanosecond format at the SQL scheduling boundary.
			at := time.Now().Add(-time.Minute).Truncate(time.Second).Add(123456789 * time.Nanosecond)
			_, err := db.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{upstream_billing_probe,next_probe_at}',to_jsonb($1::text)) WHERE id=$2`, at.UTC().Format(time.RFC3339Nano), ids[i])
			require.NoError(t, err)
		}
		run(t)
		for i := 0; i < 100; i++ {
			want := int64(1)
			if i < 20 {
				want = 2
				snapshot := readSnapshot(t, i)
				require.Equal(t, service.UpstreamBillingProbeStatusOK, snapshot.Status)
				require.Zero(t, snapshot.FailureCount)
				require.NotNil(t, snapshot.FreshUntil)
			}
			require.Equal(t, want, rateCalls[i].Load())
			require.Equal(t, int64(1), balanceCalls[i].Load(), "only recovered accounts refresh their balances")
		}
		due, err := repo.ListDueUpstreamBillingProbeAccounts(ctx, time.Now(), 101)
		require.NoError(t, err)
		require.Empty(t, due)
	})
	for i := 100; i < len(rateCalls); i++ {
		require.Zero(t, rateCalls[i].Load(), "disabled and OAuth accounts must remain excluded")
		require.Zero(t, balanceCalls[i].Load())
	}
	require.Zero(t, unexpected.Load())
	require.Zero(t, active.Load())
	require.LessOrEqual(t, maximum.Load(), int64(4), "the original shared probe-slot limit must cover both HTTP paths")
	t.Logf("maximum simultaneous HTTP requests: %d; no external provider or production requests", maximum.Load())
}
