package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Every membership writer shares the account lock with isolation/recovery.
// Read current bindings only after this lock; historical actions are not an
// exclusion list and an explicit edit may restore a removed membership.
func lockLiveAccountForGroupBindings(ctx context.Context, exec sqlExecutor, accountID int64) error {
	var id int64
	err := scanSingleRow(ctx, exec, `SELECT id FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR NO KEY UPDATE`, []any{accountID}, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrAccountNotFound
	}
	return err
}

// The ORM, outbox and optional caller transaction always use the same client.
// A notification failure rolls back owned mutations; caches are published only
// after our own commit. Caller-owned transactions publish through their outbox.
func (r *accountRepository) mutateAccountGroupBindings(ctx context.Context, accountID int64, apply func(*dbent.Client) ([]int64, error)) error {
	if scoped := r.accountTransactionScope(ctx); scoped != r {
		return scoped.mutateAccountGroupBindings(ctx, accountID, apply)
	}
	tx, err := r.client.Tx(ctx)
	if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
		return err
	}
	client := r.client
	if tx != nil {
		defer func() { _ = tx.Rollback() }()
		client = tx.Client()
	}
	changed, err := apply(client)
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountGroupsChanged, &accountID, nil, buildSchedulerGroupPayload(changed)); err != nil {
			return err
		}
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
		if len(changed) > 0 {
			r.syncSchedulerAccountSnapshotDetached(ctx, accountID)
		}
	}
	return nil
}

func (r *accountRepository) AddToGroup(ctx context.Context, accountID, groupID int64, priority int) error {
	if err := service.ValidateObserverGroupBindings(ctx, []int64{groupID}); err != nil {
		return err
	}
	return r.mutateAccountGroupBindings(ctx, accountID, func(client *dbent.Client) ([]int64, error) {
		if err := lockLiveGroups(ctx, client, []int64{groupID}); err != nil {
			return nil, err
		}
		if err := lockLiveAccountForGroupBindings(ctx, client, accountID); err != nil {
			return nil, err
		}
		_, err := client.AccountGroup.Create().SetAccountID(accountID).SetGroupID(groupID).SetPriority(priority).Save(ctx)
		return []int64{groupID}, err
	})
}

func (r *accountRepository) RemoveFromGroup(ctx context.Context, accountID, groupID int64) error {
	if !service.ObserverCanManageGroup(ctx, groupID) {
		return service.ErrObserverScope
	}
	return r.mutateAccountGroupBindings(ctx, accountID, func(client *dbent.Client) ([]int64, error) {
		if err := lockLiveAccountForGroupBindings(ctx, client, accountID); err != nil {
			return nil, err
		}
		removed, err := client.AccountGroup.Delete().Where(accountgroup.AccountIDEQ(accountID), accountgroup.GroupIDEQ(groupID)).Exec(ctx)
		if err != nil || removed == 0 {
			return nil, err
		}
		return []int64{groupID}, nil
	})
}

func (r *accountRepository) BindGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	if err := service.ValidateObserverGroupBindings(ctx, groupIDs); err != nil {
		return err
	}
	preservePriority := adminaccess.FromContext(ctx) != nil
	if preservePriority && dbent.TxFromContext(ctx) == nil {
		return fmt.Errorf("受限分组写入需要数据库事务")
	}
	return r.mutateAccountGroupBindings(ctx, accountID, func(client *dbent.Client) ([]int64, error) {
		if !preservePriority {
			if err := lockLiveGroups(ctx, client, groupIDs); err != nil {
				return nil, err
			}
		}
		if err := lockLiveAccountForGroupBindings(ctx, client, accountID); err != nil {
			return nil, err
		}
		return replaceAccountGroupBindings(ctx, client, accountID, groupIDs, preservePriority)
	})
}

// Retained rows are never deleted/recreated: keep their creation time, model
// restrictions and ownership metadata. Ordinary account forms still apply the
// requested order; scoped forms preserve priorities already approved elsewhere.
func replaceAccountGroupBindings(ctx context.Context, client *dbent.Client, accountID int64, proposed []int64, preservePriority bool) ([]int64, error) {
	entries, err := client.AccountGroup.Query().Where(accountgroup.AccountIDEQ(accountID)).All(ctx)
	if err != nil {
		return nil, err
	}
	existing := make(map[int64]*dbent.AccountGroup, len(entries))
	for _, entry := range entries {
		existing[entry.GroupID] = entry
	}
	priorities := make(map[int64]int, len(proposed))
	added := make([]int64, 0, len(proposed))
	for _, id := range proposed {
		if _, ok := priorities[id]; ok {
			continue
		}
		priorities[id] = len(priorities) + 1
		if existing[id] == nil {
			added = append(added, id)
		}
	}
	if err := lockLiveGroups(ctx, client, added); err != nil {
		return nil, err
	}
	changed := make([]int64, 0, len(entries)+len(added))
	removed := make([]int64, 0, len(entries))
	for _, entry := range entries {
		priority, retained := priorities[entry.GroupID]
		if !retained && service.ObserverCanManageGroup(ctx, entry.GroupID) {
			removed = append(removed, entry.GroupID)
		} else if retained && !preservePriority && entry.Priority != priority {
			if _, err := client.AccountGroup.Update().Where(accountgroup.AccountIDEQ(accountID), accountgroup.GroupIDEQ(entry.GroupID)).SetPriority(priority).Save(ctx); err != nil {
				return nil, err
			}
			changed = append(changed, entry.GroupID)
		}
	}
	if len(removed) > 0 {
		if _, err := client.AccountGroup.Delete().Where(accountgroup.AccountIDEQ(accountID), accountgroup.GroupIDIn(removed...)).Exec(ctx); err != nil {
			return nil, err
		}
		changed = append(changed, removed...)
	}
	for _, id := range added {
		if _, err := client.AccountGroup.Create().SetAccountID(accountID).SetGroupID(id).SetPriority(priorities[id]).Save(ctx); err != nil {
			return nil, err
		}
	}
	return append(changed, added...), nil
}

// SetGroupAllowedModels replaces restrictions only on current, authorized
// memberships; omission clears a restriction but never creates an association.
func (r *accountRepository) SetGroupAllowedModels(ctx context.Context, accountID int64, allowed map[int64][]string) error {
	return r.mutateAccountGroupBindings(ctx, accountID, func(client *dbent.Client) ([]int64, error) {
		if err := lockLiveAccountForGroupBindings(ctx, client, accountID); err != nil {
			return nil, err
		}
		entries, err := client.AccountGroup.Query().Where(accountgroup.AccountIDEQ(accountID)).All(ctx)
		if err != nil {
			return nil, err
		}
		changed := make([]int64, 0, len(entries))
		for _, entry := range entries {
			if !service.ObserverCanManageGroup(ctx, entry.GroupID) {
				continue
			}
			next := service.NormalizeGroupAllowedModels(allowed[entry.GroupID])
			if slices.Equal(next, service.NormalizeGroupAllowedModels(entry.AllowedModels)) {
				continue
			}
			update := client.AccountGroup.Update().Where(accountgroup.AccountIDEQ(accountID), accountgroup.GroupIDEQ(entry.GroupID))
			if len(next) == 0 {
				update.ClearAllowedModels()
			} else {
				update.SetAllowedModels(next)
			}
			if _, err := update.Save(ctx); err != nil {
				return nil, err
			}
			changed = append(changed, entry.GroupID)
		}
		return changed, nil
	})
}
