package repository

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"slices"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// SQL decimals, not auth snapshots or float64 caches, are the sale authority.
// Account-side sale/cost multipliers remain unit until their own contract exists.
type regionalSalePolicy struct {
	group             service.Group
	base, video, peak string
	override          sql.NullString
}

func readRegionalSalePolicy(ctx context.Context, tx *sql.Tx, user, key, account int64) (regionalSalePolicy, error) {
	var p regionalSalePolicy
	var accountUnit bool
	err := tx.QueryRowContext(ctx, `SELECT g.id,g.platform,g.subscription_type,g.rate_multiplier::text,
	 g.video_rate_independent,g.video_rate_multiplier::text,g.peak_rate_enabled,g.peak_start,g.peak_end,g.peak_rate_multiplier::text,
	 COALESCE(a.rate_multiplier,1)=1 AND COALESCE(a.group_rate_multiplier,1)=1
	 FROM api_keys k JOIN users u ON u.id=k.user_id JOIN groups g ON g.id=k.group_id
	 JOIN accounts a ON a.id=$3 AND a.platform=g.platform
	 JOIN account_groups ag ON ag.account_id=a.id AND ag.group_id=g.id
	 WHERE k.id=$1 AND k.user_id=$2 AND k.status='active' AND k.deleted_at IS NULL
	 AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp()) AND u.status='active' AND u.deleted_at IS NULL
	 AND g.status='active' AND g.deleted_at IS NULL AND a.status='active' AND a.schedulable AND a.deleted_at IS NULL
	 FOR SHARE OF k,u,g,a,ag`, key, user, account).Scan(&p.group.ID, &p.group.Platform, &p.group.SubscriptionType, &p.base,
		&p.group.VideoRateIndependent, &p.video, &p.group.PeakRateEnabled, &p.group.PeakStart, &p.group.PeakEnd, &p.peak, &accountUnit)
	if err != nil || !accountUnit {
		return p, errors.Join(ledger.ErrConflict, err)
	}
	err = tx.QueryRowContext(ctx, `SELECT rate_multiplier::text FROM user_group_rate_multipliers WHERE user_id=$1 AND group_id=$2 FOR SHARE`, user, p.group.ID).Scan(&p.override)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	return p, nil
}

func regionalSaleDecimal(raw string) (*big.Rat, error) {
	if len(raw) == 0 || len(raw) > 128 {
		return nil, ledger.ErrConflict
	}
	rate, ok := new(big.Rat).SetString(raw)
	if !ok || rate.Sign() < 0 || !rate.Num().IsInt64() || !rate.Denom().IsInt64() {
		return nil, ledger.ErrConflict
	}
	return rate, nil
}

func (p regionalSalePolicy) multiplier(provider string, peak bool) (*big.Rat, error) {
	raw := p.base
	if provider != "minimax-h3" && p.override.Valid {
		raw = p.override.String
	}
	if provider == "grok" && p.group.VideoRateIndependent {
		raw = p.video
	}
	rate, err := regionalSaleDecimal(raw)
	if err != nil {
		return nil, err
	}
	// Seedance bills completion tokens; Grok and MiniMax-H3 bill a task or
	// seconds. Neither independent video rates nor peak apply to MiniMax-H3.
	if (provider == "seedance" || provider == "") && peak {
		factor, err := regionalSaleDecimal(p.peak)
		if err != nil {
			return nil, err
		}
		rate.Mul(rate, factor)
	}
	return rate, nil
}

func (p regionalSalePolicy) supports(provider string) bool {
	switch provider {
	case "grok":
		return p.group.Platform == service.PlatformGrok
	case "minimax-h3":
		return p.group.Platform == service.PlatformMiniMax
	case "seedance":
		return p.group.Platform == service.PlatformOpenAI
	}
	return false
}

func (p regionalSalePolicy) derive(template ledger.Price, at time.Time) (ledger.Price, error) {
	if template.Video == nil || !p.supports(template.Video.Provider) {
		return ledger.Price{}, ledger.ErrConflict
	}
	multiplier, err := p.multiplier(template.Video.Provider, p.group.PeakRateActiveAt(at))
	if err != nil {
		return ledger.Price{}, err
	}
	return ledger.DeriveVideoPrice(template, multiplier)
}

