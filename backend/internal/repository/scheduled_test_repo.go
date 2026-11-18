package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// --- Plan Repository ---

type scheduledTestPlanRepository struct {
	db *sql.DB
}

func NewScheduledTestPlanRepository(db *sql.DB) service.ScheduledTestPlanRepository {
	return &scheduledTestPlanRepository{db: db}
}

func (r *scheduledTestPlanRepository) Create(ctx context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockMonitoringCoveragePlanWrite(ctx, tx); err != nil {
		return nil, err
	}
	created, err := insertScheduledTestPlan(ctx, tx, plan)
	if err != nil {
		return nil, err
	}
	return created, tx.Commit()
}

type planRowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// insertScheduledTestPlan is shared with group rules, which insert inside their own transaction.
func insertScheduledTestPlan(ctx context.Context, q *sql.Tx, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	if err := validateQualityGroupReferences(ctx, q, plan.PelicanConfig); err != nil {
		return nil, err
	}
	row := q.QueryRowContext(ctx, `
		INSERT INTO scheduled_test_plans (account_id, model_id, cron_expression, enabled, max_results, auto_recover, next_run_at, created_at, updated_at, pelican_config)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW(), $8)
		RETURNING id, account_id, model_id, cron_expression, enabled, max_results, auto_recover, last_run_at, next_run_at, created_at, updated_at, pelican_config, running_until
	`, plan.AccountID, plan.ModelID, plan.CronExpression, plan.Enabled, plan.MaxResults, plan.AutoRecover, plan.NextRunAt, marshalPelicanConfig(plan.PelicanConfig))
	created, err := scanPlan(row)
	if err != nil {
		return nil, err
	}
	if err := validateMonitoringCoveragePlanWrite(ctx, q, created.Enabled); err != nil {
		return nil, err
	}
	return created, nil
}

func (r *scheduledTestPlanRepository) GetByID(ctx context.Context, id int64) (*service.ScheduledTestPlan, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, account_id, model_id, cron_expression, enabled, max_results, auto_recover, last_run_at, next_run_at, created_at, updated_at, pelican_config, running_until
		FROM scheduled_test_plans WHERE id = $1
	`, id)
	return scanPlan(row)
}

func (r *scheduledTestPlanRepository) ListByAccountID(ctx context.Context, accountID int64) ([]*service.ScheduledTestPlan, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, account_id, model_id, cron_expression, enabled, max_results, auto_recover, last_run_at, next_run_at, created_at, updated_at, pelican_config, running_until
		FROM scheduled_test_plans WHERE account_id = $1
		ORDER BY created_at DESC, id DESC
	`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanPlans(rows)
}

