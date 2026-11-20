//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type membershipFixture struct {
	repo    *accountRepository
	account *service.Account
	groups  []int64
}

func newMembershipFixture(t *testing.T) membershipFixture {
	t.Helper()
	client := testEntClient(t)
	account := mustCreateAccount(t, client, &service.Account{Name: "membership-transaction", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 7})
	f := membershipFixture{repo: newAccountRepositoryWithSQL(client, integrationDB, nil), account: account}
	for range 3 {
		group := mustCreateGroup(t, client, &service.Group{Name: fmt.Sprintf("membership-%d-%d", account.ID, len(f.groups)), Platform: service.PlatformOpenAI})
		f.groups = append(f.groups, group.ID)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM account_groups WHERE account_id=$1", account.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM scheduler_outbox WHERE account_id=$1 OR group_id IN ($2,$3,$4)", account.ID, f.groups[0], f.groups[1], f.groups[2])
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM accounts WHERE id=$1", account.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id IN ($1,$2,$3)", f.groups[0], f.groups[1], f.groups[2])
		require.NoError(t, err)
	})
	require.NoError(t, f.repo.BindGroups(t.Context(), account.ID, f.groups[:2]))
	require.NoError(t, f.repo.SetGroupAllowedModels(t.Context(), account.ID, map[int64][]string{f.groups[0]: {"gpt-6-sol"}, f.groups[1]: {"gpt-6-*"}}))
	return f
}

func (f membershipFixture) rows(t *testing.T, client *dbent.Client) []*dbent.AccountGroup {
	t.Helper()
	rows, err := client.AccountGroup.Query().Where(accountgroup.AccountIDEQ(f.account.ID)).Order(accountgroup.ByGroupID()).All(t.Context())
	require.NoError(t, err)
	return rows
}

func (f membershipFixture) events(t *testing.T) int {
	t.Helper()
	var count int
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT count(*) FROM scheduler_outbox WHERE
	 (account_id=$1 AND event_type='account_groups_changed') OR (group_id IN ($2,$3,$4) AND event_type='group_changed')`, f.account.ID, f.groups[0], f.groups[1], f.groups[2]).Scan(&count))
	return count
}

func sameMembershipRows(t *testing.T, before, after []*dbent.AccountGroup) {
	t.Helper()
	require.Len(t, after, len(before))
	for i, row := range before {
		require.Equal(t, row.AccountID, after[i].AccountID)
		require.Equal(t, row.GroupID, after[i].GroupID)
		require.Equal(t, row.Priority, after[i].Priority)
		require.Equal(t, row.AllowedModels, after[i].AllowedModels)
		require.Equal(t, row.Ownership, after[i].Ownership)
		require.True(t, row.CreatedAt.Equal(after[i].CreatedAt), "membership identity changed for group %d", row.GroupID)
	}
}

// Fail only this fixture's notification. Other accounts/groups remain usable.
func (f membershipFixture) rejectOutbox(t *testing.T) {
	t.Helper()
	name := fmt.Sprintf("reject_membership_%d", f.account.ID)
	_, err := integrationDB.ExecContext(t.Context(), fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN RAISE EXCEPTION 'fixture membership outbox rejected'; END $$;
	CREATE TRIGGER %s BEFORE INSERT ON scheduler_outbox FOR EACH ROW
	WHEN ((NEW.event_type='account_groups_changed' AND NEW.account_id=%d) OR (NEW.event_type='group_changed' AND NEW.group_id=%d))
	EXECUTE FUNCTION %s()`, name, name, f.account.ID, f.groups[2], name))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), fmt.Sprintf("DROP TRIGGER %s ON scheduler_outbox; DROP FUNCTION %s()", name, name))
		require.NoError(t, err)
	})
}

func TestAccountGroupBindingsNotificationFailureRollsBack(t *testing.T) {
	for _, action := range []string{"add", "remove", "models", "replace", "bulk"} {
		t.Run(action, func(t *testing.T) {
			f := newMembershipFixture(t)
			before, events := f.rows(t, integrationEntClient), f.events(t)
			cache := &schedulerCacheRecorder{}
			f.repo.schedulerCache = cache
			f.rejectOutbox(t)
			var err error
			switch action {
			case "add":
				err = f.repo.AddToGroup(t.Context(), f.account.ID, f.groups[2], 9)
			case "remove":
				err = f.repo.RemoveFromGroup(t.Context(), f.account.ID, f.groups[0])
			case "models":
				err = f.repo.SetGroupAllowedModels(t.Context(), f.account.ID, map[int64][]string{f.groups[0]: {"gpt-6-astra"}})
			case "replace":
				err = f.repo.BindGroups(t.Context(), f.account.ID, []int64{f.groups[1], f.groups[2]})
			case "bulk":
				err = newGroupRepositoryWithSQL(integrationEntClient, integrationDB).BindAccountsToGroup(t.Context(), f.groups[2], []int64{f.account.ID})
			}
			require.ErrorContains(t, err, "fixture membership outbox rejected")
			sameMembershipRows(t, before, f.rows(t, integrationEntClient))
			require.Equal(t, events, f.events(t), "failed mutation must not publish a notification")
			require.Empty(t, cache.setAccounts, "failed commit must not publish an account cache")
		})
	}
}

