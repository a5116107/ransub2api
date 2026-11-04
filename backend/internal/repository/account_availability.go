package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// A round writes all model observations atomically without changing account,
// quality ownership, or native cooldowns. Claim and rule checks precede writes.
func (r *scheduledTestPlanRepository) ApplyAvailabilityOutcomes(ctx context.Context, plan *service.ScheduledTestPlan, until time.Time, outcomes []service.AvailabilityModelOutcome) (string, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var valid bool
	var rawConfig []byte
	var storedModel string
	var now time.Time
	err = tx.QueryRowContext(ctx, `SELECT enabled AND updated_at=$2 AND running_until=$3 AND running_until>clock_timestamp(),pelican_config,clock_timestamp(),model_id
 FROM scheduled_test_plans WHERE id=$1 AND account_id=$4 FOR UPDATE`, plan.ID, plan.UpdatedAt, until, plan.AccountID).Scan(&valid, &rawConfig, &now, &storedModel)
	if err == sql.ErrNoRows || (err == nil && !valid) {
		return "stale_run", nil
	}
	if err != nil {
		return "", err
	}
	var cfg service.PelicanTestConfig
	if err = json.Unmarshal(rawConfig, &cfg); err != nil {
		return "", err
	}
	if cfg.Availability == nil || cfg.Availability.ValidForSeconds < 30 || cfg.Availability.ValidForSeconds > 86400 || plan.AvailabilityAccountFingerprint == nil {
		return "stale_run", nil
	}
	fingerprint, err := qualityAccountRouteFingerprint(ctx, tx, plan.AccountID)
	if err == sql.ErrNoRows {
		return "stale_run", nil
	}
	if err != nil {
		return "", err
	}
	if fingerprint != *plan.AvailabilityAccountFingerprint {
		return "stale_run", nil
	}
	var episode int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT (state->>'quality_5xx_episode')::bigint FROM account_quality_states WHERE plan_id=$1),0)`, plan.ID).Scan(&episode); err != nil {
		return "", err
	}
	if episode != plan.AvailabilityQuality5xxEpisode {
		return "stale_run", nil
	}
	models := cfg.ModelIDs
	if len(models) == 0 {
		models = []string{storedModel}
	}
	if len(outcomes) != len(models) {
		return "", fmt.Errorf("availability round must cover every configured model")
	}
	seen := map[string]bool{}
	changed := false
	for _, outcome := range outcomes {
		allowed := false
		for _, model := range models {
			allowed = allowed || model == outcome.ModelID
		}
		probe := outcome.Probe
		if !allowed || seen[outcome.ModelID] || probe == nil || probe.Version != 1 || (probe.Status != "pass" && probe.Status != "fail" && probe.Status != "inconclusive") {
			return "", fmt.Errorf("invalid availability model observation")
		}
		seen[outcome.ModelID] = true
		if probe.Status != "inconclusive" && (probe.StartedAt.IsZero() || probe.FinishedAt.Before(probe.StartedAt) || probe.FinishedAt.After(now)) {
			return "", fmt.Errorf("invalid availability observation time")
		}
		state := service.AvailabilityState{}
		var oldVersion, oldRound time.Time
		var oldFingerprint, raw []byte
		err = tx.QueryRowContext(ctx, `SELECT plan_version,route_fingerprint,round_lease,state FROM account_availability_states WHERE plan_id=$1 AND model_id=$2`, plan.ID, outcome.ModelID).Scan(&oldVersion, &oldFingerprint, &oldRound, &raw)
		if err != nil && err != sql.ErrNoRows {
			return "", err
		}
		if err == nil && oldVersion.Equal(plan.UpdatedAt) && bytes.Equal(oldFingerprint, fingerprint[:]) {
			if oldRound.Equal(until) {
				continue // Duplicate delivery cannot revise a committed round.
			}
			if err = json.Unmarshal(raw, &state); err != nil {
				return "", err
			}
		}
		state.Observation = probe
		if probe.Status == "pass" || probe.Status == "fail" {
			state.Decision = probe
			validUntil := probe.FinishedAt.Add(time.Duration(cfg.Availability.ValidForSeconds) * time.Second)
			state.ValidUntil = &validUntil
		}
		raw, err = json.Marshal(state)
		if err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO account_availability_states(plan_id,model_id,plan_version,route_fingerprint,round_lease,state)
 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(plan_id,model_id) DO UPDATE SET plan_version=EXCLUDED.plan_version,
 route_fingerprint=EXCLUDED.route_fingerprint,round_lease=EXCLUDED.round_lease,state=EXCLUDED.state`, plan.ID, outcome.ModelID, plan.UpdatedAt, fingerprint[:], until, string(raw)); err != nil {
			return "", err
		}
		changed = true
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM account_availability_states WHERE plan_id=$1 AND NOT (model_id=ANY($2::text[]))`, plan.ID, pq.Array(models)); err != nil {
		return "", err
	}
	// Locks may have waited past the lease after the first check. Roll back all
	// model updates rather than publishing an expired round.
	if err = tx.QueryRowContext(ctx, `SELECT running_until=$2 AND running_until>clock_timestamp() FROM scheduled_test_plans WHERE id=$1`, plan.ID, until).Scan(&valid); err != nil {
		return "", err
	}
	if !valid {
		return "stale_run", nil
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	if !changed {
		return "duplicate_round", nil
	}
	return "observed", nil
}
