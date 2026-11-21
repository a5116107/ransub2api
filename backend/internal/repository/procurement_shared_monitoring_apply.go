package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

func (r *channelRepository) ApplyProcurementSharedMonitoring(ctx context.Context, command *service.ProcurementSharedMonitoringApplyCommand) (out *service.ProcurementSharedMonitoringState, err error) {
	if command.Validate() != nil {
		return nil, service.ErrProcurementSharedMonitoringCommand
	}
	if err := procurementManagementAccess(ctx, 1); err != nil {
		return nil, err
	}
	defer func() {
		var pg *pq.Error
		if errors.As(err, &pg) && slices.Contains([]pq.ErrorCode{"23505", "23503", "40001", "40P01", "55P03"}, pg.Code) {
			err = service.ErrProcurementSharedMonitoringConflict
		}
	}()
	// Do not pin a repeatable-read snapshot before waiting for the native
	// budget writer. The post-lock reads must include its committed demand.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='5s'`); err != nil {
		return nil, err
	}
	if err = lockMonitoringCoveragePlanWrite(ctx, tx); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES($1,'{"version":1,"revision":0,"products":[],"plans":[]}',clock_timestamp()) ON CONFLICT(key) DO NOTHING`, procurementSharedMonitoringSettingKey); err != nil {
		return nil, err
	}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1 FOR UPDATE`, procurementSharedMonitoringSettingKey).Scan(&raw); err != nil {
		return nil, err
	}
	compiled, err := compileProcurementSharedMonitoring(ctx, tx, &command.ProcurementSharedMonitoringCommand, command.ObservedAt)
	if err != nil {
		return nil, err
	}
	if compiled.saved.Revision != command.Revision || !validProcurementSharedConsent(compiled.preview.ApplicationReview, command) {
		return nil, service.ErrProcurementSharedMonitoringConflict
	}
	if err = lockProcurementSharedInputs(ctx, tx, compiled, command.Products); err != nil {
		return nil, err
	}
	compiled, err = compileProcurementSharedMonitoring(ctx, tx, &command.ProcurementSharedMonitoringCommand, command.ObservedAt)
	if err != nil {
		return nil, err
	}
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	review := compiled.preview.ApplicationReview
	if compiled.saved.Revision != command.Revision || !validProcurementSharedConsent(review, command) || now.Before(command.ObservedAt) || !now.Before(review.ExpiresAt) {
		return nil, service.ErrProcurementSharedMonitoringConflict
	}
	next := &procurementObservationState{Version: 1, Revision: compiled.saved.Revision + 1, Products: append([]service.ProcurementMonitoringProductSelection{}, command.Products...), Plans: []procurementObservationOwner{}}
	slices.SortFunc(next.Products, func(a, b service.ProcurementMonitoringProductSelection) int {
		if a.ProductID < b.ProductID {
			return -1
		}
		if a.ProductID > b.ProductID {
			return 1
		}
		return 0
	})
	for _, change := range review.Changes {
		plan := compiled.plans[change.AccountID]
		if plan == nil {
			return nil, service.ErrProcurementSharedMonitoringConflict
		}
		var saved *service.ScheduledTestPlan
		switch change.Operation {
		case "create":
			saved, err = insertScheduledTestPlanRow(ctx, tx, plan)
		case "update":
			saved, err = updateScheduledTestPlanTx(ctx, tx, plan, false)
		case "pause", "release":
			plan.PreserveQualityState = true
			saved, err = updateScheduledTestPlanTx(ctx, tx, plan, false)
		case "retain", "retain_paused":
			saved = plan
		default:
			return nil, service.ErrProcurementSharedMonitoringConflict
		}
		if err != nil {
			return nil, err
		}
		if change.Operation == "release" {
			continue
		}
		config, err := json.Marshal(saved.PelicanConfig)
		if err != nil {
			return nil, err
		}
		definition, err := procurementObservationDefinition(saved, config)
		if err != nil {
			return nil, err
		}
		next.Plans = append(next.Plans, procurementObservationOwner{AccountID: saved.AccountID, PlanID: saved.ID, Definition: definition})
	}
	if len(next.Plans) > service.ProcurementProductImpactAccountLimit {
		return nil, service.ErrProcurementProductImpactTooLarge
	}
	// Re-read real rows before recording their provenance; unexpected trigger
	// changes, lost rows or an unsupported definition never acquire ownership.
	native, err := loadProcurementObservationPlans(ctx, tx, next.Plans)
	if err != nil {
		return nil, err
	}
	for _, owner := range next.Plans {
		if procurementObservationOwnerReason(owner, native, now) != "compatible" {
			return nil, service.ErrProcurementSharedMonitoringConflict
		}
	}
	if err = validateMonitoringCoveragePlanWrite(ctx, tx, len(command.Products) > 0); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settings SET value=$2,updated_at=clock_timestamp() WHERE key=$1`, procurementSharedMonitoringSettingKey, string(encoded)); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	if !now.Before(review.ExpiresAt) {
		return nil, service.ErrProcurementSharedMonitoringConflict
	}
	out, err = readProcurementSharedMonitoring(ctx, tx)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func validProcurementSharedConsent(review *service.ProcurementSharedMonitoringReview, command *service.ProcurementSharedMonitoringApplyCommand) bool {
	return review != nil && review.CanConfirm && review.Revision == command.Revision && review.ObservedAt.Equal(command.ObservedAt) && review.Fingerprint == command.Fingerprint
}

// The runner also uses plan -> account locks. Product/source and preset share
// locks fence revocation; account locks plus membership locks fence both the
// original FK-backed inserts and native deletes for every affected account.
func lockProcurementSharedInputs(ctx context.Context, tx *sql.Tx, compiled *procurementSharedMonitoringCompilation, products []service.ProcurementMonitoringProductSelection) error {
	groups := append([]int64{}, compiled.sourceGroups...)
	presets := []int64{}
	for _, p := range products {
		groups = append(groups, p.ProductID)
		presets = append(presets, p.PresetID)
	}
	slices.Sort(groups)
	groups = slices.Compact(groups)
	slices.Sort(presets)
	presets = slices.Compact(presets)
	var found int64
	for _, id := range groups {
		if err := tx.QueryRowContext(ctx, `SELECT id FROM groups WHERE id=$1 AND deleted_at IS NULL FOR SHARE`, id).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return service.ErrProcurementSharedMonitoringConflict
			}
			return err
		}
	}
	for _, id := range presets {
		if err := tx.QueryRowContext(ctx, `SELECT id FROM quality_rule_presets WHERE id=$1 FOR SHARE`, id).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return service.ErrProcurementSharedMonitoringConflict
			}
			return err
		}
	}
	planIDs := []int64{}
	for _, p := range compiled.saved.Plans {
		planIDs = append(planIDs, p.PlanID)
	}
	slices.Sort(planIDs)
	for _, id := range planIDs {
		if err := tx.QueryRowContext(ctx, `SELECT id FROM scheduled_test_plans WHERE id=$1 FOR UPDATE`, id).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return service.ErrProcurementSharedMonitoringConflict
			}
			return err
		}
	}
	accounts := make([]int64, 0, len(compiled.plans))
	for id := range compiled.plans {
		accounts = append(accounts, id)
	}
	slices.Sort(accounts)
	for _, id := range accounts {
		if err := tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return service.ErrProcurementSharedMonitoringConflict
			}
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT account_id FROM account_groups WHERE account_id=ANY($1) ORDER BY account_id,group_id FOR SHARE`, pq.Array(accounts))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := rows.Scan(&found); err != nil {
			return err
		}
	}
	return rows.Err()
}