// Authorize both sides of a peak boundary at grant issuance. Reserve and Start
// still choose and revalidate the one effective at the SQL admission time.
// Only the primary's own configured templates can create these versions.
func (p regionalSalePolicy) grantPrices(ctx context.Context, tx *sql.Tx, templates []ledger.Price) ([]string, error) {
	versions := make([]string, 0)
	for _, template := range templates {
		if template.Video == nil || !p.supports(template.Video.Provider) {
			continue
		}
		for _, peak := range []bool{false, p.group.IsSubscriptionType() && p.group.PeakRateEnabled} {
			multiplier, err := p.multiplier(template.Video.Provider, peak)
			if err != nil {
				return nil, err
			}
			price, err := ledger.DeriveVideoPrice(template, multiplier)
			if err != nil {
				return nil, err
			}
			if err := ledger.InstallPrice(ctx, tx, price); err != nil {
				return nil, err
			}
			versions = append(versions, price.Version())
		}
	}
	slices.Sort(versions)
	versions = slices.Compact(versions)
	if len(versions) > 128 {
		return nil, ledger.ErrConflict
	}
	return versions, nil
}

func verifyRegionalGrantSalePolicy(ctx context.Context, tx *sql.Tx, l regional.Lease, templates []ledger.Price) error {
	p, err := readRegionalSalePolicy(ctx, tx, l.UserID, l.APIKeyID, l.AccountID)
	if err != nil {
		return err
	}
	if l.GroupID != 0 && l.GroupID != p.group.ID || len(templates) > 0 && l.GroupID == 0 {
		return ledger.ErrConflict
	}
	if len(templates) > 0 {
		expected, err := p.grantPrices(ctx, tx, templates)
		if err != nil {
			return err
		}
		if !slices.Equal(expected, l.VideoPriceVersions) {
			return ledger.ErrConflict
		}
		if len(expected) > 0 {
			return nil
		}
	}
	// Non-runtime callers may authorize already installed historical tariffs,
	// but never a derived contract that bypasses configured-template approval.
	for _, version := range l.VideoPriceVersions {
		var model string
		if err := tx.QueryRowContext(ctx, "SELECT price->>'model' FROM regional_prices WHERE version=$1", version).Scan(&model); err != nil {
			return err
		}
		price, err := ledger.ReadPrice(ctx, tx, version, model)
		if err != nil || price.Video == nil || price.Video.TemplateVersion != "" || !p.supports(price.Video.Provider) {
			return errors.Join(ledger.ErrConflict, err)
		}
	}
	// Text and pre-derived tariffs still require a unit policy at all times.
	for _, raw := range []string{p.base, p.peak} {
		rate, err := regionalSaleDecimal(raw)
		if err != nil || rate.Cmp(big.NewRat(1, 1)) != 0 {
			return errors.Join(ledger.ErrConflict, err)
		}
	}
	if p.override.Valid {
		rate, err := regionalSaleDecimal(p.override.String)
		if err != nil || rate.Cmp(big.NewRat(1, 1)) != 0 {
			return errors.Join(ledger.ErrConflict, err)
		}
	}
	return nil
}

// Both request ledgers call this inside Reserve and Start. A configured video
// tariff is multiplied by the authoritative effective policy, never repriced
// after dispatch. Text remains unit-only even when sharing a video grant.
func VerifyRegionalSalePolicy(ctx context.Context, tx *sql.Tx, r ledger.Reservation) error {
	p, err := readRegionalSalePolicy(ctx, tx, r.UserID, r.APIKeyID, r.AccountID)
	if err != nil {
		return err
	}
	if r.LeaseID != "" {
		var group sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT (lease->>'group_id')::bigint FROM regional_local_leases WHERE lease_id=$1`, r.LeaseID).Scan(&group); err != nil {
			return err
		}
		if group.Valid && group.Int64 != p.group.ID {
			return ledger.ErrConflict
		}
	}
	if r.Video == nil {
		rate, err := p.multiplier("", p.group.PeakRateActiveAt(r.AdmittedAt))
		if err != nil || rate.Cmp(big.NewRat(1, 1)) != 0 {
			return errors.Join(ledger.ErrConflict, err)
		}
		return nil
	}
	if r.VideoOrigin == nil || !r.VideoOrigin.Owner.Valid() || r.VideoOrigin.Owner.GroupID != p.group.ID || !p.supports(r.Video.Provider) {
		return ledger.ErrConflict
	}
	price, err := ledger.ReadPrice(ctx, tx, r.PricingVersion, r.Model)
	if err != nil || price.Video == nil {
		return errors.Join(ledger.ErrConflict, err)
	}
	if price.Video.TemplateVersion == "" {
		// Historical immutable tariffs certify only unit dynamic policies.
		rate, err := p.multiplier(r.Video.Provider, p.group.PeakRateActiveAt(r.AdmittedAt))
		if err != nil || rate.Cmp(big.NewRat(1, 1)) != 0 {
			return errors.Join(ledger.ErrConflict, err)
		}
		return nil
	}
	template, err := ledger.ReadPrice(ctx, tx, price.Video.TemplateVersion, r.Model)
	if err != nil {
		return err
	}
	expected, err := p.derive(template, r.AdmittedAt)
	if err != nil || expected.Version() != r.PricingVersion {
		return errors.Join(ledger.ErrConflict, err)
	}
	return nil
}
