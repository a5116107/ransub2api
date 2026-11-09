package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// The event counter is internal fencing data, not part of the public report.
type availabilityStoredState struct {
	service.AvailabilityState
	FaultVersion               *int64                               `json:"fault_version"`
	ObservationConnectionScope string                               `json:"observation_connection_scope,omitempty"`
	DecisionConnectionScope    string                               `json:"decision_connection_scope,omitempty"`
	Connections                map[string]service.AvailabilityState `json:"connections,omitempty"`
}

// Bound storage even if connection revisions are rotated repeatedly. Evicted
// evidence becomes unknown, never a pass, and needs another successful probe.
const maxAvailabilityConnectionStates = 256

func validAvailabilityConnectionScope(scope string) bool {
	if len(scope) != 64 {
		return false
	}
	value, err := hex.DecodeString(scope)
	return err == nil && hex.EncodeToString(value) == scope
}

func restoreAvailabilityConnectionScope(state *service.AvailabilityState, scope string) {
	if state.Observation != nil && state.Observation.Scope != nil {
		state.Observation.Scope.ConnectionScope = scope
	}
	if state.Decision != nil && state.Decision.Scope != nil {
		state.Decision.Scope.ConnectionScope = scope
	}
}

// Promote the previous single managed-connection record in place. An
// unbound parent-only decision is deliberately not evidence for any grant.
func (s *availabilityStoredState) promoteLegacyConnection() {
	s.restoreConnectionScopes()
	if s.Decision != nil && s.Decision.Scope != nil {
		scope := s.Decision.Scope.ConnectionScope
		if validAvailabilityConnectionScope(scope) {
			if s.Connections == nil {
				s.Connections = make(map[string]service.AvailabilityState)
			}
			if _, exists := s.Connections[scope]; !exists {
				legacy := service.AvailabilityState{Decision: s.Decision, ValidUntil: s.ValidUntil}
				if s.Observation != nil && s.Observation.Scope != nil && s.Observation.Scope.ConnectionScope == scope {
					legacy.Observation = s.Observation
				}
				s.Connections[scope] = legacy
			}
		}
	}
	s.AvailabilityState = service.AvailabilityState{}
	s.ObservationConnectionScope, s.DecisionConnectionScope = "", ""
}

func (s *availabilityStoredState) pruneConnections(keep string) {
	for scope := range s.Connections {
		if !validAvailabilityConnectionScope(scope) {
			delete(s.Connections, scope)
		}
	}
	if len(s.Connections) <= maxAvailabilityConnectionStates {
		return
	}
	keys := make([]string, 0, len(s.Connections))
	for scope := range s.Connections {
		if scope != keep {
			keys = append(keys, scope)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := s.Connections[keys[i]].ValidUntil, s.Connections[keys[j]].ValidUntil
		if a == nil || b == nil {
			if (a == nil) != (b == nil) {
				return a == nil
			}
		} else if !a.Equal(*b) {
			return a.Before(*b)
		}
		return keys[i] < keys[j]
	})
	for _, scope := range keys[:len(s.Connections)-maxAvailabilityConnectionStates] {
		delete(s.Connections, scope)
	}
}

func availabilityAccountManaged(ctx context.Context, tx *sql.Tx, accountID int64) (bool, error) {
	var managed bool
	err := tx.QueryRowContext(ctx, `SELECT platform='openai' AND COALESCE(extra->'openai_connections_managed'='true'::jsonb,false)
 FROM accounts WHERE id=$1 AND deleted_at IS NULL`, accountID).Scan(&managed)
	return managed, err
}

func (s *availabilityStoredState) restoreConnectionScopes() {
	if s.Observation != nil && s.Observation.Scope != nil {
		s.Observation.Scope.ConnectionScope = s.ObservationConnectionScope
	}
	if s.Decision != nil && s.Decision.Scope != nil {
		s.Decision.Scope.ConnectionScope = s.DecisionConnectionScope
	}
}

