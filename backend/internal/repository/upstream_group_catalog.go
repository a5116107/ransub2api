package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.UpstreamGroupCatalogRepository = (*newAPIAuthorizationRepository)(nil)

func decodeGroupCatalog(raw []byte) (*service.UpstreamGroupCatalog, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var out service.UpstreamGroupCatalog
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Version != 1 || out.Provider != "new_api" || out.Scope != "management_user" || out.AttemptedAt.IsZero() ||
		(out.Status != "ok" && out.Status != "failed") || len(out.Groups) > 1024 || len(out.Changes) > 100 {
		return nil, service.ErrUpstreamBillingProbeUnavailable
	}
	return &out, nil
}

func (r *newAPIAuthorizationRepository) ReadNewAPIGroupCatalog(ctx context.Context, id, revision int64) (*service.UpstreamGroupCatalog, error) {
	var raw []byte
	if err := r.db.QueryRowContext(ctx, `SELECT group_catalog FROM new_api_site_authorizations WHERE id=$1 AND revision=$2`, id, revision).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrUpstreamBillingProbeIdentityChanged
		}
		return nil, err
	}
	out, err := decodeGroupCatalog(raw)
	if err != nil || out == nil {
		return out, err
	}
	if out.AuthorizationRevision != revision {
		out.Status = "authorization_changed"
	}
	return out, nil
}

func (r *newAPIAuthorizationRepository) ReadUpstreamCatalogImpact(ctx context.Context, accountID int64) (service.UpstreamCatalogImpact, error) {
	out := service.UpstreamCatalogImpact{Sources: []service.UpstreamCatalogRelated{}, Products: []service.UpstreamCatalogRelated{}}
	if err := procurementSourceMappingAccess(ctx); err != nil {
		return out, err
	}
	// These are explicit declarations and configured relationships, never a
	// claim that a particular supplier group actually served a request. Bounded
	// reads keep an administrator's catalog request independent of traffic size.
	rows, err := r.db.QueryContext(ctx, `WITH sources AS (
 SELECT m.id,g.local_group_id,s.name||' / '||COALESCE(NULLIF(g.external_group_name,''),g.external_group_id) AS name
 FROM procurement_supplier_group_accounts m
 JOIN procurement_supplier_groups g ON g.id=m.source_group_id
 JOIN procurement_supplier_sites s ON s.id=g.site_id
 WHERE m.account_id=$1
), related AS (
 SELECT 'source' AS kind,id,name FROM sources
 UNION ALL
	 SELECT DISTINCT 'product',p.id,p.name FROM groups p JOIN accounts a ON a.id=$1
	 WHERE p.deleted_at IS NULL AND p.platform=a.platform AND (
   EXISTS(SELECT 1 FROM account_groups ag WHERE ag.group_id=p.id AND ag.account_id=$1)
   OR (
     EXISTS(SELECT 1 FROM sources s WHERE p.procurement_policy->'source_group_ids' @> jsonb_build_array(s.local_group_id))
     AND (p.procurement_policy #> '{sources,account_ids}' IS NULL
       OR p.procurement_policy #> '{sources,account_ids}'='null'::jsonb
       OR p.procurement_policy #> '{sources,account_ids}' @> jsonb_build_array($1::bigint))
   )
 )
) SELECT kind,id,name FROM (
 SELECT *,row_number() OVER (PARTITION BY kind ORDER BY id) AS n FROM related
) r WHERE n<=101 ORDER BY kind,id`, accountID)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind string
		var item service.UpstreamCatalogRelated
		if err := rows.Scan(&kind, &item.ID, &item.Name); err != nil {
			return out, err
		}
		dest := &out.Products
		if kind == "source" {
			dest = &out.Sources
		}
		if len(*dest) == 100 {
			out.Truncated = true
			continue
		}
		*dest = append(*dest, item)
	}
	return out, rows.Err()
}
