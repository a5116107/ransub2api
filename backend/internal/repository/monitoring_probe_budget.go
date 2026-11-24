package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// BPSPROBE: all admissions and policy saves share one short transaction lock.
// No database connection is held while an upstream request is in flight.
const monitoringProbeBudgetLockID int64 = 0x42505350524f4245

type monitoringProbeBudgetRepository struct{ db *sql.DB }

func NewMonitoringProbeBudgetRepository(db *sql.DB) service.MonitoringProbeBudgetRepository {
	return &monitoringProbeBudgetRepository{db: db}
}

func loadMonitoringProbeBudgetSettings(ctx context.Context, db sqlQueryer, suffix string) (service.MonitoringProbeBudgetSettings, error) {
	settings := service.DefaultMonitoringProbeBudgetSettings()
	var raw string
	err := scanSingleRow(ctx, db, `SELECT value FROM settings WHERE key=$1 `+suffix, []any{service.MonitoringProbeBudgetSettingKey}, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return settings, err
	}
	return settings, settings.Validate()
}

func (r *monitoringProbeBudgetRepository) GetSettings(ctx context.Context) (service.MonitoringProbeBudgetSettings, error) {
	return loadMonitoringProbeBudgetSettings(ctx, r.db, "")
}

func (r *monitoringProbeBudgetRepository) SaveSettings(ctx context.Context, settings service.MonitoringProbeBudgetSettings) (service.MonitoringProbeBudgetSettings, error) {
	if err := settings.Validate(); err != nil {
		return settings, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return settings, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, monitoringProbeBudgetLockID); err != nil {
		return settings, err
	}
	current, err := loadMonitoringProbeBudgetSettings(ctx, tx, "FOR UPDATE")
	if err != nil {
		return settings, err
	}
	if current.Revision != settings.Revision || current.Revision == math.MaxInt64 {
		return settings, service.ErrMonitoringProbeBudgetConflict
	}
	coverage, err := loadMonitoringProbeCoverage(ctx, tx, settings)
	if err != nil {
		return settings, err
	}
	if err := coverage.ValidateSave(); err != nil {
		return settings, err
	}
	settings.Revision++
	raw, err := json.Marshal(settings)
	if err != nil {
		return settings, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES($1,$2,NOW())
 ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=EXCLUDED.updated_at`, service.MonitoringProbeBudgetSettingKey, string(raw)); err != nil {
		return settings, err
	}
	return settings, tx.Commit()
}

func (r *monitoringProbeBudgetRepository) Summary(ctx context.Context) (*service.MonitoringProbeBudgetSummary, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	settings, err := loadMonitoringProbeBudgetSettings(ctx, tx, "")
	if err != nil {
		return nil, err
	}
	result := &service.MonitoringProbeBudgetSummary{Settings: settings}
	result.Coverage, err = loadMonitoringProbeCoverage(ctx, tx, settings)
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&result.ObservedAt); err != nil {
		return nil, err
	}
	for _, counter := range []struct {
		dimension string
		start     time.Time
		used      *int64
		regular   *int64
		tokens    *int64
	}{
		{"global_minute", result.ObservedAt.UTC().Truncate(time.Minute), &result.MinuteUsed, &result.MinuteRegular, &result.MinuteTokens},
		{"global_day", result.ObservedAt.UTC().Truncate(24 * time.Hour), &result.DayUsed, &result.DayRegular, &result.DayTokens},
	} {
		err := tx.QueryRowContext(ctx, `SELECT used,regular_used,tokens_used FROM monitoring_probe_budget_counters
 WHERE dimension=$1 AND scope_hash='' AND bucket_start=$2`, counter.dimension, counter.start).Scan(counter.used, counter.regular, counter.tokens)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	err = tx.QueryRowContext(ctx, `SELECT usage_reported,usage_unknown FROM monitoring_probe_budget_counters
 WHERE dimension='global_day' AND scope_hash='' AND bucket_start=$1`, result.ObservedAt.UTC().Truncate(24*time.Hour)).Scan(&result.UsageReported, &result.UsageUnknown)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM monitoring_probe_budget_leases
 WHERE meter_tokens AND released_at IS NULL`).Scan(&result.UsagePending); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(*) FILTER(WHERE NOT recovery)
 FROM monitoring_probe_budget_leases WHERE expires_at>$1 AND released_at IS NULL`, result.ObservedAt).Scan(&result.ActiveRequests, &result.ActiveRegular); err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

type monitoringBudgetCounter struct {
	dimension  string
	scope      string
	start      time.Time
	end        time.Time
	limit      int64
	reserve    int
	tokenLimit int64
}

const monitoringBudgetWaitDuration = time.Second
const monitoringBudgetWaitPoll = 50 * time.Millisecond
const monitoringBudgetMaxWaiters = 256

type monitoringBudgetWaiter struct {
	id       string
	deadline time.Time
	queued   bool
	denial   *service.MonitoringProbeBudgetDenial
}

func (r *monitoringProbeBudgetRepository) Acquire(ctx context.Context, request service.MonitoringProbeBudgetRequest) (string, error) {
	if request.SiteHash == "" || len(request.SiteHash) > 128 || len(request.KeyHashes) == 0 || len(request.KeyHashes) > 16 {
		return "", service.ErrMonitoringProbeBudgetUnavailable
	}
	request.KeyHashes = slices.Clone(request.KeyHashes)
	slices.Sort(request.KeyHashes)
	request.KeyHashes = slices.Compact(request.KeyHashes)
	for _, key := range request.KeyHashes {
		if strings.TrimSpace(key) == "" || len(key) > 128 {
			return "", service.ErrMonitoringProbeBudgetUnavailable
		}
	}
	waiter := &monitoringBudgetWaiter{id: uuid.NewString(), deadline: time.Now().Add(monitoringBudgetWaitDuration)}
	defer func() {
		if !waiter.queued {
			return
		}
		// Only this call owns its ticket. Cancellation does not delete peers or
		// cancel admitted work; a crashed process's ticket expires independently.
		cleanup, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if _, err := r.db.ExecContext(cleanup, `DELETE FROM monitoring_probe_budget_waiters WHERE id=$1`, waiter.id); err != nil {
			slog.Warn("monitoring_probe: contention ticket cleanup failed")
		}
	}()
	pause := monitoringBudgetWaitPoll
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if waiter.queued && !time.Now().Before(waiter.deadline) {
			return "", waiter.denial
		}
		id, err := r.acquire(ctx, request, waiter)
		var denial *service.MonitoringProbeBudgetDenial
		if !waiter.queued || !errors.As(err, &denial) || !monitoringBudgetWaitable(denial) {
			return id, err
		}
		waiter.denial = denial
		remaining := time.Until(waiter.deadline)
		if remaining <= 0 {
			return "", denial
		}
		timer := time.NewTimer(min(pause, remaining))
		pause = min(4*monitoringBudgetWaitPoll, 2*pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

func monitoringBudgetWaitable(denial *service.MonitoringProbeBudgetDenial) bool {
	return denial != nil && denial.Scope == "global" && (denial.Reason == "concurrency_limit" || denial.Reason == "recovery_reserve")
}

// Every attempt uses the original short transaction. Waiting never holds its
// lock, database connection or a speculative request/token reservation.
func (r *monitoringProbeBudgetRepository) acquire(ctx context.Context, request service.MonitoringProbeBudgetRequest, waiter *monitoringBudgetWaiter) (string, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	// Lock before reading policy rows: reversing this order can deadlock an
	// admission waiting behind a concurrent policy update.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, monitoringProbeBudgetLockID); err != nil {
		return "", err
	}
	settings, err := loadMonitoringProbeBudgetSettings(ctx, tx, "FOR SHARE")
	if err != nil {
		return "", err
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return "", err
	}
	if waiter.queued && !time.Now().Before(waiter.deadline) {
		denial := *waiter.denial
		denial.RetryAt = now.Add(time.Second)
		return "", &denial
	}
	minute := now.UTC().Truncate(time.Minute)
	day := now.UTC().Truncate(24 * time.Hour)
	counters := []monitoringBudgetCounter{
		{"global_minute", "", minute, minute.Add(time.Minute), settings.RequestsPerMinute, settings.RecoveryReservePercent, settings.TokensPerMinute},
		{"global_day", "", day, day.Add(24 * time.Hour), settings.RequestsPerDay, settings.RecoveryReservePercent, settings.TokensPerDay},
		{"site_minute", request.SiteHash, minute, minute.Add(time.Minute), settings.SiteRequestsPerMinute, 0, settings.SiteTokensPerMinute},
	}
	for _, key := range request.KeyHashes {
		counters = append(counters, monitoringBudgetCounter{"key_minute", key, minute, minute.Add(time.Minute), settings.KeyRequestsPerMinute, 0, settings.KeyTokensPerMinute})
	}
	if settings.Enabled {
		for _, counter := range counters {
			var used, regular, tokens, regularTokens int64
			err := tx.QueryRowContext(ctx, `SELECT used,regular_used,tokens_used,tokens_regular FROM monitoring_probe_budget_counters
 WHERE dimension=$1 AND scope_hash=$2 AND bucket_start=$3`, counter.dimension, counter.scope, counter.start).Scan(&used, &regular, &tokens, &regularTokens)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return "", err
			}
			reason := "request_limit"
			denied := used >= counter.limit
			if !denied && !request.Recovery && counter.reserve > 0 && regular >= monitoringBudgetRegularLimit(counter.limit, counter.reserve) {
				denied, reason = true, "recovery_reserve"
			}
			if denied {
				return "", &service.MonitoringProbeBudgetDenial{Reason: reason, Scope: counter.dimension, RetryAt: counter.end}
			}
			// No token pre-reservations: concurrent admitted requests may exceed
			// the threshold. Management-only calls still obey request limits.
			if request.MeterTokens && counter.tokenLimit > 0 {
				if tokens >= counter.tokenLimit {
					return "", &service.MonitoringProbeBudgetDenial{Reason: "token_limit", Scope: counter.dimension, RetryAt: counter.end}
				}
				if !request.Recovery && counter.reserve > 0 && regularTokens >= monitoringBudgetRegularLimit(counter.tokenLimit, counter.reserve) {
					return "", &service.MonitoringProbeBudgetDenial{Reason: "recovery_reserve", Scope: counter.dimension, RetryAt: counter.end}
				}
			}
		}
		denial, err := monitoringBudgetConcurrency(ctx, tx, now, request, settings)
		if err != nil {
			return "", err
		}
		if denial != nil && !monitoringBudgetWaitable(denial) {
			return "", denial
		}
		allowed, err := monitoringBudgetFairTurn(ctx, tx, now, request, settings, waiter, denial != nil)
		if err != nil {
			return "", err
		}
		if !allowed {
			if denial == nil {
				denial = &service.MonitoringProbeBudgetDenial{Reason: "concurrency_limit", Scope: "global", RetryAt: now.Add(time.Second)}
			}
			if err := tx.Commit(); err != nil {
				return "", err
			}
			return "", denial
		}
	}
	if waiter.queued {
		if _, err := tx.ExecContext(ctx, `DELETE FROM monitoring_probe_budget_waiters WHERE id=$1`, waiter.id); err != nil {
			return "", err
		}
	}
	regular := 1
	if request.Recovery {
		regular = 0
	}
	for _, counter := range counters {
		if _, err := tx.ExecContext(ctx, `INSERT INTO monitoring_probe_budget_counters(dimension,scope_hash,bucket_start,used,regular_used)
 VALUES($1,$2,$3,1,$4) ON CONFLICT(dimension,scope_hash,bucket_start)
 DO UPDATE SET used=monitoring_probe_budget_counters.used+1,regular_used=monitoring_probe_budget_counters.regular_used+EXCLUDED.regular_used`,
			counter.dimension, counter.scope, counter.start, regular); err != nil {
			return "", err
		}
	}
	id := waiter.id
	if _, err := tx.ExecContext(ctx, `INSERT INTO monitoring_probe_budget_leases(id,site_hash,key_hashes,recovery,expires_at,meter_tokens)
 VALUES($1,$2,$3,$4,$5,$6)`, id, request.SiteHash, pq.Array(request.KeyHashes), request.Recovery, now.Add(service.MonitoringProbeBudgetLeaseTTL), request.MeterTokens); err != nil {
		return "", err
	}
	// Bounded pruning shares the existing request transaction, not a new worker.
	if _, err := tx.ExecContext(ctx, `DELETE FROM monitoring_probe_budget_leases WHERE id IN
 (SELECT id FROM monitoring_probe_budget_leases
 WHERE (released_at IS NOT NULL AND released_at<$2) OR (NOT meter_tokens AND released_at IS NULL AND expires_at<=$1)
 ORDER BY expires_at LIMIT 128)`, now, day.Add(-48*time.Hour)); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM monitoring_probe_budget_counters WHERE ctid IN
 (SELECT ctid FROM monitoring_probe_budget_counters WHERE bucket_start<$1 ORDER BY bucket_start LIMIT 64)`, day.Add(-48*time.Hour)); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	waiter.queued = false
	return id, nil
}

