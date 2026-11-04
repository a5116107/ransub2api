package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ReadAvailability does not reuse a scheduler/process snapshot: rule, account
// route, event counter and observations belong to one repeatable-read view.
func (r *scheduledTestPlanRepository) ReadAvailability(ctx context.Context, planID int64) (*service.AvailabilityReport, error) {
	reports, err := r.readAvailabilityReports(ctx, "id", planID, nil)
	if err != nil {
		return nil, err
	}
	if len(reports) == 0 {
		return nil, sql.ErrNoRows
	}
	return reports[0], nil
}

// ReadAccountAvailability includes paused and unconfigured rules. It compares
// evidence and the prepared route against the same authoritative account view.
func (r *scheduledTestPlanRepository) ReadAccountAvailability(ctx context.Context, account *service.Account) ([]*service.AvailabilityReport, error) {
	if account == nil || account.ID <= 0 {
		return nil, service.ErrAccountNotFound
	}
	return r.readAvailabilityReports(ctx, "account_id", account.ID, account)
}

type availabilityPlanRead struct {
	report       *service.AvailabilityReport
	version      time.Time
	model        string
	raw          []byte
	faultVersion int64
	config       *service.PelicanTestConfig
}

func (r *scheduledTestPlanRepository) readAvailabilityReports(ctx context.Context, column string, id int64, prepared *service.Account) ([]*service.AvailabilityReport, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// column is selected only by the two repository methods above.
	rows, err := tx.QueryContext(ctx, `SELECT id,account_id,enabled,updated_at,model_id,pelican_config,availability_fault_version FROM scheduled_test_plans WHERE `+column+`=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var plans []availabilityPlanRead
	for rows.Next() {
		plan := availabilityPlanRead{report: &service.AvailabilityReport{Models: []service.AvailabilityModelState{}}}
		if err = rows.Scan(&plan.report.PlanID, &plan.report.AccountID, &plan.report.Enabled, &plan.version, &plan.model, &plan.raw, &plan.faultVersion); err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	reports := make([]*service.AvailabilityReport, 0, len(plans))
	for i := range plans {
		plan := &plans[i]
		if len(plan.raw) > 0 {
			if err = json.Unmarshal(plan.raw, &plan.config); err != nil {
				return nil, err
			}
		}
		plan.report.Configured = plan.config != nil && plan.config.Availability != nil
		if plan.report.Configured {
			if err = readAvailabilityModels(ctx, tx, plan, prepared); err != nil {
				return nil, err
			}
		}
		reports = append(reports, plan.report)
	}
	// Use the clock after ALL reads: database/lock latency cannot keep an
	// expired pass alive, including the first rule in a multi-rule account.
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	for i := range plans {
		plan := &plans[i]
		plan.report.EvaluatedAt = now
		for j := range plan.report.Models {
			item := &plan.report.Models[j]
			decision := item.Decision
			if decision == nil || item.ValidUntil == nil {
				continue
			}
			if plan.config.Availability.ValidForSeconds < 30 || plan.config.Availability.ValidForSeconds > 86400 || decision.Version != 1 || (decision.Status != "pass" && decision.Status != "fail") || decision.StartedAt.IsZero() || decision.FinishedAt.Before(decision.StartedAt) || decision.FinishedAt.After(now) || !item.ValidUntil.Equal(decision.FinishedAt.Add(time.Duration(plan.config.Availability.ValidForSeconds)*time.Second)) {
				item.AvailabilityState = service.AvailabilityState{}
				item.Reason = "invalid_evidence"
				continue
			}
			item.Status = item.StatusAt(now)
			item.Reason = "latest_decision"
			if item.Status == "stale" {
				item.Reason = "expired"
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return reports, nil
}

func readAvailabilityModels(ctx context.Context, tx *sql.Tx, plan *availabilityPlanRead, prepared *service.Account) error {
	report := plan.report
	models := plan.config.ModelIDs
	if len(models) == 0 {
		models = []string{plan.model}
	}
	fingerprint, routeErr := accountRouteFingerprintWithBinding(ctx, tx, report.AccountID, false, prepared)
	if routeErr != nil && !errors.Is(routeErr, sql.ErrNoRows) {
		return routeErr
	}
	for _, model := range models {
		item := service.AvailabilityModelState{ModelID: model, Status: "unknown", Reason: "not_observed"}
		var storedVersion time.Time
		var storedFingerprint, stateJSON []byte
		err := tx.QueryRowContext(ctx, `SELECT plan_version,route_fingerprint,state FROM account_availability_states WHERE plan_id=$1 AND model_id=$2`, report.PlanID, model).Scan(&storedVersion, &storedFingerprint, &stateJSON)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var state availabilityStoredState
			if err = json.Unmarshal(stateJSON, &state); err != nil {
				return err
			}
			switch {
			case !storedVersion.Equal(plan.version):
				item.Reason = "rule_changed"
			case routeErr != nil || !bytes.Equal(storedFingerprint, fingerprint[:]):
				item.Reason = "route_changed"
			case state.FaultVersion == nil:
				item.Reason = "evidence_version_missing"
			case *state.FaultVersion != plan.faultVersion:
				item.Reason = "new_fault_event"
			default:
				item.AvailabilityState = state.AvailabilityState
				item.Reason = "response_not_confirmed"
			}
		}
		if !report.Enabled {
			item.AvailabilityState = service.AvailabilityState{}
			item.Reason = "rule_paused"
		}
		report.Models = append(report.Models, item)
	}
	return nil
}
