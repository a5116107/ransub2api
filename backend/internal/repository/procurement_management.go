package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

func procurementManagementAccess(ctx context.Context, id int64) error {
	if id < 1 || id > domain.MaxProcurementPolicyRevision {
		return service.ErrProcurementManagementInvalid
	}
	if _, scoped := service.ObserverGroupIDs(ctx); scoped || adminaccess.FromContext(ctx) != nil {
		return service.ErrObserverScope
	}
	return nil
}

// Independent of price catalogs, source inventory and eligibility decoding, so
// operators can read authorization or return to manual during a config fault.
func readProcurementManagement(ctx context.Context, db sqlQueryer, id int64) (*service.ProcurementManagementState, error) {
	var raw []byte
	err := scanSingleRow(ctx, db, `SELECT procurement_management FROM groups WHERE id=$1 AND deleted_at IS NULL`, []any{id}, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrGroupNotFound
	}
	if err != nil {
		return nil, err
	}
	out := &service.ProcurementManagementState{Version: 1, GroupID: id, Mode: "manual"}
	if len(raw) > 0 {
		var saved domain.ProcurementManagement
		if json.Unmarshal(raw, &saved) != nil {
			return nil, service.ErrProcurementManagementUnavailable
		}
		out.Revision, out.Mode = saved.Revision, saved.Mode
	}
	err = scanSingleRow(ctx, db, `SELECT
 count(*) FILTER (WHERE ownership IS NOT NULL AND $3='review' AND ownership->>'revision'=$2),
 count(*) FILTER (WHERE ownership IS NOT NULL AND NOT ($3='review' AND ownership->>'revision'=$2)),
 count(*) FILTER (WHERE ownership IS NULL) FROM account_groups WHERE group_id=$1`,
		[]any{id, strconv.FormatInt(out.Revision, 10), out.Mode}, &out.CurrentOwned, &out.RetainedOwned, &out.ManualMemberships)
	return out, err
}

