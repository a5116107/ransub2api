package service

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/robfig/cron/v3"
	"github.com/tidwall/gjson"
)

const ProcurementSharedMonitoringProductLimit = 20

var ErrProcurementSharedMonitoringCommand = infraerrors.BadRequest("PROCUREMENT_SHARED_MONITORING_COMMAND_INVALID", "select distinct products and observation presets within the shared-plan limit")
var ErrProcurementSharedMonitoringUnavailable = infraerrors.ServiceUnavailable("PROCUREMENT_SHARED_MONITORING_UNAVAILABLE", "shared monitoring preview is unavailable")
var ErrProcurementSharedMonitoringConflict = infraerrors.Conflict("PROCUREMENT_SHARED_MONITORING_CHANGED", "shared observation inputs changed; read saved participation and preview again")
var ErrProcurementSharedMonitoringOwned = infraerrors.Conflict("PROCUREMENT_SHARED_MONITORING_OWNED", "this observation plan is managed by shared products; pause and release shared management before editing it manually")

type ProcurementMonitoringProductSelection struct {
	ProductID int64 `json:"product_id"`
	PresetID  int64 `json:"preset_id"`
}

type ProcurementSharedMonitoringCommand struct {
	Version          int                                     `json:"version"`
	Products         []ProcurementMonitoringProductSelection `json:"products"`
	ReleaseOwnership bool                                    `json:"release_ownership,omitempty"`
}

func (c *ProcurementSharedMonitoringCommand) Validate() error {
	if c == nil || (c.Version != 1 && c.Version != 2) || c.Products == nil || c.Version == 1 && (len(c.Products) == 0 || c.ReleaseOwnership) || len(c.Products) > ProcurementSharedMonitoringProductLimit || c.ReleaseOwnership && len(c.Products) != 0 {
		return ErrProcurementSharedMonitoringCommand
	}
	seen := map[int64]bool{}
	for _, p := range c.Products {
		if p.ProductID < 1 || p.ProductID > domain.MaxProcurementPolicyRevision || p.PresetID < 1 || p.PresetID > domain.MaxProcurementPolicyRevision || seen[p.ProductID] {
			return ErrProcurementSharedMonitoringCommand
		}
		seen[p.ProductID] = true
	}
	return nil
}

func ParseProcurementSharedMonitoringCommand(raw []byte) (*ProcurementSharedMonitoringCommand, error) {
	root := gjson.ParseBytes(raw)
	var c ProcurementSharedMonitoringCommand
	validObject := procurementCatalogObject(root, "version", "products") || root.Get("version").Int() == 2 && procurementCatalogObject(root, "version", "products", "release_ownership")
	release := root.Get("release_ownership")
	if !gjson.ValidBytes(raw) || !validObject || !root.Get("products").IsArray() || release.Exists() && release.Type != gjson.True && release.Type != gjson.False {
		return nil, ErrProcurementSharedMonitoringCommand
	}
	for _, p := range root.Get("products").Array() {
		if !procurementCatalogObject(p, "product_id", "preset_id") {
			return nil, ErrProcurementSharedMonitoringCommand
		}
	}
	if json.Unmarshal(raw, &c) != nil || c.Validate() != nil {
		return nil, ErrProcurementSharedMonitoringCommand
	}
	return &c, nil
}

type ProcurementSharedMonitoringProduct struct {
	ProductID      int64  `json:"product_id"`
	Name           string `json:"name"`
	PresetID       int64  `json:"preset_id"`
	SourceRevision int64  `json:"source_revision"`
	State          string `json:"state"`
	Accounts       *int   `json:"accounts"`
}

type ProcurementSharedMonitoringAccount struct {
	AccountID       int64    `json:"account_id"`
	Name            string   `json:"name"`
	ProductIDs      []int64  `json:"product_ids"`
	Models          []string `json:"models"`
	CronExpression  string   `json:"cron_expression"`
	State           string   `json:"state"`
	Reason          string   `json:"reason"`
	ExistingPlanID  *int64   `json:"existing_plan_id,omitempty"`
	ExistingEnabled *bool    `json:"existing_enabled,omitempty"`
}

