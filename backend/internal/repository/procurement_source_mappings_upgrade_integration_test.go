//go:build integration

package repository

import (
	"testing"
	"testing/fstest"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestProcurementSourceMappingMigrationCompatibilityAndEmptyReverse(t *testing.T) {
	db, _ := UpstreamChannelsTestDatabase(t)
	group, account := seedProcurementSourceMappingRoute(t, db, "migration-preserve", "openai")
	checksum := func() string {
		var value string
		require.NoError(t, db.QueryRow(`SELECT string_agg(filename||':'||checksum,',' ORDER BY filename) FROM schema_migrations WHERE filename<>'357_procurement_source_mappings.sql'`).Scan(&value))
		return value
	}
	before := checksum()
	legacy := fstest.MapFS{}
	entries, err := migrations.FS.ReadDir(".")
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "357_procurement_source_mappings.sql" {
			continue
		}
		raw, err := migrations.FS.ReadFile(entry.Name())
		require.NoError(t, err)
		legacy[entry.Name()] = &fstest.MapFile{Data: raw, Mode: 0444}
	}
	// Old applications see their existing tables unchanged; app rollback keeps
	// the registry so its declarations need not be destroyed.
	require.NoError(t, applyMigrationsFS(t.Context(), db, legacy))
	require.NoError(t, ApplyMigrations(t.Context(), db))
	require.Equal(t, before, checksum())

	// Only this fresh, empty test registry is schema-reversed. Real rollback
	// keeps populated registry tables and their audit/provenance.
	_, err = db.Exec(`BEGIN;
 DROP TABLE procurement_supplier_group_accounts;
 DROP TABLE procurement_supplier_groups;
 DROP TABLE procurement_supplier_sites;
 DELETE FROM schema_migrations WHERE filename='357_procurement_source_mappings.sql';
 COMMIT;`)
	require.NoError(t, err)
	require.NoError(t, applyMigrationsFS(t.Context(), db, legacy))
	var member bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS(SELECT 1 FROM account_groups WHERE group_id=$1 AND account_id=$2)`, group, account).Scan(&member))
	require.True(t, member)
	require.Equal(t, before, checksum())
	require.NoError(t, ApplyMigrations(t.Context(), db))
	require.NoError(t, ApplyMigrations(t.Context(), db))
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM procurement_supplier_group_accounts`).Scan(&count))
	require.Zero(t, count)
	require.Equal(t, before, checksum())
}
