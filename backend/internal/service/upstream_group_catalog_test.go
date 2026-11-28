package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpstreamGroupCatalogChangesUseIDsAndPreserveUnknown(t *testing.T) {
	now := time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC)
	quote := func(id, name string, rate float64) UpstreamGroupQuote {
		return UpstreamGroupQuote{ID: id, Name: name, PersonalRate: &rate}
	}
	observe := func(at time.Time, groups ...UpstreamGroupQuote) *UpstreamGroupCatalog {
		return &UpstreamGroupCatalog{Version: 1, Provider: "new_api", Scope: "management_user", Status: "ok", AttemptedAt: at, ReceivedAt: probeTimePtr(at), FreshUntil: probeTimePtr(at.Add(time.Hour)), Groups: groups}
	}
	first := MergeUpstreamGroupCatalog(nil, observe(now, quote("one", "same", .1), quote("two", "same", .2)))
	require.Len(t, first.Groups, 2)
	require.Empty(t, first.Changes, "the first complete read establishes a baseline")
	next := MergeUpstreamGroupCatalog(first, observe(now.Add(time.Minute), quote("one", "renamed", 0), quote("two", "same", .2)))
	require.Len(t, next.Changes, 1)
	require.Equal(t, "group_updated", next.Changes[0].Kind)
	require.Equal(t, "one", next.Changes[0].GroupID)
	require.Equal(t, 0.0, *next.Changes[0].After.PersonalRate)
	failed := MergeUpstreamGroupCatalog(next, &UpstreamGroupCatalog{Version: 1, Provider: "new_api", Scope: "management_user", Status: "failed", AttemptedAt: now.Add(2 * time.Minute)})
	require.Equal(t, "failed", failed.Status)
	require.Equal(t, next.ReceivedAt, failed.ReceivedAt)
	require.Equal(t, next.FreshUntil, failed.FreshUntil)
	require.Equal(t, next.Groups, failed.Groups)
	require.Len(t, failed.Changes, 1, "a failed read must not fabricate a deletion or a public-price fallback")
	require.Equal(t, failed, MergeUpstreamGroupCatalog(failed, observe(now, quote("one", "late", .5))), "late reads must not overwrite a newer attempt")
	recovered := MergeUpstreamGroupCatalog(failed, observe(now.Add(3*time.Minute), quote("one", "renamed", 0)))
	require.Len(t, recovered.Changes, 2)
	require.Equal(t, "group_removed", recovered.Changes[0].Kind)
	require.Equal(t, "two", recovered.Changes[0].GroupID)
}

