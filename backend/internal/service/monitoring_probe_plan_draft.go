package service

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/tidwall/gjson"
)

var ErrMonitoringProbePlanDraft = infraerrors.BadRequest("INVALID_MONITORING_PROBE_PLAN_DRAFT", "invalid account monitoring plan draft")

// A read-only addition or replacement of one native account plan. It is never
// persisted, reserved, or treated as a product's complete detection recipe.
type MonitoringProbePlanDraft struct {
	PlanID         int64              `json:"plan_id"`
	AccountID      int64              `json:"account_id"`
	ModelID        string             `json:"model_id"`
	CronExpression string             `json:"cron_expression"`
	Enabled        bool               `json:"enabled"`
	AutoRecover    bool               `json:"auto_recover"`
	MaxResults     int                `json:"max_results"`
	PelicanConfig  *PelicanTestConfig `json:"pelican_config,omitempty"`
}

type MonitoringProbePlanChange struct {
	Version                       int   `json:"version"`
	PlanID                        int64 `json:"plan_id"`
	AccountID                     int64 `json:"account_id"`
	Enabled                       bool  `json:"enabled"`
	CurrentMinimumRequestsPerDay  int64 `json:"current_minimum_requests_per_day"`
	DeltaMinimumRequestsPerDay    int64 `json:"delta_minimum_requests_per_day"`
	ProposedMinimumRequestsPerDay int64 `json:"proposed_minimum_requests_per_day"`
}

// Validate new input strictly without changing the legacy settings-only JSON
// contract. In particular, explicit null and duplicate/unknown draft fields
// must not silently turn an incremental preview into an unchanged baseline.
func ParseMonitoringProbePlanDraft(raw []byte) (*MonitoringProbePlanDraft, error) {
	if !gjson.ValidBytes(raw) || !gjson.ParseBytes(raw).IsObject() {
		return nil, ErrMonitoringProbePlanDraft
	}
	var value gjson.Result
	count := 0
	gjson.ParseBytes(raw).ForEach(func(key, v gjson.Result) bool {
		if key.Str == "plan_draft" {
			value, count = v, count+1
		}
		return true
	})
	if count == 0 {
		return nil, nil
	}
	if count != 1 || !value.IsObject() {
		return nil, ErrMonitoringProbePlanDraft
	}
	fields := []string{"plan_id", "account_id", "model_id", "cron_expression", "enabled", "auto_recover", "max_results"}
	if value.Get("pelican_config").Exists() {
		fields = append(fields, "pelican_config")
		if !value.Get("pelican_config").IsObject() {
			return nil, ErrMonitoringProbePlanDraft
		}
	}
	seen := map[string]bool{}
	valid := true
	value.ForEach(func(key, v gjson.Result) bool {
		if seen[key.Str] {
			valid = false
		}
		seen[key.Str] = true
		return true
	})
	for _, field := range fields {
		valid = valid && seen[field]
	}
	if !valid || len(seen) != len(fields) || value.Get("plan_id").Type != gjson.Number || value.Get("account_id").Type != gjson.Number ||
		value.Get("max_results").Type != gjson.Number || value.Get("model_id").Type != gjson.String || value.Get("cron_expression").Type != gjson.String ||
		(value.Get("enabled").Type != gjson.True && value.Get("enabled").Type != gjson.False) ||
		(value.Get("auto_recover").Type != gjson.True && value.Get("auto_recover").Type != gjson.False) {
		return nil, ErrMonitoringProbePlanDraft
	}
	decoder := json.NewDecoder(bytes.NewBufferString(value.Raw))
	decoder.DisallowUnknownFields()
	var draft MonitoringProbePlanDraft
	if decoder.Decode(&draft) != nil {
		return nil, ErrMonitoringProbePlanDraft
	}
	if _, err := draft.Plan(time.Now()); err != nil {
		return nil, err
	}
	return &draft, nil
}

func (d *MonitoringProbePlanDraft) Plan(at time.Time) (*ScheduledTestPlan, error) {
	if d == nil || d.PlanID < 0 || d.PlanID > 9007199254740991 || d.AccountID < 1 || d.AccountID > 9007199254740991 ||
		strings.TrimSpace(d.ModelID) == "" || len(d.ModelID) > 100 || len(d.CronExpression) > 256 || d.MaxResults < 1 || d.MaxResults > 2147483647 {
		return nil, ErrMonitoringProbePlanDraft
	}
	p := &ScheduledTestPlan{ID: d.PlanID, AccountID: d.AccountID, ModelID: d.ModelID, CronExpression: d.CronExpression,
		Enabled: d.Enabled, AutoRecover: d.AutoRecover, MaxResults: d.MaxResults}
	if d.PelicanConfig != nil {
		// Native validation normalizes fields; never mutate the caller's draft.
		raw, err := json.Marshal(d.PelicanConfig)
		if err != nil || json.Unmarshal(raw, &p.PelicanConfig) != nil {
			return nil, ErrMonitoringProbePlanDraft
		}
		if p.PelicanConfig.Quality != nil && p.PelicanConfig.Quality.Action == QualityActionOpenAITwoFAAdmission {
			return nil, ErrMonitoringProbePlanDraft
		}
	}
	next, err := nextPlanRun(p, at)
	if err != nil || next.IsZero() {
		return nil, ErrMonitoringProbePlanDraft
	}
	p.NextRunAt = &next
	return p, nil
}
