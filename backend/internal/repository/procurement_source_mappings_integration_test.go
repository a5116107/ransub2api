//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestProcurementSourceMappingsRequireExplicitCurrentRouteAndNeverAuthorizeDispatch(t *testing.T) {
	ctx := context.Background()
	db, client := UpstreamChannelsTestDatabase(t)
	defer func() { _ = db.Close(); _ = client.Close() }()
	repo := NewChannelRepository(db).(service.ProcurementSourceMappingRepository)

	groupA, accountA := seedProcurementSourceMappingRoute(t, db, "source-map-a", "openai")
	groupB, accountB := seedProcurementSourceMappingRoute(t, db, "source-map-b", "openai")
	externalID := strconv.FormatInt(groupA, 10)
	first, err := repo.CreateProcurementSourceMapping(ctx, domain.ProcurementSourceMappingCommand{
		Version: 1, SiteCode: "supplier-a", SiteName: "Supplier A", Platform: "openai",
		ExternalGroupID: externalID, ExternalGroupName: "Tier A", LocalGroupID: groupA, AccountID: accountA, Confirm: true,
	})
	require.NoError(t, err)
	require.Equal(t, externalID, first.ExternalGroupID)
	require.Equal(t, groupA, first.LocalGroupID, "equal numeric IDs remain separate explicit fields")
	require.Equal(t, service.ProcurementSourceConclusionPending, first.Conclusion)
	require.Equal(t, "current", first.RouteIdentity)
	require.Equal(t, "declared", first.SourceIdentity)
	require.Equal(t, "unverified", first.FixedGroupSelection)
	require.False(t, first.Dispatchable)

	// The same external ID at another site is a distinct namespace and can only
	// be attached through an explicit local account/group relation.
	second, err := repo.CreateProcurementSourceMapping(ctx, domain.ProcurementSourceMappingCommand{
		Version: 1, SiteCode: "supplier-b", SiteName: "Supplier B", Platform: "openai",
		ExternalGroupID: externalID, ExternalGroupName: "Tier B", LocalGroupID: groupB, AccountID: accountB, Confirm: true,
	})
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)
	require.Equal(t, groupB, second.LocalGroupID)

	// An account cannot silently claim a second supplier site/group route.
	_, err = repo.CreateProcurementSourceMapping(ctx, domain.ProcurementSourceMappingCommand{
		Version: 1, SiteCode: "supplier-c", SiteName: "Supplier C", Platform: "openai",
		ExternalGroupID: "other", LocalGroupID: groupA, AccountID: accountA, Confirm: true,
	})
	require.ErrorIs(t, err, service.ErrProcurementSourceMappingConflict)

	// A changed endpoint invalidates the identity snapshot, while no map result
	// becomes dispatchable merely because the account route still exists.
	_, err = db.ExecContext(ctx, `UPDATE accounts SET credentials=jsonb_set(credentials,'{base_url}','"https://changed.example/v1"'::jsonb) WHERE id=$1`, accountA)
	require.NoError(t, err)
	page, err := repo.ListProcurementSourceMappings(ctx, "supplier-a", "openai", 1, 10)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, service.ProcurementSourceConclusionStale, page.Items[0].Conclusion)
	require.Contains(t, page.Items[0].Reasons, "source_account_route_changed")
	require.False(t, page.Items[0].Dispatchable)

	// Pausing the external group is a versioned, explicit state change.
	paused, err := repo.SetProcurementSourceGroupEnabled(ctx, first.SourceGroupID, first.SourceGroupRevision, false, true)
	require.NoError(t, err)
	require.False(t, paused.Enabled)
	page, err = repo.ListProcurementSourceMappings(ctx, "supplier-a", "openai", 1, 10)
	require.NoError(t, err)
	require.Equal(t, service.ProcurementSourceConclusionPaused, page.Items[0].Conclusion)

	// Deleting a mapping removes only the declaration, not the account or its
	// existing local Group membership.
	require.NoError(t, repo.DeleteProcurementSourceMapping(ctx, first.ID, first.Revision, true))
	var stillExists, stillMember bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1),EXISTS(SELECT 1 FROM account_groups WHERE account_id=$1 AND group_id=$2)`, accountA, groupA).Scan(&stillExists, &stillMember))
	require.True(t, stillExists)
	require.True(t, stillMember)
}

func TestProcurementSourceMappingWritersLockSiteBeforeChildren(t *testing.T) {
	db, _ := UpstreamChannelsTestDatabase(t)
	repo := NewChannelRepository(db).(service.ProcurementSourceMappingRepository)
	group, account := seedProcurementSourceMappingRoute(t, db, "lock-order", "openai")
	mapping, err := repo.CreateProcurementSourceMapping(t.Context(), domain.ProcurementSourceMappingCommand{
		Version: 1, SiteCode: "lock-order", SiteName: "Lock order", Platform: "openai",
		ExternalGroupID: "external-1", LocalGroupID: group, AccountID: account, Confirm: true,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	holder, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback() }()
	var id int64
	require.NoError(t, holder.QueryRowContext(ctx, `SELECT id FROM procurement_supplier_sites WHERE id=$1 FOR UPDATE`, mapping.SourceSiteID).Scan(&id))
	finished := make(chan error, 1)
	go func() {
		_, err := repo.SetProcurementSourceMappingEnabled(ctx, mapping.ID, mapping.Revision, false, true)
		finished <- err
	}()
	// The mutation must wait on the parent site without already holding a child
	// row lock. Otherwise create (site -> child) can deadlock with this writer.
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event='transactionid')`).Scan(&waiting)
		return err == nil && waiting
	}, 3*time.Second, 20*time.Millisecond)
	err = holder.QueryRowContext(ctx, `SELECT id FROM procurement_supplier_group_accounts WHERE id=$1 FOR UPDATE NOWAIT`, mapping.ID).Scan(&id)
	_ = holder.Rollback()
	require.NoError(t, <-finished)
	require.NoError(t, err, "child must remain unlocked while the writer waits for its parent")
}