func (r *scheduledTestPlanRepository) ListDue(ctx context.Context, now time.Time) ([]*service.ScheduledTestPlan, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, account_id, model_id, cron_expression, enabled, max_results, auto_recover, last_run_at, next_run_at, created_at, updated_at, pelican_config, running_until
		FROM scheduled_test_plans
		WHERE enabled = true AND next_run_at <= $1
		ORDER BY next_run_at ASC
	`, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanPlans(rows)
}

func (r *scheduledTestPlanRepository) Update(ctx context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	const query = `
		UPDATE scheduled_test_plans
		SET model_id = $2, cron_expression = $3, enabled = $4, max_results = $5, auto_recover = $6, next_run_at = $7, updated_at = NOW(), pelican_config = $8
		WHERE id = $1 AND COALESCE(pelican_config->'quality'->>'action','') <> 'openai_two_fa_admission'
		RETURNING id, account_id, model_id, cron_expression, enabled, max_results, auto_recover, last_run_at, next_run_at, created_at, updated_at, pelican_config, running_until
	`
	args := []any{plan.ID, plan.ModelID, plan.CronExpression, plan.Enabled, plan.MaxResults, plan.AutoRecover, plan.NextRunAt, marshalPelicanConfig(plan.PelicanConfig)}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockMonitoringCoveragePlanWrite(ctx, tx); err != nil {
		return nil, err
	}
	updated, err := scanPlan(tx.QueryRowContext(ctx, query, args...))
	if err != nil {
		return nil, err
	}
	state, err := readQualityState(ctx, tx, plan.ID)
	if err != nil {
		return nil, err
	}
	// Importing configuration cannot implicitly undo account restrictions or
	// discard their ownership. Incompatible live isolation still rejects below.
	if !plan.PreserveQualityState && updated.PelicanConfig != nil && updated.PelicanConfig.Quality != nil &&
		updated.PelicanConfig.Quality.Action == service.QualityActionIsolateModels &&
		(state.Action == "remove_groups" || state.Action == "disable_scheduling") {
		if err = restoreQualityAccountActionForIsolation(ctx, tx, updated, &state); err != nil {
			return nil, err
		}
	}
	if err = checkQualityIsolationScope(updated, &state); err != nil {
		return nil, err
	}
	if hasQualityCooldownOwnership(&state) {
		account := &service.Account{}
		var credentials, extra []byte
		// Use the persisted account ID, never the caller's copy. This follows
		// detection's plan -> account lock order and fences concurrent remapping.
		if err = tx.QueryRowContext(ctx, `SELECT platform,type,COALESCE(credentials,'{}'::jsonb),COALESCE(extra,'{}'::jsonb) FROM accounts WHERE id=$1 FOR UPDATE`, updated.AccountID).Scan(&account.Platform, &account.Type, &credentials, &extra); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(credentials, &account.Credentials); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(extra, &account.Extra); err != nil {
			return nil, err
		}
		if err = checkQualityCooldownTargets(updated, account, &state); err != nil {
			return nil, err
		}
	}
	if err = validateQualityGroupReferences(ctx, tx, updated.PelicanConfig); err != nil {
		return nil, err
	}
	if !plan.PreserveQualityState && updated.PelicanConfig != nil && updated.PelicanConfig.Quality != nil && updated.PelicanConfig.Quality.Action == service.QualityActionObserveOnly {
		if _, err = tx.ExecContext(ctx, `DELETE FROM account_quality_states WHERE plan_id=$1`, plan.ID); err != nil {
			return nil, err
		}
	}

	if err = validateMonitoringCoveragePlanWrite(ctx, tx, updated.Enabled); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

func (r *scheduledTestPlanRepository) Delete(ctx context.Context, id int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var locked int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM scheduled_test_plans WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	if err := deleteScheduledTestPlanTx(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// The caller holds the plan lock. Manual and automated deletion share exactly
// the same ownership checks and never restore or modify the account.
func deleteScheduledTestPlanTx(ctx context.Context, tx *sql.Tx, id int64) error {
	state, err := readQualityState(ctx, tx, id)
	if err != nil {
		return err
	}
	if state.Action == service.QualityActionIsolateModels || state.Models != nil || state.Groups != nil {
		return service.QualityScopeConflict("restore this rule's isolated models and groups before deleting it")
	}
	if hasQualityCooldownOwnership(&state) {
		return service.QualityScopeConflict("restore this rule's cooled models and concurrency before deleting it")
	}
	// Revalidate live ownership as well as durable snapshots. A stale/cleared
	// snapshot must not let deletion leave a marker pointing at a missing rule.
	var liveIsolation, liveCooldowns []byte
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(extra->'quality_model_isolation','{}'::jsonb),COALESCE(extra->'model_rate_limits','{}'::jsonb) FROM accounts WHERE id=(SELECT account_id FROM scheduled_test_plans WHERE id=$1) FOR UPDATE`, id).Scan(&liveIsolation, &liveCooldowns)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	for _, live := range [][]byte{liveIsolation, liveCooldowns} {
		if len(live) == 0 {
			continue
		}
		var entries map[string]json.RawMessage
		if json.Unmarshal(live, &entries) != nil {
			return service.QualityScopeConflict("review unreadable model ownership before deleting this rule")
		}
		for _, raw := range entries {
			owner, _ := qualityEntryOwner(raw, id)
			if owner == "this_rule" {
				return service.QualityScopeConflict("review remaining model ownership before deleting this rule")
			}
		}
	}

	if _, err = tx.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE id=$1`, id); err != nil {
		return err
	}
	return nil
}

func (r *scheduledTestPlanRepository) UpdateAfterRun(ctx context.Context, id int64, lastRunAt time.Time, nextRunAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE scheduled_test_plans SET last_run_at = $2, next_run_at = $3, updated_at = NOW() WHERE id = $1
	`, id, lastRunAt, nextRunAt)
	return err
}