// A read-only combined draft. It neither records product participation nor
// grants ownership of a native plan, including an apparently compatible one.
type ProcurementSharedMonitoringPreview struct {
	Version                      int                                  `json:"version"`
	Scope                        string                               `json:"scope"`
	ObservedAt                   time.Time                            `json:"observed_at"`
	State                        string                               `json:"state"`
	Products                     []ProcurementSharedMonitoringProduct `json:"products"`
	Accounts                     []ProcurementSharedMonitoringAccount `json:"accounts"`
	Requirements                 int                                  `json:"requirements"`
	SharedAccounts               int                                  `json:"shared_accounts"`
	CurrentMinimumRequestsPerDay *int64                               `json:"current_minimum_requests_per_day"`
	DeltaMinimumRequestsPerDay   *int64                               `json:"delta_minimum_requests_per_day"`
	Coverage                     *MonitoringProbeCoverage             `json:"coverage,omitempty"`
	ApplicationReview            *ProcurementSharedMonitoringReview   `json:"application_review,omitempty"`
}

type ProcurementSharedMonitoringChange struct {
	AccountID int64  `json:"account_id"`
	PlanID    int64  `json:"plan_id,omitempty"`
	Operation string `json:"operation"`
	Reason    string `json:"reason"`
}

type ProcurementSharedMonitoringReview struct {
	Revision         int64                               `json:"revision"`
	ObservedAt       time.Time                           `json:"observed_at"`
	ExpiresAt        time.Time                           `json:"expires_at"`
	Fingerprint      string                              `json:"fingerprint"`
	CanConfirm       bool                                `json:"can_confirm"`
	ReleaseOwnership bool                                `json:"release_ownership"`
	Changes          []ProcurementSharedMonitoringChange `json:"changes"`
}

type ProcurementSharedMonitoringSavedPlan struct {
	AccountID int64  `json:"account_id"`
	PlanID    int64  `json:"plan_id"`
	State     string `json:"state"`
	Enabled   bool   `json:"enabled"`
}

type ProcurementSharedMonitoringState struct {
	Version     int                                     `json:"version"`
	Revision    int64                                   `json:"revision"`
	ObservedAt  time.Time                               `json:"observed_at"`
	Products    []ProcurementMonitoringProductSelection `json:"products"`
	Plans       []ProcurementSharedMonitoringSavedPlan  `json:"plans"`
	Maintenance *ProcurementSharedMonitoringMaintenance `json:"maintenance,omitempty"`
}

// ChangedAt is the time of the last distinct outcome, not a heartbeat or
// proof of provider health. An unchanged reconciliation performs no writes.
type ProcurementSharedMonitoringMaintenance struct {
	Enabled   bool      `json:"enabled"`
	State     string    `json:"state"`
	Reason    string    `json:"reason"`
	ChangedAt time.Time `json:"changed_at"`
	Created   int       `json:"created"`
	Updated   int       `json:"updated"`
	Paused    int       `json:"paused"`
}

type ProcurementSharedMonitoringMaintenanceCommand struct {
	Version          int   `json:"version"`
	ExpectedRevision int64 `json:"expected_revision"`
	Enabled          bool  `json:"enabled"`
	Confirm          bool  `json:"confirm"`
}

func ParseProcurementSharedMonitoringMaintenanceCommand(raw []byte) (*ProcurementSharedMonitoringMaintenanceCommand, error) {
	root := gjson.ParseBytes(raw)
	var out ProcurementSharedMonitoringMaintenanceCommand
	if !gjson.ValidBytes(raw) || !procurementCatalogObject(root, "version", "expected_revision", "enabled", "confirm") ||
		root.Get("enabled").Type != gjson.True && root.Get("enabled").Type != gjson.False ||
		root.Get("expected_revision").Type != gjson.Number || json.Unmarshal(raw, &out) != nil ||
		out.Version != 1 || !out.Confirm || out.ExpectedRevision < 0 || out.ExpectedRevision >= domain.MaxProcurementPolicyRevision {
		return nil, ErrProcurementSharedMonitoringCommand
	}
	return &out, nil
}

