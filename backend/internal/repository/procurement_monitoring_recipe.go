package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func loadProcurementMonitoringRecipe(ctx context.Context, tx *sql.Tx, presetID int64, sources *service.ProcurementSourceInventory) (*service.ProcurementMonitoringRecipe, error) {
	preset, err := scanQualityRulePreset(tx.QueryRowContext(ctx, `SELECT `+qualityRulePresetColumns+` FROM quality_rule_presets WHERE id=$1`, presetID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrProcurementMonitoringRecipeNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := service.ProcurementMonitoringRecipePlan(preset, time.Now()); err != nil {
		return nil, err
	}
	if err := validateQualityGroupReferences(ctx, tx, preset.PelicanConfig); err != nil {
		return nil, service.ErrProcurementMonitoringRecipeInvalid
	}
	out := &service.ProcurementMonitoringRecipe{Version: 1, PresetID: preset.ID, PresetName: preset.Name, PresetUpdatedAt: preset.UpdatedAt, State: "sources_unavailable"}
	if sources == nil {
		return out, nil
	}
	out.SourcePolicyRevision = sources.PolicyRevision
	if sources.State != "approved_sources" {
		return out, nil
	}
	ids := make([]int64, 0)
	for _, account := range sources.Accounts {
		if account.InApprovedSources != nil && *account.InApprovedSources {
			ids = append(ids, account.AccountID)
		}
	}
	n := len(ids)
	out.Accounts = &n
	if n == 0 {
		out.State = "no_approved_accounts"
		return out, nil
	}
	settings, err := loadMonitoringProbeBudgetSettings(ctx, tx, "")
	if err != nil {
		return nil, err
	}
	inventory, err := loadMonitoringProbeCoverageInventory(ctx, tx, nil)
	if err != nil {
		return nil, err
	}
	plan, err := service.ProcurementMonitoringRecipePlan(preset, inventory.observedAt)
	if err != nil {
		return nil, err
	}
	current := inventory.compile(settings, inventory.plans, inventory.plansComplete)
	plans := append([]service.MonitoringProbeCoveragePlan(nil), inventory.plans...)
	for _, id := range ids {
		copy := *plan
		copy.AccountID = id
		plans = append(plans, service.MonitoringProbeCoveragePlan{Plan: copy})
	}
	proposed := inventory.compile(settings, plans, inventory.plansComplete && len(plans) <= monitoringCoverageInventoryLimit)
	out.State = "previewed"
	out.Demand = &service.ProcurementMonitoringRecipeDemand{AdditionalPlans: n, CurrentMinimumRequestsPerDay: current.MinimumTotalRequestsPerDay,
		DeltaMinimumRequestsPerDay: proposed.MinimumTotalRequestsPerDay - current.MinimumTotalRequestsPerDay, Coverage: proposed}
	return out, nil
}