// --- Result Repository ---

type scheduledTestResultRepository struct {
	db *sql.DB
}

func NewScheduledTestResultRepository(db *sql.DB) service.ScheduledTestResultRepository {
	return &scheduledTestResultRepository{db: db}
}

func (r *scheduledTestResultRepository) Create(ctx context.Context, result *service.ScheduledTestResult) (*service.ScheduledTestResult, error) {
	return insertScheduledTestResult(ctx, r.db, result)
}

func insertScheduledTestResult(ctx context.Context, q planRowQuerier, result *service.ScheduledTestResult) (*service.ScheduledTestResult, error) {
	row := q.QueryRowContext(ctx, `
		INSERT INTO scheduled_test_results (plan_id, status, response_text, error_message, latency_ms, started_at, finished_at, created_at, pelican_config, quality_action, quality_judgment, quality_round_id, budget_deferral)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), $8, $9, $10, $11, $12)
		RETURNING id, plan_id, status, response_text, error_message, latency_ms, started_at, finished_at, created_at, pelican_config, quality_action, quality_judgment, quality_round_id, budget_deferral
	`, result.PlanID, result.Status, result.ResponseText, result.ErrorMessage, result.LatencyMs, result.StartedAt, result.FinishedAt, marshalQualityResultConfig(result), result.QualityAction, marshalQualityJudgment(result.QualityJudgment), result.QualityRoundID, marshalMonitoringProbeBudgetDeferral(result.BudgetDeferral))

	out := &service.ScheduledTestResult{}
	var judgment []byte
	var config []byte
	var deferral []byte
	if err := row.Scan(
		&out.ID, &out.PlanID, &out.Status, &out.ResponseText, &out.ErrorMessage,
		&out.LatencyMs, &out.StartedAt, &out.FinishedAt, &out.CreatedAt, &config, &out.QualityAction, &judgment, &out.QualityRoundID, &deferral,
	); err != nil {
		return nil, err
	}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &out.PelicanConfig); err != nil {
			return nil, err
		}
	}
	if len(judgment) > 0 {
		if err := json.Unmarshal(judgment, &out.QualityJudgment); err != nil {
			return nil, err
		}
	}
	if len(deferral) > 0 {
		if err := json.Unmarshal(deferral, &out.BudgetDeferral); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *scheduledTestResultRepository) ListByPlanID(ctx context.Context, planID int64, limit int, includeContent ...bool) ([]*service.ScheduledTestResult, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, plan_id, status, CASE WHEN $3 THEN response_text ELSE '' END, error_message, latency_ms, started_at, finished_at, created_at, `+qualityHistoryConfig+`, quality_action, quality_judgment, quality_round_id, budget_deferral
		FROM scheduled_test_results
		WHERE plan_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2
	`, planID, limit, len(includeContent) == 0 || includeContent[0])
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []*service.ScheduledTestResult
	for rows.Next() {
		r := &service.ScheduledTestResult{}
		var judgment []byte
		var config []byte
		var deferral []byte
		if err := rows.Scan(
			&r.ID, &r.PlanID, &r.Status, &r.ResponseText, &r.ErrorMessage,
			&r.LatencyMs, &r.StartedAt, &r.FinishedAt, &r.CreatedAt, &config, &r.QualityAction, &judgment, &r.QualityRoundID, &deferral,
		); err != nil {
			return nil, err
		}
		if len(config) > 0 {
			if err := json.Unmarshal(config, &r.PelicanConfig); err != nil {
				return nil, err
			}
		}
		if len(judgment) > 0 {
			if err := json.Unmarshal(judgment, &r.QualityJudgment); err != nil {
				return nil, err
			}
		}
		if len(deferral) > 0 {
			if err := json.Unmarshal(deferral, &r.BudgetDeferral); err != nil {
				return nil, err
			}
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

func (r *scheduledTestResultRepository) PruneOldResults(ctx context.Context, planID int64, keepCount int) error {
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM scheduled_test_results
		WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY plan_id ORDER BY created_at DESC, id DESC) AS rn
				FROM scheduled_test_results
				WHERE plan_id = $1
			) ranked
			WHERE rn > $2
		)
	`, planID, keepCount)
	return err
}

