package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/tidwall/gjson"
)

const procurementSharedMonitoringSettingKey = "upstream_procurement_shared_observation"

type procurementObservationOwner struct {
	AccountID  int64  `json:"account_id"`
	PlanID     int64  `json:"plan_id"`
	Definition string `json:"definition"`
}

// One durable authority; neither native plan content nor old creation history
// is evidence that an administrator opted a product into shared management.
type procurementObservationState struct {
	Version     int                                             `json:"version"`
	Revision    int64                                           `json:"revision"`
	Products    []service.ProcurementMonitoringProductSelection `json:"products"`
	Plans       []procurementObservationOwner                   `json:"plans"`
	Maintenance *service.ProcurementSharedMonitoringMaintenance `json:"maintenance,omitempty"`
}

func loadProcurementObservationState(ctx context.Context, q sqlQueryer) (*procurementObservationState, error) {
	out := &procurementObservationState{Version: 1, Products: []service.ProcurementMonitoringProductSelection{}, Plans: []procurementObservationOwner{}}
	var raw string
	err := scanSingleRow(ctx, q, `SELECT value FROM settings WHERE key=$1`, []any{procurementSharedMonitoringSettingKey}, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	root := gjson.Parse(raw)
	if len(raw) > 1<<20 || !gjson.Valid(raw) || !(procurementObservationJSONKeys(root, "version", "revision", "products", "plans") || procurementObservationJSONKeys(root, "version", "revision", "products", "plans", "maintenance")) || root.Get("revision").Type != gjson.Number || !root.Get("products").IsArray() || !root.Get("plans").IsArray() {
		return nil, service.ErrProcurementSharedMonitoringUnavailable
	}
	if m := root.Get("maintenance"); m.Exists() {
		if !procurementObservationJSONKeys(m, "enabled", "state", "reason", "changed_at", "created", "updated", "paused") ||
			m.Get("enabled").Type != gjson.True && m.Get("enabled").Type != gjson.False ||
			m.Get("created").Type != gjson.Number || m.Get("updated").Type != gjson.Number || m.Get("paused").Type != gjson.Number {
			return nil, service.ErrProcurementSharedMonitoringUnavailable
		}
	}
	for _, p := range root.Get("products").Array() {
		if !procurementObservationJSONKeys(p, "product_id", "preset_id") {
			return nil, service.ErrProcurementSharedMonitoringUnavailable
		}
	}
	for _, p := range root.Get("plans").Array() {
		if !procurementObservationJSONKeys(p, "account_id", "plan_id", "definition") {
			return nil, service.ErrProcurementSharedMonitoringUnavailable
		}
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || out.Version != 1 || out.Revision < 0 || out.Revision > domain.MaxProcurementPolicyRevision || out.Plans == nil || len(out.Plans) > service.ProcurementProductImpactAccountLimit || (&service.ProcurementSharedMonitoringCommand{Version: 2, Products: out.Products}).Validate() != nil {
		return nil, service.ErrProcurementSharedMonitoringUnavailable
	}
	if root.Get("maintenance").Exists() && (out.Maintenance == nil || !validProcurementMaintenance(out.Maintenance)) {
		return nil, service.ErrProcurementSharedMonitoringUnavailable
	}
	if out.Revision == 0 && (len(out.Products) > 0 || len(out.Plans) > 0 || out.Maintenance != nil) {
		return nil, service.ErrProcurementSharedMonitoringUnavailable
	}
	accounts, plans := map[int64]bool{}, map[int64]bool{}
	for _, p := range out.Plans {
		if p.AccountID < 1 || p.AccountID > domain.MaxProcurementPolicyRevision || p.PlanID < 1 || p.PlanID > domain.MaxProcurementPolicyRevision || len(p.Definition) != 64 || strings.Trim(p.Definition, "0123456789abcdef") != "" || accounts[p.AccountID] || plans[p.PlanID] {
			return nil, service.ErrProcurementSharedMonitoringUnavailable
		}
		accounts[p.AccountID], plans[p.PlanID] = true, true
	}
	return out, nil
}

func procurementObservationJSONKeys(value gjson.Result, names ...string) bool {
	if !value.IsObject() {
		return false
	}
	seen, valid := map[string]bool{}, true
	value.ForEach(func(key, _ gjson.Result) bool {
		if seen[key.Str] {
			valid = false
		}
		seen[key.Str] = true
		return true
	})
	if !valid || len(seen) != len(names) {
		return false
	}
	for _, name := range names {
		if !seen[name] {
			return false
		}
	}
	return true
}

type procurementObservationNativePlan struct {
	Plan       service.ScheduledTestPlan
	Definition string
}

func procurementObservationDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Hash the actual stored business definition, including unknown fields. Only
// the three native executor markers are excluded; timestamps/leases are not
// business edits. Raw JSON is decoded with numbers preserved before sorting.
func procurementObservationDefinition(plan *service.ScheduledTestPlan, raw []byte) (string, error) {
	var config map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&config); err != nil {
		return "", err
	}
	for _, key := range []string{"_manual_quality_run", "_quality_latest_round", "_quality_observed_route"} {
		delete(config, key)
	}
	return procurementObservationDigest([]any{plan.AccountID, plan.ModelID, plan.CronExpression, plan.Enabled, plan.MaxResults, plan.AutoRecover, config})
}

func loadProcurementObservationPlans(ctx context.Context, q sqlQueryer, owners []procurementObservationOwner) (map[int64]procurementObservationNativePlan, error) {
	ids := make([]int64, 0, len(owners))
	for _, p := range owners {
		ids = append(ids, p.PlanID)
	}
	rows, err := q.QueryContext(ctx, `SELECT to_jsonb(p),p.pelican_config FROM scheduled_test_plans p WHERE id=ANY($1) ORDER BY id`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]procurementObservationNativePlan{}
	for rows.Next() {
		var raw, config []byte
		var p procurementObservationNativePlan
		if err := rows.Scan(&raw, &config); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &p.Plan); err != nil {
			return nil, err
		}
		p.Definition, err = procurementObservationDefinition(&p.Plan, config)
		if err != nil {
			return nil, err
		}
		out[p.Plan.ID] = p
	}
	return out, rows.Err()
}

