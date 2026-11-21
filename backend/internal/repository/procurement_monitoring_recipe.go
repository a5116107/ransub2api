package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

func loadProcurementMonitoringRecipe(ctx context.Context, tx *sql.Tx, group *service.Group, presetID int64, sources *service.ProcurementSourceInventory, quoteAt time.Time) (*service.ProcurementMonitoringRecipe, error) {
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
	distribution := &service.ProcurementRecipeDistributionReview{State: "unknown", Conflicts: []service.ProcurementRecipeDistributionConflict{}}
	out := &service.ProcurementMonitoringRecipe{Version: 3, PresetID: preset.ID, PresetName: preset.Name, PresetUpdatedAt: preset.UpdatedAt, State: "sources_unavailable", DistributionReview: distribution}
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
	// Match migration 258, including paused and BPS observation rules. The
	// enabled-only budget inventory cannot establish this native constraint.
	distribution.Conflicts, err = loadProcurementObservationConflicts(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	distribution.ExaminedAccounts = n
	distribution.State = "ready"
	if len(distribution.Conflicts) > 0 {
		distribution.State = "conflict"
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
	if quoteAt.IsZero() {
		quoteAt = inventory.observedAt
	}
	// Freeze the review's clock, not the inventory used by the budget recheck.
	// Only the generated observation time is excluded from the configuration CAS.
	canonical := *out
	demand, coverage := *out.Demand, *proposed
	coverage.ObservedAt = time.Time{}
	demand.Coverage = &coverage
	canonical.Demand = &demand
	raw, err := json.Marshal(struct {
		GroupID     int64
		GroupStatus string
		ObservedAt  time.Time
		Recipe      service.ProcurementMonitoringRecipe
		Preset      *service.QualityRulePreset
		Sources     *service.ProcurementSourceInventory
		Budget      service.MonitoringProbeBudgetSettings
	}{group.ID, group.Status, quoteAt, canonical, preset, sources, settings})
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(raw)
	out.ApplicationReview = &service.ProcurementRecipeApplicationReview{ObservedAt: quoteAt, ExpiresAt: quoteAt.Add(service.ProcurementRecipeReviewTTL),
		Fingerprint: hex.EncodeToString(hash[:]), CanConfirm: group.Status == service.StatusActive && distribution.State == "ready" && proposed.Enforced && proposed.CanSave}
	return out, nil
}

func loadProcurementObservationConflicts(ctx context.Context, tx *sql.Tx, ids []int64) ([]service.ProcurementRecipeDistributionConflict, error) {
	conflicts := []service.ProcurementRecipeDistributionConflict{}
	rows, err := tx.QueryContext(ctx, `SELECT account_id,id,enabled FROM scheduled_test_plans
 WHERE account_id=ANY($1) AND pelican_config#>>'{quality,action}'=$2 ORDER BY account_id,id LIMIT $3`,
		pq.Array(ids), service.QualityActionObserveOnly, len(ids)+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var c service.ProcurementRecipeDistributionConflict
		if err := rows.Scan(&c.AccountID, &c.PlanID, &c.Enabled); err != nil {
			return nil, err
		}
		conflicts = append(conflicts, c)
	}
	return conflicts, errors.Join(rows.Err(), rows.Close())
}

func (r *channelRepository) ApplyProcurementRecipe(ctx context.Context, id int64, command *service.ProcurementRecipeCommand) (out *service.ProcurementRecipeApplication, err error) {
	if err := procurementManagementAccess(ctx, id); err != nil {
		return nil, err
	}
	if command.Validate() != nil {
		return nil, service.ErrProcurementRecipeCommand
	}
	defer func() {
		var pg *pq.Error
		if errors.As(err, &pg) && slices.Contains([]pq.ErrorCode{"23505", "23503", "40001", "40P01", "55P03"}, pg.Code) {
			err = service.ErrProcurementRecipeConflict
		}
	}()
	// Read committed is deliberate: after waiting for the original advisory
	// budget lock, each read must see the preceding writer's committed plans.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='5s'`); err != nil {
		return nil, err
	}
	if err = lockMonitoringCoveragePlanWrite(ctx, tx); err != nil {
		return nil, err
	}
	var found int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM groups WHERE id=$1 AND deleted_at IS NULL FOR NO KEY UPDATE`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrGroupNotFound
	}
	if err != nil {
		return nil, err
	}
	_, _, initial, err := loadProcurementProductReview(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if initial.SourceInventory.State != "approved_sources" {
		return nil, service.ErrProcurementRecipeConflict
	}
	ids, groups := []int64{}, []int64{}
	for _, a := range initial.SourceInventory.Accounts {
		if a.InApprovedSources != nil && *a.InApprovedSources {
			ids = append(ids, a.AccountID)
		}
	}
	for _, g := range initial.SourceInventory.Groups {
		groups = append(groups, g.ID)
	}
	if !service.ValidProcurementManagementIDs(ids) {
		return nil, service.ErrProcurementRecipeConflict
	}
	slices.Sort(ids)
	slices.Sort(groups)
	for _, groupID := range groups {
		if err = tx.QueryRowContext(ctx, `SELECT id FROM groups WHERE id=$1 AND deleted_at IS NULL AND status='active' FOR SHARE`, groupID).Scan(&found); errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrProcurementRecipeConflict
		} else if err != nil {
			return nil, err
		}
	}
	for _, accountID := range ids {
		if err = lockLiveAccountForGroupBindings(ctx, tx, accountID); errors.Is(err, service.ErrAccountNotFound) {
			return nil, service.ErrProcurementRecipeConflict
		} else if err != nil {
			return nil, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT account_id FROM account_groups WHERE account_id=ANY($1) AND group_id=ANY($2) ORDER BY account_id,group_id FOR SHARE`, pq.Array(ids), pq.Array(groups))
	if err != nil {
		return nil, err
	}
	linked := map[int64]bool{}
	for rows.Next() {
		var accountID int64
		if err := rows.Scan(&accountID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		linked[accountID] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(linked) != len(ids) {
		return nil, service.ErrProcurementRecipeConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT id FROM quality_rule_presets WHERE id=$1 FOR SHARE`, command.PresetID).Scan(&found); errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrProcurementRecipeConflict
	} else if err != nil {
		return nil, err
	}
	group, _, review, err := loadProcurementProductReview(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	recipe, err := loadProcurementMonitoringRecipe(ctx, tx, group, command.PresetID, review.SourceInventory, command.ObservedAt)
	if errors.Is(err, service.ErrProcurementMonitoringRecipeInvalid) || errors.Is(err, service.ErrProcurementMonitoringRecipeNotFound) {
		return nil, service.ErrProcurementRecipeConflict
	}
	if err != nil {
		return nil, err
	}
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	application := recipe.ApplicationReview
	if application == nil || !application.CanConfirm || application.Fingerprint != command.Fingerprint || now.Before(command.ObservedAt) || !now.Before(application.ExpiresAt) {
		return nil, service.ErrProcurementRecipeConflict
	}
	preset, err := scanQualityRulePreset(tx.QueryRowContext(ctx, `SELECT `+qualityRulePresetColumns+` FROM quality_rule_presets WHERE id=$1`, command.PresetID))
	if err != nil {
		return nil, err
	}
	base, err := service.ProcurementMonitoringRecipePlan(preset, now)
	if err != nil {
		return nil, err
	}
	out = &service.ProcurementRecipeApplication{Version: 1, GroupID: id, PresetID: command.PresetID, Plans: []service.ProcurementRecipeCreatedPlan{}}
	for _, accountID := range ids {
		plan := *base
		plan.AccountID = accountID
		created, err := insertScheduledTestPlanRow(ctx, tx, &plan)
		if err != nil {
			return nil, err
		}
		out.Plans = append(out.Plans, service.ProcurementRecipeCreatedPlan{AccountID: accountID, PlanID: created.ID})
	}
	if err = validateMonitoringCoveragePlanWrite(ctx, tx, true); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	if !now.Before(application.ExpiresAt) {
		return nil, service.ErrProcurementRecipeConflict
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
