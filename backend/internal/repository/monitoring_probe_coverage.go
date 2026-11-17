package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const monitoringCoverageInventoryLimit = 10000

func (r *monitoringProbeBudgetRepository) PreviewCoverage(ctx context.Context, settings service.MonitoringProbeBudgetSettings) (*service.MonitoringProbeCoverage, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := loadMonitoringProbeBudgetSettings(ctx, tx, "")
	if err != nil {
		return nil, err
	}
	if settings.Revision != current.Revision {
		return nil, service.ErrMonitoringProbeBudgetConflict
	}
	result, err := loadMonitoringProbeCoverage(ctx, tx, settings)
	if err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

// Preview and saving read the same inventory/settings contract. No network
// requests, leases, counters or account observations are changed here.
func loadMonitoringProbeCoverage(ctx context.Context, tx *sql.Tx, settings service.MonitoringProbeBudgetSettings) (*service.MonitoringProbeCoverage, error) {
	return compileMonitoringProbeCoverage(ctx, tx, settings, nil)
}

func compileMonitoringProbeCoverage(ctx context.Context, tx *sql.Tx, settings service.MonitoringProbeBudgetSettings, override *service.UpstreamBillingProbeSettings) (*service.MonitoringProbeCoverage, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, service.SettingKeyUpstreamBillingProbeSettings).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	billing, err := service.ParseUpstreamBillingProbeSettings(raw)
	if err != nil {
		return nil, err
	}
	if override != nil {
		billing = override
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.platform,a.type,a.status,a.credentials,a.extra,a.proxy_id,
 COALESCE(b.account_fingerprint,'') FROM accounts a
 LEFT JOIN new_api_account_bindings b ON b.account_id=a.id
 WHERE a.deleted_at IS NULL AND a.status='active' AND a.type='apikey'
 AND a.extra @> '{"upstream_billing_probe_enabled":true}'::jsonb
 ORDER BY a.id LIMIT $1`, monitoringCoverageInventoryLimit+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	accounts := make([]service.MonitoringProbeCoverageAccount, 0)
	complete := true
	for rows.Next() {
		if len(accounts) == monitoringCoverageInventoryLimit {
			complete = false
			break
		}
		var target service.MonitoringProbeCoverageAccount
		var credentials, extra []byte
		var proxyID sql.NullInt64
		a := &target.Account
		if err := rows.Scan(&a.ID, &a.Platform, &a.Type, &a.Status, &credentials, &extra, &proxyID, &target.NewAPIBindingIdentity); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(credentials, &a.Credentials); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(extra, &a.Extra); err != nil {
			return nil, err
		}
		if proxyID.Valid {
			a.ProxyID = &proxyID.Int64
		}
		accounts = append(accounts, target)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	return service.CompileMonitoringProbeCoverage(settings, *billing, accounts, complete, now), nil
}

// The production runner's interval write uses the same short lock as policy
// saves/admission so two individually valid concurrent edits cannot combine
// into a budget/interval pair that this compiler already proves impossible.
func (r *monitoringProbeBudgetRepository) SaveBillingProbeSettings(ctx context.Context, billing service.UpstreamBillingProbeSettings) error {
	if err := billing.Validate(); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, monitoringProbeBudgetLockID); err != nil {
		return err
	}
	settings, err := loadMonitoringProbeBudgetSettings(ctx, tx, "FOR SHARE")
	if err != nil {
		return err
	}
	coverage, err := compileMonitoringProbeCoverage(ctx, tx, settings, &billing)
	if err != nil {
		return err
	}
	// Disabling either enforcement or the runner must remain possible. An
	// enabled runner still has its fixed throughput even in observe-only mode.
	coverage.CanSave = !billing.Enabled || len(coverage.BlockingReasons) == 0
	if err := coverage.ValidateSave(); err != nil {
		return err
	}
	raw, err := json.Marshal(billing)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES($1,$2,NOW())
 ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=EXCLUDED.updated_at`, service.SettingKeyUpstreamBillingProbeSettings, string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}
