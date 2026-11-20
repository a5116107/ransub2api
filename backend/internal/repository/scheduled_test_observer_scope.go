package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// The expression is a fixed repository-owned account column, never input. Keep
// result and plan reads in the same SQL snapshot as their membership authority.
func scheduledObserverAccountFilter(ctx context.Context, accountColumn string, args []any) (string, []any) {
	ids, scoped := service.ObserverGroupIDs(ctx)
	if !scoped {
		return "", args
	}
	args = append(args, pq.Array(ids))
	return fmt.Sprintf(` AND EXISTS (SELECT 1 FROM accounts observer_account
 JOIN account_groups observer_group ON observer_group.account_id=observer_account.id
 WHERE observer_account.id=%s AND observer_account.deleted_at IS NULL
 AND observer_group.group_id=ANY($%d::bigint[]))`, accountColumn, len(args)), args
}

// Writers already own the plan lock. Native membership writes share the account
// row lock; check grants after acquiring it and retain it through commit. A
// concurrent membership removal therefore either precedes or follows the write.
func authorizeScheduledObserverWrite(ctx context.Context, tx *sql.Tx, accountID int64) error {
	if _, scoped := service.ObserverGroupIDs(ctx); !scoped {
		return nil
	}
	var locked int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, accountID).Scan(&locked)
	if err == sql.ErrNoRows {
		return service.ErrObserverScope
	}
	if err != nil {
		return err
	}
	filter, args := scheduledObserverAccountFilter(ctx, "accounts.id", []any{accountID})
	var allowed bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1`+filter+`)`, args...).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return service.ErrObserverScope
	}
	return nil
}
