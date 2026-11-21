package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *channelRepository) PreviewProcurementSharedMonitoring(ctx context.Context, command *service.ProcurementSharedMonitoringCommand) (*service.ProcurementSharedMonitoringPreview, error) {
	if err := command.Validate(); err != nil {
		return nil, err
	}
	if err := procurementManagementAccess(ctx, 1); err != nil {
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
	compiled, err := compileProcurementSharedMonitoring(ctx, tx, command, time.Time{}, false)
	if err != nil {
		return nil, err
	}
	return compiled.preview, tx.Commit()
}

type procurementSharedMonitoringCompilation struct {
	preview       *service.ProcurementSharedMonitoringPreview
	saved         *procurementObservationState
	plans         map[int64]*service.ScheduledTestPlan
	sourceGroups  []int64
	productGroups []int64
	presets       []int64
	requireBudget bool
}

func compileProcurementSharedMonitoring(ctx context.Context, tx *sql.Tx, command *service.ProcurementSharedMonitoringCommand, observed time.Time, maintenance bool) (*procurementSharedMonitoringCompilation, error) {
	out := &service.ProcurementSharedMonitoringPreview{Version: command.Version, Scope: "approved_sources_only", State: "compatible",
		Products: []service.ProcurementSharedMonitoringProduct{}, Accounts: []service.ProcurementSharedMonitoringAccount{}}
	saved := &procurementObservationState{Version: 1, Products: []service.ProcurementMonitoringProductSelection{}, Plans: []procurementObservationOwner{}}
	var err error
	if command.Version == 2 {
		saved, err = loadProcurementObservationState(ctx, tx)
		if err != nil {
			return nil, err
		}
	}
	compiled := &procurementSharedMonitoringCompilation{preview: out, saved: saved, plans: map[int64]*service.ScheduledTestPlan{}}
	owners := map[int64]procurementObservationOwner{}
	for _, p := range saved.Plans {
		owners[p.AccountID] = p
	}
	native, err := loadProcurementObservationPlans(ctx, tx, saved.Plans)
	if err != nil {
		return nil, err
	}
	fences := []any{}
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
		compiled.productGroups = append(compiled.productGroups, selection.ProductID)
		if maintenance {
			var status, name string
			var deleted bool
			if err := tx.QueryRowContext(ctx, `SELECT status,name,deleted_at IS NOT NULL FROM groups WHERE id=$1`, selection.ProductID).Scan(&status, &name, &deleted); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil, service.ErrGroupNotFound
				}
				return nil, err
			}
			if deleted || status != service.StatusActive {
				fences = append(fences, []any{selection.ProductID, status, deleted})
				out.Products = append(out.Products, service.ProcurementSharedMonitoringProduct{ProductID: selection.ProductID, Name: name, PresetID: selection.PresetID, State: "product_inactive"})
				continue
			}
		}
		group, _, review, err := loadProcurementProductReview(ctx, tx, selection.ProductID)
		if err != nil {
			return nil, err
		}
		compiled.presets = append(compiled.presets, selection.PresetID)
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
		fences = append(fences, []any{group.ID, group.Status, group.ProcurementPolicy, preset})
		entry := service.ProcurementSharedMonitoringProduct{ProductID: group.ID, Name: group.Name, PresetID: preset.ID, State: "sources_unavailable"}
		if sources := review.SourceInventory; sources != nil {
			for _, g := range sources.Groups {
				compiled.sourceGroups = append(compiled.sourceGroups, g.ID)
			}
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
		if entry.State != "approved_sources" && !(maintenance && entry.State == "no_approved_accounts") {
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
	currentTime := inventory.observedAt
	if !observed.IsZero() {
		inventory.observedAt = observed
	}
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
	proposed := []service.MonitoringProbeCoveragePlan{}
	ownedIDs := map[int64]bool{}
	for _, p := range saved.Plans {
		ownedIDs[p.PlanID] = true
	}
	for _, p := range inventory.plans {
		if p.Group || !ownedIDs[p.Plan.ID] {
			proposed = append(proposed, p)
		}
	}
	complete := out.State == "compatible" && (len(ids) > 0 || command.Version == 2)
	changes := []service.ProcurementSharedMonitoringChange{}
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
			if owner, found := owners[id]; found {
				plan.ID = owner.PlanID
			}
			compiled.plans[id] = plan
			proposed = append(proposed, service.MonitoringProbeCoveragePlan{Plan: *plan})
		}
		if c, found := occupied[id]; found {
			row.ExistingPlanID, row.ExistingEnabled = &c.PlanID, &c.Enabled
			if owner, ours := owners[id]; !ours || owner.PlanID != c.PlanID {
				row.State, row.Reason = "conflict", "existing_observation_plan"
			}
		}
		change := service.ProcurementSharedMonitoringChange{AccountID: id, Operation: "create", Reason: "compatible"}
		if owner, found := owners[id]; found {
			change.PlanID = owner.PlanID
			if reason := procurementObservationOwnerReason(owner, native, currentTime); reason != "compatible" {
				row.State, row.Reason = "conflict", reason
				complete = false
			}
			if row.State == "compatible" {
				raw, err := json.Marshal(plan.PelicanConfig)
				if err != nil {
					return nil, err
				}
				definition, err := procurementObservationDefinition(plan, raw)
				if err != nil {
					return nil, err
				}
				change.Operation = "update"
				if definition == owner.Definition {
					change.Operation = "retain"
				}
			}
		}
		if row.State == "conflict" {
			change.Operation, change.Reason = "conflict", row.Reason
			if out.State != "incomplete" {
				out.State = "conflict"
			}
		}
		changes = append(changes, change)
		out.Accounts = append(out.Accounts, *row)
	}
	for _, owner := range saved.Plans {
		if _, required := accounts[owner.AccountID]; required {
			continue
		}
		change := service.ProcurementSharedMonitoringChange{AccountID: owner.AccountID, PlanID: owner.PlanID, Operation: "pause", Reason: procurementObservationOwnerReason(owner, native, currentTime)}
		if change.Reason != "compatible" {
			change.Operation, complete = "conflict", false
			if out.State != "incomplete" {
				out.State = "conflict"
			}
		} else {
			plan := native[owner.PlanID].Plan
			if !plan.Enabled {
				change.Operation = "retain_paused"
			}
			if command.ReleaseOwnership {
				change.Operation = "release"
			}
			plan.Enabled = false
			compiled.plans[owner.AccountID] = &plan
		}
		changes = append(changes, change)
	}
	if len(changes) > service.ProcurementProductImpactAccountLimit {
		return nil, service.ErrProcurementProductImpactTooLarge
	}
	slices.SortFunc(changes, func(a, b service.ProcurementSharedMonitoringChange) int {
		if a.AccountID < b.AccountID {
			return -1
		}
		if a.AccountID > b.AccountID {
			return 1
		}
		return 0
	})
	// An incomplete requirement set has no honest total. Existing native-plan
	// conflicts remain visible independently of an otherwise computable budget.
	if complete {
		current := inventory.compile(settings, inventory.plans, inventory.plansComplete)
		out.Coverage = inventory.compile(settings, proposed, inventory.plansComplete && len(proposed) <= monitoringCoverageInventoryLimit)
		out.CurrentMinimumRequestsPerDay = &current.MinimumTotalRequestsPerDay
		delta := out.Coverage.MinimumTotalRequestsPerDay - current.MinimumTotalRequestsPerDay
		out.DeltaMinimumRequestsPerDay = &delta
	}
	if command.Version == 2 {
		if observed.IsZero() {
			observed = out.ObservedAt
		}
		review := &service.ProcurementSharedMonitoringReview{Revision: saved.Revision, ObservedAt: observed, ExpiresAt: observed.Add(2 * time.Minute), ReleaseOwnership: command.ReleaseOwnership, Changes: changes}
		// Pausing/releasing all requirements remains an available repair when
		// budget protection is off or unrelated old demand is already excessive.
		compiled.requireBudget = len(products) > 0 && !(maintenance && len(accounts) == 0)
		review.CanConfirm = out.State == "compatible" && out.Coverage != nil && (!compiled.requireBudget || out.Coverage.Enforced && out.Coverage.CanSave)
		definitions := make([]any, 0, len(native))
		planIDs := make([]int64, 0, len(native))
		for id := range native {
			planIDs = append(planIDs, id)
		}
		slices.Sort(planIDs)
		for _, id := range planIDs {
			definitions = append(definitions, []any{id, native[id].Definition})
		}
		review.Fingerprint, err = procurementObservationDigest([]any{observed, command.ReleaseOwnership, products, saved, fences, definitions, changes, settings, out.Coverage, review.CanConfirm})
		if err != nil {
			return nil, err
		}
		out.ApplicationReview = review
	}
	return compiled, nil
}
