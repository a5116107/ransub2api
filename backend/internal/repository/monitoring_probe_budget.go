package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

type monitoringBudgetQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadMonitoringProbeBudgetSettings(ctx context.Context, db monitoringBudgetQueryer, suffix string) (service.MonitoringProbeBudgetSettings, error) {
	settings := service.DefaultMonitoringProbeBudgetSettings()
	var raw string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1 `+suffix, service.MonitoringProbeBudgetSettingKey).Scan(&raw)
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
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&result.ObservedAt); err != nil {
		return nil, err
	}
	for _, counter := range []struct {
		dimension string
		start     time.Time
		used      *int64
		regular   *int64
	}{
		{"global_minute", result.ObservedAt.UTC().Truncate(time.Minute), &result.MinuteUsed, &result.MinuteRegular},
		{"global_day", result.ObservedAt.UTC().Truncate(24 * time.Hour), &result.DayUsed, &result.DayRegular},
	} {
		err := tx.QueryRowContext(ctx, `SELECT used,regular_used FROM monitoring_probe_budget_counters
 WHERE dimension=$1 AND scope_hash='' AND bucket_start=$2`, counter.dimension, counter.start).Scan(counter.used, counter.regular)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(*) FILTER(WHERE NOT recovery)
 FROM monitoring_probe_budget_leases WHERE expires_at>$1`, result.ObservedAt).Scan(&result.ActiveRequests, &result.ActiveRegular); err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

type monitoringBudgetCounter struct {
	dimension string
	scope     string
	start     time.Time
	end       time.Time
	limit     int64
	reserve   int
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
	minute := now.UTC().Truncate(time.Minute)
	day := now.UTC().Truncate(24 * time.Hour)
	counters := []monitoringBudgetCounter{
		{"global_minute", "", minute, minute.Add(time.Minute), settings.RequestsPerMinute, settings.RecoveryReservePercent},
		{"global_day", "", day, day.Add(24 * time.Hour), settings.RequestsPerDay, settings.RecoveryReservePercent},
		{"site_minute", request.SiteHash, minute, minute.Add(time.Minute), settings.SiteRequestsPerMinute, 0},
	}
	for _, key := range request.KeyHashes {
		counters = append(counters, monitoringBudgetCounter{"key_minute", key, minute, minute.Add(time.Minute), settings.KeyRequestsPerMinute, 0})
	}
	if settings.Enabled {
		for _, counter := range counters {
			var used, regular int64
			err := tx.QueryRowContext(ctx, `SELECT used,regular_used FROM monitoring_probe_budget_counters
 WHERE dimension=$1 AND scope_hash=$2 AND bucket_start=$3`, counter.dimension, counter.scope, counter.start).Scan(&used, &regular)
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
		}
		if denial, err := monitoringBudgetConcurrency(ctx, tx, now, request, settings); err != nil || denial != nil {
			if err != nil {
				return "", err
			}
			return "", denial
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
	id := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `INSERT INTO monitoring_probe_budget_leases(id,site_hash,key_hashes,recovery,expires_at)
 VALUES($1,$2,$3,$4,$5)`, id, request.SiteHash, pq.Array(request.KeyHashes), request.Recovery, now.Add(service.MonitoringProbeBudgetLeaseTTL)); err != nil {
		return "", err
	}
	// Bounded pruning shares the existing request transaction, not a new worker.
	if _, err := tx.ExecContext(ctx, `DELETE FROM monitoring_probe_budget_leases WHERE id IN
 (SELECT id FROM monitoring_probe_budget_leases WHERE expires_at<=$1 ORDER BY expires_at LIMIT 128)`, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM monitoring_probe_budget_counters WHERE ctid IN
 (SELECT ctid FROM monitoring_probe_budget_counters WHERE bucket_start<$1 ORDER BY bucket_start LIMIT 64)`, day.Add(-48*time.Hour)); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func monitoringBudgetConcurrency(ctx context.Context, tx *sql.Tx, now time.Time, request service.MonitoringProbeBudgetRequest, settings service.MonitoringProbeBudgetSettings) (*service.MonitoringProbeBudgetDenial, error) {
	var total, regular, site int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(*) FILTER(WHERE NOT recovery),COUNT(*) FILTER(WHERE site_hash=$2)
 FROM monitoring_probe_budget_leases WHERE expires_at>$1`, now, request.SiteHash).Scan(&total, &regular, &site); err != nil {
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
 WHERE expires_at>$1 AND key_hashes @> ARRAY[$2]::text[]`, now, key).Scan(&count); err != nil {
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
 WHERE id=$1 AND expires_at>clock_timestamp()`, id, service.MonitoringProbeBudgetLeaseTTL.Milliseconds())
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (r *monitoringProbeBudgetRepository) Release(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM monitoring_probe_budget_leases WHERE id=$1`, id)
	return err
}