func monitoringBudgetConcurrency(ctx context.Context, tx *sql.Tx, now time.Time, request service.MonitoringProbeBudgetRequest, settings service.MonitoringProbeBudgetSettings) (*service.MonitoringProbeBudgetDenial, error) {
	var total, regular, site int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(*) FILTER(WHERE NOT recovery),COUNT(*) FILTER(WHERE site_hash=$2)
 FROM monitoring_probe_budget_leases WHERE expires_at>$1 AND released_at IS NULL`, now, request.SiteHash).Scan(&total, &regular, &site); err != nil {
		return nil, err
	}
	scope, reason := "", "concurrency_limit"
	switch {
	case total >= settings.Concurrency:
		scope = "global"
	case !request.Recovery && int64(regular) >= monitoringBudgetRegularLimit(int64(settings.Concurrency), settings.RecoveryReservePercent):
		scope, reason = "global", "recovery_reserve"
	case site >= settings.SiteConcurrency:
		scope = "site"
	}
	if scope == "" {
		for _, key := range request.KeyHashes {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM monitoring_probe_budget_leases
 WHERE expires_at>$1 AND released_at IS NULL AND key_hashes @> ARRAY[$2]::text[]`, now, key).Scan(&count); err != nil {
				return nil, err
			}
			if count >= settings.KeyConcurrency {
				scope = "key"
				break
			}
		}
	}
	if scope == "" {
		return nil, nil
	}
	return &service.MonitoringProbeBudgetDenial{Reason: reason, Scope: scope, RetryAt: now.Add(time.Second)}, nil
}

