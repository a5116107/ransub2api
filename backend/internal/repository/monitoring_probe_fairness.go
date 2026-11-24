package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

func monitoringBudgetFairTurn(ctx context.Context, tx *sql.Tx, now time.Time, request service.MonitoringProbeBudgetRequest,
	settings service.MonitoringProbeBudgetSettings, waiter *monitoringBudgetWaiter, congested bool) (bool, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM monitoring_probe_budget_waiters WHERE id IN
 (SELECT id FROM monitoring_probe_budget_waiters WHERE expires_at<=$1 ORDER BY expires_at LIMIT 128)`, now); err != nil {
		return false, err
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM monitoring_probe_budget_waiters WHERE expires_at>$1`, now).Scan(&pending); err != nil {
		return false, err
	}
	if pending == 0 && !congested {
		return true, nil
	}
	remaining := time.Until(waiter.deadline)
	if remaining <= 0 {
		return false, nil
	}
	if !waiter.queued && pending < monitoringBudgetMaxWaiters {
		if _, err := tx.ExecContext(ctx, `INSERT INTO monitoring_probe_budget_waiters
 (id,site_hash,key_hashes,recovery,meter_tokens,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, waiter.id,
			request.SiteHash, pq.Array(request.KeyHashes), request.Recovery, request.MeterTokens, now.Add(remaining)); err != nil {
			return false, err
		}
		waiter.queued = true
	}
	if congested {
		return false, nil // This request has no global slot; selection cannot admit it.
	}
	// A locally blocked head cannot reserve global capacity. Evaluate the same
	// hard counters and reserve constraints for every live candidate in one SQL
	// snapshot, not a query per ticket or a second accounting implementation.
	var winner string
	err := tx.QueryRowContext(ctx, `WITH active AS MATERIALIZED (
 SELECT site_hash,key_hashes,recovery FROM monitoring_probe_budget_leases
 WHERE expires_at>$1 AND released_at IS NULL
 ), limits(dimension,capacity,regular_capacity,token_capacity,regular_tokens,bucket) AS (VALUES
 ('global_minute',$8::bigint,$9::bigint,$14::bigint,$15::bigint,$2::timestamptz),
 ('global_day',$10::bigint,$11::bigint,$16::bigint,$17::bigint,$3::timestamptz),
 ('site_minute',$12::bigint,$12::bigint,$18::bigint,$18::bigint,$2::timestamptz),
 ('key_minute',$13::bigint,$13::bigint,$19::bigint,$19::bigint,$2::timestamptz))
 SELECT w.id::text FROM monitoring_probe_budget_waiters w WHERE w.expires_at>$1
 AND (SELECT count(*) FROM active)<$4
 AND (w.recovery OR (SELECT count(*) FROM active WHERE NOT recovery)<$5)
 AND (SELECT count(*) FROM active a WHERE a.site_hash=w.site_hash)<$6
 AND NOT EXISTS(SELECT 1 FROM unnest(w.key_hashes) AS k(key_hash)
 WHERE (SELECT count(*) FROM active a WHERE a.key_hashes @> ARRAY[k.key_hash]::text[]) >= $7)
 AND NOT EXISTS(SELECT 1 FROM monitoring_probe_budget_counters c JOIN limits l
 ON c.dimension=l.dimension AND c.bucket_start=l.bucket
 WHERE ((c.dimension IN ('global_minute','global_day') AND c.scope_hash='')
 OR (c.dimension='site_minute' AND c.scope_hash=w.site_hash)
 OR (c.dimension='key_minute' AND c.scope_hash=ANY(w.key_hashes)))
 AND (c.used>=l.capacity OR (NOT w.recovery AND c.regular_used>=l.regular_capacity)
 OR (w.meter_tokens AND l.token_capacity>0 AND
 (c.tokens_used>=l.token_capacity OR (NOT w.recovery AND c.tokens_regular>=l.regular_tokens)))))
 ORDER BY w.position LIMIT 1`, now, now.UTC().Truncate(time.Minute), now.UTC().Truncate(24*time.Hour),
		settings.Concurrency, monitoringBudgetRegularLimit(int64(settings.Concurrency), settings.RecoveryReservePercent),
		settings.SiteConcurrency, settings.KeyConcurrency, settings.RequestsPerMinute,
		monitoringBudgetRegularLimit(settings.RequestsPerMinute, settings.RecoveryReservePercent), settings.RequestsPerDay,
		monitoringBudgetRegularLimit(settings.RequestsPerDay, settings.RecoveryReservePercent), settings.SiteRequestsPerMinute,
		settings.KeyRequestsPerMinute, settings.TokensPerMinute, monitoringBudgetRegularLimit(settings.TokensPerMinute, settings.RecoveryReservePercent),
		settings.TokensPerDay, monitoringBudgetRegularLimit(settings.TokensPerDay, settings.RecoveryReservePercent),
		settings.SiteTokensPerMinute, settings.KeyTokensPerMinute).Scan(&winner)
	if errors.Is(err, sql.ErrNoRows) {
		// A full waiting inventory is not a global capacity gate. The current
		// request already passed every hard limit; if no older ticket is
		// eligible it may proceed without registering a 257th ticket.
		return !waiter.queued, nil
	}
	if err != nil {
		return false, err
	}
	return winner == waiter.id, nil
}
