//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestAccountGroupOwnershipMigration(t *testing.T) {
	ctx := t.Context()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `CREATE TEMP TABLE account_groups(account_id BIGINT,group_id BIGINT,priority INT,allowed_models JSONB,created_at TIMESTAMPTZ) ON COMMIT DROP;
 INSERT INTO account_groups VALUES(1,2,13,'["gpt-6-sol"]','2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	var before, identity string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT to_jsonb(g)::text,ctid::text||xmin::text FROM account_groups g`).Scan(&before, &identity))
	_, err = tx.ExecContext(ctx, `SAVEPOINT before_ownership`)
	require.NoError(t, err)
	raw, err := migrations.FS.ReadFile("349_account_group_ownership.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = tx.ExecContext(ctx, string(raw))
		require.NoError(t, err, "migration must be repeatable without overwriting any membership")
	}
	var after, afterIdentity string
	var manual bool
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT (to_jsonb(g)-'ownership')::text,ctid::text||xmin::text,ownership IS NULL FROM account_groups g`).Scan(&after, &afterIdentity, &manual))
	require.JSONEq(t, before, after)
	require.Equal(t, identity, afterIdentity, "adding the nullable column must not rewrite old rows")
	require.True(t, manual)
	for _, value := range []string{`null`, `{}`, `[]`, `{"version":1,"kind":"manual","revision":1}`,
		`{"version":2,"kind":"procurement","revision":1}`, `{"version":1,"kind":"procurement","revision":0}`,
		`{"version":1,"kind":"procurement","revision":null}`, `{"version":1,"kind":"procurement","revision":"1"}`,
		`{"version":1,"kind":"procurement","revision":1.1}`, `{"version":1,"kind":"procurement","revision":9007199254740992}`,
		`{"version":1,"kind":"procurement","revision":1,"future":true}`, `{"kind":"procurement","revision":1}`} {
		_, err = tx.ExecContext(ctx, `SAVEPOINT invalid_owner`)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `UPDATE account_groups SET ownership=$1::jsonb`, value)
		require.Error(t, err, value)
		_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT invalid_owner; RELEASE SAVEPOINT invalid_owner`)
		require.NoError(t, err)
	}
	for _, revision := range []int64{1, 9007199254740991} {
		_, err = tx.ExecContext(ctx, `UPDATE account_groups SET ownership=$1::jsonb`, fmt.Sprintf(`{"version":1,"kind":"procurement","revision":%d}`, revision))
		require.NoError(t, err)
	}
	_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT before_ownership`)
	require.NoError(t, err)
	var columns int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM pg_attribute WHERE attrelid='account_groups'::regclass AND attname='ownership' AND NOT attisdropped`).Scan(&columns))
	require.Zero(t, columns, "failed upgrade transaction must roll back the new column")
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT to_jsonb(g)::text,ctid::text||xmin::text FROM account_groups g`).Scan(&after, &afterIdentity))
	require.JSONEq(t, before, after)
	require.Equal(t, identity, afterIdentity)
}

func TestAccountGroupOwnershipRoundTrip(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := t.Context()
	const owner = `{"version":1,"kind":"procurement","revision":7}`
	_, err := integrationDB.ExecContext(ctx, `UPDATE account_groups SET ownership=$3::jsonb WHERE account_id=$1 AND group_id=$2`, f.account.ID, f.groups[0], owner)
	require.NoError(t, err, "membership ownership must live on the original relationship")
	read := func() string {
		t.Helper()
		var raw []byte
		require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT ownership FROM account_groups WHERE account_id=$1 AND group_id=$2`, f.account.ID, f.groups[0]).Scan(&raw))
		return string(raw)
	}
	row, err := f.repo.client.AccountGroup.Query().Where(accountgroup.AccountIDEQ(f.account.ID), accountgroup.GroupIDEQ(f.groups[0])).Only(ctx)
	require.NoError(t, err)
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.JSONEq(t, owner, string(fields["ownership"]), "ORM reads must not discard the owner")
	created := row.CreatedAt
	require.NoError(t, f.repo.BindGroups(ctx, f.account.ID, []int64{f.groups[1], f.groups[0]}))
	require.NoError(t, f.repo.SetGroupAllowedModels(ctx, f.account.ID, map[int64][]string{f.groups[0]: {"gpt-6-sol"}}))
	require.JSONEq(t, owner, read())
	row, err = f.repo.client.AccountGroup.Query().Where(accountgroup.AccountIDEQ(f.account.ID), accountgroup.GroupIDEQ(f.groups[0])).Only(ctx)
	require.NoError(t, err)
	require.True(t, row.CreatedAt.Equal(created))
	require.NoError(t, f.repo.RemoveFromGroup(ctx, f.account.ID, f.groups[0]))
	require.NoError(t, f.repo.AddToGroup(ctx, f.account.ID, f.groups[0], 13))
	require.Empty(t, read(), "a fresh manual association must not resurrect a removed program owner")
}

