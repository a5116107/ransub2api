package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func procurementSourceMappingAccess(ctx context.Context) error {
	if _, scoped := service.ObserverGroupIDs(ctx); scoped || adminaccess.FromContext(ctx) != nil {
		return service.ErrObserverScope
	}
	return nil
}

func (r *channelRepository) CreateProcurementSourceMapping(ctx context.Context, command domain.ProcurementSourceMappingCommand) (*service.ProcurementSourceMapping, error) {
	if err := procurementSourceMappingAccess(ctx); err != nil {
		return nil, err
	}
	if err := command.Validate(); err != nil {
		return nil, service.ErrProcurementSourceMappingInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO procurement_supplier_sites(code,name) VALUES($1,$2) ON CONFLICT(code) DO NOTHING`, command.SiteCode, command.SiteName)
	if err != nil {
		return nil, err
	}
	var siteID int64
	var siteName string
	if err := tx.QueryRowContext(ctx, `SELECT id,name FROM procurement_supplier_sites WHERE code=$1 FOR UPDATE`, command.SiteCode).Scan(&siteID, &siteName); err != nil {
		return nil, err
	}
	if siteName != command.SiteName {
		return nil, service.ErrProcurementSourceMappingConflict
	}
	// AccountRouteFingerprint takes the account/parent row locks once. Taking a
	// shared account lock first and upgrading it later deadlocks two suppliers
	// trying to claim the same account concurrently.
	routeFingerprint, err := accountRouteFingerprint(ctx, tx, command.AccountID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrProcurementSourceMappingInvalid
	}
	if err != nil {
		return nil, err
	}
	var accountPlatform, groupPlatform string
	err = tx.QueryRowContext(ctx, `SELECT a.platform,g.platform
 FROM accounts a JOIN groups g ON g.id=$2 JOIN account_groups ag ON ag.account_id=a.id AND ag.group_id=g.id
 WHERE a.id=$1 AND a.deleted_at IS NULL AND g.deleted_at IS NULL
		FOR SHARE OF g,ag`, command.AccountID, command.LocalGroupID).Scan(&accountPlatform, &groupPlatform)
	if err == sql.ErrNoRows {
		return nil, service.ErrProcurementSourceMappingInvalid
	}
	if err != nil {
		return nil, err
	}
	if accountPlatform != command.Platform || groupPlatform != command.Platform {
		return nil, service.ErrProcurementSourceMappingInvalid
	}
	var groupID int64
	var externalName string
	var localGroupID int64
	err = tx.QueryRowContext(ctx, `SELECT id,external_group_name,local_group_id FROM procurement_supplier_groups
 WHERE site_id=$1 AND platform=$2 AND external_group_id=$3 FOR UPDATE`, siteID, command.Platform, command.ExternalGroupID).Scan(&groupID, &externalName, &localGroupID)
	switch {
	case err == sql.ErrNoRows:
		err = tx.QueryRowContext(ctx, `INSERT INTO procurement_supplier_groups(site_id,platform,external_group_id,external_group_name,local_group_id)
 VALUES($1,$2,$3,$4,$5) RETURNING id`, siteID, command.Platform, command.ExternalGroupID, command.ExternalGroupName, command.LocalGroupID).Scan(&groupID)
	case err == nil:
		if externalName != command.ExternalGroupName || localGroupID != command.LocalGroupID {
			return nil, service.ErrProcurementSourceMappingConflict
		}
	default:
		return nil, err
	}
	if err != nil {
		if isUniqueViolation(err) {
			return nil, service.ErrProcurementSourceMappingConflict
		}
		return nil, err
	}
	enabled := true
	if command.Enabled != nil {
		enabled = *command.Enabled
	}
	var routeID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO procurement_supplier_group_accounts(source_group_id,site_id,platform,account_id,route_fingerprint,enabled)
 VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, groupID, siteID, command.Platform, command.AccountID, routeFingerprint[:], enabled).Scan(&routeID)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, service.ErrProcurementSourceMappingConflict
		}
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE procurement_supplier_groups SET revision=revision+1,updated_at=CURRENT_TIMESTAMP WHERE id=$1`, groupID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE procurement_supplier_sites SET revision=revision+1,updated_at=CURRENT_TIMESTAMP WHERE id=$1`, siteID); err != nil {
		return nil, err
	}
	out, err := readProcurementSourceMappingInTx(ctx, tx, routeID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *channelRepository) SetProcurementSourceMappingEnabled(ctx context.Context, id, expectedRevision int64, enabled, confirm bool) (*service.ProcurementSourceMapping, error) {
	if err := procurementSourceMappingAccess(ctx); err != nil {
		return nil, err
	}
	if !validProcurementMappingMutation(id, expectedRevision, confirm) {
		return nil, service.ErrProcurementSourceMappingInvalid
	}
	var out *service.ProcurementSourceMapping
	err := r.runInTx(ctx, func(tx *sql.Tx) error {
		if _, err := lockProcurementSourceMappingSite(ctx, tx, id); err != nil {
			return err
		}
		var groupID int64
		err := tx.QueryRowContext(ctx, `UPDATE procurement_supplier_group_accounts SET enabled=$3,revision=revision+1,updated_at=CURRENT_TIMESTAMP
 WHERE id=$1 AND revision=$2 RETURNING source_group_id`, id, expectedRevision, enabled).Scan(&groupID)
		if err == sql.ErrNoRows {
			var exists bool
			if checkErr := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM procurement_supplier_group_accounts WHERE id=$1)`, id).Scan(&exists); checkErr != nil {
				return checkErr
			}
			if exists {
				return service.ErrProcurementSourceMappingConflict
			}
			return service.ErrProcurementSourceMappingNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE procurement_supplier_groups SET revision=revision+1,updated_at=CURRENT_TIMESTAMP WHERE id=$1`, groupID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE procurement_supplier_sites SET revision=revision+1,updated_at=CURRENT_TIMESTAMP WHERE id=(SELECT site_id FROM procurement_supplier_groups WHERE id=$1)`, groupID)
		if err != nil {
			return err
		}
		out, err = readProcurementSourceMappingInTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r *channelRepository) DeleteProcurementSourceMapping(ctx context.Context, id, expectedRevision int64, confirm bool) error {
	if err := procurementSourceMappingAccess(ctx); err != nil {
		return err
	}
	if !validProcurementMappingMutation(id, expectedRevision, confirm) {
		return service.ErrProcurementSourceMappingInvalid
	}
	return r.runInTx(ctx, func(tx *sql.Tx) error {
		if _, err := lockProcurementSourceMappingSite(ctx, tx, id); err != nil {
			return err
		}
		var groupID int64
		err := tx.QueryRowContext(ctx, `DELETE FROM procurement_supplier_group_accounts WHERE id=$1 AND revision=$2 RETURNING source_group_id`, id, expectedRevision).Scan(&groupID)
		if err == sql.ErrNoRows {
			var exists bool
			if checkErr := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM procurement_supplier_group_accounts WHERE id=$1)`, id).Scan(&exists); checkErr != nil {
				return checkErr
			}
			if exists {
				return service.ErrProcurementSourceMappingConflict
			}
			return service.ErrProcurementSourceMappingNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE procurement_supplier_groups SET revision=revision+1,updated_at=CURRENT_TIMESTAMP WHERE id=$1`, groupID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE procurement_supplier_sites SET revision=revision+1,updated_at=CURRENT_TIMESTAMP WHERE id=(SELECT site_id FROM procurement_supplier_groups WHERE id=$1)`, groupID)
		return err
	})
}

func (r *channelRepository) SetProcurementSourceGroupEnabled(ctx context.Context, id, expectedRevision int64, enabled, confirm bool) (*service.ProcurementSourceGroupState, error) {
	if err := procurementSourceMappingAccess(ctx); err != nil {
		return nil, err
	}
	if !validProcurementMappingMutation(id, expectedRevision, confirm) {
		return nil, service.ErrProcurementSourceMappingInvalid
	}
	state := &service.ProcurementSourceGroupState{ID: id}
	err := r.runInTx(ctx, func(tx *sql.Tx) error {
		if _, err := lockProcurementSourceGroupSite(ctx, tx, id); err != nil {
			return err
		}
		var siteID int64
		err := tx.QueryRowContext(ctx, `UPDATE procurement_supplier_groups SET enabled=$3,revision=revision+1,updated_at=CURRENT_TIMESTAMP
 WHERE id=$1 AND revision=$2 RETURNING site_id,enabled,revision`, id, expectedRevision, enabled).Scan(&siteID, &state.Enabled, &state.Revision)
		if err == sql.ErrNoRows {
			var exists bool
			if checkErr := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM procurement_supplier_groups WHERE id=$1)`, id).Scan(&exists); checkErr != nil {
				return checkErr
			}
			if exists {
				return service.ErrProcurementSourceMappingConflict
			}
			return service.ErrProcurementSourceMappingNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE procurement_supplier_sites SET revision=revision+1,updated_at=CURRENT_TIMESTAMP WHERE id=$1`, siteID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (r *channelRepository) RenameProcurementSourceSite(ctx context.Context, id int64, command domain.ProcurementSupplierSiteRenameCommand) (*service.ProcurementSourceSiteState, error) {
	if err := procurementSourceMappingAccess(ctx); err != nil {
		return nil, err
	}
	if id < 1 || id > domain.MaxProcurementPolicyRevision || command.Validate() != nil {
		return nil, service.ErrProcurementSourceMappingInvalid
	}
	state := &service.ProcurementSourceSiteState{ID: id}
	err := r.runInTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `UPDATE procurement_supplier_sites SET name=$3,revision=revision+1,updated_at=CURRENT_TIMESTAMP
 WHERE id=$1 AND revision=$2 RETURNING code,name,revision`, id, command.ExpectedRevision, command.Name).Scan(&state.Code, &state.Name, &state.Revision)
		if err == sql.ErrNoRows {
			var exists bool
			if checkErr := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM procurement_supplier_sites WHERE id=$1)`, id).Scan(&exists); checkErr != nil {
				return checkErr
			}
			if exists {
				return service.ErrProcurementSourceMappingConflict
			}
			return service.ErrProcurementSourceMappingNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (r *channelRepository) DeleteProcurementSourceSite(ctx context.Context, id, expectedRevision int64, confirm bool) error {
	if err := procurementSourceMappingAccess(ctx); err != nil {
		return err
	}
	if !validProcurementMappingMutation(id, expectedRevision, confirm) {
		return service.ErrProcurementSourceMappingInvalid
	}
	return r.runInTx(ctx, func(tx *sql.Tx) error {
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM procurement_supplier_sites WHERE id=$1 FOR UPDATE`, id).Scan(&revision); err != nil {
			if err == sql.ErrNoRows {
				return service.ErrProcurementSourceMappingNotFound
			}
			return err
		}
		if revision != expectedRevision {
			return service.ErrProcurementSourceMappingConflict
		}
		var mappingCount int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM procurement_supplier_groups WHERE site_id=$1`, id).Scan(&mappingCount); err != nil {
			return err
		}
		if mappingCount != 0 {
			return service.ErrProcurementSourceMappingConflict
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM procurement_supplier_sites WHERE id=$1 AND revision=$2`, id, revision)
		return err
	})
}

func (r *channelRepository) DeleteProcurementSourceGroup(ctx context.Context, id, expectedRevision int64, confirm bool) error {
	if err := procurementSourceMappingAccess(ctx); err != nil {
		return err
	}
	if !validProcurementMappingMutation(id, expectedRevision, confirm) {
		return service.ErrProcurementSourceMappingInvalid
	}
	return r.runInTx(ctx, func(tx *sql.Tx) error {
		if _, err := lockProcurementSourceGroupSite(ctx, tx, id); err != nil {
			return err
		}
		var siteID int64
		var mappingCount int
		err := tx.QueryRowContext(ctx, `SELECT site_id,(SELECT count(*) FROM procurement_supplier_group_accounts WHERE source_group_id=procurement_supplier_groups.id)
 FROM procurement_supplier_groups WHERE id=$1 AND revision=$2 FOR UPDATE`, id, expectedRevision).Scan(&siteID, &mappingCount)
		if err == sql.ErrNoRows {
			var exists bool
			if checkErr := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM procurement_supplier_groups WHERE id=$1)`, id).Scan(&exists); checkErr != nil {
				return checkErr
			}
			if exists {
				return service.ErrProcurementSourceMappingConflict
			}
			return service.ErrProcurementSourceMappingNotFound
		}
		if err != nil {
			return err
		}
		if mappingCount != 0 {
			return service.ErrProcurementSourceMappingConflict
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM procurement_supplier_groups WHERE id=$1 AND revision=$2`, id, expectedRevision); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE procurement_supplier_sites SET revision=revision+1,updated_at=CURRENT_TIMESTAMP WHERE id=$1`, siteID)
		return err
	})
}

