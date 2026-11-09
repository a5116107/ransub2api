package video

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func directoryFixture() Snapshot {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return Snapshot{SchemaVersion: SchemaVersion, Authority: LocalAuthority("us"), Revision: 1, IssuedAt: now, ExpiresAt: now.Add(Lifetime), Entries: []Entry{{
		TaskKey: "concurrency:upstream_key:async_video:task:v2:" + strings.Repeat("a", 64),
		Member:  "video:00000000-0000-4000-8000-000000000001",
		Owner:   Owner{UserID: 1, APIKeyID: 2, GroupID: 0, AccountID: 3, Provider: "grok", RouteFingerprint: [32]byte{1}},
	}}}
}

func TestDirectorySnapshotValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Snapshot)
	}{
		{"schema", func(s *Snapshot) { s.SchemaVersion++ }},
		{"authority", func(s *Snapshot) { s.Authority = "regional:local:../us" }},
		{"empty region", func(s *Snapshot) { s.Authority = LocalAuthority("") }},
		{"zero revision", func(s *Snapshot) { s.Revision = 0 }},
		{"unissued", func(s *Snapshot) { s.IssuedAt = time.Time{} }},
		{"long lifetime", func(s *Snapshot) { s.ExpiresAt = s.ExpiresAt.Add(time.Microsecond) }},
		{"no lifetime", func(s *Snapshot) { s.ExpiresAt = s.IssuedAt }},
		{"missing entries", func(s *Snapshot) { s.Entries = nil }},
		{"overfull", func(s *Snapshot) { s.Entries = make([]Entry, MaxEntries+1) }},
		{"task key", func(s *Snapshot) { s.Entries[0].TaskKey = "raw-task-id" }},
		{"member", func(s *Snapshot) { s.Entries[0].Member = "video:not-a-uuid" }},
		{"owner", func(s *Snapshot) { s.Entries[0].Owner.UserID = 0 }},
		{"route", func(s *Snapshot) { s.Entries[0].Owner.RouteFingerprint = [32]byte{} }},
		{"provider", func(s *Snapshot) { s.Entries[0].Owner.Provider = "unknown" }},
		{"duplicate task", func(s *Snapshot) { s.Entries = append(s.Entries, s.Entries[0]) }},
		{"duplicate member", func(s *Snapshot) {
			e := s.Entries[0]
			e.TaskKey = strings.ReplaceAll(e.TaskKey, "aaaa", "bbbb")
			s.Entries = append(s.Entries, e)
		}},
		{"unordered", func(s *Snapshot) {
			e := s.Entries[0]
			e.TaskKey = strings.ReplaceAll(e.TaskKey, "aaaa", "0000")
			e.Member = "video:00000000-0000-4000-8000-000000000002"
			s.Entries = append(s.Entries, e)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := directoryFixture()
			tc.edit(&s)
			require.Error(t, s.Validate())
		})
	}
	s := directoryFixture()
	require.NoError(t, s.Validate())
	require.True(t, s.Fresh(s.IssuedAt))
	require.True(t, s.Fresh(s.IssuedAt.Add(-5*time.Second)))
	require.False(t, s.Fresh(s.IssuedAt.Add(-5*time.Second-time.Microsecond)))
	require.False(t, s.Fresh(s.ExpiresAt))
	require.False(t, s.Fresh(time.Time{}))
	s.Entries = []Entry{}
	require.NoError(t, s.Validate(), "an explicit complete empty stream is valid")
}

func TestDirectoryRequiresBothAssignedAuthorities(t *testing.T) {
	local := directoryFixture()
	primary := directoryFixture()
	primary.Authority = PrimaryAuthority
	require.NoError(t, (Directory{Snapshots: []Snapshot{primary, local}}).Validate("us"))
	for _, snapshots := range [][]Snapshot{nil, {local}, {local, local}, {primary, primary}, {primary, local, local}} {
		require.Error(t, (Directory{Snapshots: snapshots}).Validate("us"))
	}
	require.Error(t, (Directory{Snapshots: []Snapshot{primary, local}}).Validate("eu"))
	require.Error(t, (Directory{Snapshots: []Snapshot{primary, local}}).Validate(""))
	copy := local
	copy.IssuedAt = local.IssuedAt.In(time.FixedZone("other", 3600))
	copy.ExpiresAt = local.ExpiresAt.In(time.FixedZone("other", 3600))
	require.True(t, local.Equal(copy), "clock representation does not change snapshot identity")
	copy.Revision++
	require.False(t, local.Equal(copy))
}
