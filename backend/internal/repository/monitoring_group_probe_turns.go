package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

func (r *channelMonitorV2Repository) SyncGroupMonitoringTurns(ctx context.Context, cfg service.GroupMonitoringQueueConfig, targets []service.GroupMonitoringTurn) (*service.GroupMonitoringQueue, error) {
	return syncGroupMonitoringTurns(ctx, r.db, "v2", cfg, targets)
}
func (r *channelMonitorV3Repository) SyncGroupMonitoringTurns(ctx context.Context, cfg service.GroupMonitoringQueueConfig, targets []service.GroupMonitoringTurn) (*service.GroupMonitoringQueue, error) {
	return syncGroupMonitoringTurns(ctx, r.db, "v3", cfg, targets)
}
func (r *channelMonitorV2Repository) ClaimGroupMonitoringTurn(ctx context.Context, turn service.GroupMonitoringTurn) (service.GroupMonitoringTurn, bool, error) {
	return claimGroupMonitoringTurn(ctx, r.db, "v2", turn)
}
func (r *channelMonitorV3Repository) ClaimGroupMonitoringTurn(ctx context.Context, turn service.GroupMonitoringTurn) (service.GroupMonitoringTurn, bool, error) {
	return claimGroupMonitoringTurn(ctx, r.db, "v3", turn)
}
func (r *channelMonitorV2Repository) FinishGroupMonitoringTurn(ctx context.Context, turn service.GroupMonitoringTurn, round int64) (bool, error) {
	return finishGroupMonitoringTurn(ctx, r.db, "v2", turn, round)
}
func (r *channelMonitorV3Repository) FinishGroupMonitoringTurn(ctx context.Context, turn service.GroupMonitoringTurn, round int64) (bool, error) {
	return finishGroupMonitoringTurn(ctx, r.db, "v3", turn, round)
}

func validGroupMonitoringTurn(turn service.GroupMonitoringTurn) bool {
	return validCNMonitoringTurn(service.CNMonitoringTurn{AccountID: turn.GroupID, IdentityHash: turn.ScopeHash}) &&
		validCNMonitoringTurn(service.CNMonitoringTurn{AccountID: turn.GroupID, IdentityHash: turn.IdentityHash, Served: turn.Served, Tried: turn.Tried})
}