func validProcurementMappingMutation(id, revision int64, confirm bool) bool {
	return id > 0 && id <= domain.MaxProcurementPolicyRevision && revision > 0 && revision < domain.MaxProcurementPolicyRevision && confirm
}

// Every registry writer takes the site lock before any child row. Reading the
// parent in a join locks only s: child rows are rechecked by the CAS statement
// after waiting, so a concurrent deletion cannot resurrect a stale relation.
func lockProcurementSourceMappingSite(ctx context.Context, tx *sql.Tx, id int64) (int64, error) {
	var siteID int64
	err := tx.QueryRowContext(ctx, `SELECT s.id FROM procurement_supplier_sites s
 JOIN procurement_supplier_group_accounts m ON m.site_id=s.id WHERE m.id=$1 FOR UPDATE OF s`, id).Scan(&siteID)
	if err == sql.ErrNoRows {
		err = service.ErrProcurementSourceMappingNotFound
	}
	return siteID, err
}

func lockProcurementSourceGroupSite(ctx context.Context, tx *sql.Tx, id int64) (int64, error) {
	var siteID int64
	err := tx.QueryRowContext(ctx, `SELECT s.id FROM procurement_supplier_sites s
 JOIN procurement_supplier_groups g ON g.site_id=s.id WHERE g.id=$1 FOR UPDATE OF s`, id).Scan(&siteID)
	if err == sql.ErrNoRows {
		err = service.ErrProcurementSourceMappingNotFound
	}
	return siteID, err
}