func (s *availabilityStoredState) captureConnectionScopes() {
	s.ObservationConnectionScope = ""
	if s.Observation != nil && s.Observation.Scope != nil {
		s.ObservationConnectionScope = s.Observation.Scope.ConnectionScope
	}
	s.DecisionConnectionScope = ""
	if s.Decision != nil && s.Decision.Scope != nil {
		s.DecisionConnectionScope = s.Decision.Scope.ConnectionScope
	}
}

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
	var faultVersion int64
	err = tx.QueryRowContext(ctx, `SELECT enabled AND updated_at=$2 AND running_until=$3 AND running_until>clock_timestamp(),pelican_config,clock_timestamp(),model_id,availability_fault_version
 FROM scheduled_test_plans WHERE id=$1 AND account_id=$4 FOR UPDATE`, plan.ID, plan.UpdatedAt, until, plan.AccountID).Scan(&valid, &rawConfig, &now, &storedModel, &faultVersion)
	if err == sql.ErrNoRows || (err == nil && !valid) {
		return "stale_run", nil
	}
	if err != nil {
		return "", err
	}
	if faultVersion != plan.AvailabilityFaultVersion {
		return "stale_run", nil
	}
	var cfg service.PelicanTestConfig
	if err = json.Unmarshal(rawConfig, &cfg); err != nil {
		return "", err
	}
	if cfg.Availability == nil || cfg.Availability.ValidForSeconds < 30 || cfg.Availability.ValidForSeconds > 86400 || plan.AvailabilityAccountFingerprint == nil {
		return "stale_run", nil
	}
	fingerprint, err := accountRouteFingerprint(ctx, tx, plan.AccountID, true)
	if err == sql.ErrNoRows {
		return "stale_run", nil
	}
	if err != nil {
		return "", err
	}
	if fingerprint != *plan.AvailabilityAccountFingerprint {
		return "stale_run", nil
	}
	managed, err := availabilityAccountManaged(ctx, tx, plan.AccountID)
	if err != nil {
		return "", err
	}
	models := cfg.ModelIDs
	if len(models) == 0 {
		models = []string{storedModel}
	}
	if len(outcomes) != len(models) {
		return "", fmt.Errorf("availability round must cover every configured model")
	}
	seen := map[string]bool{}
	unattributedFailure := false
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
		if managed && probe.Status == "fail" && (probe.Scope == nil || !validAvailabilityConnectionScope(probe.Scope.ConnectionScope)) {
			unattributedFailure = true
		}
	}
	if unattributedFailure {
		var committed int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM account_availability_states
 WHERE plan_id=$1 AND model_id=ANY($2::text[]) AND plan_version=$3 AND route_fingerprint=$4 AND round_lease=$5`,
			plan.ID, pq.Array(models), plan.UpdatedAt, fingerprint[:], until).Scan(&committed); err != nil {
			return "", err
		}
		if committed == len(models) {
			return "duplicate_round", nil
		}
		// A real failure whose connection could not be established must not
		// leave any potentially affected managed grant authorized.
		// Avoid a tight self-triggering loop when each probe encounters the
		// same unattributed transport failure; an earlier cron is preserved.
		if _, err = tx.ExecContext(ctx, `UPDATE scheduled_test_plans SET availability_fault_version=availability_fault_version+1,next_run_at=LEAST(next_run_at,clock_timestamp()+interval '30 seconds') WHERE id=$1`, plan.ID); err != nil {
			return "", err
		}
		if err = tx.QueryRowContext(ctx, `SELECT running_until=$2 AND running_until>clock_timestamp() FROM scheduled_test_plans WHERE id=$1`, plan.ID, until).Scan(&valid); err != nil {
			return "", err
		}
		if !valid {
			return "stale_run", nil
		}
		if err = tx.Commit(); err != nil {
			return "", err
		}
		return "unattributed_failure", nil
	}
	changed := false
	for _, outcome := range outcomes {
		probe := outcome.Probe
		state := availabilityStoredState{}
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
			state.restoreConnectionScopes()
			if state.FaultVersion == nil || *state.FaultVersion != faultVersion {
				state = availabilityStoredState{}
			}
		}
		if managed {
			state.promoteLegacyConnection()
			// No confirmed connection is not permission to change any one of
			// the existing decisions, but record the lease so duplicate delivery
			// cannot later revise an already committed unattributed round.
			scope := ""
			if probe.Scope != nil && validAvailabilityConnectionScope(probe.Scope.ConnectionScope) {
				scope = probe.Scope.ConnectionScope
				if state.Connections == nil {
					state.Connections = make(map[string]service.AvailabilityState)
				}
				connection := state.Connections[scope]
				restoreAvailabilityConnectionScope(&connection, scope)
				connection.Observation = probe
				if probe.Status == "pass" || probe.Status == "fail" {
					connection.Decision = probe
					validUntil := probe.FinishedAt.Add(time.Duration(cfg.Availability.ValidForSeconds) * time.Second)
					connection.ValidUntil = &validUntil
				}
				state.Connections[scope] = connection
			}
			state.pruneConnections(scope)
		} else {
			state.Connections = nil
			state.Observation = probe
			if probe.Status == "pass" || probe.Status == "fail" {
				state.Decision = probe
				validUntil := probe.FinishedAt.Add(time.Duration(cfg.Availability.ValidForSeconds) * time.Second)
				state.ValidUntil = &validUntil
			}
		}
		state.FaultVersion = &faultVersion
		state.captureConnectionScopes()
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