func TestNewAPIGroupCatalogRejectsIncompleteAndInvalidRates(t *testing.T) {
	for _, raw := range []string{
		`{"success":false,"data":{"vip":{"ratio":0.1}}}`,
		`{"success":true,"data":{"vip":{"ratio":null}}}`,
		`{"success":true,"data":{"vip":{"ratio":-1}}}`,
		`{"success":true,"data":{"vip":{"ratio":1e999}}}`,
		`{"success":true,"data":{"vip":{"ratio":0.1,"ratio":0.2}}}`,
		`{"success":true,"data":{"vip":{"ratio":0.1},"vip":{"ratio":0.2}}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, ok := parseNewAPIGroupCatalog(json.RawMessage(raw), nil)
			require.False(t, ok)
		})
	}
	groups, ok := parseNewAPIGroupCatalog(json.RawMessage(`{"success":true,"data":{"free":{"ratio":0,"desc":"Free"},"vip":{"ratio":0.2}}}`), map[string]float64{"free": 1, "public-only": 2})
	require.True(t, ok)
	require.Len(t, groups, 2, "a public-only group is not an authorized available group")
	require.Equal(t, "free", groups[0].ID)
	require.Equal(t, 0.0, *groups[0].PersonalRate)
	require.Equal(t, 1.0, *groups[0].PublicRate)
}

func TestUpstreamGroupCatalogHistoryIsBounded(t *testing.T) {
	now := time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC)
	var catalog *UpstreamGroupCatalog
	for i := range 140 {
		at, rate := now.Add(time.Duration(i)*time.Minute), float64(i)
		catalog = MergeUpstreamGroupCatalog(catalog, &UpstreamGroupCatalog{Version: 1, Provider: "new_api", Scope: "management_user", Status: "ok", AttemptedAt: at, ReceivedAt: &at, FreshUntil: probeTimePtr(at.Add(time.Hour)), Groups: []UpstreamGroupQuote{{ID: "vip", PersonalRate: &rate}}})
	}
	require.Len(t, catalog.Changes, 100)
	catalog = MergeUpstreamGroupCatalog(catalog, &UpstreamGroupCatalog{Version: 1, Provider: "new_api", Scope: "management_user", Status: "failed", AttemptedAt: now.Add(31 * 24 * time.Hour)})
	require.Empty(t, catalog.Changes)
}

func TestUpstreamGroupCatalogPublicReadFailureIsNotAPriceChange(t *testing.T) {
	now, personal, public := time.Now().UTC(), 0.2, 0.5
	previous := &UpstreamGroupCatalog{Version: 1, Provider: "new_api", Scope: "management_user", PublicStatus: "ok", Status: "ok", AttemptedAt: now, ReceivedAt: &now, Groups: []UpstreamGroupQuote{{ID: "vip", PersonalRate: &personal, PublicRate: &public}}}
	next := MergeUpstreamGroupCatalog(previous, &UpstreamGroupCatalog{Version: 1, Provider: "new_api", Scope: "management_user", Status: "ok", AttemptedAt: now.Add(time.Minute), ReceivedAt: probeTimePtr(now.Add(time.Minute)), Groups: []UpstreamGroupQuote{{ID: "vip", PersonalRate: &personal}}})
	require.Empty(t, next.Changes)
	require.Nil(t, next.Groups[0].PublicRate)
	require.Equal(t, personal, *next.Groups[0].PersonalRate)
}

func TestUpstreamGroupCatalogKeyObservationKeepsFailureAndRecoverySeparate(t *testing.T) {
	s, _, _, accounts := newAPIConfigFixture()
	a := accounts.accounts[1]
	now := time.Now().UTC()
	snapshot := func(rate float64, at time.Time) *UpstreamBillingProbeSnapshot {
		return &UpstreamBillingProbeSnapshot{Status: "ok", LastAttemptAt: at, ReceivedAt: &at, FreshUntil: probeTimePtr(at.Add(time.Hour)), Data: map[string]any{"billing_scope": "token", "resolved_rate_multiplier": rate}}
	}
	first := snapshot(.2, now)
	require.NoError(t, s.updateSnapshot(context.Background(), a, first, nil))
	require.Equal(t, "call_key", first.GroupCatalog.Scope)
	require.Equal(t, "current_key", first.GroupCatalog.Groups[0].ID, "no stable group ID was declared")
	failed := upstreamBillingProbeFailureSnapshot(a, 5, now.Add(time.Minute), 503, "http_error", 0)
	require.NoError(t, s.updateSnapshot(context.Background(), a, failed, nil))
	require.Equal(t, "failed", failed.GroupCatalog.Status)
	require.Equal(t, first.GroupCatalog.FreshUntil, failed.GroupCatalog.FreshUntil)
	require.Empty(t, failed.GroupCatalog.Changes)
	last := snapshot(0, now.Add(2*time.Minute))
	require.NoError(t, s.updateSnapshot(context.Background(), a, last, nil))
	require.Len(t, last.GroupCatalog.Changes, 1)
	require.Equal(t, 0.0, *last.GroupCatalog.Changes[0].After.PersonalRate)
	require.Nil(t, last.SyncedRateMultiplier)
}

func TestUpstreamGroupCatalogRejectsObserverBeforeReadingAnyAccount(t *testing.T) {
	s := NewUpstreamBillingProbeService(nil, nil, nil)
	_, err := s.GetUpstreamGroupCatalog(WithObserverScope(t.Context(), []int64{1}), 1)
	require.ErrorIs(t, err, ErrObserverScope)
}

type upstreamCatalogReaderFake struct {
	*newAPIAuthRepoFake
	catalog *UpstreamGroupCatalog
}

func (r upstreamCatalogReaderFake) ReadNewAPIGroupCatalog(context.Context, int64, int64) (*UpstreamGroupCatalog, error) {
	return r.catalog, nil
}

func (upstreamCatalogReaderFake) ReadUpstreamCatalogImpact(context.Context, int64) (UpstreamCatalogImpact, error) {
	return UpstreamCatalogImpact{Sources: []UpstreamCatalogRelated{}, Products: []UpstreamCatalogRelated{}}, nil
}

func TestUpstreamGroupCatalogReadExpiresAndPrunesWithoutMutatingStoredEvidence(t *testing.T) {
	s, auth, upstream, accounts := newAPIConfigFixture()
	now, rate := s.currentTime().UTC(), .2
	old := now.Add(-31 * 24 * time.Hour)
	auth.bindings[1] = &NewAPIAccountBinding{Profile: NewAPISiteAuthorization{ID: 1, Revision: 1, UserID: 7}, Fingerprint: NewAPIAccountFingerprint(accounts.accounts[1])}
	catalog := &UpstreamGroupCatalog{Version: 1, Provider: "new_api", Scope: "management_user", Status: "ok", AttemptedAt: old, ReceivedAt: &old, FreshUntil: probeTimePtr(old.Add(time.Hour)), Groups: []UpstreamGroupQuote{{ID: "vip", PersonalRate: &rate}}, Changes: []UpstreamGroupChange{{Kind: "group_added", GroupID: "vip", At: old}}}
	s.newAPIRepo = upstreamCatalogReaderFake{auth, catalog}
	upstream.check = func(*http.Request) { t.Error("reading a catalog must not contact the supplier") }
	view, err := s.GetUpstreamGroupCatalog(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, "expired", view.Catalog.Status)
	require.Empty(t, view.Catalog.Changes)
	require.Len(t, catalog.Changes, 1)
	require.Equal(t, "ok", catalog.Status)
}
