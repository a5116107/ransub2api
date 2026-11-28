package service

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/tidwall/gjson"
)

// Catalogs describe quotations, not model authenticity, a routing contract or
// permission to dispatch. Only these whitelisted fields leave the collector.
type UpstreamGroupQuote struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	PersonalRate *float64 `json:"personal_rate"`
	PublicRate   *float64 `json:"public_rate"`
}

type UpstreamGroupChange struct {
	Kind    string              `json:"kind"`
	GroupID string              `json:"group_id"`
	At      time.Time           `json:"at"`
	Before  *UpstreamGroupQuote `json:"before"`
	After   *UpstreamGroupQuote `json:"after"`
}

type UpstreamGroupCatalog struct {
	Version               int                   `json:"version"`
	Provider              string                `json:"provider"`
	Scope                 string                `json:"scope"`
	PublicStatus          string                `json:"public_status,omitempty"`
	AuthorizationRevision int64                 `json:"authorization_revision,omitempty"`
	Status                string                `json:"status"`
	AttemptedAt           time.Time             `json:"attempted_at"`
	ReceivedAt            *time.Time            `json:"received_at"`
	FreshUntil            *time.Time            `json:"fresh_until"`
	Groups                []UpstreamGroupQuote  `json:"groups"`
	Changes               []UpstreamGroupChange `json:"changes"`
}

// MergeUpstreamGroupCatalog is called under the existing persistence lock/CAS.
// A failure never advances the last complete observation or its TTL. A first
// complete observation establishes a baseline, rather than a burst of alerts.
func MergeUpstreamGroupCatalog(previous, incoming *UpstreamGroupCatalog) *UpstreamGroupCatalog {
	if previous != nil && !incoming.AttemptedAt.After(previous.AttemptedAt) {
		return previous
	}
	out := *incoming
	out.Groups, out.Changes = []UpstreamGroupQuote{}, []UpstreamGroupChange{}
	if previous != nil && previous.Version == 1 && previous.Provider == incoming.Provider && previous.Scope == incoming.Scope {
		out.Groups = append(out.Groups, previous.Groups...)
		out.ReceivedAt, out.FreshUntil = previous.ReceivedAt, previous.FreshUntil
		if incoming.Status != "ok" {
			out.PublicStatus = previous.PublicStatus
		}
		for _, change := range previous.Changes {
			if !change.At.Before(incoming.AttemptedAt.Add(-30 * 24 * time.Hour)) {
				out.Changes = append(out.Changes, change)
			}
		}
	} else {
		previous = nil
	}
	if incoming.Status == "ok" {
		if previous != nil && previous.ReceivedAt != nil {
			old := make(map[string]UpstreamGroupQuote, len(previous.Groups))
			for _, group := range previous.Groups {
				old[group.ID] = group
			}
			changes := []UpstreamGroupChange{}
			for _, group := range incoming.Groups {
				before, exists := old[group.ID]
				delete(old, group.ID)
				if !exists {
					changes = append(changes, UpstreamGroupChange{Kind: "group_added", GroupID: group.ID, At: incoming.AttemptedAt, After: &group})
				} else if !sameUpstreamGroupQuote(before, group, previous.PublicStatus == "ok" && incoming.PublicStatus == "ok") {
					changes = append(changes, UpstreamGroupChange{Kind: "group_updated", GroupID: group.ID, At: incoming.AttemptedAt, Before: &before, After: &group})
				}
			}
			for id, group := range old {
				changes = append(changes, UpstreamGroupChange{Kind: "group_removed", GroupID: id, At: incoming.AttemptedAt, Before: &group})
			}
			sort.Slice(changes, func(i, j int) bool { return changes[i].GroupID < changes[j].GroupID })
			out.Changes = append(changes, out.Changes...)
		}
		out.Groups = append([]UpstreamGroupQuote{}, incoming.Groups...)
		out.ReceivedAt, out.FreshUntil = incoming.ReceivedAt, incoming.FreshUntil
	}
	if len(out.Changes) > 100 {
		out.Changes = out.Changes[:100]
	}
	return &out
}

func sameUpstreamGroupQuote(a, b UpstreamGroupQuote, comparePublic bool) bool {
	equal := func(x, y *float64) bool { return x == nil && y == nil || x != nil && y != nil && *x == *y }
	return a.ID == b.ID && a.Name == b.Name && equal(a.PersonalRate, b.PersonalRate) && (!comparePublic || equal(a.PublicRate, b.PublicRate))
}

