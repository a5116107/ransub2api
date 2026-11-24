//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCNMonitoringTurnsPersistenceCASIdentityAndInventory(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := newAccountRepositoryWithSQL(client, db, nil)
	create := func(name string) service.CNMonitoringTurn {
		t.Helper()
		account := mustCreateAccount(t, client, &service.Account{Name: name, Platform: service.PlatformDeepseek,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
			Credentials: map[string]any{"api_key": "private-cn-fixture", "base_url": "https://fixture.invalid"}})
		return service.CNMonitoringTurn{AccountID: account.ID, IdentityHash: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
	}
	a, b := create("turn-a"), create("turn-b")
	accounts := func() string {
		t.Helper()
		var rows string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_agg(row_to_json(a) ORDER BY id)::text FROM accounts a`).Scan(&rows))
		return rows
	}
	before := accounts()
	initial, err := repo.SyncCNMonitoringTurns(ctx, []service.CNMonitoringTurn{a, b}, true)
	require.NoError(t, err)
	require.Equal(t, initial.Turns[a.AccountID].Served, initial.Turns[b.AccountID].Served)
	claimed, ok, err := repo.ClaimCNMonitoringTurn(ctx, initial.Turns[a.AccountID])
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = repo.ClaimCNMonitoringTurn(ctx, initial.Turns[a.AccountID])
	require.NoError(t, err)
	require.False(t, ok, "the same inventory cannot acquire one target twice")
	ok, err = repo.FinishCNMonitoringTurn(ctx, claimed, initial.Round)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = repo.FinishCNMonitoringTurn(ctx, claimed, initial.Round)
	require.NoError(t, err)
	require.False(t, ok, "late duplicate completion cannot overwrite a consumed turn")
	peer := newAccountRepositoryWithSQL(client, db, nil)
	next, err := peer.SyncCNMonitoringTurns(ctx, []service.CNMonitoringTurn{a, b}, true)
	require.NoError(t, err)
	require.Equal(t, initial.Round, next.Turns[a.AccountID].Served)
	require.Less(t, next.Turns[b.AccountID].Served, next.Turns[a.AccountID].Served)
	stale, ok, err := repo.ClaimCNMonitoringTurn(ctx, next.Turns[b.AccountID])
	require.NoError(t, err)
	require.True(t, ok)
	b.IdentityHash = fmt.Sprintf("%x", sha256.Sum256([]byte("new-identity")))
	changed, err := peer.SyncCNMonitoringTurns(ctx, []service.CNMonitoringTurn{a, b}, true)
	require.NoError(t, err)
	require.Greater(t, changed.Turns[b.AccountID].Served, next.Turns[b.AccountID].Served)
	require.Zero(t, changed.Turns[b.AccountID].Tried)
	ok, err = repo.FinishCNMonitoringTurn(ctx, stale, next.Round)
	require.NoError(t, err)
	require.False(t, ok, "an old credential/route cannot consume its replacement's age")
	_, err = peer.SyncCNMonitoringTurns(ctx, []service.CNMonitoringTurn{a}, false)
	require.NoError(t, err)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM cn_provider_monitoring_turns`).Scan(&count))
	require.Equal(t, 2, count, "partial inventory must preserve absent targets")
	_, err = peer.SyncCNMonitoringTurns(ctx, []service.CNMonitoringTurn{a}, true)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM cn_provider_monitoring_turns`).Scan(&count))
	require.Equal(t, 1, count)
	_, err = peer.SyncCNMonitoringTurns(ctx, nil, false)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM cn_provider_monitoring_turns`).Scan(&count))
	require.Equal(t, 1, count)
	_, err = peer.SyncCNMonitoringTurns(ctx, nil, true)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM cn_provider_monitoring_turns`).Scan(&count))
	require.Zero(t, count)
	require.Equal(t, before, accounts(), "queue metadata must not rewrite account observations, timestamps, or schedulability")
}

func TestCNMonitoringTurnsConcurrentClaimAndSuccessorFence(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := newAccountRepositoryWithSQL(client, db, nil)
	a := mustCreateAccount(t, client, &service.Account{Name: "concurrent-turn", Platform: service.PlatformDeepseek,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive})
	targets := []service.CNMonitoringTurn{{AccountID: a.ID, IdentityHash: fmt.Sprintf("%x", sha256.Sum256([]byte("identity")))}}
	queue, err := repo.SyncCNMonitoringTurns(ctx, targets, true)
	require.NoError(t, err)
	type reply struct {
		turn service.CNMonitoringTurn
		ok   bool
		err  error
	}
	start, done := make(chan struct{}), make(chan reply, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			turn, ok, err := newAccountRepositoryWithSQL(client, db, nil).ClaimCNMonitoringTurn(ctx, queue.Turns[a.ID])
			done <- reply{turn, ok, err}
		})
	}
	close(start)
	wg.Wait()
	close(done)
	var winner service.CNMonitoringTurn
	count := 0
	for result := range done {
		require.NoError(t, result.err)
		if result.ok {
			count++
			winner = result.turn
		}
	}
	require.Equal(t, 1, count)
	// The first owner's unadmitted/abandoned attempt retains served age. The
	// next cycle's claim replaces only tried; late completion cannot win.
	next, err := repo.SyncCNMonitoringTurns(ctx, targets, true)
	require.NoError(t, err)
	successor, ok, err := repo.ClaimCNMonitoringTurn(ctx, next.Turns[a.ID])
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = repo.FinishCNMonitoringTurn(ctx, winner, queue.Round)
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = repo.FinishCNMonitoringTurn(ctx, successor, next.Round)
	require.NoError(t, err)
	require.True(t, ok)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = repo.SyncCNMonitoringTurns(canceled, targets, true)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCNMonitoringTurnsMigrationFailureAndRollbackPreserveLedger(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := newAccountRepositoryWithSQL(client, db, nil)
	a := mustCreateAccount(t, client, &service.Account{Name: "migration-turn", Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Status: service.StatusActive})
	targets := []service.CNMonitoringTurn{{AccountID: a.ID, IdentityHash: fmt.Sprintf("%x", sha256.Sum256([]byte("migration")))}}
	budget := NewMonitoringProbeBudgetRepository(db)
	_, err := budget.SaveSettings(ctx, monitoringBudgetPolicy())
	require.NoError(t, err)
	id, err := budget.Acquire(ctx, service.MonitoringProbeBudgetRequest{SiteHash: "fixture-site", KeyHashes: []string{"fixture-key"}})
	require.NoError(t, err)
	require.NoError(t, budget.Release(ctx, id, service.MonitoringProbeTokenUsage{}))
	queue, err := repo.SyncCNMonitoringTurns(ctx, targets, true)
	require.NoError(t, err)
	ddl, err := os.ReadFile("../../migrations/354_cn_provider_monitoring_turns.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(ddl))
	require.NoError(t, err)
	next, err := repo.SyncCNMonitoringTurns(ctx, targets, true)
	require.NoError(t, err)
	require.Equal(t, queue.Turns, next.Turns, "migration replay must not restart sequence or pending ages")
	_, err = db.ExecContext(ctx, `DROP TABLE cn_provider_monitoring_turns`)
	require.NoError(t, err, "rollback is confined to a private fixture")
	_, err = repo.SyncCNMonitoringTurns(ctx, targets, true)
	require.Error(t, err, "missing shared state must not recreate a process-local queue")
	_, err = db.ExecContext(ctx, string(ddl))
	require.NoError(t, err)
	_, err = repo.SyncCNMonitoringTurns(ctx, targets, true)
	require.NoError(t, err)
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, summary.DayUsed)
	require.Zero(t, summary.ActiveRequests)
	_, err = repo.SyncCNMonitoringTurns(ctx, []service.CNMonitoringTurn{{AccountID: a.ID, IdentityHash: "raw-credential"}}, true)
	require.Error(t, err)
	_, err = repo.SyncCNMonitoringTurns(ctx, append(targets, targets...), true)
	require.Error(t, err)
	// Query cancellation while the inventory lock is held does not change ages.
	lock, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = lock.Rollback() }()
	_, err = lock.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, cnMonitoringTurnsLockID)
	require.NoError(t, err)
	limited, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = repo.SyncCNMonitoringTurns(limited, targets, true)
	require.Error(t, err)
	require.NoError(t, lock.Commit())
}
