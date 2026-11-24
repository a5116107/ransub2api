package repository

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

const cnMonitoringTurnsLockID int64 = 0x434e50524f4245

func (r *accountRepository) SyncCNMonitoringTurns(ctx context.Context, targets []service.CNMonitoringTurn, complete bool) (*service.CNMonitoringQueue, error) {
	if r == nil || r.sql == nil {
		return nil, service.ErrMonitoringProbeBudgetUnavailable
	}
	ids, hashes := make([]int64, 0, len(targets)), make([]string, 0, len(targets))
	seen := make(map[int64]bool, len(targets))
	for _, target := range targets {
		if !validCNMonitoringTurn(target) || seen[target.AccountID] {
			return nil, errors.New("invalid CN monitoring inventory")
		}
		seen[target.AccountID] = true
		ids, hashes = append(ids, target.AccountID), append(hashes, target.IdentityHash)
	}
	// Automatic inventory synchronization owns this short transaction; it is
	// never part of an account observation or an administrator's transaction.
	db, ok := r.sql.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		return nil, service.ErrMonitoringProbeBudgetUnavailable
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, cnMonitoringTurnsLockID); err != nil {
		return nil, err
	}
	var born int64
	if err = tx.QueryRowContext(ctx, `SELECT nextval('cn_provider_monitoring_turn_sequence')`).Scan(&born); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO cn_provider_monitoring_turns(account_id,identity_hash,served,tried)
 SELECT t.id,t.hash,$3,0 FROM unnest($1::bigint[],$2::text[]) AS t(id,hash)
 JOIN accounts a ON a.id=t.id WHERE a.deleted_at IS NULL AND a.status='active'
 ON CONFLICT(account_id) DO UPDATE SET identity_hash=EXCLUDED.identity_hash,served=EXCLUDED.served,tried=0
 WHERE cn_provider_monitoring_turns.identity_hash<>EXCLUDED.identity_hash`, pq.Array(ids), pq.Array(hashes), born)
	if err != nil {
		return nil, err
	}
	if complete {
		if _, err = tx.ExecContext(ctx, `DELETE FROM cn_provider_monitoring_turns WHERE NOT(account_id=ANY($1::bigint[]))`, pq.Array(ids)); err != nil {
			return nil, err
		}
	}
	queue := &service.CNMonitoringQueue{Turns: make(map[int64]service.CNMonitoringTurn, len(targets))}
	if err = tx.QueryRowContext(ctx, `SELECT nextval('cn_provider_monitoring_turn_sequence')`).Scan(&queue.Round); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT account_id,identity_hash,served,tried FROM cn_provider_monitoring_turns WHERE account_id=ANY($1::bigint[])`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var turn service.CNMonitoringTurn
		if err = rows.Scan(&turn.AccountID, &turn.IdentityHash, &turn.Served, &turn.Tried); err != nil {
			_ = rows.Close()
			return nil, err
		}
		queue.Turns[turn.AccountID] = turn
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	return queue, tx.Commit()
}

func validCNMonitoringTurn(turn service.CNMonitoringTurn) bool {
	decoded, err := hex.DecodeString(turn.IdentityHash)
	return turn.AccountID > 0 && err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == turn.IdentityHash && turn.Served >= 0 && turn.Tried >= 0
}

func (r *accountRepository) ClaimCNMonitoringTurn(ctx context.Context, turn service.CNMonitoringTurn) (service.CNMonitoringTurn, bool, error) {
	if r == nil || r.sql == nil || !validCNMonitoringTurn(turn) || turn.Served == 0 {
		return turn, false, service.ErrMonitoringProbeBudgetUnavailable
	}
	err := scanSingleRow(ctx, r.sql, `UPDATE cn_provider_monitoring_turns SET tried=nextval('cn_provider_monitoring_turn_sequence')
 WHERE account_id=$1 AND identity_hash=$2 AND served=$3 AND tried=$4 RETURNING tried`, []any{turn.AccountID, turn.IdentityHash, turn.Served, turn.Tried}, &turn.Tried)
	if errors.Is(err, sql.ErrNoRows) {
		return turn, false, nil
	}
	return turn, err == nil, err
}

func (r *accountRepository) FinishCNMonitoringTurn(ctx context.Context, turn service.CNMonitoringTurn, round int64) (bool, error) {
	if r == nil || r.sql == nil || !validCNMonitoringTurn(turn) || turn.Tried == 0 || round <= turn.Served {
		return false, service.ErrMonitoringProbeBudgetUnavailable
	}
	result, err := r.sql.ExecContext(ctx, `UPDATE cn_provider_monitoring_turns SET served=$5
 WHERE account_id=$1 AND identity_hash=$2 AND served=$3 AND tried=$4`, turn.AccountID, turn.IdentityHash, turn.Served, turn.Tried, round)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}
