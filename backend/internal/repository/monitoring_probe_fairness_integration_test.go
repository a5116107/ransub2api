//go:build integration

package repository

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestMonitoringProbeBudgetSharedFIFOIndependentProcessesHTTP(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	repo := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.Concurrency = 1
	_, err := repo.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	seed, err := repo.Acquire(t.Context(), service.MonitoringProbeBudgetRequest{SiteHash: "seed-site", KeyHashes: []string{"seed-key"}})
	require.NoError(t, err)
	entered, release := make(chan string, 2), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- r.URL.Path
		if r.URL.Path == "/v1/responses" {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"completed","usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7}}`)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(unblock)
	command := monitoringBudgetNodeCommand{DSN: monitoringBudgetNodeDSN(t, db), Site: server.URL,
		Path: "/v1/responses", Method: http.MethodPost, Header: http.Header{"Authorization": {"Bearer shared-fair-fixture"}}, Requests: 1, TimeoutMillis: 5000}
	replies := make(chan monitoringBudgetNodeReply, 2)
	older := startMonitoringBudgetNode(t, command, replies)
	require.Eventually(t, func() bool { return monitoringFairWaiters(t, db) == 1 }, time.Second, 5*time.Millisecond)
	command.Path, command.Method = "/quota", http.MethodGet
	newer := startMonitoringBudgetNode(t, command, replies)
	require.NotEqual(t, older.pid, newer.pid)
	require.Eventually(t, func() bool { return monitoringFairWaiters(t, db) == 2 }, time.Second, 5*time.Millisecond)
	require.NoError(t, repo.Release(t.Context(), seed, service.MonitoringProbeTokenUsage{}))
	select {
	case path := <-entered:
		require.Equal(t, "/v1/responses", path, "later quota queries cannot steal a slot from the earlier eligible model call")
	case <-time.After(time.Second):
		t.Fatal("first independent physical caller did not receive the released slot")
	}
	select {
	case path := <-entered:
		t.Fatalf("only one physical request may enter while its predecessor is still active: %s", path)
	default:
	}
	unblock()
	select {
	case path := <-entered:
		require.Equal(t, "/quota", path)
	case <-time.After(time.Second):
		t.Fatal("second independent caller did not recover")
	}
	older.wait()
	newer.wait()
	for range 2 {
		reply := receiveMonitoringBudgetReply(t, replies)
		require.Equal(t, http.StatusOK, reply.Status)
		require.Empty(t, reply.Reason)
	}
	require.Zero(t, monitoringFairWaiters(t, db))
	summary, err := repo.Summary(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 3, summary.DayUsed)
	require.EqualValues(t, 7, summary.DayTokens)
	require.EqualValues(t, 1, summary.UsageReported)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.ActiveRequests)
	var downstream int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_logs`).Scan(&downstream))
	require.Zero(t, downstream)
}

func TestMonitoringProbeBudgetSharedWaitDoesNotInterceptBusiness(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	repo := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.Concurrency = 1
	_, err := repo.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	held, err := repo.Acquire(t.Context(), service.MonitoringProbeBudgetRequest{SiteHash: "held-site", KeyHashes: []string{"held-key"}})
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(server.Close)
	// Outbound requests without the internal monitoring context never reach
	// this repository, even if headers try to supply a budget/fairness policy.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer business-fixture")
	req.Header.Set("X-Monitoring-Probe", "true")
	req.Header.Set("X-Monitoring-Fairness", "fifo")
	budgeted, finish, err := service.BeginMonitoringProbeRequest(req)
	require.NoError(t, err)
	require.Nil(t, finish)
	require.Same(t, req, budgeted)
	response, err := server.Client().Do(budgeted)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	require.Zero(t, monitoringFairWaiters(t, db))
	require.NoError(t, repo.Release(t.Context(), held, service.MonitoringProbeTokenUsage{}))
	summary, err := repo.Summary(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, summary.DayUsed)
	// A changed/unknown credential is still rejected before registering any
	// ticket; FIFO never substitutes an inferred key for final credentials.
	bad := req.Clone(service.NewMonitoringProbeBudgetService(repo).Context(t.Context(), server.URL, false))
	bad.Header.Set("Authorization", "Custom opaque")
	_, finish, err = service.BeginMonitoringProbeRequest(bad)
	require.ErrorIs(t, err, service.ErrMonitoringProbeBudgetUnavailable)
	require.Nil(t, finish)
	require.Zero(t, monitoringFairWaiters(t, db))
}

type monitoringFairAdmission struct {
	id  string
	err error
}

func monitoringFairWaiters(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM monitoring_probe_budget_waiters`).Scan(&count))
	return count
}