type ProcurementSharedMonitoringMaintainer interface {
	ReconcileProcurementSharedMonitoring(context.Context) error
}

type ProcurementSharedMonitoringApplyCommand struct {
	ProcurementSharedMonitoringCommand
	Revision    int64     `json:"revision"`
	ObservedAt  time.Time `json:"observed_at"`
	Fingerprint string    `json:"fingerprint"`
	Confirm     bool      `json:"confirm"`
}

func (c ProcurementSharedMonitoringApplyCommand) MarshalJSON() ([]byte, error) {
	type command ProcurementSharedMonitoringApplyCommand
	return json.Marshal(struct {
		command
		ReleaseOwnership bool `json:"release_ownership"`
	}{command: command(c), ReleaseOwnership: c.ReleaseOwnership})
}

func (c *ProcurementSharedMonitoringApplyCommand) Validate() error {
	if c == nil || c.Version != 2 || c.ProcurementSharedMonitoringCommand.Validate() != nil || !c.Confirm || c.Revision < 0 || c.Revision >= domain.MaxProcurementPolicyRevision || c.ObservedAt.IsZero() || len(c.Fingerprint) != 64 || strings.Trim(c.Fingerprint, "0123456789abcdef") != "" {
		return ErrProcurementSharedMonitoringCommand
	}
	return nil
}

func ParseProcurementSharedMonitoringApplyCommand(raw []byte) (*ProcurementSharedMonitoringApplyCommand, error) {
	root := gjson.ParseBytes(raw)
	var c ProcurementSharedMonitoringApplyCommand
	if !gjson.ValidBytes(raw) || !procurementCatalogObject(root, "version", "products", "release_ownership", "revision", "observed_at", "fingerprint", "confirm") || !root.Get("products").IsArray() || root.Get("release_ownership").Type != gjson.True && root.Get("release_ownership").Type != gjson.False || root.Get("revision").Type != gjson.Number {
		return nil, ErrProcurementSharedMonitoringCommand
	}
	for _, p := range root.Get("products").Array() {
		if !procurementCatalogObject(p, "product_id", "preset_id") {
			return nil, ErrProcurementSharedMonitoringCommand
		}
	}
	if json.Unmarshal(raw, &c) != nil || c.Validate() != nil {
		return nil, ErrProcurementSharedMonitoringCommand
	}
	return &c, nil
}

type ProcurementSharedMonitoringRepository interface {
	PreviewProcurementSharedMonitoring(context.Context, *ProcurementSharedMonitoringCommand) (*ProcurementSharedMonitoringPreview, error)
	ReadProcurementSharedMonitoring(context.Context) (*ProcurementSharedMonitoringState, error)
	ApplyProcurementSharedMonitoring(context.Context, *ProcurementSharedMonitoringApplyCommand) (*ProcurementSharedMonitoringState, error)
	ConfigureProcurementSharedMonitoringMaintenance(context.Context, *ProcurementSharedMonitoringMaintenanceCommand) (*ProcurementSharedMonitoringState, error)
}

func (s *ChannelService) ProcurementSharedMonitoringRepository() (ProcurementSharedMonitoringRepository, error) {
	if s != nil {
		if r, ok := s.repo.(ProcurementSharedMonitoringRepository); ok {
			return r, nil
		}
	}
	return nil, ErrProcurementSharedMonitoringUnavailable
}