func seedProcurementSourceMappingRoute(t *testing.T, db *sql.DB, name, platform string) (int64, int64) {
	t.Helper()
	var groupID, accountID int64
	err := db.QueryRow(`INSERT INTO groups(name,platform,status) VALUES($1,$2,'active') RETURNING id`, name+"-group", platform).Scan(&groupID)
	require.NoError(t, err)
	credentials, err := json.Marshal(map[string]string{"api_key": "fixture-" + name, "base_url": "https://" + name + ".example/v1"})
	require.NoError(t, err)
	err = db.QueryRow(`INSERT INTO accounts(name,platform,type,credentials,status,schedulable) VALUES($1,$2,'apikey',$3::jsonb,'active',true) RETURNING id`, name+"-account", platform, string(credentials)).Scan(&accountID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, accountID, groupID)
	require.NoError(t, err)
	return groupID, accountID
}

func TestProcurementSourceMappingCreateRejectsPlatformAndMembershipMismatch(t *testing.T) {
	ctx := context.Background()
	db, client := UpstreamChannelsTestDatabase(t)
	defer func() { _ = db.Close(); _ = client.Close() }()
	repo := NewChannelRepository(db).(service.ProcurementSourceMappingRepository)
	groupID, accountID := seedProcurementSourceMappingRoute(t, db, "source-map-negative", "openai")
	_, err := repo.CreateProcurementSourceMapping(ctx, domain.ProcurementSourceMappingCommand{
		Version: 1, SiteCode: "supplier-negative", SiteName: "Supplier Negative", Platform: "anthropic",
		ExternalGroupID: "group-9", LocalGroupID: groupID, AccountID: accountID, Confirm: true,
	})
	require.ErrorIs(t, err, service.ErrProcurementSourceMappingInvalid)
	_, err = db.ExecContext(ctx, `DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, accountID, groupID)
	require.NoError(t, err)
	_, err = repo.CreateProcurementSourceMapping(ctx, domain.ProcurementSourceMappingCommand{
		Version: 1, SiteCode: "supplier-negative", SiteName: "Supplier Negative", Platform: "openai",
		ExternalGroupID: "group-9", LocalGroupID: groupID, AccountID: accountID, Confirm: true,
	})
	require.ErrorIs(t, err, service.ErrProcurementSourceMappingInvalid)
}

func TestProcurementSourceMappingInventoryIsPagedAndSearchable(t *testing.T) {
	ctx := context.Background()
	db, client := UpstreamChannelsTestDatabase(t)
	defer func() { _ = db.Close(); _ = client.Close() }()
	repo := NewChannelRepository(db).(service.ProcurementSourceMappingRepository)
	for i := range 100 {
		groupID, accountID := seedProcurementSourceMappingRoute(t, db, fmt.Sprintf("source-map-page-%d", i), "openai")
		_, err := repo.CreateProcurementSourceMapping(ctx, domain.ProcurementSourceMappingCommand{
			Version: 1, SiteCode: fmt.Sprintf("supplier-page-%d", i), SiteName: fmt.Sprintf("Supplier Page %d", i), Platform: "openai",
			ExternalGroupID: fmt.Sprintf("external-%05d", i), LocalGroupID: groupID, AccountID: accountID, Confirm: true,
		})
		require.NoError(t, err)
	}
	page, err := repo.ListProcurementSourceMappings(ctx, "supplier-page", "openai", 1, 2)
	require.NoError(t, err)
	require.EqualValues(t, 100, page.Total)
	require.Len(t, page.Items, 2)
	page, err = repo.ListProcurementSourceMappings(ctx, "external-00042", "openai", 1, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, page.Total)
	require.Equal(t, "external-00042", page.Items[0].ExternalGroupID)
	last, err := repo.ListProcurementSourceMappings(ctx, "supplier-page", "openai", 5, 20)
	require.NoError(t, err)
	require.Len(t, last.Items, 20)
	for _, item := range last.Items {
		require.False(t, item.Dispatchable)
	}
	_, err = repo.ListProcurementSourceMappings(ctx, "", "", 1, 101)
	require.ErrorIs(t, err, service.ErrProcurementSourceMappingInvalid)
}

func TestProcurementSourceMappingRouteAndMembershipDriftStaysUnverified(t *testing.T) {
	db, _ := UpstreamChannelsTestDatabase(t)
	repo := NewChannelRepository(db).(service.ProcurementSourceMappingRepository)
	group, account := seedProcurementSourceMappingRoute(t, db, "drift", "openai")
	mapping, err := repo.CreateProcurementSourceMapping(t.Context(), domain.ProcurementSourceMappingCommand{
		Version: 1, SiteCode: "drift", SiteName: "Drift", Platform: "openai", ExternalGroupID: "fixed-1", LocalGroupID: group, AccountID: account, Confirm: true,
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE accounts SET credentials=jsonb_set(credentials,'{api_key}','"rotated-private-key"'::jsonb) WHERE id=$1`, account)
	require.NoError(t, err)
	changed, err := repo.SetProcurementSourceMappingEnabled(t.Context(), mapping.ID, mapping.Revision, true, true)
	require.NoError(t, err)
	require.Equal(t, service.ProcurementSourceConclusionStale, changed.Conclusion, "resuming a declaration does not renew its route evidence")
	_, err = db.Exec(`DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, account, group)
	require.NoError(t, err)
	page, err := repo.ListProcurementSourceMappings(t.Context(), "drift", "", 1, 20)
	require.NoError(t, err)
	require.Contains(t, page.Items[0].Reasons, "source_account_not_in_local_group")
	_, err = db.Exec(`UPDATE accounts SET deleted_at=NOW() WHERE id=$1`, account)
	require.NoError(t, err)
	page, err = repo.ListProcurementSourceMappings(t.Context(), "drift", "", 1, 20)
	require.NoError(t, err)
	require.Contains(t, page.Items[0].Reasons, "source_account_deleted")
	require.Equal(t, "unknown", page.Items[0].RouteIdentity)
}

func TestProcurementSourceMappingFailedInsertRollsBackEntireDeclaration(t *testing.T) {
	db, _ := UpstreamChannelsTestDatabase(t)
	repo := NewChannelRepository(db).(service.ProcurementSourceMappingRepository)
	group, account := seedProcurementSourceMappingRoute(t, db, "rollback", "openai")
	_, err := db.Exec(`CREATE FUNCTION source_mapping_fixture_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture insert failure'; END $$;
 CREATE TRIGGER source_mapping_fixture_fail BEFORE INSERT ON procurement_supplier_group_accounts FOR EACH ROW EXECUTE FUNCTION source_mapping_fixture_fail()`)
	require.NoError(t, err)
	command := domain.ProcurementSourceMappingCommand{Version: 1, SiteCode: "rollback", SiteName: "Rollback", Platform: "openai", ExternalGroupID: "fixed-1", LocalGroupID: group, AccountID: account, Confirm: true}
	_, err = repo.CreateProcurementSourceMapping(t.Context(), command)
	require.Error(t, err)
	var sites, groups, mappings int
	require.NoError(t, db.QueryRow(`SELECT (SELECT count(*) FROM procurement_supplier_sites),(SELECT count(*) FROM procurement_supplier_groups),(SELECT count(*) FROM procurement_supplier_group_accounts)`).Scan(&sites, &groups, &mappings))
	require.Zero(t, sites)
	require.Zero(t, groups)
	require.Zero(t, mappings)
	_, err = db.Exec(`DROP TRIGGER source_mapping_fixture_fail ON procurement_supplier_group_accounts; DROP FUNCTION source_mapping_fixture_fail()`)
	require.NoError(t, err)
	created, err := repo.CreateProcurementSourceMapping(t.Context(), command)
	require.NoError(t, err)
	require.False(t, created.Dispatchable)
}
