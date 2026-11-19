//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestRegionalSchedulerDetailsNoPrimaryFallback(t *testing.T) {
	ctx := context.Background()
	replica := regionalTestReplica(t, filepath.Join(t.TempDir(), "state"), true)
	doc := RegionalSchedulerDocument{Version: 1, Groups: []*Group{{ID: 10, Status: StatusActive, Platform: PlatformOpenAI, Hydrated: true}}, Buckets: []RegionalSchedulerBucket{{Bucket: SchedulerBucket{GroupID: 10, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}, Accounts: []*Account{{ID: 1, GroupIDs: []int64{10}, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "test-secret"}}}}}}
	publish := func() {
		t.Helper()
		payload, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := replica.PublishNext(ctx, "test", time.Minute, payload); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	reader, err := NewRegionalSchedulerReader(replica)
	if err != nil {
		t.Fatal(err)
	}
	// Nil primary dependencies turn any accidental fallback into a test failure.
	svc := NewSchedulerSnapshotService(nil, nil, nil, nil, nil)
	svc.SetLocalSnapshotReader(reader)
	account, err := svc.GetAccount(ctx, 1)
	if err != nil || account == nil || account.Credentials["api_key"] != "test-secret" {
		t.Fatalf("account hydration failed: %v", err)
	}
	account.Credentials["api_key"] = "changed"
	batch, err := svc.GetAccounts(ctx, []int64{1, 1, 2, -1})
	if err != nil || len(batch) != 1 || batch[1].Credentials["api_key"] != "test-secret" {
		t.Fatalf("batch hydration failed: %v", err)
	}
	for _, get := range []func(context.Context, int64) (*Group, error){svc.GetGroupByID, svc.GetGroupByIDLite} {
		g, err := get(ctx, 10)
		if err != nil || g == nil || !g.Hydrated {
			t.Fatalf("group hydration failed: %v", err)
		}
		g.Status = "disabled"
		if _, err := get(ctx, 11); err == nil {
			t.Fatal("missing group accepted")
		}
	}
	doc.Buckets[0].Accounts = []*Account{}
	doc.Groups = nil
	publish()
	account, err = svc.GetAccount(ctx, 1)
	if err != nil || account != nil {
		t.Fatalf("removed account resurrected: %v", err)
	}
	if _, err := svc.GetGroupByID(ctx, 10); err == nil {
		t.Fatal("removed group resurrected")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := svc.GetAccounts(canceled, []int64{1}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRegionalSchedulerCandidateOnlyFailsClosedForDetails(t *testing.T) {
	svc := NewSchedulerSnapshotService(nil, nil, nil, nil, nil)
	svc.SetLocalSnapshotReader(NewLocalSchedulerSnapshotStore(nil))
	ctx := context.Background()
	if _, err := svc.GetAccount(ctx, 1); !errors.Is(err, ErrLocalSchedulerSnapshotUnavailable) {
		t.Fatal(err)
	}
	if _, err := svc.GetAccounts(ctx, []int64{1}); !errors.Is(err, ErrLocalSchedulerSnapshotUnavailable) {
		t.Fatal(err)
	}
	if _, err := svc.GetGroupByID(ctx, 1); !errors.Is(err, ErrLocalSchedulerSnapshotUnavailable) {
		t.Fatal(err)
	}
	if _, err := svc.GetGroupByIDLite(ctx, 1); !errors.Is(err, ErrLocalSchedulerSnapshotUnavailable) {
		t.Fatal(err)
	}
}

func TestRegionalSchedulerRejectsConflictingDetails(t *testing.T) {
	a := &Account{ID: 1, Platform: PlatformOpenAI, Credentials: map[string]any{"api_key": "a"}}
	b := &Account{ID: 1, Platform: PlatformOpenAI, Credentials: map[string]any{"api_key": "b"}}
	doc := RegionalSchedulerDocument{Version: 1, Buckets: []RegionalSchedulerBucket{
		{Bucket: SchedulerBucket{Platform: PlatformOpenAI, Mode: SchedulerModeSingle}, Accounts: []*Account{a}},
		{Bucket: SchedulerBucket{Platform: PlatformOpenAI, Mode: SchedulerModeForced}, Accounts: []*Account{b}},
	}}
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegionalSchedulerValidator([]int64{1})(payload); err == nil {
		t.Fatal("conflicting credentials accepted")
	}
}

func TestRegionalSchedulerPreSendRechecksDoNotUsePrimaryOrAdmissionCache(t *testing.T) {
	ctx := context.Background()
	replica := regionalTestReplica(t, filepath.Join(t.TempDir(), "state"), true)
	selected := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "fixture"}}
	if err := replica.PublishNext(ctx, "test", time.Minute, regionalDoc(t, selected)); err != nil {
		t.Fatal(err)
	}
	reader, err := NewRegionalSchedulerReader(replica)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := NewSchedulerSnapshotService(nil, nil, nil, nil, nil)
	snapshot.SetLocalSnapshotReader(reader)
	primary := &turnAdmissionRepo{err: errors.New("must never access primary")}
	svc := &OpenAIGatewayService{schedulerSnapshot: snapshot, accountRepo: primary, requireLatestTurnAdmission: true}
	if _, err := svc.latestOpenAITurnAccountForGroup(ctx, selected, 0, true); err != nil {
		t.Fatal(err)
	}
	if primary.reads != 0 {
		t.Fatal("regional admission accessed primary")
	}
	// An older process cache must not keep a removed account admissible.
	svc.openAITurnAdmissionCache.store(selected, nil)
	if err := replica.PublishNext(ctx, "test", time.Minute, regionalDoc(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.latestOpenAITurnAccountForGroup(ctx, selected, 0, true); !IsOpenAITurnAdmissionError(err) {
		t.Fatalf("revocation ignored: %v", err)
	}
	if primary.reads != 0 {
		t.Fatal("regional admission fell back after revocation")
	}
}

func TestRegionalSchedulerProcurementCatalogAtomicRefreshAndRestore(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "procurement")
	replica := regionalTestReplica(t, path, true)
	source := regionalTestReplica(t, filepath.Join(t.TempDir(), "procurement-source"), true)
	server := httptest.NewTLSServer(source.Handler())
	defer server.Close()
	group := profitControlTestGroup(10, 0, 0)
	group.ProcurementPolicy = &ProcurementPolicy{Version: 3, Revision: 2, Sources: &domain.ProcurementSources{}, Buckets: &domain.ProcurementBuckets{Codes: []string{"low"}}}
	group.ProcurementPolicy.Version, group.ProcurementPolicy.Quality, group.ProcurementPolicy.Availability = 5, "not_required", "not_required"
	account := profitControlTestAccountWithRate(upstreamCostTestOAuthAccount(1), .025)
	account.Status, account.Schedulable, account.GroupIDs = StatusActive, true, []int64{group.ID}
	doc := RegionalSchedulerDocument{Version: 3, Groups: []*Group{group}, ProcurementCatalog: procurementBucketTestCatalog(), Buckets: []RegionalSchedulerBucket{{Bucket: SchedulerBucket{GroupID: group.ID, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}, Accounts: []*Account{account}}}, Availability: []RegionalAccountAvailability{{AccountID: 1, Reports: []*AvailabilityReport{}}}}
	publish := func(lifetime time.Duration) {
		t.Helper()
		raw, err := json.Marshal(doc)
		require.NoError(t, err)
		require.NoError(t, source.PublishNext(ctx, "test", lifetime, raw))
		require.NoError(t, replica.Pull(ctx, server.URL, server.Client().Transport))
	}
	publish(time.Minute)
	reader, err := NewRegionalSchedulerReader(replica)
	require.NoError(t, err)
	snapshot := NewSchedulerSnapshotService(nil, nil, nil, nil, nil)
	snapshot.SetLocalSnapshotReader(reader)
	gateway := &OpenAIGatewayService{schedulerSnapshot: snapshot}
	request, _ := gateway.WithOpenAIRequestPricingContext(profitControlTestCtx(group), &group.ID)
	veto, reason := openAIProfitControlLatestPolicyVetoReason(request, account)
	require.False(t, veto, reason)
	loaded, err := snapshot.GetGroupByIDLite(ctx, group.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, loaded.ProcurementCatalog.Revision)
	*loaded.ProcurementCatalog.Buckets[0].MaxRate = "0.021"
	loaded, err = snapshot.GetGroupByIDLite(ctx, group.ID)
	require.NoError(t, err)
	require.Equal(t, "0.03", *loaded.ProcurementCatalog.Buckets[0].MaxRate)
	reports, err := reader.ReadAccountAvailability(ctx, account)
	require.NoError(t, err)
	require.Empty(t, reports, "v3 retains v2 availability coverage")
	doc.ProcurementCatalog = doc.ProcurementCatalog.WithRevision(2)
	upper := "0.025"
	doc.ProcurementCatalog.Buckets[0].MaxRate = &upper
	publish(time.Minute)
	veto, reason = openAIProfitControlLatestPolicyVetoReason(request, account)
	require.True(t, veto)
	require.Equal(t, "procurement_bucket", reason)
	original := request.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.EqualValues(t, 1, original.procurementCatalog.Revision, "terminal refresh never mutates a shared request snapshot")
	restored := regionalTestReplica(t, path, false)
	restoredReader, err := NewRegionalSchedulerReader(restored)
	require.NoError(t, err)
	snapshot.SetLocalSnapshotReader(restoredReader)
	veto, reason = openAIProfitControlLatestPolicyVetoReason(request, account)
	require.True(t, veto)
	require.Equal(t, "procurement_bucket", reason)
	doc.ProcurementCatalog = procurementBucketTestCatalog().WithRevision(3)
	publish(time.Minute)
	snapshot.SetLocalSnapshotReader(reader)
	veto, reason = openAIProfitControlLatestPolicyVetoReason(request, account)
	require.False(t, veto, reason)
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	for _, invalid := range []func(*RegionalSchedulerDocument){
		func(d *RegionalSchedulerDocument) { d.Version = 2 },
		func(d *RegionalSchedulerDocument) { d.ProcurementCatalog = nil },
		func(d *RegionalSchedulerDocument) { d.ProcurementCatalog.Version = 99 },
		func(d *RegionalSchedulerDocument) { d.Availability = nil },
		func(d *RegionalSchedulerDocument) { d.Groups[0].ProcurementPolicy.Buckets = nil },
	} {
		var broken RegionalSchedulerDocument
		require.NoError(t, json.Unmarshal(raw, &broken))
		invalid(&broken)
		payload, err := json.Marshal(broken)
		require.NoError(t, err)
		require.Error(t, RegionalSchedulerValidator([]int64{1})(payload))
	}
	doc.ProcurementCatalog.Buckets = doc.ProcurementCatalog.Buckets[1:]
	publish(time.Minute)
	veto, reason = openAIProfitControlLatestPolicyVetoReason(request, account)
	require.True(t, veto)
	require.Equal(t, "procurement_catalog_unavailable", reason)
	doc.ProcurementCatalog = procurementBucketTestCatalog().WithRevision(4)
	publish(time.Second)
	require.Eventually(t, func() bool { _, err := snapshot.GetGroupByIDLite(ctx, group.ID); return err != nil }, 2*time.Second, 10*time.Millisecond)
	veto, reason = openAIProfitControlLatestPolicyVetoReason(request, account)
	require.True(t, veto)
	require.Equal(t, "procurement_policy_unavailable", reason)
	publish(time.Minute)
	veto, reason = openAIProfitControlLatestPolicyVetoReason(request, account)
	require.False(t, veto, reason)
	// A region keeps the explicit v5 constraint across updates, and consumes
	// only its own installed authority. Missing observations never use primary.
	request = context.WithValue(request, procurementActualScopeKey{}, func() *AvailabilityRequestScope {
		return &AvailabilityRequestScope{Version: 2, Protocol: "responses_http", Model: "gpt-fixture", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}
	})
	group.ProcurementPolicy.Availability = "current_state"
	publish(time.Minute)
	veto, reason = openAIProfitControlLatestPolicyVetoReason(request, account)
	require.True(t, veto)
	require.Equal(t, "procurement_availability_unavailable", reason)
	group.ProcurementPolicy.Availability = "not_required"
	publish(time.Minute)
	veto, reason = openAIProfitControlLatestPolicyVetoReason(request, account)
	require.False(t, veto, reason)
}
