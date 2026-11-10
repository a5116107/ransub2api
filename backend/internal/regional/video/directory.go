// Package video defines the metadata-only regional async task directory.
// Directory evidence never grants the right to execute or release a task.
package video

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/regional"
)

const (
	PrimaryAuthority = "regional:primary"
	SchemaVersion    = 1
	Lifetime         = 30 * time.Second
	MaxEntries       = 128
)

var (
	ErrUnavailable  = errors.New("regional video directory unavailable or incomplete")
	ErrConflict     = errors.New("regional video directory ownership or revision conflict")
	ErrTaskNotLocal = errors.New("async video task belongs to another regional authority")
	ErrTaskUnknown  = errors.New("async video task is absent from a fresh regional directory")
	memberPattern   = regexp.MustCompile(`^video:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	taskPattern     = regexp.MustCompile(`^concurrency:upstream_key:async_video:task:v2:[0-9a-f]{64}$`)
)

// Owner includes routing identity, never credentials. The downstream principal
// is necessary for both task uniqueness and control-peer scope enforcement.
type Owner struct {
	UserID           int64    `json:"user_id"`
	APIKeyID         int64    `json:"api_key_id"`
	GroupID          int64    `json:"group_id"`
	AccountID        int64    `json:"account_id"`
	Provider         string   `json:"provider"`
	RouteFingerprint [32]byte `json:"route_fingerprint"`
}

func (o Owner) ValidPrincipal() bool {
	return o.UserID > 0 && o.APIKeyID > 0 && o.GroupID >= 0 &&
		(o.Provider == "grok" || o.Provider == "seedance" || o.Provider == "minimax-h3")
}

func (o Owner) Valid() bool {
	return o.ValidPrincipal() && o.AccountID > 0 && o.RouteFingerprint != [32]byte{}
}

// TaskKey is the existing v2 tenant/provider index. Account and route are
// deliberately excluded: a different route must not overwrite the same task.
func TaskKey(owner Owner, taskID string) string {
	if !owner.ValidPrincipal() || strings.TrimSpace(taskID) == "" {
		return ""
	}
	value, _ := json.Marshal(struct {
		UserID   int64  `json:"user_id"`
		APIKeyID int64  `json:"api_key_id"`
		GroupID  int64  `json:"group_id"`
		Provider string `json:"provider"`
		TaskID   string `json:"task_id"`
	}{owner.UserID, owner.APIKeyID, owner.GroupID, owner.Provider, strings.TrimSpace(taskID)})
	sum := sha256.Sum256(value)
	return "concurrency:upstream_key:async_video:task:v2:" + hex.EncodeToString(sum[:])
}

func ValidMember(member string) bool { return memberPattern.MatchString(member) }

func LocalAuthority(region string) string { return "regional:local:" + region }

func ValidAuthority(authority string) bool {
	if authority == PrimaryAuthority {
		return true
	}
	if !strings.HasPrefix(authority, "regional:local:") {
		return false
	}
	return (regional.Config{RegionID: strings.TrimPrefix(authority, "regional:local:"), MaxSnapshotAgeSeconds: 1}).Validate() == nil
}

type Entry struct {
	TaskKey string `json:"task_key"`
	Member  string `json:"member"`
	Owner   Owner  `json:"owner"`
	Fenced  bool   `json:"fenced"`
}

func (e Entry) Valid() bool {
	return taskPattern.MatchString(e.TaskKey) && ValidMember(e.Member) && e.Owner.Valid()
}

// Snapshot is a complete bounded view of one source's bound tasks. Unknown
// submissions lack a task key and remain solely in the original lease store.
// Empty Entries is explicit evidence, not a missing or truncated page.
type Snapshot struct {
	SchemaVersion int       `json:"schema_version"`
	Authority     string    `json:"authority"`
	Revision      int64     `json:"revision,string"`
	IssuedAt      time.Time `json:"issued_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Entries       []Entry   `json:"entries"`
}

func (s Snapshot) Validate() error {
	if s.SchemaVersion != SchemaVersion || !ValidAuthority(s.Authority) || s.Revision <= 0 || s.IssuedAt.IsZero() ||
		!s.ExpiresAt.After(s.IssuedAt) || s.ExpiresAt.Sub(s.IssuedAt) > Lifetime ||
		s.Entries == nil || len(s.Entries) > MaxEntries {
		return ErrUnavailable
	}
	members := make(map[string]bool, len(s.Entries))
	previous := ""
	for _, entry := range s.Entries {
		if !entry.Valid() || entry.TaskKey <= previous || members[entry.Member] {
			return ErrConflict
		}
		previous = entry.TaskKey
		members[entry.Member] = true
	}
	return nil
}

// Fresh uses the reading SQL authority's clock. A small future-clock tolerance
// never extends the original expiry and never refreshes an identical revision.
func (s Snapshot) Fresh(now time.Time) bool {
	return s.Validate() == nil && !now.IsZero() && !s.IssuedAt.After(now.Add(5*time.Second)) && now.Before(s.ExpiresAt)
}

func (s Snapshot) Equal(other Snapshot) bool {
	return s.SchemaVersion == other.SchemaVersion && s.Authority == other.Authority && s.Revision == other.Revision &&
		s.IssuedAt.Equal(other.IssuedAt) && s.ExpiresAt.Equal(other.ExpiresAt) && slices.Equal(s.Entries, other.Entries)
}

// Directory covers the existing control listener's primary/assigned-region
// pair. It is not evidence that arbitrary unconfigured regions are covered.
type Directory struct {
	Snapshots []Snapshot `json:"snapshots"`
}

func (d Directory) Validate(region string) error {
	if !ValidAuthority(LocalAuthority(region)) || len(d.Snapshots) != 2 {
		return ErrUnavailable
	}
	seen := make(map[string]bool, 2)
	for _, snapshot := range d.Snapshots {
		if err := snapshot.Validate(); err != nil {
			return err
		}
		if seen[snapshot.Authority] || (snapshot.Authority != PrimaryAuthority && snapshot.Authority != LocalAuthority(region)) {
			return ErrConflict
		}
		seen[snapshot.Authority] = true
	}
	return nil
}
