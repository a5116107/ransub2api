//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/stretchr/testify/require"
)

func TestRegionalSchedulerProcurementJSONRejectsPartialPublish(t *testing.T) {
	group := profitControlTestGroup(7, 0, 0)
	group.ProcurementPolicy = &ProcurementPolicy{Version: 3, Revision: 1, Sources: &domain.ProcurementSources{}, Buckets: &domain.ProcurementBuckets{Codes: []string{"low"}}}
	doc := RegionalSchedulerDocument{Version: 3, Groups: []*Group{group}, ProcurementCatalog: procurementBucketTestCatalog(), Buckets: []RegionalSchedulerBucket{}, Availability: []RegionalAccountAvailability{}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	policy, err := json.Marshal(group.ProcurementPolicy)
	require.NoError(t, err)
	replica := regionalTestReplica(t, filepath.Join(t.TempDir(), "strict-json"), true)
	require.NoError(t, replica.PublishNext(t.Context(), "test", time.Minute, raw))
	before, err := replica.Current()
	require.NoError(t, err)
	for name, broken := range map[string]string{
		"missing policy bound":      strings.Replace(string(raw), string(policy), strings.Replace(string(policy), `"max_rate":null,`, "", 1), 1),
		"duplicate policy version":  strings.Replace(string(raw), string(policy), strings.Replace(string(policy), `"version":3`, `"version":1,"version":3`, 1), 1),
		"overwritten bucket filter": strings.Replace(string(raw), string(policy), strings.Replace(string(policy), `"buckets":{"codes":["low"]}`, `"buckets":{"codes":["low"]},"buckets":{"codes":null}`, 1), 1),
		"missing catalog bound":     strings.Replace(string(raw), `"max_rate":"0.03"`, `"unused":null`, 1),
		"overwritten catalog bound": strings.Replace(string(raw), `"max_rate":"0.03"`, `"max_rate":"0.03","max_rate":null`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			require.NotEqual(t, string(raw), broken)
			require.Error(t, replica.PublishNext(t.Context(), "test", time.Minute, []byte(broken)))
			after, err := replica.Current()
			require.NoError(t, err)
			require.Equal(t, before.Revision, after.Revision)
			require.Equal(t, before.ExpiresAt, after.ExpiresAt)
			require.Equal(t, before.Payload, after.Payload, "a rejected document must leave the last complete view intact")
		})
	}
	require.NoError(t, replica.PublishNext(t.Context(), "test", time.Minute, raw))
	after, err := replica.Current()
	require.NoError(t, err)
	require.Greater(t, after.Revision, before.Revision)
	loaded, err := decodeRegionalScheduler(after.Payload)
	require.NoError(t, err)
	require.Equal(t, []string{"low"}, loaded.Groups[0].ProcurementPolicy.Buckets.Codes)
}