func monitoringFairStart(t *testing.T, repo service.MonitoringProbeBudgetRepository, ctx context.Context, request service.MonitoringProbeBudgetRequest) <-chan monitoringFairAdmission {
	t.Helper()
	done := make(chan monitoringFairAdmission, 1)
	go func() { id, err := repo.Acquire(ctx, request); done <- monitoringFairAdmission{id, err} }()
	return done
}

func receiveMonitoringFairAdmission(t *testing.T, done <-chan monitoringFairAdmission) monitoringFairAdmission {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("bounded physical admission did not return")
		return monitoringFairAdmission{}
	}
}

func insertMonitoringFairWaiter(t *testing.T, db *sql.DB, request service.MonitoringProbeBudgetRequest, expires time.Duration) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.ExecContext(t.Context(), `INSERT INTO monitoring_probe_budget_waiters(id,site_hash,key_hashes,recovery,meter_tokens,expires_at)
 VALUES($1,$2,$3,$4,$5,clock_timestamp()+$6*interval '1 second')`, id, request.SiteHash, pq.Array(request.KeyHashes), request.Recovery, request.MeterTokens, expires.Seconds())
	require.NoError(t, err)
	return id
}

func TestMonitoringProbeBudgetSharedFIFOAcrossRepositories(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	repo := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.Concurrency = 1
	_, err := repo.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	request := service.MonitoringProbeBudgetRequest{SiteHash: "busy-site", KeyHashes: []string{"shared-key"}}
	first, err := repo.Acquire(t.Context(), request)
	require.NoError(t, err)
	older := monitoringFairStart(t, NewMonitoringProbeBudgetRepository(db), t.Context(), request)
	require.Eventually(t, func() bool { return monitoringFairWaiters(t, db) == 1 }, time.Second, 5*time.Millisecond)
	newerRequest := service.MonitoringProbeBudgetRequest{SiteHash: "new-site", KeyHashes: []string{"new-key"}, MeterTokens: true}
	newer := monitoringFairStart(t, NewMonitoringProbeBudgetRepository(db), t.Context(), newerRequest)
	require.Eventually(t, func() bool { return monitoringFairWaiters(t, db) == 2 }, time.Second, 5*time.Millisecond)
	require.NoError(t, repo.Release(t.Context(), first, service.MonitoringProbeTokenUsage{}))
	oldResult := receiveMonitoringFairAdmission(t, older)
	require.NoError(t, oldResult.err)
	require.NotEmpty(t, oldResult.id)
	select {
	case result := <-newer:
		t.Fatalf("a later generation may not pass the earlier eligible management call: %+v", result)
	default:
	}
	require.NoError(t, repo.Release(t.Context(), oldResult.id, service.MonitoringProbeTokenUsage{}))
	newResult := receiveMonitoringFairAdmission(t, newer)
	require.NoError(t, newResult.err)
	require.NoError(t, repo.Release(t.Context(), newResult.id, service.MonitoringProbeTokenUsage{Tokens: 7, Reported: true}))
	require.Zero(t, monitoringFairWaiters(t, db))
	summary, err := repo.Summary(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 3, summary.DayUsed)
	require.EqualValues(t, 7, summary.DayTokens)
	require.EqualValues(t, 1, summary.UsageReported)
	require.Zero(t, summary.ActiveRequests)
}