// --- scan helpers ---

type scannable interface {
	Scan(dest ...any) error
}

func scanPlan(row scannable, withAccountName ...bool) (*service.ScheduledTestPlan, error) {
	p := &service.ScheduledTestPlan{}
	var config []byte
	dest := []any{
		&p.ID, &p.AccountID, &p.ModelID, &p.CronExpression, &p.Enabled, &p.MaxResults, &p.AutoRecover,
		&p.LastRunAt, &p.NextRunAt, &p.CreatedAt, &p.UpdatedAt, &config, &p.RunningUntil,
	}
	if len(withAccountName) > 0 && withAccountName[0] {
		dest = append(dest, &p.AccountName)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &p.PelicanConfig); err != nil {
			return nil, err
		}
		var internal struct {
			ManualQualityRun bool `json:"_manual_quality_run"`
		}
		if err := json.Unmarshal(config, &internal); err != nil {
			return nil, err
		}
		p.ManualQualityRun = internal.ManualQualityRun
	}
	return p, nil
}

func scanPlans(rows *sql.Rows, withAccountName ...bool) ([]*service.ScheduledTestPlan, error) {
	var plans []*service.ScheduledTestPlan
	for rows.Next() {
		p, err := scanPlan(rows, withAccountName...)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, rows.Err()
}

func marshalPelicanConfig(config *service.PelicanTestConfig) any {
	if config == nil {
		return nil
	}
	data, _ := json.Marshal(config)
	return string(data)
}

// Compare the saved version as well as the due time: a concurrent pause/edit wins.
func (r *scheduledTestPlanRepository) ClaimPelican(ctx context.Context, plan *service.ScheduledTestPlan, now, until, next time.Time) (bool, error) {
	plan.QualityFaultVersion = nil
	plan.QualityFaultRound = ""
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('pelican-account:' || $1::text, 0))`, plan.AccountID).Scan(&locked); err != nil {
		return false, err
	}
	if !locked {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE scheduled_test_plans
 SET running_until = $3, next_run_at = $4, pelican_config = (pelican_config - '_manual_quality_run') ||
 CASE WHEN NOT $9 AND jsonb_typeof(pelican_config->'quality')='object' THEN
 jsonb_build_object('_quality_latest_round',jsonb_build_object('version',1,'lease_until',$3::timestamptz,'rule_version',updated_at))
 ELSE '{}'::jsonb END
 WHERE id = $1 AND enabled = true
 AND (next_run_at <= $2 OR ($7 AND (pelican_config->'quality'->>'trigger_on_upstream_5xx'='true' OR jsonb_typeof(pelican_config->'availability')='object')) OR ($9 AND jsonb_typeof(pelican_config->'availability')='object'))
 AND (NOT $7 OR ((pelican_config->'quality'->>'trigger_on_upstream_5xx'='true' OR jsonb_typeof(pelican_config->'availability')='object')
   AND EXISTS (SELECT 1 FROM accounts WHERE accounts.id=account_id AND type IN ('oauth','apikey'))
   AND ($8::timestamptz IS NULL OR last_run_at IS NULL OR last_run_at < $8 OR pelican_config->'quality'->>'action'='remove_models' OR jsonb_typeof(pelican_config->'availability')='object')))
 AND (NOT $9 OR (jsonb_typeof(pelican_config->'availability')='object' AND next_run_at > clock_timestamp()))
 AND (running_until IS NULL OR running_until < $2) AND updated_at = $5 AND next_run_at = $6
 AND NOT EXISTS (SELECT 1 FROM account_quality_states rest WHERE rest.plan_id=scheduled_test_plans.id
 AND COALESCE((pelican_config->'quality'->>'retest_rest_minutes')::int,0)>0
 AND (rest.state->>'retest_after')::timestamptz>$2)
 AND EXISTS (SELECT 1 FROM accounts WHERE accounts.id = account_id AND deleted_at IS NULL)
 AND NOT EXISTS (SELECT 1 FROM scheduled_test_plans other WHERE other.account_id = scheduled_test_plans.account_id
 AND other.id <> scheduled_test_plans.id AND other.pelican_config IS NOT NULL AND other.running_until > $2)`, plan.ID, now, until, next, plan.UpdatedAt, plan.NextRunAt, plan.TriggerSource == "upstream_5xx", plan.TriggerObservedAt, plan.TriggerSource == service.AvailabilityCoverageSource)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	var bpsRecoveryPending bool
	var accountFingerprint *[32]byte
	var availabilityProbeConnectionID string
	var availabilityEligibleConnections int
	if n == 1 && plan.PelicanConfig != nil && (plan.PelicanConfig.Availability != nil || (plan.PelicanConfig.Quality != nil && plan.PelicanConfig.Quality.UsesModelResults())) {
		fingerprint, err := accountRouteFingerprint(ctx, tx, plan.AccountID, true)
		if err != nil {
			return false, err
		}
		accountFingerprint = &fingerprint
	}
	if n == 1 && plan.PelicanConfig != nil && plan.PelicanConfig.Availability != nil {
		availabilityProbeConnectionID, availabilityEligibleConnections, err = availabilityProbeTarget(ctx, tx, plan, *accountFingerprint)
		if err != nil {
			return false, err
		}
		if plan.TriggerSource == service.AvailabilityCoverageSource && (availabilityProbeConnectionID == "" || plan.PelicanConfig.Availability.MaxExtraSamples <= 0) {
			return false, nil // Roll back the claim when no new eligible grant remains.
		}
	}
	if n == 1 && plan.PelicanConfig != nil && plan.PelicanConfig.Quality != nil {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM account_quality_states WHERE plan_id=$1 AND state->>'action'=$2)`,
			plan.ID, service.QualityActionEnableBPS).Scan(&bpsRecoveryPending); err != nil {
			return false, err
		}
	}
	if n == 1 && plan.PelicanConfig != nil && plan.PelicanConfig.Quality != nil && plan.PelicanConfig.Quality.Action == service.QualityActionRemoveModel {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT (state->>'quality_5xx_episode')::bigint FROM account_quality_states WHERE plan_id=$1),0)`, plan.ID).Scan(&plan.Quality5xxEpisode); err != nil {
			return false, err
		}
	}
	var currentNext time.Time
	var qualityFaultVersion *int64
	if n == 1 && plan.PelicanConfig != nil && (plan.PelicanConfig.Availability != nil || plan.PelicanConfig.TracksQualityFaults()) {
		if err := tx.QueryRowContext(ctx, `SELECT next_run_at,availability_fault_version FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&currentNext, &plan.AvailabilityFaultVersion); err != nil {
			return false, err
		}
		if plan.PelicanConfig.TracksQualityFaults() && plan.TriggerSource != service.AvailabilityCoverageSource {
			version := plan.AvailabilityFaultVersion
			qualityFaultVersion = &version
		}
		if plan.TriggerSource == service.AvailabilityCoverageSource && plan.AvailabilityFaultVersion != plan.AvailabilityExpectedFault {
			return false, nil // A newer business fault owns the next probe.
		}
	}
	if n == 1 && plan.PelicanConfig != nil && plan.PelicanConfig.Quality != nil && plan.PelicanConfig.Quality.OAuth401MaxRounds > 0 {
		if err := claimQualityOAuth401Tx(ctx, tx, plan, until); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if n == 1 && plan.PelicanConfig != nil {
		config := *plan.PelicanConfig
		config.BPSRecoveryPending = bpsRecoveryPending
		plan.PelicanConfig = &config
	}
	if n == 1 {
		plan.QualityFaultVersion = qualityFaultVersion
		if qualityFaultVersion != nil {
			plan.QualityFaultRound = until.Format(time.RFC3339Nano)
		}
		plan.QualityAccountFingerprint = accountFingerprint
		plan.AvailabilityAccountFingerprint = accountFingerprint
		plan.AvailabilityProbeConnectionID = availabilityProbeConnectionID
		plan.AvailabilityEligibleConnections = availabilityEligibleConnections
		if !currentNext.IsZero() && plan.TriggerSource != "upstream_5xx" {
			plan.NextRunAt = &currentNext
		}
	}
	return n == 1, nil
}

// All native quality finish paths use the same marker. Legacy finishers leave
// it incomplete, or change last_run_at without its completion time; neither can
// certify an old result as the latest completed quality round.
const finishQualityRoundMarker = `CASE WHEN pelican_config->'_quality_latest_round'->'lease_until'=to_jsonb($2::timestamptz)
 THEN jsonb_set(pelican_config,'{_quality_latest_round,completed_at}',to_jsonb($3::timestamptz)) ELSE pelican_config END`

func (r *scheduledTestPlanRepository) FinishPelican(ctx context.Context, id int64, until, finished time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE scheduled_test_plans SET running_until = NULL, last_run_at = $3,
 pelican_config=`+finishQualityRoundMarker+`,
 next_run_at=CASE WHEN pelican_config->'quality'->>'action'='openai_two_fa_admission'
 THEN $3::timestamptz+make_interval(secs => (pelican_config->'quality'->'admission'->>'wait_seconds')::int) ELSE next_run_at END
 WHERE id = $1 AND running_until = $2`, id, until, finished)
	return err
}

