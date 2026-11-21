package repository

import (
	"context"
	"database/sql"
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *channelRepository) PreviewProcurementSharedMonitoring(ctx context.Context, command *service.ProcurementSharedMonitoringCommand) (*service.ProcurementSharedMonitoringPreview, error) {
	if err := command.Validate(); err != nil {
		return nil, err
	}
	for _, p := range command.Products {
		if err := procurementManagementAccess(ctx, p.ProductID); err != nil {
			return nil, err
		}
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	out := &service.ProcurementSharedMonitoringPreview{Version: 1, Scope: "approved_sources_only", State: "compatible",
		Products: []service.ProcurementSharedMonitoringProduct{}, Accounts: []service.ProcurementSharedMonitoringAccount{}}
	products := append([]service.ProcurementMonitoringProductSelection(nil), command.Products...)
	slices.SortFunc(products, func(a, b service.ProcurementMonitoringProductSelection) int {
		if a.ProductID < b.ProductID {
			return -1
		}
		if a.ProductID > b.ProductID {
			return 1
		}
		return 0
	})
	requirements := map[int64][]*service.QualityRulePreset{}
	accounts := map[int64]*service.ProcurementSharedMonitoringAccount{}
	for _, selection := range products {
		group, _, review, err := loadProcurementProductReview(ctx, tx, selection.ProductID)
		if err != nil {
			return nil, err
		}
		preset, err := scanQualityRulePreset(tx.QueryRowContext(ctx, `SELECT `+qualityRulePresetColumns+` FROM quality_rule_presets WHERE id=$1`, selection.PresetID))
		if err == sql.ErrNoRows {
			return nil, service.ErrProcurementMonitoringRecipeNotFound
		}
		if err != nil {
			return nil, err
		}
		if err := validateQualityGroupReferences(ctx, tx, preset.PelicanConfig); err != nil {
			return nil, service.ErrProcurementMonitoringRecipeInvalid
		}
		entry := service.ProcurementSharedMonitoringProduct{ProductID: group.ID, Name: group.Name, PresetID: preset.ID, State: "sources_unavailable"}
		if sources := review.SourceInventory; sources != nil {
			entry.SourceRevision = sources.PolicyRevision
			if sources.State == "approved_sources" {
				n := 0
				for _, a := range sources.Accounts {
					if a.InApprovedSources == nil || !*a.InApprovedSources {
						continue
					}
					n++
					row := accounts[a.AccountID]
					if row == nil {
						row = &service.ProcurementSharedMonitoringAccount{AccountID: a.AccountID, Name: a.Name, ProductIDs: []int64{}, Models: []string{}}
						accounts[a.AccountID] = row
					}
					row.ProductIDs = append(row.ProductIDs, group.ID)
					requirements[a.AccountID] = append(requirements[a.AccountID], preset)
					out.Requirements++
				}
				entry.Accounts, entry.State = &n, "approved_sources"
				if n == 0 {
					entry.State = "no_approved_accounts"
				}
			}
		}
		if group.Status != service.StatusActive {
			entry.State = "product_inactive"
		}
		if entry.State != "approved_sources" {
			out.State = "incomplete"
		}
		out.Products = append(out.Products, entry)
	}
	if len(accounts) > service.ProcurementProductImpactAccountLimit {
		return nil, service.ErrProcurementProductImpactTooLarge
	}
	settings, err := loadMonitoringProbeBudgetSettings(ctx, tx, "")
	if err != nil {
		return nil, err
	}
	inventory, err := loadMonitoringProbeCoverageInventory(ctx, tx, nil)
	if err != nil {
		return nil, err
	}
	out.ObservedAt = inventory.observedAt
	ids := make([]int64, 0, len(accounts))
	for id := range accounts {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	conflicts, err := loadProcurementObservationConflicts(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	occupied := map[int64]service.ProcurementRecipeDistributionConflict{}
	for _, c := range conflicts {
		occupied[c.AccountID] = c
	}
	proposed := append([]service.MonitoringProbeCoveragePlan(nil), inventory.plans...)
	complete := out.State == "compatible" && len(ids) > 0
	for _, id := range ids {
		row := accounts[id]
		if len(row.ProductIDs) > 1 {
			out.SharedAccounts++
		}
		plan, reason := service.MergeProcurementMonitoringRecipes(requirements[id], inventory.observedAt)
		row.State, row.Reason = "compatible", reason
		if plan == nil {
			row.State, complete = "conflict", false
			if out.State != "incomplete" {
				out.State = "conflict"
			}
		} else {
			row.Models = append(row.Models, plan.PelicanConfig.ModelIDs...)
			row.CronExpression = plan.CronExpression
			plan.AccountID = id
			proposed = append(proposed, service.MonitoringProbeCoveragePlan{Plan: *plan})
		}
		if c, found := occupied[id]; found {
			row.ExistingPlanID, row.ExistingEnabled = &c.PlanID, &c.Enabled
			row.State, row.Reason = "conflict", "existing_observation_plan"
			if out.State != "incomplete" {
				out.State = "conflict"
			}
		}
		out.Accounts = append(out.Accounts, *row)
	}
	// An incomplete requirement set has no honest total. Existing native-plan
	// conflicts remain visible independently of an otherwise computable budget.
	if complete {
		current := inventory.compile(settings, inventory.plans, inventory.plansComplete)
		out.Coverage = inventory.compile(settings, proposed, inventory.plansComplete && len(proposed) <= monitoringCoverageInventoryLimit)
		out.CurrentMinimumRequestsPerDay = &current.MinimumTotalRequestsPerDay
		delta := out.Coverage.MinimumTotalRequestsPerDay - current.MinimumTotalRequestsPerDay
		out.DeltaMinimumRequestsPerDay = &delta
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