func TestMonitoringProbeBudgetSharedWaitLifecycle(t *testing.T) {
	for _, action := range []string{"cancel", "deadline", "wait_exhausted", "policy_off", "rate_changed", "tokens_changed"} {
		t.Run(action, func(t *testing.T) {
			db, _ := newBillingAuditDatabase(t)
			repo := NewMonitoringProbeBudgetRepository(db)
			policy := monitoringBudgetPolicy()
			policy.Concurrency = 1
			policy, err := repo.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			request := service.MonitoringProbeBudgetRequest{SiteHash: "waiting-site", KeyHashes: []string{"waiting-key"}, MeterTokens: true}
			first, err := repo.Acquire(t.Context(), request)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			if action == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 150*time.Millisecond)
			}
			defer cancel()
			started := time.Now()
			done := monitoringFairStart(t, NewMonitoringProbeBudgetRepository(db), ctx, request)
			require.Eventually(t, func() bool { return monitoringFairWaiters(t, db) == 1 }, time.Second, 5*time.Millisecond)
			switch action {
			case "cancel":
				cancel()
			case "policy_off":
				policy.Enabled = false
				_, err := repo.SaveSettings(t.Context(), policy)
				require.NoError(t, err)
			case "rate_changed":
				policy.RequestsPerDay = 1
				_, err := repo.SaveSettings(t.Context(), policy)
				require.NoError(t, err)
			case "tokens_changed":
				policy.TokensPerDay = 1
				_, err := repo.SaveSettings(t.Context(), policy)
				require.NoError(t, err)
				require.NoError(t, repo.Release(t.Context(), first, service.MonitoringProbeTokenUsage{Tokens: 7, Reported: true}))
			}
			result := receiveMonitoringFairAdmission(t, done)
			if action == "policy_off" {
				require.NoError(t, result.err)
				require.NotEmpty(t, result.id)
				require.NoError(t, repo.Release(t.Context(), result.id, service.MonitoringProbeTokenUsage{Tokens: 3, Reported: true}))
			} else {
				require.Empty(t, result.id)
				switch action {
				case "cancel":
					require.ErrorIs(t, result.err, context.Canceled)
				case "deadline":
					require.ErrorIs(t, result.err, context.DeadlineExceeded)
				default:
					var denial *service.MonitoringProbeBudgetDenial
					require.ErrorAs(t, result.err, &denial)
					want := map[string]string{"wait_exhausted": "concurrency_limit", "rate_changed": "request_limit", "tokens_changed": "token_limit"}
					require.Equal(t, want[action], denial.Reason)
				}
			}
			require.Less(t, time.Since(started), 2*time.Second)
			require.Zero(t, monitoringFairWaiters(t, db), "one call cleans only its own pending state on every exit")
			require.NoError(t, repo.Release(t.Context(), first, service.MonitoringProbeTokenUsage{Tokens: 7, Reported: true}))
			summary, err := repo.Summary(t.Context())
			require.NoError(t, err)
			expected := int64(1)
			if action == "policy_off" {
				expected = 2
			}
			require.Equal(t, expected, summary.DayUsed)
			require.Zero(t, summary.ActiveRequests)
		})
	}
}