// FinishAvailabilityCoverage preserves last_run_at and the scheduled cron.
// It releases only the lease held by the current coverage round.
func (r *scheduledTestPlanRepository) FinishAvailabilityCoverage(ctx context.Context, plan *service.ScheduledTestPlan, until time.Time) error {
	result, err := r.db.ExecContext(ctx, `UPDATE scheduled_test_plans SET running_until=NULL WHERE id=$1 AND running_until=$2`, plan.ID, until)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("availability coverage lease changed")
	}
	return nil
}

func (r *scheduledTestPlanRepository) FinishTriggeredQuality(ctx context.Context, plan *service.ScheduledTestPlan, until, finished, next time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE scheduled_test_plans SET running_until=NULL,last_run_at=$3,
 pelican_config=`+finishQualityRoundMarker+`,
 next_run_at=CASE WHEN updated_at=$4 AND next_run_at=$5 AND next_run_at<=$3 THEN $6 ELSE next_run_at END
 WHERE id=$1 AND running_until=$2`, plan.ID, until, finished, plan.UpdatedAt, plan.NextRunAt, next)
	return err
}

// Also prunes paused plans; bounded batches avoid long transactions on large histories.
func (r *scheduledTestResultRepository) PruneExpiredPelican(ctx context.Context, before time.Time) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM scheduled_test_results WHERE id IN (
 SELECT results.id FROM scheduled_test_results results
 JOIN scheduled_test_plans plans ON plans.id = results.plan_id
 WHERE plans.pelican_config IS NOT NULL AND results.created_at < $1
 ORDER BY results.created_at LIMIT 1000)`, before)
	return err
}

