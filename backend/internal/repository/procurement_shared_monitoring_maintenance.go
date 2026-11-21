package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func validProcurementMaintenance(m *service.ProcurementSharedMonitoringMaintenance) bool {
	if m == nil || m.ChangedAt.IsZero() || m.Created < 0 || m.Updated < 0 || m.Paused < 0 || m.Created > 1000 || m.Updated > 1000 || m.Paused > 1000 || m.Created+m.Updated+m.Paused > service.ProcurementProductImpactAccountLimit {
		return false
	}
	changes := m.Created + m.Updated + m.Paused
	switch m.State {
	case "manual":
		return !m.Enabled && m.Reason == "manual" && changes == 0
	case "waiting":
		return m.Enabled && m.Reason == "awaiting_tick" && changes == 0
	case "current":
		return m.Enabled && m.Reason == "compatible" && changes == 0
	case "updated":
		return m.Enabled && m.Reason == "compatible" && changes > 0
	case "blocked":
		return m.Enabled && changes == 0 && slices.Contains([]string{"sources_unavailable", "invalid_preset", "inputs_changed", "write_failed", "budget_disabled", "budget_exceeded", "too_many_accounts", "product_missing", "managed_plan_missing", "managed_plan_changed", "managed_plan_running", "existing_observation_plan", "invalid_requirements", "observation_settings_conflict", "native_model_limit", "schedule_timezones_conflict", "invalid_merged_plan"}, m.Reason)
	default:
		return false
	}
}

// Every writer, including background ticks, uses the native budget lock before
// the single authority row. ReadCommitted avoids pre-lock stale snapshots.
func beginProcurementMaintenance(ctx context.Context, db *sql.DB) (*sql.Tx, *procurementObservationState, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	_, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='5s'`)
	if err == nil {
		err = lockMonitoringCoveragePlanWrite(ctx, tx)
	}
	if err == nil {
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1 FOR UPDATE`, procurementSharedMonitoringSettingKey).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
	}
	var saved *procurementObservationState
	if err == nil {
		saved, err = loadProcurementObservationState(ctx, tx)
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, nil, err
	}
	return tx, saved, nil
}

