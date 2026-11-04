package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ApplyQuality5xx runs in the response observer, independently of probe leases.
// Availability fencing and opted-in native quality protection commit together.
func (r *accountRepository) ApplyQuality5xx(ctx context.Context, accountID int64) error {
	return r.applyObservedAccountFault(ctx, accountID, true)
}

// ApplyAvailabilityFault invalidates opted-in availability evidence without
// applying quality protection whose trigger is explicitly limited to 5xx.
func (r *accountRepository) ApplyAvailabilityFault(ctx context.Context, accountID int64) error {
	return r.applyObservedAccountFault(ctx, accountID, false)
}

func (r *accountRepository) applyObservedAccountFault(ctx context.Context, accountID int64, quality5xx bool) error {
	db, ok := r.sql.(*sql.DB)
	if !ok {
		return fmt.Errorf("quality protection requires transactional database")
	}
	plans := &scheduledTestPlanRepository{db: db}
	candidates, err := plans.ListByAccountID(ctx, accountID)
	if err != nil {
		return err
	}
	changed := false
	for _, p := range candidates {
		if !p.Enabled || p.PelicanConfig == nil || (p.PelicanConfig.Availability == nil && (!quality5xx || !p.PelicanConfig.TriggersOnUpstream5xx())) {
			continue
		}
		applied, e := plans.applyImmediateAccountFault(ctx, p.ID, accountID, quality5xx)
		if e != nil {
			return e
		}
		changed = changed || applied
	}
	if changed {
		r.syncSchedulerAccountSnapshot(ctx, accountID)
	}
	return nil
}

func (r *scheduledTestPlanRepository) applyImmediateAccountFault(ctx context.Context, planID, accountID int64, quality5xx bool) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	// All quality transitions lock plan then account; never acquire a probe lease.
	plan, err := scanPlan(tx.QueryRowContext(ctx, `SELECT id,account_id,model_id,cron_expression,enabled,max_results,auto_recover,last_run_at,next_run_at,created_at,updated_at,pelican_config,running_until FROM scheduled_test_plans WHERE id=$1 AND account_id=$2 FOR UPDATE`, planID, accountID))
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !plan.Enabled || plan.PelicanConfig == nil {
		return false, nil
	}
	policy := plan.PelicanConfig.Quality
	qualityProtection := quality5xx && policy != nil && policy.TriggerOnUpstream5xx && policy.Action == service.QualityActionRemoveModel
	availability := plan.PelicanConfig.Availability != nil
	if !availability && !qualityProtection {
		return false, nil
	}
	var accountType, status string
	if err = tx.QueryRowContext(ctx, `SELECT type,status FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, accountID).Scan(&accountType, &status); err == sql.ErrNoRows {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if accountType != service.AccountTypeOAuth && accountType != service.AccountTypeAPIKey {
		return false, nil
	}
	if availability {
		if _, err = tx.ExecContext(ctx, `UPDATE scheduled_test_plans SET availability_fault_version=availability_fault_version+1 WHERE id=$1`, planID); err != nil {
			return false, err
		}
	}
	changed := availability
	if qualityProtection && status == "active" {
		applied, applyErr := applyImmediateQualityProtection(ctx, tx, plan, status)
		if applyErr != nil {
			return false, applyErr
		}
		changed = changed || applied
	}
	if !changed {
		return false, nil
	}
	// Durable fallback if Redis publication fails; a running lease stays intact.
	if _, err = tx.ExecContext(ctx, `UPDATE scheduled_test_plans SET next_run_at=$2 WHERE id=$1`, planID, time.Now().UTC()); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// Quality ownership conflicts do not suppress independent availability faults.
// The caller owns the plan/account locks and commits both transitions together.
func applyImmediateQualityProtection(ctx context.Context, tx *sql.Tx, plan *service.ScheduledTestPlan, status string) (bool, error) {
	planID, accountID := plan.ID, plan.AccountID
	policy := plan.PelicanConfig.Quality
	var raw []byte
	state := qualityState{}
	err := tx.QueryRowContext(ctx, `SELECT state FROM account_quality_states WHERE plan_id=$1`, planID).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &state); err != nil {
			return false, err
		}
	}
	if state.Action != "" && state.Action != service.QualityActionRemoveModel {
		return false, nil
	}
	var otherOwner bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_quality_states s JOIN scheduled_test_plans p ON p.id=s.plan_id WHERE p.account_id=$1 AND p.id<>$2 AND COALESCE(s.state->>'action','') NOT IN ('','enable_bps'))`, accountID, planID).Scan(&otherOwner); err != nil {
		return false, err
	}
	if otherOwner {
		return false, nil
	}
	state.Quality5xxEpisode++
	state.NativeRecovery = true
	state.RecoveryConcurrency = policy.RecoveryConcurrency
	if _, err = tx.ExecContext(ctx, `SELECT set_config('oauth_observation.source','quality_5xx',true),set_config('oauth_observation.rule_id',$1,true),set_config('oauth_observation.outcome','upstream_5xx',true)`, fmt.Sprint(planID)); err != nil {
		return false, err
	}
	action, err := applyQualityModelOutcome(ctx, tx, plan, "pending", status, state)
	if err != nil {
		return false, err
	}
	if action != "probe_pending" {
		return false, nil
	}
	return true, nil
}
