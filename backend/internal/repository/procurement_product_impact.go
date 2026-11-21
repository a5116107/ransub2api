package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbaccountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	dbgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

func (r *channelRepository) PreviewProcurementProductImpact(ctx context.Context, groupID int64, presetIDs ...int64) (*service.ProcurementProductImpact, error) {
	if groupID < 1 || groupID > domain.MaxProcurementPolicyRevision {
		return nil, service.ErrProcurementCatalogInvalid
	}
	if _, scoped := service.ObserverGroupIDs(ctx); scoped || adminaccess.FromContext(ctx) != nil {
		return nil, service.ErrObserverScope
	}
	if len(presetIDs) > 1 || len(presetIDs) == 1 && (presetIDs[0] < 1 || presetIDs[0] > domain.MaxProcurementPolicyRevision) {
		return nil, service.ErrProcurementMonitoringRecipeInvalid
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	group, accounts, review, err := loadProcurementProductReview(ctx, tx, groupID)
	if err != nil {
		return nil, err
	}
	out := &service.ProcurementProductImpact{Version: 1, GroupStatus: group.Status, GateEnabled: group.ProfitControlEnabled, BoundAccounts: len(accounts), ManagementReview: review}
	if len(presetIDs) == 1 {
		out.MonitoringRecipe, err = loadProcurementMonitoringRecipe(ctx, tx, presetIDs[0], review.SourceInventory)
		if err != nil {
			return nil, err
		}
	}
	var minOverride sql.NullFloat64
	if err = tx.QueryRowContext(ctx, `SELECT min(rate_multiplier)::float8,count(*) FROM user_group_rate_multipliers
	 WHERE group_id=$1 AND rate_multiplier>=0 AND rate_multiplier NOT IN ('NaN'::numeric,'Infinity'::numeric)`, groupID).Scan(&minOverride, &out.UserOverrideCount); err != nil {
		return nil, err
	}
	// Time-dependent peaks and evidence expire against the end of this single
	// snapshot read, not an earlier time captured before waiting on SQL.
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&out.ObservedAt); err != nil {
		return nil, err
	}
	in := service.ProfitPreviewGroupInput{Group: group, AssumeEnabled: true}
	if minOverride.Valid {
		in.UserOverrides = map[int64]float64{0: minOverride.Float64} // no user identities leave this read
	}
	for i := range accounts {
		account := &accounts[i]
		// Only saved administrative eligibility is considered. Temporary health,
		// model/scope support, cooldown, quota and live slots are not simulated.
		if account.Status == service.StatusActive && account.Schedulable {
			in.Accounts = append(in.Accounts, account)
		}
	}
	out.ConsideredAccounts = len(in.Accounts)
	out.Report = service.PreviewProfitAdmission([]service.ProfitPreviewGroupInput{in}, out.ObservedAt)[0]
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func loadProcurementProductReview(ctx context.Context, tx *sql.Tx, groupID int64) (*service.Group, []service.Account, *service.ProcurementManagementReview, error) {
	client := dbent.NewClient(dbent.Driver(entsql.NewDriver(dialect.Postgres, entsql.Conn{ExecQuerier: tx})))
	entity, err := client.Group.Query().Where(dbgroup.IDEQ(groupID), dbgroup.DeletedAtIsNil()).Only(ctx)
	if dbent.IsNotFound(err) {
		return nil, nil, nil, service.ErrGroupNotFound
	}
	if err != nil {
		return nil, nil, nil, err
	}
	group := groupEntityToService(entity)
	group.ProcurementCatalog, err = loadProcurementCatalog(ctx, tx)
	if err != nil {
		return nil, nil, nil, err
	}
	// Preserve an unavailable source authority as unknown in the original price
	// verdict, but never suppress database/cancellation errors as empty sources.
	group.ProcurementSources, err = loadProcurementSources(ctx, tx, group)
	if err != nil && !errors.Is(err, service.ErrProcurementPolicyInvalid) {
		return nil, nil, nil, err
	}
	entities, err := client.Account.Query().Where(dbaccount.DeletedAtIsNil(),
		dbaccount.HasAccountGroupsWith(dbaccountgroup.GroupIDEQ(groupID))).Order(dbaccount.ByID()).
		Limit(service.ProcurementProductImpactAccountLimit + 1).All(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(entities) > service.ProcurementProductImpactAccountLimit {
		return nil, nil, nil, service.ErrProcurementProductImpactTooLarge
	}
	accounts, err := newAccountRepositoryWithSQL(client, tx, nil).accountsToService(ctx, entities)
	if err != nil {
		return nil, nil, nil, err
	}
	review, err := loadProcurementManagementReview(ctx, tx, group, accounts)
	if err != nil {
		return nil, nil, nil, err
	}
	if entity.ProcurementManagement != nil {
		review.Mode = entity.ProcurementManagement.Mode
	}
	return group, accounts, review, nil
}

const procurementManagementRuleLimit = 1000

func loadProcurementManagementReview(ctx context.Context, tx sqlQueryer, group *service.Group, accounts []service.Account) (*service.ProcurementManagementReview, error) {
	groupID := group.ID
	ids := make([]int64, 0, len(accounts))
	bound, approved := make(map[int64]bool, len(accounts)), map[int64]bool{}
	for i := range accounts {
		ids = append(ids, accounts[i].ID)
		bound[accounts[i].ID] = true
	}
	inventory := &service.ProcurementSourceInventory{Version: 1, State: "original_pool", Groups: []service.ProcurementSourceGroup{}, Accounts: []service.ProcurementSourceAccount{}}
	if policy := group.ProcurementPolicy; policy != nil {
		inventory.PolicyRevision = policy.Revision
		if policy.Version >= 6 && policy.SourceGroupIDs != nil {
			inventory.State = "unavailable"
			rows, err := tx.QueryContext(ctx, `SELECT id,name,status,platform,deleted_at IS NULL FROM groups WHERE id=ANY($1)`, pq.Array(policy.SourceGroupIDs))
			if err != nil {
				return nil, err
			}
			groups := map[int64]service.ProcurementSourceGroup{}
			for rows.Next() {
				var entry service.ProcurementSourceGroup
				var status, platform string
				var live bool
				if err := rows.Scan(&entry.ID, &entry.Name, &status, &platform, &live); err != nil {
					_ = rows.Close()
					return nil, err
				}
				entry.State = "active"
				switch {
				case !live:
					entry.State = "unavailable"
				case entry.ID == groupID:
					entry.State = "self_source"
				case platform != group.Platform:
					entry.State = "platform_mismatch"
				case status != service.StatusActive:
					entry.State = "inactive"
				}
				groups[entry.ID] = entry
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return nil, err
			}
			for _, id := range policy.SourceGroupIDs {
				entry, ok := groups[id]
				if !ok {
					entry = service.ProcurementSourceGroup{ID: id, State: "unavailable"}
				}
				inventory.Groups = append(inventory.Groups, entry)
			}
			// Consume the same authority as the actual procurement gate; do not
			// infer permission from names, pricing or the existence of an account.
			if group.ProcurementSources != nil && group.ProcurementSources.ValidateFor(groupID, policy) == nil {
				inventory.State = "approved_sources"
				for _, id := range group.ProcurementSources.AccountIDs {
					allowed, err := policy.AllowsAccount(id)
					if err != nil {
						return nil, service.ErrProcurementPolicyInvalid
					}
					if allowed {
						approved[id] = true
						if !bound[id] {
							ids = append(ids, id)
						}
					}
				}
				count, unbound := len(approved), len(ids)-len(bound)
				inventory.ApprovedAccounts, inventory.UnboundApprovedAccounts = &count, &unbound
			}
		}
	}
	if len(ids) > service.ProcurementProductImpactAccountLimit {
		return nil, service.ErrProcurementProductImpactTooLarge
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,name,platform,status,schedulable FROM accounts WHERE id=ANY($1) AND deleted_at IS NULL ORDER BY id`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var entry service.ProcurementSourceAccount
		if err := rows.Scan(&entry.AccountID, &entry.Name, &entry.Platform, &entry.Status, &entry.Schedulable); err != nil {
			_ = rows.Close()
			return nil, err
		}
		entry.InProduct = bound[entry.AccountID]
		if inventory.State == "approved_sources" {
			value := approved[entry.AccountID]
			entry.InApprovedSources = &value
		}
		inventory.Accounts = append(inventory.Accounts, entry)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(inventory.Accounts) != len(ids) {
		return nil, service.ErrProcurementPolicyInvalid
	}
	// Saved removals remain relevant even after membership disappears or the
	// schedule is paused. Do not equate a paused rule with relinquished consent.
	rows, err = tx.QueryContext(ctx, `SELECT p.id,p.account_id,a.name,p.enabled,p.auto_recover,p.pelican_config,s.state
 FROM scheduled_test_plans p JOIN accounts a ON a.id=p.account_id
 LEFT JOIN account_quality_states s ON s.plan_id=p.id
 WHERE a.deleted_at IS NULL AND (p.enabled OR s.plan_id IS NOT NULL) AND (
 p.account_id=ANY($1) OR p.pelican_config#>'{quality,remove_group_ids}' @> jsonb_build_array($2::bigint)
 OR s.state->'removed' @> jsonb_build_array(jsonb_build_object('group_id',$2::bigint))
 OR s.state#>'{groups,removed}' @> jsonb_build_array(jsonb_build_object('group_id',$2::bigint))
 OR s.state->'isolation_group_ids' @> jsonb_build_array($2::bigint))
 ORDER BY p.id LIMIT $3`, pq.Array(ids), groupID, procurementManagementRuleLimit+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := &service.ProcurementManagementReview{Version: 1, Mode: "manual", Scope: "current_bindings_and_saved_rule_targets", InventoryComplete: true, Rules: []service.ProcurementManagementRule{}}
	out.SourceInventory = inventory
	if inventory.State == "approved_sources" {
		out.Scope = "current_bindings_and_approved_sources_and_saved_rule_targets"
	}
	for rows.Next() {
		if out.RulesExamined == procurementManagementRuleLimit {
			out.InventoryComplete = false
			break
		}
		var rule service.ProcurementManagementRule
		var autoRecover bool
		var config, rawState []byte
		if err := rows.Scan(&rule.PlanID, &rule.AccountID, &rule.AccountName, &rule.Enabled, &autoRecover, &config, &rawState); err != nil {
			return nil, err
		}
		var cfg *service.PelicanTestConfig
		var state *qualityState
		invalid := len(config) > 0 && json.Unmarshal(config, &cfg) != nil
		if len(rawState) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(rawState))
			decoder.DisallowUnknownFields()
			invalid = decoder.Decode(&state) != nil || state == nil || invalid
		}
		rule.Action, rule.Risk = "connectivity", "observation"
		if cfg != nil && cfg.Quality != nil {
			q := cfg.Quality
			rule.Action = q.Action
			switch q.Action {
			case "remove_groups", service.QualityActionIsolateModels:
				rule.WritesProduct = rule.Enabled && slices.Contains(q.RemoveGroupIDs, groupID)
			case "disable_scheduling", service.QualityActionRemoveModel, service.QualityActionEnableBPS, service.QualityActionObserveOnly, service.QualityActionOpenAITwoFAAdmission:
			default:
				invalid = true
			}
			rule.JudgeDependsOnProduct = rule.Enabled && q.Judge != nil && (q.Judge.Kind == "" || q.Judge.Kind == "llm") && q.Judge.GroupID == groupID
		}
		if state != nil {
			rule.RetainedOwnership = state.Action != "" || len(state.Removed) > 0 || len(state.IsolationGroupIDs) > 0 || len(state.IsolationModels) > 0 ||
				state.PersistentModels || len(state.BPSPrevious) > 0 || len(state.BPSApplied) > 0 || state.Models != nil || state.Groups != nil || hasQualityCooldownOwnership(state)
			switch state.Action {
			case "", "remove_groups", "disable_scheduling", service.QualityActionIsolateModels, service.QualityActionRemoveModel, service.QualityActionEnableBPS:
			default:
				invalid = true
			}
			rule.SavedProductOwnership = slices.Contains(state.IsolationGroupIDs, groupID) || slices.ContainsFunc(state.Removed, func(g qualityGroup) bool { return g.GroupID == groupID }) ||
				state.Groups != nil && slices.ContainsFunc(state.Groups.Removed, func(g qualityGroup) bool { return g.GroupID == groupID })
		}
		switch {
		case invalid:
			rule.Risk = "unknown"
			rule.Action = "unknown"
			out.UnknownRules++
		case rule.WritesProduct || rule.SavedProductOwnership:
			rule.Risk = "membership_conflict"
			out.ConflictingRules++
		case rule.RetainedOwnership || rule.Enabled && (autoRecover || rule.Action != "connectivity" && rule.Action != service.QualityActionObserveOnly):
			rule.Risk = "account_protection"
			out.ProtectionRules++
		}
		if rule.JudgeDependsOnProduct {
			out.JudgeDependencyRules++
		}
		out.Rules = append(out.Rules, rule)
		out.RulesExamined++
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out.WriterReview, err = loadProcurementWriterReview(ctx, tx, group, ids)
	if err != nil {
		return nil, err
	}
	settings, err := loadMonitoringProbeBudgetSettings(ctx, tx, "")
	if err != nil {
		return nil, err
	}
	coverage, err := loadMonitoringProbeCoverage(ctx, tx, settings)
	if err != nil {
		return nil, err
	}
	budget := service.ProcurementManagementBudget{State: "known_lower_bound_fits", Revision: settings.Revision, Enforced: settings.Enabled,
		InventoryComplete:     coverage.InventoryComplete && coverage.PlanInventoryComplete && coverage.CandyInventoryComplete && coverage.ActiveInventoryComplete,
		MinimumRequestsPerDay: coverage.MinimumTotalRequestsPerDay, RegularDayCapacity: coverage.RegularDayCapacity,
		UnestimatedSources: len(coverage.UnestimatedDemandSources), Reasons: coverage.BlockingReasons, Warnings: coverage.Warnings,
		WindowStart: coverage.ScheduleWindowStart, WindowEnd: coverage.ScheduleWindowEnd}
	switch {
	case !settings.Enabled:
		budget.State = "not_enforced"
	case !coverage.CanSave:
		budget.State = "insufficient"
	case !budget.InventoryComplete || budget.UnestimatedSources > 0 || len(budget.Warnings) > 0:
		budget.State = "partial"
	}
	out.Budget = budget
	return out, nil
}