func TestMonitoringProbeBudgetSharedWaitSkipsIneligibleHead(t *testing.T) {
	for _, blocked := range []string{"site_concurrency", "key_concurrency", "site_requests", "key_requests", "site_tokens", "key_tokens", "global_tokens", "recovery_reserve", "minute_request_reserve", "day_request_reserve", "minute_token_reserve", "day_token_reserve", "expired"} {
		t.Run(blocked, func(t *testing.T) {
			db, _ := newBillingAuditDatabase(t)
			repo := NewMonitoringProbeBudgetRepository(db)
			policy := monitoringBudgetPolicy()
			policy.Concurrency = 3
			switch blocked {
			case "site_concurrency":
				policy.SiteConcurrency = 1
			case "key_concurrency":
				policy.KeyConcurrency = 1
			case "site_requests":
				policy.SiteRequestsPerMinute = 1
			case "key_requests":
				policy.KeyRequestsPerMinute = 1
			case "site_tokens":
				policy.SiteTokensPerMinute = 7
			case "key_tokens":
				policy.KeyTokensPerMinute = 7
			case "global_tokens":
				policy.TokensPerDay = 7
			case "minute_request_reserve":
				policy.RecoveryReservePercent, policy.RequestsPerMinute = 50, 2
			case "day_request_reserve":
				policy.RecoveryReservePercent, policy.RequestsPerDay = 50, 2
			case "minute_token_reserve":
				policy.RecoveryReservePercent, policy.TokensPerMinute = 50, 10
			case "day_token_reserve":
				policy.RecoveryReservePercent, policy.TokensPerDay = 50, 10
			}
			if blocked == "recovery_reserve" {
				policy.RecoveryReservePercent = 50
				policy.Concurrency = 2
			}
			_, err := repo.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			head := service.MonitoringProbeBudgetRequest{SiteHash: "blocked-site", KeyHashes: []string{"blocked-key"}}
			candidate := service.MonitoringProbeBudgetRequest{SiteHash: "independent-site", KeyHashes: []string{"independent-key"}}
			var active string
			if blocked != "expired" {
				seed := head
				seed.MeterTokens = true
				if blocked == "key_concurrency" || blocked == "key_requests" || blocked == "key_tokens" {
					seed.SiteHash = "other-blocked-site"
				}
				active, err = repo.Acquire(t.Context(), seed)
				require.NoError(t, err)
				if blocked != "site_concurrency" && blocked != "key_concurrency" && blocked != "recovery_reserve" {
					require.NoError(t, repo.Release(t.Context(), active, service.MonitoringProbeTokenUsage{Tokens: 7, Reported: true}))
				}
			}
			if blocked == "site_tokens" || blocked == "key_tokens" || blocked == "global_tokens" {
				head.MeterTokens = true
			}
			if blocked == "minute_token_reserve" || blocked == "day_token_reserve" {
				head.MeterTokens = true
				candidate.MeterTokens = true
			}
			if blocked == "recovery_reserve" || blocked == "minute_request_reserve" || blocked == "day_request_reserve" || blocked == "minute_token_reserve" || blocked == "day_token_reserve" {
				candidate.Recovery = true
			}
			expiry := 5 * time.Second
			if blocked == "expired" {
				expiry = -time.Second
			}
			oldID := insertMonitoringFairWaiter(t, db, head, expiry)
			id, err := repo.Acquire(t.Context(), candidate)
			require.NoError(t, err, "an ineligible/expired head must not block a different eligible source")
			require.NotEmpty(t, id)
			require.NoError(t, repo.Release(t.Context(), id, service.MonitoringProbeTokenUsage{}))
			if active != "" {
				require.NoError(t, repo.Release(t.Context(), active, service.MonitoringProbeTokenUsage{Tokens: 7, Reported: true}))
			}
			var count int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM monitoring_probe_budget_waiters WHERE id=$1`, oldID).Scan(&count))
			if blocked == "expired" {
				require.Zero(t, count)
			} else {
				require.Equal(t, 1, count, "do not retire another call's still-live ticket")
			}
		})
	}
}

func TestMonitoringProbeBudgetSharedWaitFailureAtomicity(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	repo := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.Concurrency = 1
	_, err := repo.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	request := service.MonitoringProbeBudgetRequest{SiteHash: "failure-site", KeyHashes: []string{"failure-key"}, MeterTokens: true}
	first, err := repo.Acquire(t.Context(), request)
	require.NoError(t, err)
	done := monitoringFairStart(t, repo, t.Context(), request)
	require.Eventually(t, func() bool { return monitoringFairWaiters(t, db) == 1 }, time.Second, 5*time.Millisecond)
	_, err = db.ExecContext(t.Context(), `CREATE FUNCTION fixture_reject_fair_lease() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'fixture admission rollback'; END $$;
 CREATE TRIGGER fixture_reject_fair_lease BEFORE INSERT ON monitoring_probe_budget_leases
 FOR EACH ROW EXECUTE FUNCTION fixture_reject_fair_lease()`)
	require.NoError(t, err)
	require.NoError(t, repo.Release(t.Context(), first, service.MonitoringProbeTokenUsage{Tokens: 7, Reported: true}))
	got := receiveMonitoringFairAdmission(t, done)
	require.Error(t, got.err)
	require.Empty(t, got.id)
	require.Zero(t, monitoringFairWaiters(t, db), "rolling back the admission must still retire only its own failed ticket")
	summary, err := repo.Summary(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, summary.DayUsed)
	require.EqualValues(t, 7, summary.DayTokens)
	require.Zero(t, summary.ActiveRequests)
	_, err = db.ExecContext(t.Context(), `DROP TRIGGER fixture_reject_fair_lease ON monitoring_probe_budget_leases; DROP FUNCTION fixture_reject_fair_lease()`)
	require.NoError(t, err)
	id, err := repo.Acquire(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, repo.Release(t.Context(), id, service.MonitoringProbeTokenUsage{Reported: true}))
}

func TestMonitoringProbeBudgetSharedWaitCannotResumeAfterLockExpiry(t *testing.T) {
	for _, reason := range []string{"concurrency_limit", "recovery_reserve"} {
		t.Run(reason, func(t *testing.T) {
			db, _ := newBillingAuditDatabase(t)
			repo := NewMonitoringProbeBudgetRepository(db)
			policy := monitoringBudgetPolicy()
			policy.Concurrency = 1
			if reason == "recovery_reserve" {
				policy.Concurrency = 2
				policy.RecoveryReservePercent = 50
			}
			_, err := repo.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			request := service.MonitoringProbeBudgetRequest{SiteHash: "expiry-site", KeyHashes: []string{"expiry-key"}}
			held, err := repo.Acquire(t.Context(), request)
			require.NoError(t, err)
			done := monitoringFairStart(t, repo, t.Context(), request)
			require.Eventually(t, func() bool { return monitoringFairWaiters(t, db) == 1 }, time.Second, 5*time.Millisecond)
			lock, err := db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = lock.Rollback() }()
			_, err = lock.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock($1)`, monitoringProbeBudgetLockID)
			require.NoError(t, err)
			_, err = lock.ExecContext(t.Context(), `UPDATE monitoring_probe_budget_leases SET released_at=clock_timestamp() WHERE id=$1`, held)
			require.NoError(t, err)
			time.Sleep(monitoringBudgetWaitDuration + 100*time.Millisecond)
			require.NoError(t, lock.Commit())
			got := receiveMonitoringFairAdmission(t, done)
			require.Empty(t, got.id, "a slot freed after the waiting window cannot retroactively admit the expired ticket")
			var denial *service.MonitoringProbeBudgetDenial
			require.ErrorAs(t, got.err, &denial)
			require.Equal(t, reason, denial.Reason)
			require.Zero(t, monitoringFairWaiters(t, db))
			summary, err := repo.Summary(t.Context())
			require.NoError(t, err)
			require.EqualValues(t, 1, summary.DayUsed)
			require.Zero(t, summary.ActiveRequests)
		})
	}
}

