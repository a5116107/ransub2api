package repository

import (
	"context"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/regional/slots"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// The transport retains the existing shared lease lifecycle. This adapter
// changes its authority to the same SQL budget used by regional grants.
func (c *regionalConcurrencyCache) acquireUpstreamKeySlot(ctx context.Context, revision int64, digest, fingerprint, request string) (slots.Resource, int, error) {
	if c.runtimeFailed.Load() {
		return slots.Resource{}, 0, service.ErrUpstreamKeyPolicy
	}
	tx, err := c.database().BeginTx(ctx, nil)
	if err != nil {
		return slots.Resource{}, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var resource slots.Resource
	var limited bool
	if c.primary != nil {
		if c.primary.GrantGuard != nil {
			if err := c.primary.GrantGuard(ctx, tx); err != nil {
				return resource, 0, err
			}
		}
		resource, limited, err = c.primary.UpstreamResourceTx(ctx, tx, revision, digest, fingerprint)
	} else {
		resource, limited, err = c.local.UpstreamResourceTx(ctx, tx, revision, digest, fingerprint)
	}
	if err != nil {
		if limited && errors.Is(err, slots.ErrUnavailable) {
			return resource, 0, nil
		}
		return resource, 0, err
	}
	if !limited {
		return resource, 2, nil
	}
	ok, err := c.acquireTx(ctx, tx, resource, request)
	if err != nil {
		return resource, 0, err
	}
	if !ok {
		// A refused attempt changed no ownership. Roll back its row locks rather
		// than forcing a durable commit for every contender on a full budget.
		return resource, 0, nil
	}
	if err = tx.Commit(); err != nil {
		return resource, 0, err
	}
	return resource, 1, nil
}

func (c *regionalConcurrencyCache) upstreamKeyHeld(ctx context.Context, r slots.Resource, request string) (bool, error) {
	if c.runtimeFailed.Load() {
		return false, service.ErrUpstreamKeyPolicy
	}
	if c.primary != nil {
		return c.primary.Held(ctx, r, request)
	}
	return c.local.Held(ctx, r, request)
}

// Only a new physical send checks current policy/freshness. Holding an already
// admitted media session does not reallocate it or depend on a new grant.
func (c *regionalConcurrencyCache) confirmLiveUpstreamPolicy(ctx context.Context, revision int64, digest string, resources []service.LiveUpstreamKeyResource) error {
	tx, err := c.database().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, held := range resources {
		var r slots.Resource
		var limited bool
		if c.primary != nil {
			r, limited, err = c.primary.UpstreamResourceTx(ctx, tx, revision, digest, held.Fingerprint)
		} else {
			r, limited, err = c.local.UpstreamResourceTx(ctx, tx, revision, digest, held.Fingerprint)
		}
		if err != nil {
			return err
		}
		if !limited || r.ID != held.ResourceID {
			return service.ErrUpstreamKeyPolicy
		}
	}
	return nil
}

func (c *regionalConcurrencyCache) releaseUpstreamKeySlot(ctx context.Context, r slots.Resource, request string) error {
	tx, err := c.database().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := c.releaseTx(ctx, tx, r, request); err != nil {
		return err
	}
	return tx.Commit()
}
