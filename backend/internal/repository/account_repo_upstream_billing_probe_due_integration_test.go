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
			INSERT INTO accounts (name, platform, type, status, extra, created_at)
			VALUES ($1, 'openai', $2, 'active', $3::jsonb, $4)
			RETURNING id
		`, []any{name, service.AccountTypeAPIKey, extra, now.Add(-time.Hour)}, &id)
		require.NoError(t, err)
		return id
	}

	invalidID := insert("probe-invalid-calendar-date", "2026-99-99T12:00:00Z")
	dueID := insert("probe-due", "2026-07-14T11:59:59Z")
	_ = insert("probe-not-due", "2026-07-14T12:00:01Z")

	accounts, err := repo.ListDueUpstreamBillingProbeAccounts(ctx, now, 20)
	require.NoError(t, err)
	require.Len(t, accounts, 2)
	// An invalid schedule remains exploratory; a due existing review now has
	// priority even when the malformed account was created earlier.
	require.Equal(t, dueID, accounts[0].ID)
	require.Equal(t, invalidID, accounts[1].ID)
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

func TestListDueUpstreamBillingProbeAccountsDoesNotStarveRecoveryWithNewInventory(t *testing.T) {
	for _, kind := range []string{"supplier_failure", "budget_deferral", "invalid_snapshot", "zero_snapshot", "unknown_status"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			tx := testEntTx(t)
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			now := time.Now().UTC().Truncate(time.Microsecond)
			_, err := tx.ExecContext(ctx, `UPDATE accounts SET extra=extra-'upstream_billing_probe_enabled'`)
			require.NoError(t, err)
			for i := 0; i < service.UpstreamBillingProbeMaxBatchSize; i++ {
				_, err := tx.ExecContext(ctx, `INSERT INTO accounts(name,platform,type,status,extra,created_at)
 VALUES($1,'openai','apikey','active','{"upstream_billing_probe_enabled":true}'::jsonb,$2)`,
					fmt.Sprintf("fairness-new-%d", i), now.Add(-time.Minute))
				require.NoError(t, err)
			}
			extra := map[string]any{service.UpstreamBillingProbeEnabledExtraKey: true}
			switch kind {
			case "supplier_failure":
				extra[service.UpstreamBillingProbeExtraKey] = map[string]any{
					"status": "failed", "next_probe_at": now.Add(-10 * time.Minute), "failure_count": 3,
				}
			case "budget_deferral":
				extra[service.UpstreamBillingProbeDeferralExtraKey] = map[string]any{
					"reason": "request_limit", "scope": "global_minute", "retry_at": now.Add(-5 * time.Minute),
				}
			case "invalid_snapshot":
				extra[service.UpstreamBillingProbeExtraKey] = map[string]any{"status": "ok", "next_probe_at": "invalid"}
			case "zero_snapshot":
				extra[service.UpstreamBillingProbeExtraKey] = &service.UpstreamBillingProbeSnapshot{Status: service.UpstreamBillingProbeStatusOK}
			case "unknown_status":
				extra[service.UpstreamBillingProbeExtraKey] = map[string]any{"status": "future-status", "next_probe_at": now.Add(time.Hour)}
			}
			raw, err := json.Marshal(extra)
			require.NoError(t, err)
			var recoveryID int64
			require.NoError(t, scanSingleRow(ctx, tx, `INSERT INTO accounts(name,platform,type,status,extra,created_at)
 VALUES('fairness-recovery','openai','apikey','active',$1::jsonb,$2) RETURNING id`,
				[]any{raw, now.Add(-time.Hour)}, &recoveryID))

			accounts, err := repo.ListDueUpstreamBillingProbeAccounts(ctx, now, service.UpstreamBillingProbeMaxBatchSize)
			require.NoError(t, err)
			require.Len(t, accounts, service.UpstreamBillingProbeMaxBatchSize)
			require.Equal(t, recoveryID, accounts[0].ID, "expired recovery must precede newer unobserved inventory before LIMIT")
		})
	}
}

func TestListDueUpstreamBillingProbeAccountsPrioritizesRechecksAndEvidenceDeadlines(t *testing.T) {
	for _, scenario := range []string{"failure_before_exploration", "refresh_before_exploration", "earliest_evidence_deadline"} {
		t.Run(scenario, func(t *testing.T) {
			tx := testEntTx(t)
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			now := time.Date(2026, time.November, 24, 12, 0, 0, 123456000, time.UTC)
			_, err := tx.ExecContext(t.Context(), `UPDATE accounts SET extra=extra-'upstream_billing_probe_enabled'`)
			require.NoError(t, err)
			var priorityID int64
			for i := 0; i <= service.UpstreamBillingProbeMaxBatchSize; i++ {
				extra := map[string]any{service.UpstreamBillingProbeEnabledExtraKey: true}
				if scenario == "earliest_evidence_deadline" || i == service.UpstreamBillingProbeMaxBatchSize {
					snapshot := &service.UpstreamBillingProbeSnapshot{Status: service.UpstreamBillingProbeStatusOK,
						NextProbeAt: now.Add(-time.Minute), LastAttemptAt: now.Add(-time.Hour)}
					received, fresh := now.Add(-time.Hour), now.Add(time.Hour)
					if scenario == "failure_before_exploration" {
						snapshot.Status = service.UpstreamBillingProbeStatusFailed
					}
					if scenario == "earliest_evidence_deadline" && i == service.UpstreamBillingProbeMaxBatchSize {
						fresh = now.Add(-time.Minute)
					}
					snapshot.ReceivedAt, snapshot.FreshUntil = &received, &fresh
					extra[service.UpstreamBillingProbeExtraKey] = snapshot
				}
				raw, err := json.Marshal(extra)
				require.NoError(t, err)
				require.NoError(t, scanSingleRow(t.Context(), tx, `INSERT INTO accounts(name,platform,type,status,extra,created_at)
 VALUES($1,'openai','apikey','active',$2::jsonb,$3) RETURNING id`,
					[]any{fmt.Sprintf("priority-%d", i), raw, now.Add(-24 * time.Hour)}, &priorityID))
			}
			accounts, err := repo.ListDueUpstreamBillingProbeAccounts(t.Context(), now, service.UpstreamBillingProbeMaxBatchSize)
			require.NoError(t, err)
			require.Len(t, accounts, service.UpstreamBillingProbeMaxBatchSize)
			require.Equal(t, priorityID, accounts[0].ID, "review/expiry priority must apply before SQL LIMIT, not only after hydration")
		})
	}
}

func TestListDueUpstreamBillingProbeAccountsPriorityClocksAndDeferrals(t *testing.T) {
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Date(2026, time.November, 24, 12, 0, 0, 123456000, time.UTC)
	_, err := tx.ExecContext(t.Context(), `UPDATE accounts SET extra=extra-'upstream_billing_probe_enabled'`)
	require.NoError(t, err)
	insert := func(name string, snapshot map[string]any, deferred any, created time.Time) int64 {
		t.Helper()
		extra := map[string]any{service.UpstreamBillingProbeEnabledExtraKey: true,
			service.UpstreamBillingProbeExtraKey: snapshot, service.UpstreamBillingProbeDeferralExtraKey: deferred}
		raw, err := json.Marshal(extra)
		require.NoError(t, err)
		var id int64
		require.NoError(t, scanSingleRow(t.Context(), tx, `INSERT INTO accounts(name,platform,type,status,extra,created_at)
 VALUES($1,'openai','apikey','active',$2::jsonb,$3) RETURNING id`, []any{name, raw, created}, &id))
		return id
	}
	newSnapshot := func(due time.Duration, received, fresh any) map[string]any {
		return map[string]any{"status": "ok", "next_probe_at": now.Add(due), "received_at": received, "fresh_until": fresh}
	}
	created := now.Add(-24 * time.Hour)
	exploration := insert("old-exploration", nil, nil, now.Add(-240*time.Hour))
	unsupported := insert("unsupported", map[string]any{"status": "unsupported", "next_probe_at": now.Add(-48 * time.Hour)}, nil, created)
	zero := insert("zero-next", newSnapshot(0, nil, nil), nil, created)
	_, err = tx.ExecContext(t.Context(), `UPDATE accounts SET extra=jsonb_set(extra,'{upstream_billing_probe,next_probe_at}','"0001-01-01T00:00:00Z"') WHERE id=$1`, zero)
	require.NoError(t, err)
	invalid := insert("invalid-next", map[string]any{"status": "ok", "next_probe_at": "2026-99-99T12:00:00Z"}, nil, created)
	unknown := insert("unknown-status", map[string]any{"status": "future-version", "next_probe_at": now.Add(time.Hour)}, nil, created)
	latest := insert("live-deadline", newSnapshot(-2*time.Minute, now.Add(-time.Hour), now.Add(time.Hour)), nil, created)
	expired := insert("expired-deadline", newSnapshot(-time.Minute, now.Add(-time.Hour), now.Add(-time.Minute)), nil, created)
	failed := insert("failed-review", map[string]any{"status": "failed", "next_probe_at": now.Add(-10 * time.Minute)}, nil, created)
	invalidFresh := insert("invalid-fresh", newSnapshot(-20*time.Minute, now.Add(-time.Hour), "invalid"), nil, created)
	futureReceived := insert("future-received", newSnapshot(-30*time.Minute, now.Add(time.Minute), now.Add(time.Hour)), nil, created)
	reversed := insert("reversed-window", newSnapshot(-40*time.Minute, now.Add(-time.Hour), now.Add(-2*time.Hour)), nil, created)
	nanoFirst := insert("nano-id-first", newSnapshot(-50*time.Minute+789*time.Nanosecond, nil, nil), nil, created)
	nanoSecond := insert("nano-id-second", newSnapshot(-50*time.Minute+123*time.Nanosecond, nil, nil), nil, created)
	deferred := insert("expired-budget-review", newSnapshot(-60*time.Minute, nil, nil), map[string]any{"retry_at": now.Add(-25 * time.Minute)}, created)
	offset := time.FixedZone("fixture", 5*60*60+45*60)
	timezoneID := insert("offset-deadline", newSnapshot(-2*time.Minute, now.Add(-time.Hour).In(offset), now.Add(-5*time.Minute).In(offset)), nil, created)
	_ = insert("not-due-damaged-evidence", newSnapshot(time.Hour, false, "invalid"), nil, created)
	_ = insert("future-budget", newSnapshot(-time.Hour, nil, nil), map[string]any{"retry_at": now.Add(time.Hour)}, created)
	read := func() string {
		t.Helper()
		var value string
		require.NoError(t, scanSingleRow(t.Context(), tx, `SELECT jsonb_agg(jsonb_build_array(id,extra) ORDER BY id)::text FROM accounts`, nil, &value))
		return value
	}
	before := read()
	accounts, err := repo.ListDueUpstreamBillingProbeAccounts(t.Context(), now, 100)
	require.NoError(t, err)
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	require.Equal(t, []int64{nanoFirst, nanoSecond, reversed, futureReceived, deferred, invalidFresh, failed, timezoneID, expired, latest,
		exploration, unsupported, zero, invalid, unknown}, ids)
	require.Equal(t, before, read(), "selection alone cannot renew stale evidence, clear backoff, or mutate account metadata")
}

func TestUpstreamBillingProbeHundredAccountsRoundTripFairness(t *testing.T) {
	ctx := t.Context()
	db, client := newBillingAuditDatabase(t)
	repo := newAccountRepositoryWithSQL(client, db, nil)
	var rateCalls, balanceCalls [123]atomic.Int64
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
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerMinute, policy.RequestsPerDay, policy.SiteRequestsPerMinute = 220, 220, 220
	policy.KeyRequestsPerMinute = 3
	policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency = 4, 4, 1
	policy, err := budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	ids := make([]int64, len(rateCalls))
	for i := 0; i < 103; i++ {
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
	newRunner := func(t *testing.T) *service.UpstreamBillingProbeService {
		t.Helper()
		// A new service and repository on each cycle must use persisted due
		// times and shared physical budget, not a process-local cursor/counter.
		freshRepo := newAccountRepositoryWithSQL(client, db, nil)
		tester := service.NewAccountTestService(freshRepo, nil, nil, nil, nil, upstream, cfg, nil)
		runner := service.NewUpstreamBillingProbeService(freshRepo, tester, service.NewSettingService(NewSettingRepository(client), cfg))
		runner.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)))
		t.Cleanup(runner.Stop)
		return runner
	}
	run := func(t *testing.T) {
		t.Helper()
		require.NoError(t, newRunner(t).RunDue(ctx))
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
		for i := 103; i < len(ids); i++ {
			credentials, err := json.Marshal(map[string]string{"api_key": fmt.Sprintf("fixture-probe-%d", i), "base_url": server.URL})
			require.NoError(t, err)
			require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra,rate_multiplier)
 VALUES($1,'openai','apikey','active',$2::jsonb,'{"upstream_billing_probe_enabled":true}'::jsonb,1) RETURNING id`,
				fmt.Sprintf("probe-new-inventory-%d", i), credentials).Scan(&ids[i]))
		}
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
		require.Len(t, due, 20, "new inventory waits behind already-due supplier recovery")
		for i := 103; i < len(ids); i++ {
			require.Zero(t, rateCalls[i].Load(), "new inventory must not displace overdue recovery")
		}
	})
	t.Run("shared_budget_exhaustion_and_persisted_recovery", func(t *testing.T) {
		before := make([]service.UpstreamBillingProbeSnapshot, 100)
		for i := range before {
			before[i] = readSnapshot(t, i)
		}
		run(t)
		for i := range before {
			require.Equal(t, before[i], readSnapshot(t, i), "budget denial must not renew or invalidate existing evidence")
		}
		for i := 103; i < len(ids); i++ {
			require.Zero(t, rateCalls[i].Load())
			require.Zero(t, balanceCalls[i].Load())
			account, err := repo.GetByID(ctx, ids[i])
			require.NoError(t, err)
			require.NotNil(t, account.Extra[service.UpstreamBillingProbeDeferralExtraKey])
			require.NotContains(t, account.Extra, service.UpstreamBillingProbeExtraKey)
		}
		summary, err := NewMonitoringProbeBudgetRepository(db).Summary(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 220, summary.DayUsed, "physical rate and balance calls share one budget across service rebuilds")
		require.Zero(t, summary.ActiveRequests)
		run(t)
		policy.RequestsPerMinute, policy.RequestsPerDay, policy.SiteRequestsPerMinute = 260, 260, 260
		_, err = budget.SaveSettings(ctx, policy)
		require.ErrorContains(t, err, "day_capacity", "new inventory made the old five-minute promise impossible")
		// The existing under-provisioned configuration stays enforceable at
		// runtime. Repair it through the real admin contract: observe-only,
		// relax cadence, then re-enable without resetting the shared counters.
		policy.Enabled = false
		policy, err = budget.SaveSettings(ctx, policy)
		require.NoError(t, err)
		require.NoError(t, budget.SaveBillingProbeSettings(ctx, service.UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 1440}))
		policy.Enabled = true
		_, err = budget.SaveSettings(ctx, policy)
		require.NoError(t, err)
		for i := 103; i < len(ids); i++ {
			// Advance only the private fixture's retry state, as in the original
			// supplier-backoff case; this is not a production wall-clock SLA.
			_, err := db.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{upstream_billing_probe_deferral,retry_at}',to_jsonb($1::text)) WHERE id=$2`,
				time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), ids[i])
			require.NoError(t, err)
		}
		run(t)
		for i := 103; i < len(ids); i++ {
			require.EqualValues(t, 1, rateCalls[i].Load())
			require.EqualValues(t, 1, balanceCalls[i].Load())
			require.Equal(t, service.UpstreamBillingProbeStatusOK, readSnapshot(t, i).Status)
			account, err := repo.GetByID(ctx, ids[i])
			require.NoError(t, err)
			require.Nil(t, account.Extra[service.UpstreamBillingProbeDeferralExtraKey])
		}
		summary, err = budget.Summary(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 260, summary.DayUsed)
		require.Zero(t, summary.ActiveRequests)
	})
	for i := 100; i < 103; i++ {
		require.Zero(t, rateCalls[i].Load(), "disabled and OAuth accounts must remain excluded")
		require.Zero(t, balanceCalls[i].Load())
	}
	require.Zero(t, unexpected.Load())
	require.Zero(t, active.Load())
	require.LessOrEqual(t, maximum.Load(), int64(4), "the original shared probe-slot limit must cover both HTTP paths")
	t.Logf("maximum simultaneous HTTP requests: %d; no external provider or production requests", maximum.Load())
}