func TestMonitoringProbeBudgetSharedWaitMigrationAndBound(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	_, err := repo.SaveSettings(ctx, policy)
	require.NoError(t, err)
	seed, err := repo.Acquire(ctx, service.MonitoringProbeBudgetRequest{SiteHash: "recorded-site", KeyHashes: []string{"recorded-key"}, MeterTokens: true})
	require.NoError(t, err)
	require.NoError(t, repo.Release(ctx, seed, service.MonitoringProbeTokenUsage{Tokens: 7, Reported: true}))
	accounting := func() string {
		t.Helper()
		var value string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT json_build_array(
 (SELECT json_agg(c ORDER BY dimension,scope_hash,bucket_start) FROM monitoring_probe_budget_counters c),
 (SELECT json_agg(l ORDER BY id) FROM monitoring_probe_budget_leases l))::text`).Scan(&value))
		return value
	}
	before := accounting()
	request := service.MonitoringProbeBudgetRequest{SiteHash: "private-site", KeyHashes: []string{"private-key"}}
	oldID := insertMonitoringFairWaiter(t, db, request, 5*time.Second)
	ddl, err := os.ReadFile("../../migrations/353_monitoring_probe_budget_waiters.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(ddl))
	require.NoError(t, err)
	require.Equal(t, 1, monitoringFairWaiters(t, db), "migration replay cannot clear active waiters")
	_, err = db.ExecContext(ctx, `INSERT INTO monitoring_probe_budget_waiters(id,site_hash,key_hashes,recovery,meter_tokens,expires_at)
 SELECT gen_random_uuid(),'capacity-site',ARRAY['capacity-key'],false,false,clock_timestamp()+interval '5 seconds'
 FROM generate_series(1,$1)`, monitoringBudgetMaxWaiters-1)
	require.NoError(t, err)
	started := time.Now()
	id, err := repo.Acquire(ctx, request)
	var denial *service.MonitoringProbeBudgetDenial
	require.ErrorAs(t, err, &denial)
	require.Equal(t, "global", denial.Scope)
	require.Empty(t, id)
	require.Less(t, time.Since(started), monitoringBudgetWaitDuration, "full queue must not retain an unregistered request")
	require.Equal(t, monitoringBudgetMaxWaiters, monitoringFairWaiters(t, db))
	_, err = db.ExecContext(ctx, `DELETE FROM monitoring_probe_budget_waiters WHERE id<>$1`, oldID)
	require.NoError(t, err)
	var counters, leases int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM monitoring_probe_budget_counters`).Scan(&counters))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM monitoring_probe_budget_leases`).Scan(&leases))
	require.Positive(t, counters)
	require.Equal(t, 1, leases)
	require.Equal(t, before, accounting(), "waiting and migration replay cannot change previously recorded actual usage")
	_, err = db.ExecContext(ctx, `DROP TABLE monitoring_probe_budget_waiters`)
	require.NoError(t, err, "rollback is confined to the disposable private database")
	_, err = repo.Acquire(ctx, request)
	require.Error(t, err, "enabled admission fails closed when shared scheduling state is unavailable")
	require.Equal(t, before, accounting(), "dropping the private queue must preserve the original nonempty ledger")
	_, err = db.ExecContext(ctx, string(ddl))
	require.NoError(t, err)
	id, err = repo.Acquire(ctx, request)
	require.NoError(t, err)
	require.NoError(t, repo.Release(ctx, id, service.MonitoringProbeTokenUsage{}))
	var downstream int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&downstream))
	require.Zero(t, downstream)
	t.Logf("waiter bound=%d; replay/rollback preserve accounting and recovery", monitoringBudgetMaxWaiters)
}

func TestMonitoringProbeBudgetSharedFullQueueDoesNotBlockIndependentSource(t *testing.T) {
	for _, blocked := range []string{"site_concurrency", "key_concurrency", "site_requests", "key_requests", "site_tokens", "key_tokens", "global_tokens", "recovery_reserve"} {
		t.Run(blocked, func(t *testing.T) {
			db, _ := newBillingAuditDatabase(t)
			repo := NewMonitoringProbeBudgetRepository(db)
			policy := monitoringBudgetPolicy()
			policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency = 3, 3, 3
			switch blocked {
			case "site_concurrency":
				policy.SiteConcurrency = 1
			case "key_concurrency":
				policy.KeyConcurrency = 1
			case "site_requests":
				policy.SiteRequestsPerMinute = 1
			case "key_requests":
				policy.KeyRequestsPerMinute = 1
			case "site_tokens":
				policy.SiteTokensPerMinute = 7
			case "key_tokens":
				policy.KeyTokensPerMinute = 7
			case "global_tokens":
				policy.TokensPerDay = 7
			case "recovery_reserve":
				policy.Concurrency, policy.RecoveryReservePercent = 2, 50
			}
			policy, err := repo.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			busy := service.MonitoringProbeBudgetRequest{SiteHash: "full-blocked-site", KeyHashes: []string{"full-blocked-key"}, MeterTokens: true}
			held, err := repo.Acquire(t.Context(), busy)
			require.NoError(t, err)
			tokens := int64(0)
			if blocked == "site_tokens" || blocked == "key_tokens" || blocked == "global_tokens" {
				tokens = 7
				require.NoError(t, repo.Release(t.Context(), held, service.MonitoringProbeTokenUsage{Tokens: tokens, Reported: true}))
			}
			_, err = db.ExecContext(t.Context(), `INSERT INTO monitoring_probe_budget_waiters(id,site_hash,key_hashes,recovery,meter_tokens,expires_at)
 SELECT gen_random_uuid(),$1,ARRAY[$2]::text[],false,true,clock_timestamp()+interval '5 seconds'
 FROM generate_series(1,$3)`, busy.SiteHash, busy.KeyHashes[0], monitoringBudgetMaxWaiters)
			require.NoError(t, err)
			independent := service.MonitoringProbeBudgetRequest{SiteHash: "full-independent-site", KeyHashes: []string{"full-independent-key"},
				MeterTokens: blocked != "global_tokens", Recovery: blocked == "recovery_reserve"}
			id, err := repo.Acquire(t.Context(), independent)
			require.NoError(t, err, "a queue full of locally blocked waiters must not deny another source's free physical slot")
			require.NotEmpty(t, id)
			require.Equal(t, monitoringBudgetMaxWaiters, monitoringFairWaiters(t, db), "direct independent admission must not exceed the waiting bound or erase peers")
			if independent.MeterTokens {
				require.NoError(t, repo.Release(t.Context(), id, service.MonitoringProbeTokenUsage{Tokens: 7, Reported: true}))
				tokens += 7
			} else {
				require.NoError(t, repo.Release(t.Context(), id, service.MonitoringProbeTokenUsage{}))
			}
			require.NoError(t, repo.Release(t.Context(), held, service.MonitoringProbeTokenUsage{}))
			// Restore each old candidate's specific constraint through the original
			// settings contract. Its older ticket must then protect the next free slot.
			policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency, policy.RecoveryReservePercent = 3, 3, 3, 0
			policy.SiteRequestsPerMinute, policy.KeyRequestsPerMinute = 100, 100
			policy.SiteTokensPerMinute, policy.KeyTokensPerMinute, policy.TokensPerDay = 0, 0, 0
			_, err = repo.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			_, err = repo.Acquire(t.Context(), independent)
			var denial *service.MonitoringProbeBudgetDenial
			require.ErrorAs(t, err, &denial, "once older peers are eligible again the full queue must protect their turn")
			require.Equal(t, "global", denial.Scope)
			summary, err := repo.Summary(t.Context())
			require.NoError(t, err)
			require.EqualValues(t, 2, summary.DayUsed)
			require.EqualValues(t, tokens, summary.DayTokens)
			require.Zero(t, summary.ActiveRequests)
			var downstream int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_logs`).Scan(&downstream))
			require.Zero(t, downstream)
		})
	}
}

