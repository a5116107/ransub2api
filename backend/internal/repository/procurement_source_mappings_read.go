package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const sourceMappingColumns = `COALESCE(m.id,0),s.id,s.code,s.name,s.revision,g.id,g.platform,g.external_group_id,g.external_group_name,
 g.local_group_id,lg.platform,lg.name,lg.status,lg.deleted_at IS NOT NULL,g.enabled,g.revision,
 COALESCE(a.id,0),COALESCE(a.name,''),COALESCE(a.status,''),COALESCE(a.deleted_at IS NOT NULL,false),COALESCE(a.platform,''),
 ag.account_id IS NOT NULL,COALESCE(m.enabled,false),COALESCE(m.revision,0),COALESCE(m.updated_at,g.updated_at),m.route_fingerprint,
 COALESCE(a.type,''),COALESCE(a.credentials,'{}'::jsonb),a.proxy_id,COALESCE(a.extra->'upstream_billing_probe','null'::jsonb),
 COALESCE(nb.authorization_id,0),COALESCE(np.site_url,''),COALESCE(np.upstream_user_id,0),COALESCE(np.revision,0),
 COALESCE(nb.upstream_token_id,0),COALESCE(nb.account_fingerprint,''),COALESCE(nb.token_group,'')`

const sourceMappingFrom = ` FROM procurement_supplier_groups g
 JOIN procurement_supplier_sites s ON s.id=g.site_id
 JOIN groups lg ON lg.id=g.local_group_id
 LEFT JOIN procurement_supplier_group_accounts m ON m.source_group_id=g.id
 LEFT JOIN accounts a ON a.id=m.account_id
 LEFT JOIN account_groups ag ON ag.account_id=a.id AND ag.group_id=g.local_group_id
 LEFT JOIN new_api_account_bindings nb ON nb.account_id=a.id
 LEFT JOIN new_api_site_authorizations np ON np.id=nb.authorization_id`

const sourceMappingFilter = `($1='' OR s.code ILIKE '%'||$1||'%' OR s.name ILIKE '%'||$1||'%' OR g.external_group_id ILIKE '%'||$1||'%' OR g.external_group_name ILIKE '%'||$1||'%' OR a.name ILIKE '%'||$1||'%' OR lg.name ILIKE '%'||$1||'%') AND ($2='' OR g.platform=$2)`

type sourceMappingRow struct {
	item    service.ProcurementSourceMapping
	route   []byte
	account *service.Account
	binding *service.NewAPIAccountBinding
}

func scanProcurementSourceMapping(row interface{ Scan(...any) error }) (sourceMappingRow, error) {
	var out sourceMappingRow
	item := &out.item
	account, binding := &service.Account{}, &service.NewAPIAccountBinding{}
	var credentials, snapshot []byte
	err := row.Scan(&item.ID, &item.SourceSiteID, &item.SiteCode, &item.SiteName, &item.SiteRevision,
		&item.SourceGroupID, &item.Platform, &item.ExternalGroupID, &item.ExternalGroupName,
		&item.LocalGroupID, &item.LocalGroupPlatform, &item.LocalGroupName, &item.LocalGroupStatus, &item.LocalGroupDeleted,
		&item.SourceGroupEnabled, &item.SourceGroupRevision, &item.AccountID, &item.AccountName, &item.AccountStatus, &item.AccountDeleted, &item.AccountPlatform,
		&item.InLocalGroup, &item.Enabled, &item.Revision, &item.UpdatedAt, &out.route,
		&account.Type, &credentials, &account.ProxyID, &snapshot,
		&binding.Profile.ID, &binding.Profile.SiteURL, &binding.Profile.UserID, &binding.Profile.Revision,
		&binding.TokenID, &binding.Fingerprint, &binding.Group)
	if errors.Is(err, sql.ErrNoRows) {
		err = service.ErrProcurementSourceMappingNotFound
	}
	if err != nil {
		return out, err
	}
	if item.AccountID > 0 {
		account.ID, account.Platform = item.AccountID, item.AccountPlatform
		if err := json.Unmarshal(credentials, &account.Credentials); err != nil {
			return out, err
		}
		account.Extra = map[string]any{service.UpstreamBillingProbeExtraKey: json.RawMessage(snapshot)}
		out.account = account
	}
	if binding.Profile.ID > 0 {
		binding.AccountID = item.AccountID
		out.binding = binding
	}
	return out, err
}

func assessProcurementSourceMapping(ctx context.Context, tx *sql.Tx, row *sourceMappingRow, at time.Time) error {
	item := &row.item
	item.RouteIdentity = "unknown"
	if item.AccountID > 0 && !item.AccountDeleted {
		fingerprint, err := accountRouteFingerprint(ctx, tx, item.AccountID, false)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			item.RouteIdentity = "current"
			if !bytes.Equal(fingerprint[:], row.route) {
				item.RouteIdentity = "changed"
			}
		}
	}
	classifyProcurementSourceMapping(item)
	item.Evidence = service.BuildProcurementSourceEvidence(item, row.account, row.binding, at)
	return nil
}