func (r *scheduledTestResultRepository) GetResult(ctx context.Context, planID, resultID int64) (*service.ScheduledTestResult, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	out := &service.ScheduledTestResult{}
	var judgment []byte
	var config []byte
	var deferral []byte
	var evaluatedAt, ruleVersion time.Time
	var ruleEnabled bool
	var round qualityMeasurementRoundRead
	var accountID int64
	var faultVersion int64
	var routeJSON []byte
	err = tx.QueryRowContext(ctx, `WITH detail AS (
 SELECT r.*,p.account_id,p.updated_at AS current_rule_version,p.enabled AS current_rule_enabled,
 p.pelican_config->'_quality_latest_round' AS latest_round,p.running_until,p.last_run_at,p.availability_fault_version
 FROM scheduled_test_results r JOIN scheduled_test_plans p ON p.id=r.plan_id
 WHERE r.plan_id=$1 AND r.id=$2
 ), scoped AS (
 SELECT d.account_id,r.id,r.plan_id,r.status,r.created_at,r.pelican_config,r.quality_round_id,r.quality_action,r.quality_judgment,r.error_message,`+qualityMeasuredModel+` AS measured_model
 FROM scheduled_test_results r JOIN detail d ON d.plan_id=r.plan_id AND d.quality_round_id=r.quality_round_id
 WHERE jsonb_typeof(r.pelican_config->'quality')='object'
 `+qualityRoundClassification+`, integrity AS (
 SELECT COALESCE(bool_and(round_complete),false) AS complete,COALESCE(max(round_probes),0)::bigint AS probes,max(round_expected)::bigint AS expected,
 (SELECT COALESCE(bool_and(COALESCE(quality_action NOT IN ('','comparison_pending'),false)),false) FROM scoped) AS finalized,
 (SELECT max(r.finished_at) FROM scheduled_test_results r JOIN detail d ON d.plan_id=r.plan_id AND d.quality_round_id=r.quality_round_id) AS samples_finished_at
 FROM quality_classified
 ) SELECT id, plan_id, status, response_text, error_message, latency_ms, started_at, finished_at, created_at, `+qualityHistoryConfig+`, quality_action, quality_judgment, quality_round_id,
 current_rule_version,current_rule_enabled,latest_round,running_until,last_run_at,complete,probes,expected,finalized,samples_finished_at,account_id,pelican_config->'_quality_observed_route',availability_fault_version,budget_deferral FROM detail CROSS JOIN integrity`, planID, resultID).Scan(
		&out.ID, &out.PlanID, &out.Status, &out.ResponseText, &out.ErrorMessage,
		&out.LatencyMs, &out.StartedAt, &out.FinishedAt, &out.CreatedAt, &config, &out.QualityAction, &judgment, &out.QualityRoundID,
		&ruleVersion, &ruleEnabled, &round.marker, &round.runningUntil, &round.lastRunAt, &round.complete, &round.probes, &round.expected, &round.finalized, &round.samplesFinishedAt, &accountID, &routeJSON, &faultVersion, &deferral)
	if err != nil {
		return nil, err
	}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &out.PelicanConfig); err != nil {
			return nil, err
		}
	}
	if len(judgment) > 0 {
		if err := json.Unmarshal(judgment, &out.QualityJudgment); err != nil {
			return nil, err
		}
	}
	if len(deferral) > 0 {
		if err := json.Unmarshal(deferral, &out.BudgetDeferral); err != nil {
			return nil, err
		}
	}
	var route *qualityRouteRead
	if out.PelicanConfig != nil && out.PelicanConfig.QualityMeasurement != nil {
		route, err = readQualityRoute(ctx, tx, accountID, out, routeJSON)
		if err != nil {
			return nil, err
		}
	}
	// Use the database clock after all snapshot reads. Slow route/connection
	// reads must not extend either the evidence window or connection cooldown.
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&evaluatedAt); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	out.QualityMeasurementValidity = service.AssessQualityMeasurementValidity(out, ruleVersion, ruleEnabled, evaluatedAt)
	if out.QualityMeasurementValidity != nil {
		out.QualityMeasurementValidity.Round = round.assess(out, ruleVersion, ruleEnabled, evaluatedAt)
		out.QualityMeasurementValidity.Route = route.assess(evaluatedAt)
		out.QualityMeasurementValidity.Fault = route.assessFault(out, ruleVersion, ruleEnabled, faultVersion, evaluatedAt)
	}
	return out, nil
}

