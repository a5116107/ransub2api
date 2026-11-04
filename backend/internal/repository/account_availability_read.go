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
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	report := &service.AvailabilityReport{PlanID: planID, Models: []service.AvailabilityModelState{}}
	var version time.Time
	var model string
	var raw []byte
	var faultVersion int64
	if err = tx.QueryRowContext(ctx, `SELECT account_id,enabled,updated_at,model_id,pelican_config,availability_fault_version FROM scheduled_test_plans WHERE id=$1`, planID).Scan(&report.AccountID, &report.Enabled, &version, &model, &raw, &faultVersion); err != nil {
		return nil, err
	}
	var cfg *service.PelicanTestConfig
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
	}
	report.Configured = cfg != nil && cfg.Availability != nil
	if !report.Configured {
		if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&report.EvaluatedAt); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return report, nil
	}
	models := cfg.ModelIDs
	if len(models) == 0 {
		models = []string{model}
	}
	fingerprint, routeErr := accountRouteFingerprint(ctx, tx, report.AccountID, false)
	if routeErr != nil && !errors.Is(routeErr, sql.ErrNoRows) {
		return nil, routeErr
	}
	for _, model := range models {
		item := service.AvailabilityModelState{ModelID: model, Status: "unknown", Reason: "not_observed"}
		var storedVersion time.Time
		var storedFingerprint, stateJSON []byte
		err = tx.QueryRowContext(ctx, `SELECT plan_version,route_fingerprint,state FROM account_availability_states WHERE plan_id=$1 AND model_id=$2`, planID, model).Scan(&storedVersion, &storedFingerprint, &stateJSON)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			var state availabilityStoredState
			if err = json.Unmarshal(stateJSON, &state); err != nil {
				return nil, err
			}
			switch {
			case !storedVersion.Equal(version):
				item.Reason = "rule_changed"
			case routeErr != nil || !bytes.Equal(storedFingerprint, fingerprint[:]):
				item.Reason = "route_changed"
			case state.FaultVersion == nil:
				item.Reason = "evidence_version_missing"
			case *state.FaultVersion != faultVersion:
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
	// Use the clock after all reads: database/lock latency cannot keep an expired
	// pass alive merely because the transaction began before its deadline.
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&report.EvaluatedAt); err != nil {
		return nil, err
	}
	for i := range report.Models {
		item := &report.Models[i]
		decision := item.Decision
		if decision == nil || item.ValidUntil == nil {
			continue
		}
		if decision.Version != 1 || (decision.Status != "pass" && decision.Status != "fail") || decision.StartedAt.IsZero() || decision.FinishedAt.Before(decision.StartedAt) || decision.FinishedAt.After(report.EvaluatedAt) || !item.ValidUntil.Equal(decision.FinishedAt.Add(time.Duration(cfg.Availability.ValidForSeconds)*time.Second)) {
			item.AvailabilityState = service.AvailabilityState{}
			item.Reason = "invalid_evidence"
			continue
		}
		item.Status = item.StatusAt(report.EvaluatedAt)
		item.Reason = "latest_decision"
		if item.Status == "stale" {
			item.Reason = "expired"
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return report, nil
}
