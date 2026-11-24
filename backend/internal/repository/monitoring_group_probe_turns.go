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
	reviewIDs, reviewScopes, reviewKeys, reviewMinutes := []int64{}, []string{}, []string{}, []int{}
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		key := fmt.Sprintf("%d/%s", target.GroupID, target.ScopeHash)
		if !validGroupMonitoringTurn(target) || seen[key] {
			return nil, errors.New("invalid group monitoring inventory")
		}
		seen[key] = true
		ids, scopes, identities, due = append(ids, target.GroupID), append(scopes, target.ScopeHash), append(identities, target.IdentityHash), append(due, target.Due)
		if target.EvidenceKey == "" && target.IntervalMinutes == 0 {
			continue
		}
		maximum := 1440
		if lane == "v3" {
			maximum = 60
		}
		if cfg.ObservedAt.IsZero() || target.IntervalMinutes < 1 || target.IntervalMinutes > maximum ||
			!validCNMonitoringTurn(service.CNMonitoringTurn{AccountID: target.GroupID, IdentityHash: target.EvidenceKey}) {
			return nil, errors.New("invalid native group monitoring scope")
		}
		reviewIDs, reviewScopes, reviewKeys, reviewMinutes = append(reviewIDs, target.GroupID), append(reviewScopes, target.ScopeHash), append(reviewKeys, target.EvidenceKey), append(reviewMinutes, target.IntervalMinutes)
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
	if len(reviewIDs) > 0 {
		// Read original terminal observations, never queue-age metadata, as the
		// source of a review. Deferred/running/legacy-unknown rows do not erase
		// an older valid observation. The original next slot/window is a due
		// clock, not a newly invented health or quality TTL.
		observation := `SELECT r.slot+make_interval(mins=>w.minutes) AS due_at
 FROM channel_monitor_v2_candy_results r
 WHERE r.group_id=w.id AND r.config_key=w.config
 AND encode(sha256(convert_to(r.config_key,'UTF8')),'hex')=w.scope
 AND encode(sha256(convert_to('candy-v1:'||r.model||':'||r.reasoning_effort||':'||w.minutes::text,'UTF8')),'hex')=w.config
 AND r.verdict IN ('correct','incorrect','error') AND r.reason<>'interrupted'
 AND r.budget_deferral IS NULL AND isfinite(r.slot) AND isfinite(r.checked_at) AND isfinite(r.finished_at)
 AND r.slot>'0001-01-01'::timestamptz AND r.checked_at>'0001-01-01'::timestamptz
 AND r.finished_at>=r.checked_at AND r.finished_at<=LEAST($5::timestamptz,clock_timestamp())
 AND r.slot+make_interval(mins=>w.minutes)<=LEAST($5::timestamptz,clock_timestamp())
 ORDER BY r.slot DESC,r.id DESC LIMIT 1`
		if lane == "v3" {
			observation = `SELECT r.slot_start+make_interval(mins=>2*w.minutes)-INTERVAL '1 minute' AS due_at
 FROM channel_monitor_v3_probe_results r
 WHERE r.group_id=w.id AND r.config_key=w.config AND r.slot_minutes=w.minutes
 AND encode(sha256(convert_to(r.model,'UTF8')),'hex')=w.scope
 AND r.outcome IN ('success','failure') AND r.budget_deferral IS NULL
 AND isfinite(r.slot_start) AND isfinite(r.started_at) AND isfinite(r.finished_at)
 AND r.slot_start>'0001-01-01'::timestamptz AND r.started_at>'0001-01-01'::timestamptz
 AND r.finished_at>=r.started_at AND r.finished_at<=LEAST($5::timestamptz,clock_timestamp())
 AND r.slot_start+make_interval(mins=>2*w.minutes)-INTERVAL '1 minute'<=LEAST($5::timestamptz,clock_timestamp())
 ORDER BY r.slot_start DESC,r.id DESC LIMIT 1`
		}
		rows, err = tx.QueryContext(ctx, `SELECT w.id,w.scope,p.due_at
 FROM unnest($1::bigint[],$2::text[],$3::text[],$4::int[]) AS w(id,scope,config,minutes)
 JOIN LATERAL (`+observation+`) p ON TRUE`, pq.Array(reviewIDs), pq.Array(reviewScopes), pq.Array(reviewKeys), pq.Array(reviewMinutes), cfg.ObservedAt)
		if err != nil {
			return nil, err
		}
		byScope := make(map[string]int, len(queue.Turns))
		for i, turn := range queue.Turns {
			byScope[fmt.Sprintf("%d/%s", turn.GroupID, turn.ScopeHash)] = i
		}
		for rows.Next() {
			var observed service.GroupMonitoringTurn
			if err = rows.Scan(&observed.GroupID, &observed.ScopeHash, &observed.ReviewDueAt); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if i, exists := byScope[fmt.Sprintf("%d/%s", observed.GroupID, observed.ScopeHash)]; exists {
				queue.Turns[i].ReviewDueAt = observed.ReviewDueAt.UTC()
			}
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
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