func syncGroupMonitoringTurns(ctx context.Context, db *sql.DB, lane string, cfg service.GroupMonitoringQueueConfig, targets []service.GroupMonitoringTurn) (*service.GroupMonitoringQueue, error) {
	ids, scopes, identities, due := make([]int64, 0, len(targets)), make([]string, 0, len(targets)), make([]string, 0, len(targets)), make([]bool, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		key := fmt.Sprintf("%d/%s", target.GroupID, target.ScopeHash)
		if !validGroupMonitoringTurn(target) || seen[key] {
			return nil, errors.New("invalid group monitoring inventory")
		}
		seen[key] = true
		ids, scopes, identities, due = append(ids, target.GroupID), append(scopes, target.ScopeHash), append(identities, target.IdentityHash), append(due, target.Due)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	lockID := int64(0x47525050524f4232)
	if lane == "v3" {
		lockID++
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockID); err != nil {
		return nil, err
	}
	var matches bool
	if lane == "v2" {
		err = tx.QueryRowContext(ctx, `SELECT version=$1 AND enabled FROM channel_monitor_v2_config WHERE id=1 FOR SHARE`, cfg.Version).Scan(&matches)
	} else {
		var expected []byte
		if cfg.IdleProbe == nil || !cfg.IdleProbe.Enabled {
			return nil, service.ErrMonitoringProbeBudgetUnavailable
		}
		expected, err = json.Marshal(cfg.IdleProbe)
		if err == nil {
			err = tx.QueryRowContext(ctx, `SELECT version=$1 AND idle_probe=$2::jsonb FROM channel_monitor_v3_config WHERE id=1 FOR SHARE`, cfg.Version, expected).Scan(&matches)
		}
	}
	if err != nil {
		return nil, err
	}
	if !matches {
		return nil, errors.New("group monitoring configuration changed")
	}
	var born int64
	if err = tx.QueryRowContext(ctx, `SELECT nextval('monitoring_group_probe_turn_sequence')`).Scan(&born); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO monitoring_group_probe_turns(lane,group_id,scope_hash,identity_hash,served,tried,waiting_since)
 SELECT $1,t.id,t.scope,t.identity,$6,0,CASE WHEN t.due THEN clock_timestamp() END
 FROM unnest($2::bigint[],$3::text[],$4::text[],$5::bool[]) AS t(id,scope,identity,due)
 JOIN groups g ON g.id=t.id WHERE g.deleted_at IS NULL
 ON CONFLICT(lane,group_id,scope_hash) DO UPDATE SET identity_hash=EXCLUDED.identity_hash,
 served=CASE WHEN monitoring_group_probe_turns.identity_hash=EXCLUDED.identity_hash THEN monitoring_group_probe_turns.served ELSE EXCLUDED.served END,
 tried=CASE WHEN monitoring_group_probe_turns.identity_hash=EXCLUDED.identity_hash THEN monitoring_group_probe_turns.tried ELSE 0 END,
 claim_until=CASE WHEN monitoring_group_probe_turns.identity_hash=EXCLUDED.identity_hash THEN monitoring_group_probe_turns.claim_until END,
 waiting_since=CASE WHEN EXCLUDED.waiting_since IS NULL THEN NULL
 WHEN monitoring_group_probe_turns.identity_hash=EXCLUDED.identity_hash THEN COALESCE(monitoring_group_probe_turns.waiting_since,EXCLUDED.waiting_since)
 ELSE EXCLUDED.waiting_since END`, lane, pq.Array(ids), pq.Array(scopes), pq.Array(identities), pq.Array(due), born)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM monitoring_group_probe_turns p WHERE lane=$1 AND NOT EXISTS
 (SELECT 1 FROM unnest($2::bigint[],$3::text[]) t(id,scope) WHERE t.id=p.group_id AND t.scope=p.scope_hash)`, lane, pq.Array(ids), pq.Array(scopes)); err != nil {
		return nil, err
	}
	queue := &service.GroupMonitoringQueue{Turns: make([]service.GroupMonitoringTurn, 0, len(targets))}
	if err = tx.QueryRowContext(ctx, `SELECT nextval('monitoring_group_probe_turn_sequence')`).Scan(&queue.Round); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT group_id,scope_hash,identity_hash,served,tried,waiting_since,claim_until FROM monitoring_group_probe_turns WHERE lane=$1`, lane)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var turn service.GroupMonitoringTurn
		var waiting, lease sql.NullTime
		if err = rows.Scan(&turn.GroupID, &turn.ScopeHash, &turn.IdentityHash, &turn.Served, &turn.Tried, &waiting, &lease); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if waiting.Valid {
			turn.WaitingSince = waiting.Time.UTC()
		}
		if lease.Valid {
			turn.ClaimUntil = lease.Time.UTC()
		}
		queue.Turns = append(queue.Turns, turn)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	return queue, tx.Commit()
}

func claimGroupMonitoringTurn(ctx context.Context, db *sql.DB, lane string, turn service.GroupMonitoringTurn) (service.GroupMonitoringTurn, bool, error) {
	if !validGroupMonitoringTurn(turn) || turn.Served == 0 {
		return turn, false, service.ErrMonitoringProbeBudgetUnavailable
	}
	err := db.QueryRowContext(ctx, `UPDATE monitoring_group_probe_turns SET tried=nextval('monitoring_group_probe_turn_sequence'),claim_until=clock_timestamp()+make_interval(secs=>$7)
 WHERE lane=$1 AND group_id=$2 AND scope_hash=$3 AND identity_hash=$4 AND served=$5 AND tried=$6 AND (claim_until IS NULL OR claim_until<=clock_timestamp()) RETURNING tried,claim_until`,
		lane, turn.GroupID, turn.ScopeHash, turn.IdentityHash, turn.Served, turn.Tried, service.GroupMonitoringTurnLeaseTTL.Seconds()).Scan(&turn.Tried, &turn.ClaimUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return turn, false, nil
	}
	return turn, err == nil, err
}

func finishGroupMonitoringTurn(ctx context.Context, db *sql.DB, lane string, turn service.GroupMonitoringTurn, round int64) (bool, error) {
	if !validGroupMonitoringTurn(turn) || turn.Tried == 0 || round != 0 && round <= turn.Served {
		return false, service.ErrMonitoringProbeBudgetUnavailable
	}
	result, err := db.ExecContext(ctx, `UPDATE monitoring_group_probe_turns SET served=CASE WHEN $7::bigint=0 THEN served ELSE $7::bigint END,
 waiting_since=CASE WHEN $7::bigint=0 THEN waiting_since END,claim_until=NULL
 WHERE lane=$1 AND group_id=$2 AND scope_hash=$3 AND identity_hash=$4 AND served=$5 AND tried=$6 AND claim_until IS NOT NULL`,
		lane, turn.GroupID, turn.ScopeHash, turn.IdentityHash, turn.Served, turn.Tried, round)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}