func TestAccountGroupOwnershipCloneAndLegacySnapshot(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := t.Context()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	legacy, err := qualityGroups(ctx, tx, f.account.ID)
	require.NoError(t, err)
	require.NotContains(t, string(legacy), "ownership", "manual quality snapshots must keep their pre-migration JSON shape")
	require.NoError(t, tx.Rollback())
	_, err = integrationDB.ExecContext(ctx, `UPDATE account_groups SET ownership='{"version":1,"kind":"procurement","revision":7}' WHERE account_id=$1 AND group_id=$2`, f.account.ID, f.groups[0])
	require.NoError(t, err)
	clone := &service.Group{Name: "ownership-copy", Platform: service.PlatformOpenAI}
	repo := newGroupRepositoryWithSQL(f.repo.client, integrationDB)
	require.NoError(t, repo.CreateFromSource(ctx, clone, f.groups[0]))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := integrationDB.ExecContext(ctx, `DELETE FROM scheduler_outbox WHERE group_id=$1;`, clone.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `DELETE FROM account_groups WHERE group_id=$1`, clone.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `DELETE FROM groups WHERE id=$1`, clone.ID)
		require.NoError(t, err)
	})
	var owner []byte
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT ownership FROM account_groups WHERE group_id=$1 AND account_id=$2`, clone.ID, f.account.ID).Scan(&owner))
	require.Empty(t, owner, "new products must not inherit another product's owner")
}

func TestAccountGroupOwnershipQualityAndAstra(t *testing.T) {
	for _, kind := range []string{"quality", "astra", "astra ownership changed"} {
		t.Run(kind, func(t *testing.T) {
			f := newMembershipFixture(t)
			ctx := t.Context()
			const owner = `{"version":1,"kind":"procurement","revision":7}`
			_, err := integrationDB.ExecContext(ctx, `UPDATE account_groups SET ownership=$3::jsonb WHERE account_id=$1 AND group_id=$2`, f.account.ID, f.groups[0], owner)
			require.NoError(t, err)
			tx, err := integrationDB.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			require.NoError(t, lockLiveAccountForGroupBindings(ctx, tx, f.account.ID))
			if kind == "quality" {
				state := &qualityState{}
				require.NoError(t, removeQualityGroups(ctx, tx, f.account.ID, f.groups[:1], state))
				encoded, err := json.Marshal(state)
				require.NoError(t, err)
				require.Contains(t, string(encoded), `"ownership"`)
				var reloaded qualityState
				require.NoError(t, json.Unmarshal(encoded, &reloaded))
				conflict, err := restoreQualityGroups(ctx, tx, f.account.ID, &reloaded)
				require.NoError(t, err)
				require.False(t, conflict)
			} else {
				groups, err := astraSchedulingGroups(ctx, tx, f.account.ID)
				require.NoError(t, err)
				state := &astraActionState{Mode: "groups", GroupIDs: f.groups[:1]}
				_, _, err = reconcileAstraGroups(ctx, tx, f.account.ID, state, groups, kind == "astra ownership changed", false)
				require.NoError(t, err)
				encoded, err := json.Marshal(state)
				require.NoError(t, err)
				var reloaded astraActionState
				require.NoError(t, json.Unmarshal(encoded, &reloaded))
				if kind == "astra ownership changed" {
					_, err = tx.ExecContext(ctx, `UPDATE account_groups SET ownership=jsonb_set(ownership,'{revision}','8') WHERE account_id=$1 AND group_id=$2`, f.account.ID, f.groups[0])
					require.NoError(t, err)
				}
				groups, err = astraSchedulingGroups(ctx, tx, f.account.ID)
				require.NoError(t, err)
				_, _, err = reconcileAstraGroups(ctx, tx, f.account.ID, &reloaded, groups, kind == "astra", false)
				if kind == "astra ownership changed" {
					require.EqualError(t, err, "astra_restore_conflict", "same priority/time/models do not confer the new owner's membership")
					return
				}
				require.NoError(t, err)
			}
			var restored []byte
			require.NoError(t, tx.QueryRowContext(ctx, `SELECT ownership FROM account_groups WHERE account_id=$1 AND group_id=$2`, f.account.ID, f.groups[0]).Scan(&restored))
			require.JSONEq(t, owner, string(restored))
			require.NoError(t, tx.Commit())
		})
	}
}