type qualityMeasurementRoundRead struct {
	marker                  []byte
	runningUntil, lastRunAt sql.NullTime
	samplesFinishedAt       sql.NullTime
	complete, finalized     bool
	probes                  int64
	expected                sql.NullInt64
}

func (read *qualityMeasurementRoundRead) assess(result *service.ScheduledTestResult, ruleVersion time.Time, enabled bool, now time.Time) *service.QualityMeasurementRoundValidity {
	out := &service.QualityMeasurementRoundValidity{Status: "unknown", Reason: "round_not_tracked", SampleCount: read.probes}
	if read.expected.Valid {
		out.ExpectedSampleCount = &read.expected.Int64
	}
	var marker struct {
		Version     int        `json:"version"`
		LeaseUntil  time.Time  `json:"lease_until"`
		RuleVersion time.Time  `json:"rule_version"`
		CompletedAt *time.Time `json:"completed_at"`
	}
	if len(read.marker) == 0 || json.Unmarshal(read.marker, &marker) != nil || marker.Version != 1 || marker.LeaseUntil.IsZero() || marker.RuleVersion.IsZero() {
		return out
	}
	if !marker.RuleVersion.Equal(ruleVersion) || !result.PelicanConfig.QualityMeasurement.RuleVersion.Equal(ruleVersion) {
		out.Reason = "rule_changed"
		return out
	}
	if !enabled {
		out.Reason = "rule_paused"
		return out
	}
	// A legacy/foreign finisher can update the lease or last_run_at without
	// updating our marker. Check that coherence before comparing sample IDs.
	if (read.runningUntil.Valid && !read.runningUntil.Time.Equal(marker.LeaseUntil)) ||
		(marker.CompletedAt != nil && (read.runningUntil.Valid || marker.CompletedAt.IsZero() || marker.CompletedAt.After(now) || !read.lastRunAt.Valid || !marker.CompletedAt.Equal(read.lastRunAt.Time))) {
		out.Reason = "untracked_run"
		return out
	}
	round, err := time.Parse(time.RFC3339Nano, result.QualityRoundID)
	if err != nil {
		return out
	}
	if !round.Equal(marker.LeaseUntil) {
		// Never order by insertion ID or a late result's completion time.
		out.Status, out.Reason = "superseded", "different_claim"
		return out
	}
	if read.runningUntil.Valid {
		out.Status, out.Reason = "incomplete", "lease_expired"
		if now.Before(read.runningUntil.Time) {
			out.Status, out.Reason = "running", "round_running"
		}
		return out
	}
	if marker.CompletedAt == nil {
		out.Status, out.Reason = "incomplete", "round_not_finished"
		return out
	}
	if !read.samplesFinishedAt.Valid || read.samplesFinishedAt.Time.After(*marker.CompletedAt) {
		out.Reason = "untracked_run"
		return out
	}
	out.Status, out.Reason = "incomplete", "round_incomplete"
	if read.complete {
		out.Reason = "comparison_pending"
		if read.finalized {
			out.Status, out.Reason = "latest_complete", "latest_complete"
		}
	}
	return out
}

