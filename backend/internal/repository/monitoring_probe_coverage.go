package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
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
func loadMonitoringProbeCoverage(ctx context.Context, tx sqlQueryer, settings service.MonitoringProbeBudgetSettings) (*service.MonitoringProbeCoverage, error) {
	return compileMonitoringProbeCoverage(ctx, tx, settings, nil)
}

func compileMonitoringProbeCoverage(ctx context.Context, tx sqlQueryer, settings service.MonitoringProbeBudgetSettings, override *service.UpstreamBillingProbeSettings) (*service.MonitoringProbeCoverage, error) {
	var raw string
	err := scanSingleRow(ctx, tx, `SELECT value FROM settings WHERE key=$1`, []any{service.SettingKeyUpstreamBillingProbeSettings}, &raw)
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
	plans, plansComplete, err := loadMonitoringCoveragePlans(ctx, tx)
	if err != nil {
		return nil, err
	}
	candy, candyComplete, err := loadMonitoringCoverageCandy(ctx, tx)
	if err != nil {
		return nil, err
	}
	var now time.Time
	if err := scanSingleRow(ctx, tx, `SELECT clock_timestamp()`, nil, &now); err != nil {
		return nil, err
	}
	return service.CompileMonitoringProbeCoverage(settings, *billing, accounts, complete, plans, plansComplete, candy, candyComplete, now), nil
}

func loadMonitoringCoverageCandy(ctx context.Context, tx sqlQueryer) ([]service.ChannelMonitorV2CandyProbe, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key,value FROM settings WHERE key IN ($1,$2)`,
		service.SettingKeyChannelMonitorEnabled, service.SettingKeyChannelMonitorMode)
	if err != nil {
		return nil, false, err
	}
	values := make(map[string]string, 2)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		values[key] = value
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, false, err
	}
	if !service.ParseChannelMonitorRuntime(values).V2Active() {
		return nil, true, nil
	}
	var cfg service.ChannelMonitorV2Config
	var platforms, probes []byte
	err = scanSingleRow(ctx, tx, `SELECT enabled,platforms,group_ids,candy_probes FROM channel_monitor_v2_config WHERE id=1`,
		nil, &cfg.Enabled, &platforms, pq.Array(&cfg.GroupIDs), &probes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !cfg.Enabled {
		return nil, true, nil
	}
	if err := json.Unmarshal(platforms, &cfg.Platforms); err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(probes, &cfg.CandyProbes); err != nil {
		return nil, false, err
	}
	complete := len(cfg.CandyProbes) <= monitoringCoverageInventoryLimit
	if !complete {
		cfg.CandyProbes = cfg.CandyProbes[:monitoringCoverageInventoryLimit]
	}
	ids := make([]int64, 0, len(cfg.CandyProbes))
	for _, probe := range cfg.CandyProbes {
		ids = append(ids, probe.GroupID)
	}
	if len(ids) == 0 {
		return nil, complete, nil
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,platform,status FROM groups WHERE id=ANY($1) AND deleted_at IS NULL`, pq.Array(ids))
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	groups := make(map[int64]*service.Group)
	for rows.Next() {
		group := new(service.Group)
		if err := rows.Scan(&group.ID, &group.Platform, &group.Status); err != nil {
			return nil, false, err
		}
		groups[group.ID] = group
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	active := make([]service.ChannelMonitorV2CandyProbe, 0, len(cfg.CandyProbes))
	for _, probe := range cfg.CandyProbes {
		if cfg.CandyProbeAllowed(probe, groups[probe.GroupID]) {
			active = append(active, probe)
		}
	}
	return active, complete, nil
}

func validateMonitoringCoverageCandyWrite(ctx context.Context, tx sqlQueryer) error {
	// Explicitly disabling V2/the site switch or leaving its exclusive mode is
	// a repair path, even when other legacy schedules already exceed capacity.
	probes, complete, err := loadMonitoringCoverageCandy(ctx, tx)
	if err != nil || (len(probes) == 0 && complete) {
		return err
	}
	return validateMonitoringCoveragePlanWrite(ctx, tx, true)
}

func loadMonitoringCoveragePlans(ctx context.Context, tx sqlQueryer) ([]service.MonitoringProbeCoveragePlan, bool, error) {
	// Disabled/deleted owners do not become free coverage. Account tests also
	// run for temporarily disabled accounts as part of their recovery rules.
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.model_id,p.cron_expression,p.pelican_config,p.next_run_at,false AS is_group
 FROM scheduled_test_plans p JOIN accounts a ON a.id=p.account_id
 WHERE p.enabled AND a.deleted_at IS NULL
 UNION ALL
 SELECT p.id,p.model_id,p.cron_expression,p.pelican_config,p.next_run_at,true AS is_group
 FROM pelican_group_test_plans p JOIN groups g ON g.id=p.group_id
 WHERE p.enabled AND g.deleted_at IS NULL AND g.status='active'
 ORDER BY is_group,id LIMIT $1`, monitoringCoverageInventoryLimit+1)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	plans := make([]service.MonitoringProbeCoveragePlan, 0)
	complete := true
	for rows.Next() {
		if len(plans) == monitoringCoverageInventoryLimit {
			complete = false
			break
		}
		var target service.MonitoringProbeCoveragePlan
		var config []byte
		if err := rows.Scan(&target.Plan.ID, &target.Plan.ModelID, &target.Plan.CronExpression, &config, &target.Plan.NextRunAt, &target.Group); err != nil {
			return nil, false, err
		}
		if len(config) > 0 {
			if err := json.Unmarshal(config, &target.Plan.PelicanConfig); err != nil {
				return nil, false, err
			}
		}
		target.Plan.Enabled = true
		plans = append(plans, target)
	}
	return plans, complete, rows.Err()
}

func validateMonitoringCoveragePlanWrite(ctx context.Context, tx sqlQueryer, enabled bool) error {
	// Disabling a plan remains a valid repair even when other old settings
	// already exceed capacity. Callers hold the shared lock before writing.
	if !enabled {
		return nil
	}
	settings, err := loadMonitoringProbeBudgetSettings(ctx, tx, "")
	if err != nil {
		return err
	}
	if !settings.Enabled {
		return nil
	}
	coverage, err := loadMonitoringProbeCoverage(ctx, tx, settings)
	if err != nil {
		return err
	}
	return coverage.ValidateSave()
}

func lockMonitoringCoveragePlanWrite(ctx context.Context, tx sqlExecutor) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, monitoringProbeBudgetLockID)
	return err
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