func TestMonitoringProbeBudgetSharedSlotWaitAndRecovery(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	db.SetMaxOpenConns(1) // Waiting must leave the sole connection available for Release.
	repo := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.Concurrency = 1
	_, err := repo.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	request := service.MonitoringProbeBudgetRequest{SiteHash: "fairness-site", KeyHashes: []string{"fairness-key"}}
	first, err := repo.Acquire(t.Context(), request)
	require.NoError(t, err)
	finished := make(chan monitoringFairAdmission, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	go func() {
		id, err := NewMonitoringProbeBudgetRepository(db).Acquire(ctx, request)
		finished <- monitoringFairAdmission{id, err}
	}()
	select {
	case got := <-finished:
		t.Fatalf("shared concurrency contention must wait briefly instead of immediately losing a scheduler turn: id=%q error=%v", got.id, got.err)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, repo.Release(t.Context(), first, service.MonitoringProbeTokenUsage{}))
	select {
	case got := <-finished:
		require.NoError(t, got.err)
		require.NotEmpty(t, got.id)
		require.NoError(t, repo.Release(t.Context(), got.id, service.MonitoringProbeTokenUsage{}))
	case <-time.After(time.Second):
		t.Fatal("the waiting physical request did not acquire the released shared slot")
	}
	summary, err := repo.Summary(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 2, summary.DayUsed, "only physical admissions consume requests, never their waiting attempts")
	require.Zero(t, summary.ActiveRequests)
	require.Zero(t, summary.DayTokens)
}