func regionalTestReplica(t *testing.T, path string, bootstrap bool) *regional.Replica {
	t.Helper()
	codec, err := regional.NewCodec(map[string][]byte{"test": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := regional.OpenReplica(path, regional.Config{RegionID: "us", MaxSnapshotAgeSeconds: 60}, regional.SnapshotScheduler, codec, RegionalSchedulerValidator([]int64{1, 2}), bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func regionalDoc(t *testing.T, accounts ...*Account) []byte {
	t.Helper()
	if accounts == nil {
		accounts = []*Account{}
	}
	b, err := json.Marshal(RegionalSchedulerDocument{Version: 1, Buckets: []RegionalSchedulerBucket{{Bucket: SchedulerBucket{Platform: PlatformOpenAI, Mode: SchedulerModeSingle}, Accounts: accounts}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestRegionalSchedulerSyncActivationAndRestore(t *testing.T) {
	ctx := context.Background()
	source := regionalTestReplica(t, filepath.Join(t.TempDir(), "source"), true)
	targetPath := filepath.Join(t.TempDir(), "target")
	target := regionalTestReplica(t, targetPath, true)
	account := &Account{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true}
	if err := source.PublishNext(ctx, "test", time.Minute, regionalDoc(t, account)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(source.Handler())
	defer server.Close()
	if err := target.Pull(ctx, server.URL, server.Client().Transport); err != nil {
		t.Fatal(err)
	}
	reader, err := NewRegionalSchedulerReader(target)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewSchedulerSnapshotService(nil, nil, nil, nil, nil)
	svc.SetLocalSnapshotReader(reader)
	got, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	if err != nil || len(got) != 1 {
		t.Fatalf("activation: %v", err)
	}
	account.Status = "disabled"
	if err := source.PublishNext(ctx, "test", time.Minute, regionalDoc(t, account)); err != nil {
		t.Fatal(err)
	}
	if err := target.Pull(ctx, server.URL, server.Client().Transport); err != nil {
		t.Fatal(err)
	}
	got, _, err = svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	if err != nil || len(got) != 0 {
		t.Fatal("disabled account remained selectable")
	}
	// Whole bucket deletion also removes the previous members.
	if err := source.PublishNext(ctx, "test", time.Minute, []byte(`{"version":1,"buckets":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := target.Pull(ctx, server.URL, server.Client().Transport); err != nil {
		t.Fatal(err)
	}
	restored := regionalTestReplica(t, targetPath, false)
	restoredReader, err := NewRegionalSchedulerReader(restored)
	if err != nil {
		t.Fatal(err)
	}
	svc.SetLocalSnapshotReader(restoredReader)
	got, _, err = svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	if err != nil || len(got) != 0 {
		t.Fatal("restart resurrected deleted bucket")
	}
}
func TestRegionalSchedulerRejectsForeignAndBrokenDocuments(t *testing.T) {
	validate := RegionalSchedulerValidator([]int64{1})
	for _, a := range []*Account{
		{ID: 2, Platform: PlatformOpenAI},
		{ID: 1, Platform: PlatformAnthropic},
		{ID: 0, Platform: PlatformOpenAI},
	} {
		if err := validate(regionalDoc(t, a)); err == nil {
			t.Fatal("invalid account accepted")
		}
	}
	if err := validate(regionalDoc(t, &Account{ID: 1, Platform: PlatformOpenAI}, &Account{ID: 1, Platform: PlatformOpenAI})); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := validate([]byte(`{"version":1,"buckets":[]} {}`)); err == nil {
		t.Fatal("trailing data accepted")
	}
	if err := validate([]byte(`{"version":2,"buckets":[]}`)); err == nil {
		t.Fatal("unknown version accepted")
	}
}

type regionalExportRepo struct {
	AccountRepository
	accounts          []Account
	fail              bool
	availabilityErr   error
	availabilityReads int
}

func (r *regionalExportRepo) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]Account, error) {
	if r.fail {
		return nil, errors.New("db unavailable")
	}
	return r.accounts, nil
}

func (r *regionalExportRepo) ReadAccountAvailability(context.Context, *Account) ([]*AvailabilityReport, error) {
	r.availabilityReads++
	return []*AvailabilityReport{}, r.availabilityErr
}
func TestRegionalSchedulerExportFiltersAssignmentAndPreservesOnFailure(t *testing.T) {
	ctx := context.Background()
	repo := &regionalExportRepo{accounts: []Account{{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true}, {ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true}}}
	svc := NewSchedulerSnapshotService(nil, nil, repo, nil, nil)
	replica := regionalTestReplica(t, filepath.Join(t.TempDir(), "state"), true)
	buckets := []SchedulerBucket{{Platform: PlatformOpenAI, Mode: SchedulerModeSingle}}
	if err := svc.PublishRegionalScheduler(ctx, replica, "test", time.Minute, buckets, []int64{1}); err != nil {
		t.Fatal(err)
	}
	s, err := replica.Current()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := decodeRegionalScheduler(s.Payload)
	if err != nil || len(doc.Buckets[0].Accounts) != 1 || doc.Buckets[0].Accounts[0].ID != 1 {
		t.Fatal("foreign account exported")
	}
	repo.fail = true
	if err := svc.PublishRegionalScheduler(ctx, replica, "test", time.Minute, buckets, []int64{1}); err == nil {
		t.Fatal("partial export published")
	}
	after, err := replica.Current()
	if err != nil || after.Revision != s.Revision || !after.ExpiresAt.Equal(s.ExpiresAt) {
		t.Fatal("failure refreshed snapshot")
	}
	repo.fail, repo.availabilityErr = false, errors.New("availability authority unavailable")
	if err := svc.PublishRegionalScheduler(ctx, replica, "test", time.Minute, buckets, []int64{1}); err == nil {
		t.Fatal("availability failure published a partial view")
	}
	after, err = replica.Current()
	if err != nil || after.Revision != s.Revision || !after.ExpiresAt.Equal(s.ExpiresAt) {
		t.Fatal("availability failure refreshed snapshot")
	}
	repo.availabilityErr, repo.availabilityReads = nil, 0
	buckets = append(buckets, SchedulerBucket{Platform: PlatformOpenAI, Mode: SchedulerModeForced})
	if err := svc.PublishRegionalScheduler(ctx, replica, "test", time.Minute, buckets, []int64{1}); err != nil {
		t.Fatal(err)
	}
	if repo.availabilityReads != 1 {
		t.Fatal("repeated account exported more than one availability view")
	}
	// Exposing only the original account interface must not silently turn
	// unavailable rule authority into an assertion that no rules exist.
	accountOnly := struct{ AccountRepository }{repo}
	missing := NewSchedulerSnapshotService(nil, nil, accountOnly, nil, nil)
	if _, err := missing.ExportRegionalScheduler(ctx, buckets, []int64{1}); err == nil {
		t.Fatal("missing availability authority accepted")
	}
}
func TestRegionalSchedulerWorkerCancellation(t *testing.T) {
	r := regionalTestReplica(t, filepath.Join(t.TempDir(), "state"), true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunRegionalSchedulerSync(ctx, r, "https://unused.invalid", nil, time.Second, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type regionalProcurementGroupRepo struct {
	GroupRepository
	groups []*Group
	reads  int
}

func (r *regionalProcurementGroupRepo) GetByIDLite(context.Context, int64) (*Group, error) {
	g := r.groups[r.reads%len(r.groups)]
	r.reads++
	return g, nil
}

func TestRegionalSchedulerProcurementExportRejectsConflictingAuthority(t *testing.T) {
	group := profitControlTestGroup(7, 0, 0)
	group.ProcurementPolicy = &ProcurementPolicy{Version: 3, Revision: 1, Sources: &domain.ProcurementSources{}, Buckets: &domain.ProcurementBuckets{Codes: []string{"low"}}}
	group.ProcurementCatalog = procurementBucketTestCatalog()
	newer := *group
	newer.ProcurementCatalog = group.ProcurementCatalog.WithRevision(2)
	groups := &regionalProcurementGroupRepo{groups: []*Group{group, &newer}}
	accounts := &regionalExportRepo{accounts: []Account{{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, GroupIDs: []int64{7}}}}
	svc := NewSchedulerSnapshotService(nil, nil, accounts, groups, nil)
	buckets := []SchedulerBucket{{GroupID: 7, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}, {GroupID: 7, Platform: PlatformOpenAI, Mode: SchedulerModeForced}}
	replica := regionalTestReplica(t, filepath.Join(t.TempDir(), "export"), true)
	require.NoError(t, svc.PublishRegionalScheduler(t.Context(), replica, "test", time.Minute, buckets[:1], []int64{1}))
	before, err := replica.Current()
	require.NoError(t, err)
	doc, err := decodeRegionalScheduler(before.Payload)
	require.NoError(t, err)
	require.Equal(t, 3, doc.Version)
	require.EqualValues(t, 1, doc.ProcurementCatalog.Revision)
	groups.reads = 0
	require.ErrorContains(t, svc.PublishRegionalScheduler(t.Context(), replica, "test", time.Minute, buckets, []int64{1}), "conflicting regional procurement catalogs")
	after, err := replica.Current()
	require.NoError(t, err)
	require.Equal(t, before.Revision, after.Revision)
	require.Equal(t, before.ExpiresAt, after.ExpiresAt)
	groups.groups = []*Group{group}
	group.ProcurementCatalog = nil
	require.ErrorContains(t, svc.PublishRegionalScheduler(t.Context(), replica, "test", time.Minute, buckets[:1], []int64{1}), "requires procurement catalog authority")
	group.ProcurementCatalog = procurementBucketTestCatalog().WithRevision(3)
	require.NoError(t, svc.PublishRegionalScheduler(t.Context(), replica, "test", time.Minute, buckets, []int64{1}))
	group.ProcurementPolicy = procurementSourceGroupTestPolicy()
	group.ProcurementSources = procurementSourceGroupTestSnapshot(group.ID, []int64{1})
	require.NoError(t, svc.PublishRegionalScheduler(t.Context(), replica, "test", time.Minute, buckets, []int64{1}))
	before, err = replica.Current()
	require.NoError(t, err)
	doc, err = decodeRegionalScheduler(before.Payload)
	require.NoError(t, err)
	require.Equal(t, 4, doc.Version)
	local, err := NewRegionalSchedulerReader(replica)
	require.NoError(t, err)
	receiver := NewSchedulerSnapshotService(nil, nil, nil, nil, nil)
	receiver.SetLocalSnapshotReader(local)
	current, err := receiver.GetGroupByIDLite(t.Context(), group.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{1}, current.ProcurementSources.AccountIDs)
	group.ProcurementSources = nil
	require.ErrorContains(t, svc.PublishRegionalScheduler(t.Context(), replica, "test", time.Minute, buckets, []int64{1}), "requires procurement source authority")
	after, err = replica.Current()
	require.NoError(t, err)
	require.Equal(t, before.Revision, after.Revision)
	require.Equal(t, before.ExpiresAt, after.ExpiresAt)
	group.ProcurementSources = procurementSourceGroupTestSnapshot(group.ID, []int64{})
	require.NoError(t, svc.PublishRegionalScheduler(t.Context(), replica, "test", time.Minute, buckets, []int64{1}))
	current, err = receiver.GetGroupByIDLite(t.Context(), group.ID)
	require.NoError(t, err)
	require.Empty(t, current.ProcurementSources.AccountIDs)
	newer = *group
	newer.ProcurementSources = procurementSourceGroupTestSnapshot(group.ID, []int64{1})
	groups.groups = []*Group{group, &newer}
	groups.reads = 0
	require.ErrorContains(t, svc.PublishRegionalScheduler(t.Context(), replica, "test", time.Minute, buckets, []int64{1}), "conflicting regional procurement sources")

}

func TestRegionalSchedulerProcurementSourceGroupsAuthority(t *testing.T) {
	group := profitControlTestGroup(7, 0, 0)
	group.ProcurementPolicy = procurementSourceGroupTestPolicy()
	source := procurementSourceGroupTestSnapshot(7, []int64{901})
	doc := RegionalSchedulerDocument{Version: 4, Groups: []*Group{group}, ProcurementCatalog: procurementBucketTestCatalog(), ProcurementSources: []*domain.ProcurementSourceSnapshot{source}, Buckets: []RegionalSchedulerBucket{}, Availability: []RegionalAccountAvailability{}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	decoded, err := decodeRegionalScheduler(raw)
	require.NoError(t, err)
	require.Equal(t, []int64{901}, decoded.Groups[0].ProcurementSources.AccountIDs)
	decoded.Groups[0].ProcurementSources.AccountIDs[0] = 999
	require.Equal(t, int64(901), source.AccountIDs[0])
	for _, change := range []func(*RegionalSchedulerDocument){
		func(d *RegionalSchedulerDocument) { d.Version = 3 }, func(d *RegionalSchedulerDocument) { d.ProcurementSources = nil }, func(d *RegionalSchedulerDocument) {
			d.ProcurementSources = append(d.ProcurementSources, source.Clone())
		},
		func(d *RegionalSchedulerDocument) { d.ProcurementSources[0].PolicyRevision++ }, func(d *RegionalSchedulerDocument) { d.ProcurementSources[0].ProductGroupID++ }, func(d *RegionalSchedulerDocument) { d.ProcurementSources[0].GroupIDs = []int64{701} },
	} {
		copy := doc
		copy.ProcurementSources = []*domain.ProcurementSourceSnapshot{source.Clone()}
		change(&copy)
		raw, err := json.Marshal(copy)
		require.NoError(t, err)
		_, err = decodeRegionalScheduler(raw)
		require.Error(t, err)
	}
}