// Inputs are normalized through the original native validator. Only the
// requested model set, cron and history capacity can be combined; other test
// semantics must agree. No caller-supplied configuration is modified.
func MergeProcurementMonitoringRecipes(presets []*QualityRulePreset, at time.Time) (*ScheduledTestPlan, string) {
	if len(presets) == 0 || len(presets) > ProcurementSharedMonitoringProductLimit {
		return nil, "invalid_requirements"
	}
	var merged *ScheduledTestPlan
	var config []byte
	models, expressions := []string{}, []string{}
	for _, preset := range presets {
		plan, err := ProcurementMonitoringRecipePlan(preset, at)
		if err != nil || plan.NextRunAt == nil || plan.NextRunAt.IsZero() {
			return nil, "invalid_preset"
		}
		models = append(models, qualityPlanModels(plan)...)
		expressions = append(expressions, plan.CronExpression)
		comparison := *plan.PelicanConfig
		comparison.ModelIDs, comparison.ModelID, comparison.RuleName = nil, "", ""
		raw, err := json.Marshal(comparison)
		if err != nil {
			return nil, "invalid_preset"
		}
		if merged == nil {
			merged, config = plan, raw
		} else {
			if !bytes.Equal(config, raw) {
				return nil, "observation_settings_conflict"
			}
			merged.MaxResults = max(merged.MaxResults, plan.MaxResults)
		}
	}
	slices.Sort(models)
	models = slices.Compact(models)
	if len(models) > 50 || len(models)*merged.PelicanConfig.ParallelCount > 100 {
		return nil, "native_model_limit"
	}
	expression, ok := mergeProcurementMonitoringCron(expressions)
	if !ok {
		return nil, "schedule_timezones_conflict"
	}
	merged.CronExpression = expression
	merged.ModelID, merged.PelicanConfig.ModelIDs = models[0], models
	merged.MaxResults = max(merged.MaxResults, len(models)*merged.PelicanConfig.ParallelCount)
	next, err := nextPlanRun(merged, at)
	if err != nil || next.IsZero() {
		return nil, "invalid_merged_plan"
	}
	merged.NextRunAt = &next
	return merged, "compatible"
}

// The result is one ordinary native cron whose tick set contains every input.
// Cartesian unions can add ticks; the original budget compiler prices the
// resulting expression, rather than using the fastest input's daily count.
func mergeProcurementMonitoringCron(expressions []string) (string, bool) {
	if len(expressions) == 1 {
		return expressions[0], true
	}
	var minute, hour, month, dom, dow uint64
	var location string
	const star = uint64(1) << 63
	const fullDom = (uint64(1)<<32 - 1) &^ 1
	const fullDow = uint64(1)<<7 - 1
	allDays := false
	for _, expression := range expressions {
		schedule, err := scheduledTestCronParser.Parse(expression)
		if err != nil {
			return "", false
		}
		spec, ok := schedule.(*cron.SpecSchedule)
		if !ok || location != "" && location != spec.Location.String() {
			return "", false
		}
		location = spec.Location.String()
		minute |= spec.Minute &^ star
		hour |= spec.Hour &^ star
		month |= spec.Month &^ star
		d, w := spec.Dom&^star, spec.Dow&^star
		switch {
		case d == fullDom && w == fullDow:
			allDays = true
		case spec.Dom&star != 0 || spec.Dow&star != 0:
			// Native cron uses AND when either day field has the star flag.
			// Preserve the restricted side; a stepped star on both sides
			// needs a conservative OR envelope rather than dropping ticks.
			switch {
			case d == fullDom:
				dow |= w
			case w == fullDow:
				dom |= d
			default:
				dom, dow = dom|d, dow|w
			}
		default:
			dom, dow = dom|d, dow|w
		}
	}
	domField, dowField := "*", "*"
	if !allDays && dom != fullDom && dow != fullDow {
		if dom != 0 {
			domField = procurementCronField(dom, 1, 31, false)
		}
		if dow != 0 {
			dowField = procurementCronField(dow, 0, 6, false)
		}
	}
	prefix := ""
	if location != "Local" {
		prefix = "CRON_TZ=" + location + " "
	}
	return prefix + strings.Join([]string{procurementCronField(minute, 0, 59, true), procurementCronField(hour, 0, 23, true), domField, procurementCronField(month, 1, 12, true), dowField}, " "), true
}

func procurementCronField(mask uint64, lo, hi int, wildcard bool) string {
	values := []string{}
	for i := lo; i <= hi; i++ {
		if mask&(uint64(1)<<i) != 0 {
			values = append(values, strconv.Itoa(i))
		}
	}
	if wildcard && len(values) == hi-lo+1 {
		return "*"
	}
	return strings.Join(values, ",")
}