func procurementObservationOwnerReason(owner procurementObservationOwner, plans map[int64]procurementObservationNativePlan, now time.Time) string {
	p, exists := plans[owner.PlanID]
	if !exists {
		return "managed_plan_missing"
	}
	if p.Plan.AccountID != owner.AccountID || p.Definition != owner.Definition {
		return "managed_plan_changed"
	}
	if p.Plan.RunningUntil != nil && p.Plan.RunningUntil.After(now) {
		return "managed_plan_running"
	}
	return "compatible"
}

func readProcurementSharedMonitoring(ctx context.Context, q sqlQueryer) (*service.ProcurementSharedMonitoringState, error) {
	saved, err := loadProcurementObservationState(ctx, q)
	if err != nil {
		return nil, err
	}
	plans, err := loadProcurementObservationPlans(ctx, q, saved.Plans)
	if err != nil {
		return nil, err
	}
	out := &service.ProcurementSharedMonitoringState{Version: 1, Revision: saved.Revision, Products: saved.Products, Plans: []service.ProcurementSharedMonitoringSavedPlan{}, Maintenance: saved.Maintenance}
	if err := scanSingleRow(ctx, q, `SELECT clock_timestamp()`, nil, &out.ObservedAt); err != nil {
		return nil, err
	}
	for _, owner := range saved.Plans {
		reason := procurementObservationOwnerReason(owner, plans, out.ObservedAt)
		state := reason
		if reason == "compatible" {
			state = "managed"
			if !plans[owner.PlanID].Plan.Enabled {
				state = "paused"
			}
		}
		out.Plans = append(out.Plans, service.ProcurementSharedMonitoringSavedPlan{AccountID: owner.AccountID, PlanID: owner.PlanID, State: state, Enabled: plans[owner.PlanID].Plan.Enabled})
	}
	slices.SortFunc(out.Plans, func(a, b service.ProcurementSharedMonitoringSavedPlan) int {
		if a.AccountID < b.AccountID {
			return -1
		}
		if a.AccountID > b.AccountID {
			return 1
		}
		return 0
	})
	return out, nil
}

func (r *channelRepository) ReadProcurementSharedMonitoring(ctx context.Context) (*service.ProcurementSharedMonitoringState, error) {
	if err := procurementManagementAccess(ctx, 1); err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	out, err := readProcurementSharedMonitoring(ctx, tx)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// Called under the original plan row lock. A corrupt authority never grants
// manual write permission, and a pre-management writer cannot overwrite a
// plan that was claimed while it waited for that lock.
func checkProcurementObservationManualWrite(ctx context.Context, q sqlQueryer, planID int64) error {
	saved, err := loadProcurementObservationState(ctx, q)
	if err != nil {
		return err
	}
	for _, p := range saved.Plans {
		if p.PlanID == planID {
			return service.ErrProcurementSharedMonitoringOwned
		}
	}
	return nil
}