func (r *channelRepository) ReadProcurementManagement(ctx context.Context, id int64) (*service.ProcurementManagementState, error) {
	if err := procurementManagementAccess(ctx, id); err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	out, err := readProcurementManagement(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func (r *channelRepository) PreviewProcurementManagement(ctx context.Context, id int64, ids []int64) (*service.ProcurementManagementPreview, error) {
	if err := procurementManagementAccess(ctx, id); err != nil {
		return nil, err
	}
	if !service.ValidProcurementManagementIDs(ids) {
		return nil, service.ErrProcurementManagementInvalid
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	out, err := buildProcurementManagementPreview(ctx, tx, id, ids, time.Time{})
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func buildProcurementManagementPreview(ctx context.Context, tx *sql.Tx, id int64, selected []int64, observed time.Time) (*service.ProcurementManagementPreview, error) {
	state, err := readProcurementManagement(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	group, accounts, review, err := loadProcurementProductReview(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	out := &service.ProcurementManagementPreview{Version: 1, State: state, Review: review, CanConfirm: true, Memberships: []service.ProcurementMembershipReview{}}
	byID := map[int64]service.ProcurementSourceAccount{}
	for _, a := range review.SourceInventory.Accounts {
		byID[a.AccountID] = a
	}
	ids := slices.Sorted(slices.Values(selected))
	for _, accountID := range ids {
		row := service.ProcurementMembershipReview{AccountID: accountID, Claimable: true, Reasons: []string{}, Protections: []string{}}
		block := func(reason string) {
			if !slices.Contains(row.Reasons, reason) {
				row.Reasons = append(row.Reasons, reason)
			}
			row.Claimable = false
		}
		a, ok := byID[accountID]
		row.AccountName = a.Name
		if !ok || !a.InProduct {
			block("membership_missing")
		}
		if a.InApprovedSources == nil || !*a.InApprovedSources {
			block("source_not_approved")
		}
		if !review.InventoryComplete || review.WriterReview == nil || !review.WriterReview.InventoryComplete {
			block("inventory_incomplete")
		}
		for _, rule := range review.Rules {
			if rule.AccountID != accountID {
				continue
			}
			if rule.Risk == "membership_conflict" || rule.Risk == "unknown" {
				block("quality_" + rule.Risk)
			}
			if rule.Risk == "account_protection" {
				row.Protections = append(row.Protections, "quality_protection")
			}
			if rule.JudgeDependsOnProduct {
				row.Protections = append(row.Protections, "judge_dependency")
			}
		}
		if review.WriterReview != nil {
			for _, writer := range review.WriterReview.Writers {
				if writer.AccountID != nil && *writer.AccountID != accountID {
					continue
				}
				if writer.Risk == "membership_conflict" || writer.Risk == "unknown" {
					block("native_" + writer.Risk)
				}
				if writer.Risk == "account_protection" {
					row.Protections = append(row.Protections, "native_protection")
				}
			}
		}
		if group.Status != service.StatusActive || a.Status != service.StatusActive || !a.Schedulable {
			row.Protections = append(row.Protections, "manual_restriction")
		}
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT ownership FROM account_groups WHERE account_id=$1 AND group_id=$2`, accountID, id).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if len(raw) > 0 {
			var owner domain.AccountGroupOwnership
			if json.Unmarshal(raw, &owner) != nil {
				block("ownership_unknown")
			} else {
				row.Ownership = &owner
			}
		}
		slices.Sort(row.Protections)
		row.Protections = slices.Compact(row.Protections)
		out.Memberships = append(out.Memberships, row)
		out.CanConfirm = out.CanConfirm && row.Claimable
	}
	if observed.IsZero() {
		if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&observed); err != nil {
			return nil, err
		}
	}
	out.ObservedAt, out.ExpiresAt = observed, observed.Add(service.ProcurementManagementPreviewTTL)

	// The digest is an optimistic concurrency token, not an authorization token.
	// Bind expiry, selection and raw saved content, including same-revision edits.
	// Credentials are only hashed here and never returned to the administrator.
	accountIDs, ruleIDs := make([]int64, 0, len(accounts)), make([]int64, 0, len(review.Rules))
	for _, a := range review.SourceInventory.Accounts {
		accountIDs = append(accountIDs, a.AccountID)
	}
	for _, rule := range review.Rules {
		ruleIDs = append(ruleIDs, rule.PlanID)
	}
	for _, writer := range review.WriterReview.Writers {
		if writer.AccountID != nil {
			accountIDs = append(accountIDs, *writer.AccountID)
		}
	}
	var content []byte
	err = tx.QueryRowContext(ctx, `SELECT jsonb_build_object(
 'product',(SELECT to_jsonb(g) FROM groups g WHERE id=$1),
 'settings',COALESCE((SELECT jsonb_agg(jsonb_build_array(key,value) ORDER BY key) FROM settings WHERE key=ANY($4)), '[]'::jsonb),
 'memberships',COALESCE((SELECT jsonb_agg(to_jsonb(ag) ORDER BY account_id,group_id) FROM account_groups ag WHERE group_id=$1 OR account_id=ANY($2)), '[]'::jsonb),
 'accounts',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',id,'name',name,'revision',ai_ops_revision,
   'credentials',credentials,'extra',ai_ops_configuration_extra(extra)) ORDER BY id) FROM accounts a WHERE id=ANY($2)), '[]'::jsonb),
 'plans',COALESCE((SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM scheduled_test_plans p WHERE id=ANY($3)), '[]'::jsonb),
 'quality',COALESCE((SELECT jsonb_agg(to_jsonb(s) ORDER BY plan_id) FROM account_quality_states s WHERE plan_id=ANY($3)), '[]'::jsonb),
 'astra',COALESCE((SELECT jsonb_agg(to_jsonb(s) ORDER BY account_id) FROM astra_scheduling_states s WHERE account_id=ANY($2)), '[]'::jsonb))`, id, pq.Array(accountIDs), pq.Array(ruleIDs), pq.Array([]string{
		service.ProcurementCatalogSettingKey, service.MonitoringProbeBudgetSettingKey, service.SettingKeyUpstreamBillingProbeSettings,
		service.SettingKeyChannelMonitorEnabled, service.SettingKeyChannelMonitorMode, service.SettingKeyOAuthAutoConfig, "astra_routing_experiment_v1",
	})).Scan(&content)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(struct {
		Content json.RawMessage
		Preview *service.ProcurementManagementPreview
	}{content, out})
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(raw)
	out.Fingerprint = hex.EncodeToString(hash[:])
	return out, nil
}

func (r *channelRepository) SaveProcurementManagement(ctx context.Context, id int64, command *service.ProcurementManagementCommand) (out *service.ProcurementManagementState, err error) {
	if err := procurementManagementAccess(ctx, id); err != nil {
		return nil, err
	}
	if command == nil || command.Validate() != nil {
		return nil, service.ErrProcurementManagementInvalid
	}
	defer func() {
		var pg *pq.Error
		if errors.As(err, &pg) && (pg.Code == "40001" || pg.Code == "40P01" || pg.Code == "55P03") {
			err = service.ErrProcurementManagementConflict
		}
	}()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='5s'`); err != nil {
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
	current, err := readProcurementManagement(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if current.Revision != command.ExpectedRevision || current.Revision == domain.MaxProcurementPolicyRevision {
		return nil, service.ErrProcurementManagementConflict
	}
	if command.Mode == "review" {
		for _, accountID := range slices.Sorted(slices.Values(command.AccountIDs)) {
			if err = lockLiveAccountForGroupBindings(ctx, tx, accountID); err != nil {
				if errors.Is(err, service.ErrAccountNotFound) {
					return nil, service.ErrProcurementManagementConflict
				}
				return nil, err
			}
		}
		preview, err := buildProcurementManagementPreview(ctx, tx, id, command.AccountIDs, command.ObservedAt)
		if err != nil {
			return nil, err
		}
		var now time.Time
		if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return nil, err
		}
		if !preview.CanConfirm || preview.Fingerprint != command.Fingerprint || now.Before(preview.ObservedAt) || !now.Before(preview.ExpiresAt) {
			return nil, service.ErrProcurementManagementConflict
		}
		owner, _ := json.Marshal(domain.AccountGroupOwnership{Version: 1, Kind: "procurement", Revision: current.Revision + 1})
		result, err := tx.ExecContext(ctx, `UPDATE account_groups SET ownership=$3::jsonb WHERE group_id=$1 AND account_id=ANY($2)`, id, pq.Array(command.AccountIDs), string(owner))
		if err != nil {
			return nil, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count != int64(len(command.AccountIDs)) {
			return nil, service.ErrProcurementManagementConflict
		}
	}
	raw, err := json.Marshal(domain.ProcurementManagement{Version: 1, Revision: current.Revision + 1, Mode: command.Mode})
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE groups SET procurement_management=$2::jsonb,updated_at=clock_timestamp() WHERE id=$1`, id, string(raw)); err != nil {
		return nil, err
	}
	if err = enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventGroupChanged, nil, &id, nil); err != nil {
		return nil, err
	}
	out, err = readProcurementManagement(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
