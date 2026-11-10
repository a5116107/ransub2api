package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
)

// NewRegionalFundsAuthorities installs mandatory authoritative scope checks.
// Guard is shared with the deployment cutover fence; absent evidence refuses
// grants, never silently runs a partially migrated financial authority.
func NewRegionalFundsAuthorities(db *sql.DB, guard func(context.Context, *sql.Tx) error) (*ledger.Primary, *ledger.PrimaryRequests, ledger.VerifyEvent) {
	scope := RegionalBudgetScope{}
	primary := &ledger.Primary{DB: db}
	primary.Funding = regionalBalanceFunding
	primary.ReserveScope = func(ctx context.Context, tx *sql.Tx, l regional.Lease) error {
		if guard == nil {
			return errors.New("funds cutover evidence required")
		}
		if err := guard(ctx, tx); err != nil {
			return err
		}
		return scope.Reserve(ctx, tx, "grant:"+l.LeaseID, l.UserID, l.APIKeyID, l.AccountID, l.Granted, l.IssuedAt)
	}
	primary.CloseScope = func(ctx context.Context, tx *sql.Tx, l regional.Lease, spent regional.Amount) error {
		return scope.Close(ctx, tx, "grant:"+l.LeaseID, spent)
	}
	verify := regionalScopedPricingVerifier("grant:", func(ctx context.Context, tx *sql.Tx, e ledger.Event) error {
		return scope.Charge(ctx, tx, "grant:"+e.Usage.LeaseID, e)
	})
	holds := &ledger.PrimaryRequests{DB: db, ValidateAdmission: verifyRegionalVideoSalePolicy}
	holds.Funding = regionalBalanceFunding
	holds.Authorize = func(ctx context.Context, tx *sql.Tx, r ledger.Reservation) error {
		if guard == nil {
			return errors.New("funds cutover evidence required")
		}
		if err := guard(ctx, tx); err != nil {
			return err
		}
		id := "request:" + r.RequestID
		if err := scope.Reserve(ctx, tx, id, r.UserID, r.APIKeyID, r.AccountID, r.Maximum, r.AdmittedAt); err != nil {
			return err
		}
		return verifyRegionalVideoGroup(ctx, tx, id, r.VideoOrigin)
	}
	holds.SettleScope = regionalScopedPricingVerifier("request:", func(ctx context.Context, tx *sql.Tx, e ledger.Event) error {
		id := "request:" + e.Usage.EventID
		if err := scope.Charge(ctx, tx, id, e); err != nil {
			return err
		}
		return scope.Close(ctx, tx, id, e.Usage.Charge)
	})
	holds.CancelScope = func(ctx context.Context, tx *sql.Tx, r ledger.Reservation) error {
		return scope.Close(ctx, tx, "request:"+r.RequestID, 0)
	}
	return primary, holds, verify
}

func regionalBalanceFunding(ctx context.Context, tx *sql.Tx, user, key int64) (bool, error) {
	var locked int64
	// Balance changes never modify a referenced key. NO KEY UPDATE serializes
	// spendable funds while permitting canonical usage's FK KEY SHARE check.
	if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id=$1 AND status='active' AND deleted_at IS NULL FOR NO KEY UPDATE", user).Scan(&locked); err != nil {
		return false, err
	}
	var kind string
	if err := tx.QueryRowContext(ctx, "SELECT g.subscription_type FROM api_keys k JOIN groups g ON g.id=k.group_id WHERE k.id=$1 AND k.user_id=$2 AND k.deleted_at IS NULL AND g.deleted_at IS NULL FOR SHARE OF k,g", key, user).Scan(&kind); err != nil {
		return false, err
	}
	return kind != "subscription", nil
}

// Billing metadata comes from the durable allocation, never from a key's group
// after an administrator moved it or a subscription expired during the request.
func regionalScopedPricingVerifier(prefix string, scope ledger.VerifyEvent) ledger.VerifyEvent {
	verify := ledger.PricingVerifier(scope)
	return func(ctx context.Context, tx *sql.Tx, event ledger.Event) error {
		id := prefix + event.Usage.EventID
		if prefix == "grant:" {
			id = prefix + event.Usage.LeaseID
		}
		if err := verifyRegionalVideoGroup(ctx, tx, id, event.VideoOrigin); err != nil {
			return err
		}
		if err := verify(ctx, tx, event); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "UPDATE usage_logs u SET group_id=a.group_id,subscription_id=NULLIF(a.subscription_id,0),billing_type=CASE WHEN a.subscription_id=0 THEN 0 ELSE 1 END,account_rate_multiplier=1 FROM regional_budget_allocations a WHERE a.allocation_id=$1 AND u.request_id=$2 AND u.api_key_id=a.api_key_id", id, event.Usage.EventID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ledger.ErrConflict
		}
		return nil
	}
}

func verifyRegionalVideoGroup(ctx context.Context, tx *sql.Tx, allocation string, origin *ledger.VideoOrigin) error {
	if origin == nil {
		return nil
	}
	var group int64
	if err := tx.QueryRowContext(ctx, `SELECT group_id FROM regional_budget_allocations WHERE allocation_id=$1`, allocation).Scan(&group); err != nil {
		return err
	}
	if group != origin.Owner.GroupID {
		return ledger.ErrConflict
	}
	return nil
}

// Immutable video prices currently certify unit dynamic sale policies only.
// Read the local SQL authority at both Reserve and Start: neither an old auth
// snapshot nor an earlier regional grant proves the current sale policy.
// This is new-admission validation, not repricing of accepted provider tasks.
func verifyRegionalVideoSalePolicy(ctx context.Context, tx *sql.Tx, r ledger.Reservation) error {
	if r.Video == nil {
		return nil
	}
	if r.VideoOrigin == nil || !r.VideoOrigin.Owner.Valid() {
		return ledger.ErrConflict
	}
	o := r.VideoOrigin.Owner
	if o.UserID != r.UserID || o.APIKeyID != r.APIKeyID || o.AccountID != r.AccountID || o.Provider != r.Video.Provider {
		return ledger.ErrConflict
	}
	var allowed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM api_keys k
		JOIN users u ON u.id=k.user_id
		JOIN groups g ON g.id=k.group_id
		JOIN accounts a ON a.id=$3 AND a.platform=g.platform
		JOIN account_groups ag ON ag.account_id=a.id AND ag.group_id=g.id
		WHERE k.id=$1 AND k.user_id=$2 AND g.id=$4
		AND k.status='active' AND k.deleted_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp())
		AND u.status='active' AND u.deleted_at IS NULL
		AND g.status='active' AND g.deleted_at IS NULL AND g.rate_multiplier=1
		AND (NOT g.peak_rate_enabled OR g.peak_rate_multiplier=1)
		AND (NOT g.video_rate_independent OR g.video_rate_multiplier=1)
		AND a.status='active' AND a.schedulable AND a.deleted_at IS NULL
		AND COALESCE(a.rate_multiplier,1)=1 AND COALESCE(a.group_rate_multiplier,1)=1
		AND NOT EXISTS(SELECT 1 FROM user_group_rate_multipliers ug WHERE ug.user_id=u.id AND ug.group_id=g.id AND ug.rate_multiplier IS NOT NULL AND ug.rate_multiplier<>1)
	)`, r.APIKeyID, r.UserID, r.AccountID, o.GroupID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ledger.ErrConflict
	}
	return nil
}