func saveProcurementMaintenanceState(ctx context.Context, tx *sql.Tx, saved *procurementObservationState, report *service.ProcurementSharedMonitoringMaintenance) error {
	if saved.Revision >= domain.MaxProcurementPolicyRevision {
		return service.ErrProcurementSharedMonitoringConflict
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	report.ChangedAt = now
	saved.Revision++
	saved.Maintenance = report
	raw, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settings SET value=$2,updated_at=clock_timestamp() WHERE key=$1`, procurementSharedMonitoringSettingKey, string(raw))
	return err
}

func (r *channelRepository) ConfigureProcurementSharedMonitoringMaintenance(ctx context.Context, command *service.ProcurementSharedMonitoringMaintenanceCommand) (*service.ProcurementSharedMonitoringState, error) {
	if err := procurementManagementAccess(ctx, 1); err != nil {
		return nil, err
	}
	if command == nil || command.Version != 1 || !command.Confirm || command.ExpectedRevision < 0 || command.ExpectedRevision >= domain.MaxProcurementPolicyRevision {
		return nil, service.ErrProcurementSharedMonitoringCommand
	}
	tx, saved, err := beginProcurementMaintenance(ctx, r.db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if saved.Revision != command.ExpectedRevision || saved.Revision == 0 {
		return nil, service.ErrProcurementSharedMonitoringConflict
	}
	report := &service.ProcurementSharedMonitoringMaintenance{State: "manual", Reason: "manual"}
	if command.Enabled {
		if len(saved.Products) == 0 {
			return nil, service.ErrProcurementSharedMonitoringConflict
		}
		input := &service.ProcurementSharedMonitoringCommand{Version: 2, Products: saved.Products}
		compiled, err := compileProcurementSharedMonitoring(ctx, tx, input, time.Time{}, true)
		if err != nil {
			return nil, err
		}
		if !compiled.preview.ApplicationReview.CanConfirm {
			return nil, service.ErrProcurementSharedMonitoringConflict
		}
		report.Enabled, report.State, report.Reason = true, "waiting", "awaiting_tick"
	}
	if err := saveProcurementMaintenanceState(ctx, tx, saved, report); err != nil {
		return nil, err
	}
	out, err := readProcurementSharedMonitoring(ctx, tx)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// Attached to the existing plan repository so the original minute tick owns
// reconciliation and execution ordering; no extra goroutine or detector exists.
func (r *scheduledTestPlanRepository) ReconcileProcurementSharedMonitoring(ctx context.Context) error {
	return (&channelRepository{db: r.db}).ReconcileProcurementSharedMonitoring(ctx)
}

func (r *channelRepository) ReconcileProcurementSharedMonitoring(ctx context.Context) error {
	if err := procurementManagementAccess(ctx, 1); err != nil {
		return err
	}
	// The usual disabled case does not take a writer lock or create settings.
	saved, err := loadProcurementObservationState(ctx, r.db)
	if err != nil || saved.Maintenance == nil || !saved.Maintenance.Enabled {
		return err
	}
	tx, saved, err := beginProcurementMaintenance(ctx, r.db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if saved.Maintenance == nil || !saved.Maintenance.Enabled {
		return nil
	}
	if saved.Revision >= domain.MaxProcurementPolicyRevision {
		return service.ErrProcurementSharedMonitoringConflict
	}
	input := &service.ProcurementSharedMonitoringCommand{Version: 2, Products: saved.Products}
	// A failed native write rolls back its entire batch before recording an
	// outcome. The authority/budget locks were acquired outside this savepoint.
	if _, err = tx.ExecContext(ctx, `SAVEPOINT shared_observation_tick`); err != nil {
		return err
	}
	compiled, runErr := compileProcurementSharedMonitoring(ctx, tx, input, time.Time{}, true)
	if runErr == nil && compiled.preview.ApplicationReview.CanConfirm {
		first := compiled.preview.ApplicationReview
		runErr = lockProcurementSharedInputs(ctx, tx, compiled)
		if runErr == nil {
			compiled, runErr = compileProcurementSharedMonitoring(ctx, tx, input, first.ObservedAt, true)
			if runErr == nil && compiled.preview.ApplicationReview.Fingerprint != first.Fingerprint {
				runErr = service.ErrProcurementSharedMonitoringConflict
			}
		}
	}
	report := &service.ProcurementSharedMonitoringMaintenance{Enabled: true, State: "blocked", Reason: "write_failed"}
	if runErr == nil {
		p := compiled.preview
		switch {
		case p.State == "incomplete":
			report.Reason = "sources_unavailable"
		case p.State == "conflict":
			for _, change := range p.ApplicationReview.Changes {
				if change.Operation == "conflict" {
					report.Reason = change.Reason
					break
				}
			}
		case !p.ApplicationReview.CanConfirm:
			report.Reason = "budget_exceeded"
			if p.Coverage != nil && !p.Coverage.Enforced {
				report.Reason = "budget_disabled"
			}
		default:
			report.State, report.Reason = "current", "compatible"
			for _, change := range p.ApplicationReview.Changes {
				switch change.Operation {
				case "create":
					report.Created++
				case "update":
					report.Updated++
				case "pause":
					report.Paused++
				}
			}
			if report.Created+report.Updated+report.Paused > 0 {
				report.State, report.ChangedAt = "updated", p.ObservedAt
				_, runErr = applyProcurementSharedCompilation(ctx, tx, compiled, saved.Products, report, compiled.requireBudget)
				if runErr == nil {
					return tx.Commit()
				}
				report.State, report.Reason, report.Created, report.Updated, report.Paused = "blocked", "write_failed", 0, 0, 0
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT shared_observation_tick`); err != nil {
		return err
	}
	if runErr != nil {
		switch {
		case errors.Is(runErr, service.ErrProcurementSharedMonitoringConflict):
			report.Reason = "inputs_changed"
		case errors.Is(runErr, service.ErrProcurementMonitoringRecipeNotFound), errors.Is(runErr, service.ErrProcurementMonitoringRecipeInvalid):
			report.Reason = "invalid_preset"
		case errors.Is(runErr, service.ErrProcurementProductImpactTooLarge):
			report.Reason = "too_many_accounts"
		case errors.Is(runErr, service.ErrGroupNotFound):
			report.Reason = "product_missing"
		}
	}
	// Keep the last actual update's counts on steady ticks, and never rewrite
	// unchanged failure outcomes. ChangedAt is explicitly not a tick timestamp.
	previous := saved.Maintenance
	if report.State == "current" && previous.Reason == "compatible" || previous.State == report.State && previous.Reason == report.Reason {
		return nil
	}
	if err := saveProcurementMaintenanceState(ctx, tx, saved, report); err != nil {
		return err
	}
	return tx.Commit()
}