func (r *channelRepository) ListProcurementSourceMappings(ctx context.Context, search, platform string, page, size int) (*service.ProcurementSourceMappingPage, error) {
	if err := procurementSourceMappingAccess(ctx); err != nil {
		return nil, err
	}
	if page < 1 || page > 1_000_000 || size < 1 || size > 100 || len(search) > 200 || platform != "" && !domain.IsConcretePlatform(platform) {
		return nil, service.ErrProcurementSourceMappingInvalid
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	out := &service.ProcurementSourceMappingPage{Version: 1, Items: []service.ProcurementSourceMapping{}, Page: page, PageSize: size}
	if err := tx.QueryRowContext(ctx, `SELECT count(*)`+sourceMappingFrom+` WHERE `+sourceMappingFilter, search, platform).Scan(&out.Total); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+sourceMappingColumns+sourceMappingFrom+` WHERE `+sourceMappingFilter+`
 ORDER BY s.code,g.platform,g.external_group_id,g.id,COALESCE(a.id,0) LIMIT $3 OFFSET $4`, search, platform, size, (page-1)*size)
	if err != nil {
		return nil, err
	}
	var loaded []sourceMappingRow
	for rows.Next() {
		row, err := scanProcurementSourceMapping(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		loaded = append(loaded, row)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	// Finish the row stream before using the same connection for route checks.
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&out.ObservedAt); err != nil {
		return nil, err
	}
	for i := range loaded {
		if err := assessProcurementSourceMapping(ctx, tx, &loaded[i], out.ObservedAt); err != nil {
			return nil, err
		}
		out.Items = append(out.Items, loaded[i].item)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// Mutation responses are read before commit from the writer's transaction;
// a concurrent writer cannot change or delete the just-saved revision first.
func readProcurementSourceMappingInTx(ctx context.Context, tx *sql.Tx, id int64) (*service.ProcurementSourceMapping, error) {
	row, err := scanProcurementSourceMapping(tx.QueryRowContext(ctx, `SELECT `+sourceMappingColumns+sourceMappingFrom+` WHERE m.id=$1`, id))
	if err != nil {
		return nil, err
	}
	var at time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
		return nil, err
	}
	if err := assessProcurementSourceMapping(ctx, tx, &row, at); err != nil {
		return nil, err
	}
	return &row.item, nil
}

func (r *channelRepository) GetProcurementSourceMapping(ctx context.Context, id int64) (*service.ProcurementSourceMapping, error) {
	if err := procurementSourceMappingAccess(ctx); err != nil {
		return nil, err
	}
	if id < 1 || id > domain.MaxProcurementPolicyRevision {
		return nil, service.ErrProcurementSourceMappingInvalid
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	out, err := readProcurementSourceMappingInTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func classifyProcurementSourceMapping(item *service.ProcurementSourceMapping) {
	item.Dispatchable = false
	item.SourceIdentity = "declared"
	item.FixedGroupSelection = "unverified"
	item.Reasons = []string{}
	if !item.SourceGroupEnabled || item.ID > 0 && !item.Enabled {
		item.Conclusion = service.ProcurementSourceConclusionPaused
		item.Reasons = append(item.Reasons, "source_mapping_paused")
		return
	}
	blocked := false
	stale := false
	if item.AccountID < 1 {
		blocked = true
		item.Reasons = append(item.Reasons, "source_account_not_mapped")
	} else if item.AccountDeleted {
		blocked = true
		item.Reasons = append(item.Reasons, "source_account_deleted")
	}
	if item.LocalGroupDeleted {
		blocked = true
		item.Reasons = append(item.Reasons, "local_group_deleted")
	}
	if item.AccountID > 0 && (item.AccountPlatform != item.Platform || item.LocalGroupPlatform != item.Platform) {
		blocked = true
		item.Reasons = append(item.Reasons, "source_platform_mismatch")
	}
	if item.AccountID > 0 && !item.InLocalGroup {
		blocked = true
		item.Reasons = append(item.Reasons, "source_account_not_in_local_group")
	}
	if item.AccountID > 0 && item.AccountStatus != "active" {
		blocked = true
		item.Reasons = append(item.Reasons, "source_account_inactive")
	}
	if item.LocalGroupStatus != "active" {
		blocked = true
		item.Reasons = append(item.Reasons, "local_group_inactive")
	}
	if item.RouteIdentity == "changed" {
		stale = true
		item.Reasons = append(item.Reasons, "source_account_route_changed")
	} else if item.AccountID > 0 && item.RouteIdentity != "current" {
		blocked = true
		item.Reasons = append(item.Reasons, "source_account_route_unknown")
	}
	if blocked {
		item.Conclusion = service.ProcurementSourceConclusionBlocked
	} else if stale {
		item.Conclusion = service.ProcurementSourceConclusionStale
	} else {
		item.Conclusion = service.ProcurementSourceConclusionPending
		item.Reasons = append(item.Reasons, "fixed_group_selection_unverified")
	}
}