func parseNewAPIGroupCatalog(raw json.RawMessage, public map[string]float64) ([]UpstreamGroupQuote, bool) {
	fields, valid := availabilityJSONObject(gjson.ParseBytes(raw))
	entries, complete := availabilityJSONObject(fields["data"])
	if !valid || !complete || fields["success"].Type != gjson.True || len(entries) > 1024 {
		return nil, false
	}
	groups := make([]UpstreamGroupQuote, 0, len(entries))
	for id, raw := range entries {
		entry, unique := availabilityJSONObject(raw)
		rate := entry["ratio"]
		// "auto" is an available selector, not a fixed purchasable group.
		if id == "auto" {
			continue
		}
		if !unique || rate.Type != gjson.Number || !validNewAPIGroupRate(id, rate.Float()) {
			complete = false
			continue
		}
		r := rate.Float()
		group := UpstreamGroupQuote{ID: id, Name: sanitizeUpstreamBalanceText(entry["desc"].String(), 100), PersonalRate: &r}
		if p, ok := public[id]; ok && validNewAPIGroupRate(id, p) {
			group.PublicRate = &p
		}
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	return groups, complete
}

func upstreamKeyQuoteObservation(account *Account, snap *UpstreamBillingProbeSnapshot) *UpstreamGroupCatalog {
	observation := &UpstreamGroupCatalog{Version: 1, Provider: "sub2api", Scope: "call_key", Status: "failed", AttemptedAt: snap.LastAttemptAt}
	if snap.Status == UpstreamBillingProbeStatusOK && snap.Data["billing_mode"] != "multi_group" && upstreamBillingRequestGroupConfirmed(account, snap.Data) {
		// Keep the supplier's base group/user declaration separate from dynamic
		// peak multipliers. This is a quotation, not a cash cost calculation.
		rate, valid := resolveAccountExtraNumber(snap.Data, "resolved_rate_multiplier")
		if valid && newAPIFinite(rate) && rate >= 0 && rate <= 1000000 {
			id, _ := snap.Data["selected_group_id"].(string)
			if id == "" {
				id, _ = snap.Data["group_id"].(string)
			}
			// Legacy unpinned token prices don't disclose a stable group ID.
			// Do not label a default selector as a proven supplier group.
			if id == "" {
				id = "current_key"
			}
			observation.Status, observation.ReceivedAt, observation.FreshUntil = "ok", snap.ReceivedAt, snap.FreshUntil
			observation.Groups = []UpstreamGroupQuote{{ID: id, PersonalRate: &rate}}
		}
	}
	return observation
}

type UpstreamCatalogRelated struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type UpstreamCatalogImpact struct {
	Sources   []UpstreamCatalogRelated `json:"sources"`
	Products  []UpstreamCatalogRelated `json:"products"`
	Truncated bool                     `json:"truncated"`
}

type UpstreamGroupCatalogView struct {
	Version    int                   `json:"version"`
	AccountID  int64                 `json:"account_id"`
	Platform   string                `json:"platform"`
	UserID     *int64                `json:"user_id"`
	ObservedAt time.Time             `json:"observed_at"`
	Catalog    *UpstreamGroupCatalog `json:"catalog"`
	Impact     UpstreamCatalogImpact `json:"impact"`
}

// This extends the existing authorization repository without introducing a
// second credential store, outbound client or background scheduler.
type UpstreamGroupCatalogRepository interface {
	ReadNewAPIGroupCatalog(context.Context, int64, int64) (*UpstreamGroupCatalog, error)
	ReadUpstreamCatalogImpact(context.Context, int64) (UpstreamCatalogImpact, error)
}

func (s *UpstreamBillingProbeService) GetUpstreamGroupCatalog(ctx context.Context, accountID int64) (*UpstreamGroupCatalogView, error) {
	if _, scoped := ObserverGroupIDs(ctx); scoped || adminaccess.FromContext(ctx) != nil {
		return nil, ErrObserverScope
	}
	if s == nil || s.accountRepo == nil || s.newAPIRepo == nil {
		return nil, ErrUpstreamBillingProbeUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !isUpstreamBillingProbeAccount(account) {
		return nil, ErrUpstreamBillingProbeAccountInvalid
	}
	repo, ok := s.newAPIRepo.(UpstreamGroupCatalogRepository)
	if !ok {
		return nil, ErrUpstreamBillingProbeUnavailable
	}
	now := s.currentTime().UTC()
	out := &UpstreamGroupCatalogView{Version: 1, AccountID: accountID, Platform: account.Platform, ObservedAt: now}
	binding, err := s.newAPIRepo.GetBinding(ctx, accountID)
	if err != nil {
		return nil, newAPIError("storage_unavailable")
	}
	if binding != nil {
		if binding.Fingerprint != NewAPIAccountFingerprint(account) {
			return nil, ErrUpstreamBillingProbeIdentityChanged
		}
		out.UserID = &binding.Profile.UserID
		out.Catalog, err = repo.ReadNewAPIGroupCatalog(ctx, binding.Profile.ID, binding.Profile.Revision)
		if err != nil {
			return nil, newAPIError("storage_unavailable")
		}
	} else if snap := decodeUpstreamBillingProbeSnapshot(account.Extra); snap != nil {
		out.Catalog = snap.GroupCatalog
	}
	if out.Catalog == nil {
		out.Catalog = &UpstreamGroupCatalog{Version: 1, Provider: "sub2api", Scope: "call_key", Status: "not_observed", Groups: []UpstreamGroupQuote{}, Changes: []UpstreamGroupChange{}}
		if binding != nil {
			out.Catalog.Provider, out.Catalog.Scope = "new_api", "management_user"
		}
	}
	out.Impact, err = repo.ReadUpstreamCatalogImpact(ctx, accountID)
	if err != nil {
		return nil, newAPIError("storage_unavailable")
	}
	// Reads never renew evidence or rewrite history, including when a paused
	// collector has not had another chance to evict old journal entries.
	now = s.currentTime().UTC()
	out.ObservedAt = now
	copy := *out.Catalog
	if copy.Status == "ok" && (copy.FreshUntil == nil || !now.Before(*copy.FreshUntil)) {
		copy.Status = "expired"
	}
	copy.Changes = []UpstreamGroupChange{}
	for _, change := range out.Catalog.Changes {
		if !change.At.Before(now.Add(-30*24*time.Hour)) && !change.At.After(now) {
			copy.Changes = append(copy.Changes, change)
		}
	}
	out.Catalog = &copy
	return out, nil
}
