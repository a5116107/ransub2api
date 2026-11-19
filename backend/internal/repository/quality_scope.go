package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// Consume the same complete rule projection as the admin preview, never the
// preview's caller-facing JSON or a cached account-level green flag.
func (r *groupRepository) ReadQualityScopeEvidence(ctx context.Context, selected *service.Account, scope service.AvailabilityRequestScope) (*service.QualityScopeEvidence, error) {
	db, ok := r.sql.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok || selected == nil {
		return nil, errors.New("quality evidence authority unavailable")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	client := dbent.NewClient(dbent.Driver(entsql.NewDriver(dialect.Postgres, entsql.Conn{ExecQuerier: tx})))
	entities, err := observerAccountQuery(ctx, client.Account.Query()).Where(dbaccount.IDEQ(selected.ID), dbaccount.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := newAccountRepositoryWithSQL(client, tx, nil).accountsToService(ctx, entities)
	if err != nil {
		return nil, err
	}
	expected, confirmed := service.AvailabilityPreparedRouteFingerprint(selected)
	if len(accounts) != 1 || !confirmed || service.AccountRouteFingerprint(&accounts[0]) != expected {
		return nil, errors.New("quality account binding changed")
	}
	read, err := readQualityScopesInTx(ctx, tx, []int64{selected.ID})
	if err != nil {
		return nil, err
	}
	var at time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
		return nil, err
	}
	rules := read.assess(at)
	if selected.HasManagedOpenAIConnections() {
		for _, rule := range rules[selected.ID] {
			for _, sample := range rule.Samples {
				if observed := sample.QualityRouteEvidence; observed == nil || observed.ConnectionID != selected.AuthConnectionID || observed.ConnectionRevision != selected.AuthConnectionRevision {
					sample.QualityMeasurementValidity.Route = &service.QualityMeasurementRouteValidity{Status: "unknown", Reason: "connection_changed"}
				}
			}
		}
	}
	out := service.AssessQualityScopeEvidence(&accounts[0], scope, rules[selected.ID], at, read.limited)
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

const (
	qualityScopeMaxRules   = 512
	qualityScopeMaxHistory = 32768
	qualityScopeMaxSamples = 4096
)

type qualityScopeRuleRead struct {
	rule   service.QualityEvidenceRule
	round  qualityMeasurementRoundRead
	fault  int64
	claim  time.Time
	routes map[int64]*qualityRouteRead
}

// Preview and admission share this read-only evidence projection.
// Rule discovery, frozen-round classification and route/fault reads must finish
// before the caller supplies the database clock used for every verdict.
type qualityScopeRead struct {
	rules   map[int64]*qualityScopeRuleRead
	ordered []int64
	limited bool
}

func readQualityScopesInTx(ctx context.Context, tx *sql.Tx, accountIDs []int64) (*qualityScopeRead, error) {
	out := &qualityScopeRead{rules: make(map[int64]*qualityScopeRuleRead)}
	rows, err := tx.QueryContext(ctx, `SELECT id,account_id,model_id,enabled,updated_at,pelican_config,
 running_until,last_run_at,availability_fault_version FROM scheduled_test_plans
 WHERE account_id=ANY($1::bigint[]) AND pelican_config ? 'quality' AND pelican_config->'quality'<>'null'::jsonb
 ORDER BY id LIMIT $2`, pq.Array(accountIDs), qualityScopeMaxRules+1)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		plan := &service.ScheduledTestPlan{}
		read := &qualityScopeRuleRead{rule: service.QualityEvidenceRule{Plan: plan}, routes: make(map[int64]*qualityRouteRead)}
		var raw []byte
		if err = rows.Scan(&plan.ID, &plan.AccountID, &plan.ModelID, &plan.Enabled, &plan.UpdatedAt, &raw, &read.round.runningUntil, &read.round.lastRunAt, &read.fault); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &plan.PelicanConfig); err != nil {
			break
		}
		// JSON null is the native non-quality representation, not a paused test.
		if plan.PelicanConfig == nil || plan.PelicanConfig.Quality == nil {
			continue
		}
		var envelope struct {
			Marker json.RawMessage `json:"_quality_latest_round"`
		}
		if err = json.Unmarshal(raw, &envelope); err != nil {
			break
		}
		read.round.marker = envelope.Marker
		var marker struct {
			Version int       `json:"version"`
			Lease   time.Time `json:"lease_until"`
		}
		if json.Unmarshal(envelope.Marker, &marker) == nil && marker.Version == 1 {
			read.claim = marker.Lease
		}
		read.round.finalized = true
		out.rules[plan.ID] = read
		out.ordered = append(out.ordered, plan.ID)
	}
	rowErr := rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	if len(out.ordered) > qualityScopeMaxRules {
		out.limited = true
		return out, nil
	}
	if len(out.ordered) == 0 {
		return out, nil
	}
	// Parse round IDs with Go's RFC3339 parser. SQL string equality would lose
	// legitimate +08:00/UTC representations of the same claim; insertion order
	// and late completion times cannot select the current quality round.
	rows, err = tx.QueryContext(ctx, `SELECT id,plan_id,quality_round_id FROM scheduled_test_results
 WHERE plan_id=ANY($1::bigint[]) ORDER BY id DESC LIMIT $2`, pq.Array(out.ordered), qualityScopeMaxHistory+1)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0)
	selected := make(map[int64]int)
	seen := 0
	for rows.Next() {
		var id, planID int64
		var round string
		if err = rows.Scan(&id, &planID, &round); err != nil {
			break
		}
		seen++
		read := out.rules[planID]
		claim, parseErr := time.Parse(time.RFC3339Nano, round)
		if read != nil && !read.claim.IsZero() && parseErr == nil && claim.Equal(read.claim) {
			ids = append(ids, id)
			selected[planID]++
		}
	}
	rowErr = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	if seen > qualityScopeMaxHistory || len(ids) > qualityScopeMaxSamples {
		out.limited = true
		return out, nil
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err = tx.QueryContext(ctx, `WITH scoped AS (
 SELECT p.account_id,r.id,r.plan_id,r.status,r.created_at,r.pelican_config,r.quality_round_id,r.quality_action,r.quality_judgment,r.error_message,`+qualityMeasuredModel+` AS measured_model
 FROM scheduled_test_results r JOIN scheduled_test_plans p ON p.id=r.plan_id
 WHERE r.id=ANY($1::bigint[]) AND jsonb_typeof(r.pelican_config->'quality')='object'
 `+qualityRoundClassification+`, detail AS (
 SELECT r.id,r.plan_id,r.status,r.error_message,r.latency_ms,r.started_at,r.finished_at,r.created_at,
 r.pelican_config,r.quality_action,r.quality_judgment,r.quality_round_id,
 q.round_complete,q.round_probes,q.round_expected
 FROM scheduled_test_results r JOIN quality_classified q ON q.plan_id=r.plan_id AND q.round_key=r.quality_round_id
 AND q.measured_model IS NOT DISTINCT FROM `+qualityMeasuredModel+`
 WHERE r.id=ANY($1::bigint[])
 ) SELECT id,plan_id,status,error_message,latency_ms,started_at,finished_at,created_at,`+qualityHistoryConfig+`,
 quality_action,quality_judgment,quality_round_id,pelican_config->'_quality_observed_route',round_complete,round_probes,round_expected
 FROM detail ORDER BY id`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	type routeKey struct {
		Account                           int64
		Fingerprint, Connection, Protocol string
		Revision                          int64
	}
	cache := make(map[routeKey]*qualityRouteRead)
	// Close rows before route reads: both use this same single SQL connection.
	type pendingRoute struct {
		planID int64
		sample *service.ScheduledTestResult
		raw    []byte
	}
	var pending []pendingRoute
	for rows.Next() {
		sample := &service.ScheduledTestResult{}
		var config, judgment, route []byte
		var complete bool
		var probes int64
		var expected sql.NullInt64
		if err = rows.Scan(&sample.ID, &sample.PlanID, &sample.Status, &sample.ErrorMessage, &sample.LatencyMs, &sample.StartedAt, &sample.FinishedAt, &sample.CreatedAt, &config, &sample.QualityAction, &judgment, &sample.QualityRoundID, &route, &complete, &probes, &expected); err != nil {
			break
		}
		if err = json.Unmarshal(config, &sample.PelicanConfig); err != nil {
			break
		}
		if len(judgment) > 0 {
			if err = json.Unmarshal(judgment, &sample.QualityJudgment); err != nil {
				break
			}
		}
		read := out.rules[sample.PlanID]
		read.rule.Samples = append(read.rule.Samples, sample)
		if len(read.rule.Samples) == 1 {
			read.round.complete, read.round.probes, read.round.expected = complete, probes, expected
		} else {
			read.round.complete = read.round.complete && complete && read.round.probes == probes && read.round.expected == expected
		}
		read.round.finalized = read.round.finalized && sample.QualityAction != "" && sample.QualityAction != "comparison_pending"
		if !read.round.samplesFinishedAt.Valid || sample.FinishedAt.After(read.round.samplesFinishedAt.Time) {
			read.round.samplesFinishedAt = sql.NullTime{Time: sample.FinishedAt, Valid: true}
		}
		pending = append(pending, pendingRoute{planID: sample.PlanID, sample: sample, raw: route})
	}
	rowErr = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	for _, id := range out.ordered {
		read := out.rules[id]
		// Missing/non-quality records and alternate textual round IDs cannot
		// turn a partially decoded latest claim into a complete passing round.
		read.round.complete = read.round.complete && len(read.rule.Samples) == selected[id] && int64(selected[id]) == read.round.probes
	}
	for _, item := range pending {
		read := out.rules[item.planID]
		var evidence service.QualityRouteEvidence
		valid := json.Unmarshal(item.raw, &evidence) == nil && evidence.AccountID == read.rule.Plan.AccountID && evidence.MatchesResult(item.sample)
		var route *qualityRouteRead
		if valid {
			item.sample.QualityRouteEvidence = &evidence
			key := routeKey{Account: evidence.AccountID, Fingerprint: evidence.Fingerprint, Connection: evidence.ConnectionID, Revision: evidence.ConnectionRevision, Protocol: item.sample.PelicanConfig.QualityMeasurement.Scope.Protocol}
			if cached := cache[key]; cached != nil {
				clone := *cached
				clone.observedAt = evidence.ObservedAt
				clone.faultVersion = evidence.FaultVersion
				clone.faultRound = evidence.FaultRound
				route = &clone
			}
			if route == nil {
				route, err = readQualityRoute(ctx, tx, read.rule.Plan.AccountID, item.sample, item.raw)
				if err != nil {
					return nil, err
				}
				cache[key] = route
			}
		} else {
			route, err = readQualityRoute(ctx, tx, read.rule.Plan.AccountID, item.sample, item.raw)
			if err != nil {
				return nil, err
			}
		}
		read.routes[item.sample.ID] = route
	}
	return out, nil
}

func (read *qualityScopeRead) assess(at time.Time) map[int64][]service.QualityEvidenceRule {
	out := make(map[int64][]service.QualityEvidenceRule)
	for _, id := range read.ordered {
		r := read.rules[id]
		plan := r.rule.Plan
		for _, sample := range r.rule.Samples {
			sample.QualityMeasurementValidity = service.AssessQualityMeasurementValidity(sample, plan.UpdatedAt, plan.Enabled, at)
			if v := sample.QualityMeasurementValidity; v != nil {
				v.Round = r.round.assess(sample, plan.UpdatedAt, plan.Enabled, at)
				route := r.routes[sample.ID]
				v.Route = route.assess(at)
				v.Fault = route.assessFault(sample, plan.UpdatedAt, plan.Enabled, r.fault, at)
			}
		}
		out[plan.AccountID] = append(out[plan.AccountID], r.rule)
	}
	return out
}