func TestAccountGroupBindingsCallerTransactionAndCache(t *testing.T) {
	for _, action := range []string{"add", "remove", "models", "replace", "bulk"} {
		for _, commit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/commit=%t", action, commit), func(t *testing.T) {
				f := newMembershipFixture(t)
				before, events := f.rows(t, integrationEntClient), f.events(t)
				cache := &schedulerCacheRecorder{}
				f.repo.schedulerCache = cache
				tx, err := integrationEntClient.Tx(t.Context())
				require.NoError(t, err)
				defer func() { _ = tx.Rollback() }()
				ctx := dbent.NewTxContext(t.Context(), tx)
				switch action {
				case "add":
					err = f.repo.AddToGroup(ctx, f.account.ID, f.groups[2], 9)
				case "remove":
					err = f.repo.RemoveFromGroup(ctx, f.account.ID, f.groups[0])
				case "models":
					err = f.repo.SetGroupAllowedModels(ctx, f.account.ID, map[int64][]string{f.groups[0]: {"gpt-6-astra"}})
				case "replace":
					err = f.repo.BindGroups(ctx, f.account.ID, []int64{f.groups[1], f.groups[2]})
				case "bulk":
					err = newGroupRepositoryWithSQL(integrationEntClient, integrationDB).BindAccountsToGroup(ctx, f.groups[2], []int64{f.account.ID})
				}
				require.NoError(t, err)
				sameMembershipRows(t, before, f.rows(t, integrationEntClient))
				require.Equal(t, events, f.events(t), "caller has not committed")
				require.Empty(t, cache.setAccounts)
				after := f.rows(t, tx.Client())
				if commit {
					require.NoError(t, tx.Commit())
					sameMembershipRows(t, after, f.rows(t, integrationEntClient))
					require.Greater(t, f.events(t), events)
				} else {
					require.NoError(t, tx.Rollback())
					sameMembershipRows(t, before, f.rows(t, integrationEntClient))
					require.Equal(t, events, f.events(t))
				}
				require.Empty(t, cache.setAccounts, "caller transaction relies on committed outbox, not premature cache writes")
			})
		}
	}
}

func TestAccountGroupBindingsRemoveOnlyCurrentAndReadd(t *testing.T) {
	f := newMembershipFixture(t)
	before := f.rows(t, integrationEntClient)
	original, err := f.repo.GetByID(t.Context(), f.account.ID)
	require.NoError(t, err)
	require.NoError(t, f.repo.RemoveFromGroup(t.Context(), f.account.ID, f.groups[0]))
	sameMembershipRows(t, before[1:], f.rows(t, integrationEntClient))
	events := f.events(t)
	require.NoError(t, f.repo.RemoveFromGroup(t.Context(), f.account.ID, f.groups[0]))
	require.Equal(t, events, f.events(t), "repeated removal is an idempotent no-op")
	require.NoError(t, f.repo.AddToGroup(t.Context(), f.account.ID, f.groups[0], 4))
	after := f.rows(t, integrationEntClient)
	require.Len(t, after, 2)
	require.Equal(t, f.groups[0], after[0].GroupID)
	require.Equal(t, 4, after[0].Priority)
	require.Empty(t, after[0].AllowedModels, "a genuinely new association does not restore stale restrictions")
	sameMembershipRows(t, before[1:], after[1:])
	account, err := f.repo.GetByID(t.Context(), f.account.ID)
	require.NoError(t, err)
	require.True(t, account.Schedulable)
	require.Equal(t, 7, account.Concurrency)
	require.Equal(t, service.StatusActive, account.Status)
	require.Equal(t, original.Extra, account.Extra, "normal removal adds no hidden exclusion or administrative override")
}

func TestAccountGroupBindingsBulkNoopAndMissingAuthority(t *testing.T) {
	f := newMembershipFixture(t)
	groups := newGroupRepositoryWithSQL(integrationEntClient, integrationDB)
	require.NoError(t, groups.BindAccountsToGroup(t.Context(), f.groups[2], []int64{f.account.ID, f.account.ID}))
	before, events := f.rows(t, integrationEntClient), f.events(t)
	f.rejectOutbox(t)
	require.NoError(t, groups.BindAccountsToGroup(t.Context(), f.groups[2], []int64{f.account.ID, f.account.ID}), "already-bound accounts cause no write or notification")
	require.Equal(t, events, f.events(t))
	sameMembershipRows(t, before, f.rows(t, integrationEntClient))
	require.ErrorIs(t, f.repo.BindGroups(t.Context(), f.account.ID, []int64{f.groups[0], 9007199254740990}), service.ErrGroupNotFound)
	require.ErrorIs(t, groups.BindAccountsToGroup(t.Context(), f.groups[2], []int64{f.account.ID, 9007199254740990}), service.ErrAccountNotFound)
	sameMembershipRows(t, before, f.rows(t, integrationEntClient))
	require.Equal(t, events, f.events(t))
}