// ListPelicanHistory lists every retained output, including failures and paused plans.
// Cursor pagination is independent of account-table filters or pages. Bodies are lazy-loaded.
// A non-zero accountID narrows the same history to that account's plans.
func (r *scheduledTestResultRepository) ListPelicanHistory(ctx context.Context, beforeID int64, limit int, accountID int64) ([]*service.PelicanHistoryResult, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT r.id, r.plan_id, r.status, r.error_message,
 r.latency_ms, r.started_at, r.finished_at, r.created_at, r.pelican_config, a.id, a.name
 FROM scheduled_test_results r
 JOIN scheduled_test_plans p ON p.id = r.plan_id
 JOIN accounts a ON a.id = p.account_id
 WHERE p.pelican_config IS NOT NULL AND p.pelican_config->'quality' IS NULL AND a.deleted_at IS NULL
 AND ($1::bigint = 0 OR r.id < $1)
 AND ($3::bigint = 0 OR p.account_id = $3)
 ORDER BY r.id DESC LIMIT $2`, beforeID, limit, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	results := make([]*service.PelicanHistoryResult, 0)
	for rows.Next() {
		result := &service.PelicanHistoryResult{}
		var config []byte
		if err := rows.Scan(&result.ID, &result.PlanID, &result.Status, &result.ErrorMessage,
			&result.LatencyMs, &result.StartedAt, &result.FinishedAt, &result.CreatedAt,
			&config, &result.AccountID, &result.AccountName); err != nil {
			return nil, err
		}
		if len(config) > 0 {
			if err := json.Unmarshal(config, &result.PelicanConfig); err != nil {
				return nil, err
			}
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

func marshalQualityJudgment(judgment *service.QualityJudgment) any {
	if judgment == nil {
		return nil
	}
	data, _ := json.Marshal(judgment)
	return string(data)
}

func marshalMonitoringProbeBudgetDeferral(deferral *service.MonitoringProbeBudgetDenial) any {
	if deferral == nil {
		return nil
	}
	data, _ := json.Marshal(deferral)
	return string(data)
}