func TestUpstreamBillingProbeReviewPriorityHTTPPostgresBudgetRecovery(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	var rateCalls, balanceCalls [6]atomic.Int64
	var outOfOrder atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index, err := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer fixture-priority-"))
		if err != nil || index < 0 || index >= len(rateCalls) || r.Method != http.MethodGet {
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		if index < 4 && (balanceCalls[4].Load() == 0 || balanceCalls[5].Load() == 0) {
			outOfOrder.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/sub2api/billing":
			rateCalls[index].Add(1)
			_, _ = fmt.Fprintf(w, `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":0.03,"resolved_rate_multiplier":0.03,"peak_rate_enabled":false,"effective_rate_multiplier":0.03,"observed_at":%q}`, time.Now().UTC().Format(time.RFC3339Nano))
		case "/v1/usage":
			balanceCalls[index].Add(1)
			_, _ = w.Write([]byte(`{"mode":"unrestricted","isValid":true,"remaining":12.34,"unit":"USD","balance":12.34}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	upstream := NewHTTPUpstream(cfg).(*httpUpstreamService)
	t.Cleanup(func() {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()
		for _, entry := range upstream.clients {
			entry.client.CloseIdleConnections()
		}
	})
	settings := service.NewSettingService(NewSettingRepository(client), cfg)
	require.NoError(t, settings.SetUpstreamBillingProbeSettings(ctx, &service.UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}))
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay, policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency = 4, 4, 4, 1
	policy, err := budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	var ids [6]int64
	for i := range ids {
		// Simulate inventory growth after a previously valid budget was saved.
		// The runtime must still enforce it rather than promise full coverage.
		extra := map[string]any{service.UpstreamBillingProbeEnabledExtraKey: true}
		if i >= 4 {
			received, fresh := time.Now().Add(-time.Hour), time.Now().Add(-time.Minute)
			extra[service.UpstreamBillingProbeExtraKey] = &service.UpstreamBillingProbeSnapshot{
				Status: service.UpstreamBillingProbeStatusOK, NextProbeAt: time.Now().Add(-time.Minute),
				ReceivedAt: &received, FreshUntil: &fresh}
		}
		raw, err := json.Marshal(extra)
		require.NoError(t, err)
		credentials, err := json.Marshal(map[string]string{"api_key": fmt.Sprintf("fixture-priority-%d", i), "base_url": server.URL})
		require.NoError(t, err)
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra,rate_multiplier,created_at)
 VALUES($1,'openai','apikey','active',$2::jsonb,$3::jsonb,1,$4) RETURNING id`,
			fmt.Sprintf("priority-%d", i), credentials, raw, time.Now().Add(-30*24*time.Hour)).Scan(&ids[i]))
	}
	run := func() {
		t.Helper()
		accounts := newAccountRepositoryWithSQL(client, db, nil)
		tester := service.NewAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil)
		runner := service.NewUpstreamBillingProbeService(accounts, tester, service.NewSettingService(NewSettingRepository(client), cfg))
		runner.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)))
		t.Cleanup(runner.Stop)
		require.NoError(t, runner.RunDue(ctx))
	}
	run()
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	for i, id := range ids {
		account, err := accounts.GetByID(ctx, id)
		require.NoError(t, err)
		if i < 4 {
			require.Zero(t, rateCalls[i].Load(), "exploration cannot steal a necessary review's physical quota")
			require.NotContains(t, account.Extra, service.UpstreamBillingProbeExtraKey)
			require.Contains(t, account.Extra, service.UpstreamBillingProbeDeferralExtraKey)
		} else {
			require.EqualValues(t, 1, rateCalls[i].Load())
			require.EqualValues(t, 1, balanceCalls[i].Load())
			require.Contains(t, account.Extra, service.UpstreamBillingProbeExtraKey)
		}
		require.EqualValues(t, 1, account.BillingRateMultiplier())
	}
	run() // Rebuilding the service must not erase persisted windows or overspend.
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 4, summary.DayUsed)
	policy.Enabled = false
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	require.NoError(t, budget.SaveBillingProbeSettings(ctx, service.UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 1440}))
	policy.Enabled, policy.RequestsPerDay = true, 12
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	run()
	summary, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 4, summary.DayUsed, "raising capacity never cancels a persisted retry window")
	for _, id := range ids[:4] {
		// Move only this private fixture's retry clock; this is not a live
		// cross-midnight test or an early-retry path in the application.
		_, err := db.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{upstream_billing_probe_deferral,retry_at}',to_jsonb($1::text)) WHERE id=$2`,
			time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), id)
		require.NoError(t, err)
	}
	run()
	for i := range ids {
		require.EqualValues(t, 1, rateCalls[i].Load())
		require.EqualValues(t, 1, balanceCalls[i].Load())
	}
	require.False(t, outOfOrder.Load())
	summary, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 12, summary.DayUsed)
	require.Zero(t, summary.ActiveRequests)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.DayTokens)
	require.Zero(t, monitoringFairWaiters(t, db))
	var downstream int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&downstream))
	require.Zero(t, downstream)
}