func monitoringBudgetRegularLimit(limit int64, reserve int) int64 {
	// A fractional reservation must not starve every ordinary probe when the
	// administrator chooses a one-request or one-slot budget.
	return max(int64(1), limit*int64(100-reserve)/100)
}

func (r *monitoringProbeBudgetRepository) Renew(ctx context.Context, id string) (bool, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE monitoring_probe_budget_leases SET expires_at=clock_timestamp()+($2*INTERVAL '1 millisecond')
 WHERE id=$1 AND expires_at>clock_timestamp() AND released_at IS NULL`, id, service.MonitoringProbeBudgetLeaseTTL.Milliseconds())
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (r *monitoringProbeBudgetRepository) Release(ctx context.Context, id string, usage service.MonitoringProbeTokenUsage) error {
	if usage.Tokens < 0 || usage.Tokens > 4*(1<<53-1) {
		return service.ErrMonitoringProbeBudgetUnavailable
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, monitoringProbeBudgetLockID); err != nil {
		return err
	}
	var site string
	var keys pq.StringArray
	var recovery, meter bool
	err = tx.QueryRowContext(ctx, `SELECT site_hash,key_hashes,recovery,meter_tokens FROM monitoring_probe_budget_leases
 WHERE id=$1 AND released_at IS NULL FOR UPDATE`, id).Scan(&site, &keys, &recovery, &meter)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // Repeated close/settlement must never add usage again.
	}
	if err != nil {
		return err
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if !meter {
		if _, err := tx.ExecContext(ctx, `DELETE FROM monitoring_probe_budget_leases WHERE id=$1`, id); err != nil {
			return err
		}
		return tx.Commit()
	}
	regular, reported, unknown := usage.Tokens, int64(0), int64(0)
	if recovery {
		regular = 0
	}
	if usage.Reported {
		reported = 1
	} else {
		unknown = 1
	}
	minute, day := now.UTC().Truncate(time.Minute), now.UTC().Truncate(24*time.Hour)
	dimensions := []struct {
		dimension, scope string
		start            time.Time
	}{
		{"global_minute", "", minute}, {"global_day", "", day}, {"site_minute", site, minute},
	}
	for _, key := range keys {
		dimensions = append(dimensions, struct {
			dimension, scope string
			start            time.Time
		}{"key_minute", key, minute})
	}
	for _, d := range dimensions {
		_, err = tx.ExecContext(ctx, `INSERT INTO monitoring_probe_budget_counters
 (dimension,scope_hash,bucket_start,tokens_used,tokens_regular,usage_reported,usage_unknown)
 VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(dimension,scope_hash,bucket_start) DO UPDATE SET
 tokens_used=monitoring_probe_budget_counters.tokens_used+EXCLUDED.tokens_used,
 tokens_regular=monitoring_probe_budget_counters.tokens_regular+EXCLUDED.tokens_regular,
 usage_reported=monitoring_probe_budget_counters.usage_reported+EXCLUDED.usage_reported,
 usage_unknown=monitoring_probe_budget_counters.usage_unknown+EXCLUDED.usage_unknown`,
			d.dimension, d.scope, d.start, usage.Tokens, regular, reported, unknown)
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE monitoring_probe_budget_leases SET released_at=$2,expires_at=$2 WHERE id=$1`, id, now); err != nil {
		return err
	}
	return tx.Commit()
}