func TestAccountGroupBindingsObserverAndScopedPriority(t *testing.T) {
	f := newMembershipFixture(t)
	before := f.rows(t, integrationEntClient)
	ctx := service.WithObserverScope(t.Context(), []int64{f.groups[0], f.groups[2]})
	require.ErrorIs(t, f.repo.RemoveFromGroup(ctx, f.account.ID, f.groups[1]), service.ErrObserverScope)
	require.ErrorIs(t, f.repo.AddToGroup(ctx, f.account.ID, f.groups[1], 5), service.ErrObserverScope)
	require.ErrorIs(t, f.repo.BindGroups(ctx, f.account.ID, []int64{f.groups[1]}), service.ErrObserverScope)
	require.NoError(t, f.repo.BindGroups(ctx, f.account.ID, []int64{f.groups[2]}))
	rows := f.rows(t, integrationEntClient)
	sameMembershipRows(t, before[1:], rows[:1])
	require.NoError(t, f.repo.SetGroupAllowedModels(ctx, f.account.ID, map[int64][]string{f.groups[1]: {"forbidden"}, f.groups[2]: {"gpt-6-astra"}}))
	rows = f.rows(t, integrationEntClient)
	sameMembershipRows(t, before[1:], rows[:1])
	require.Equal(t, []string{"gpt-6-astra"}, rows[1].AllowedModels)

	tx, err := integrationEntClient.Tx(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	scoped := adminaccess.WithPrincipal(dbent.NewTxContext(t.Context(), tx), &adminaccess.Principal{})
	require.NoError(t, f.repo.BindGroups(scoped, f.account.ID, []int64{f.groups[2], f.groups[1]}))
	sameMembershipRows(t, rows, f.rows(t, tx.Client()))
	require.Error(t, f.repo.BindGroups(adminaccess.WithPrincipal(t.Context(), &adminaccess.Principal{}), f.account.ID, f.groups))
}

func TestAccountGroupBindingsConcurrentRemoveAndReaddEntered(t *testing.T) {
	f := newMembershipFixture(t)
	before := f.rows(t, integrationEntClient)
	remove, _, removePID := degradationReplica(t)
	readd, _, readdPID := degradationReplica(t)
	gate := testTx(t)
	gatePID := degradationGatePID(t, gate)
	_, err := gate.ExecContext(t.Context(), "SELECT id FROM accounts WHERE id=$1 FOR UPDATE", f.account.ID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	removed, added := make(chan error, 1), make(chan error, 1)
	go func() { removed <- remove.RemoveFromGroup(ctx, f.account.ID, f.groups[0]) }()
	degradationEntered(t, removePID, "%FROM accounts%FOR NO KEY UPDATE%", gatePID)
	go func() { added <- readd.BindGroups(ctx, f.account.ID, f.groups[:2]) }()
	degradationEntered(t, readdPID, "%FROM accounts%FOR NO KEY UPDATE%", 0)
	require.NoError(t, gate.Commit())
	require.NoError(t, <-removed)
	require.NoError(t, <-added)
	after := f.rows(t, integrationEntClient)
	require.Len(t, after, 2)
	require.True(t, after[0].CreatedAt.After(before[0].CreatedAt), "re-add reads current absence after waiting instead of overwriting a stale row")
	require.Empty(t, after[0].AllowedModels)
	sameMembershipRows(t, before[1:], after[1:])
}

func TestAccountGroupBindingsModelEditAndRemoveEntered(t *testing.T) {
	f := newMembershipFixture(t)
	before := f.rows(t, integrationEntClient)
	remove, _, removePID := degradationReplica(t)
	edit, _, editPID := degradationReplica(t)
	gate := testTx(t)
	gatePID := degradationGatePID(t, gate)
	_, err := gate.ExecContext(t.Context(), "SELECT id FROM accounts WHERE id=$1 FOR UPDATE", f.account.ID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	removed, edited := make(chan error, 1), make(chan error, 1)
	go func() { removed <- remove.RemoveFromGroup(ctx, f.account.ID, f.groups[0]) }()
	degradationEntered(t, removePID, "%FROM accounts%FOR NO KEY UPDATE%", gatePID)
	go func() {
		edited <- edit.SetGroupAllowedModels(ctx, f.account.ID, map[int64][]string{f.groups[0]: {"stale"}, f.groups[1]: {"gpt-6-*"}})
	}()
	degradationEntered(t, editPID, "%FROM accounts%FOR NO KEY UPDATE%", 0)
	require.NoError(t, gate.Commit())
	require.NoError(t, <-removed)
	require.NoError(t, <-edited)
	sameMembershipRows(t, before[1:], f.rows(t, integrationEntClient))
}
