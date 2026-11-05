package slots

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"sort"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

var upstreamFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)

const UpstreamPolicyLifetime = 15 * time.Second

// UpstreamPolicy travels only over the authenticated background control channel.
// Its complete policy distinguishes an unrestricted credential from a missing
// grant. Registry IDs are assigned by the primary, never derived from hashes.
type UpstreamPolicy struct {
	Policy    config.UpstreamKeyConcurrencyConfig `json:"policy"`
	Resources map[string]int64                    `json:"resources"`
	Overdrawn map[string]bool                     `json:"overdrawn,omitempty"`
	IssuedAt  time.Time                           `json:"issued_at"`
	ExpiresAt time.Time                           `json:"expires_at"`
}

func (p UpstreamPolicy) Validate() error {
	if err := p.Policy.Validate(); err != nil {
		return err
	}
	if p.IssuedAt.IsZero() || !p.ExpiresAt.After(p.IssuedAt) || p.ExpiresAt.Sub(p.IssuedAt) > UpstreamPolicyLifetime || len(p.Resources) != len(p.Policy.Limits) {
		return ErrConflict
	}
	seen := map[int64]bool{}
	for fingerprint := range p.Policy.Limits {
		id := p.Resources[fingerprint]
		if id <= 0 || seen[id] {
			return ErrConflict
		}
		seen[id] = true
	}
	for fingerprint, blocked := range p.Overdrawn {
		if _, exists := p.Policy.Limits[fingerprint]; !exists || !blocked {
			return ErrConflict
		}
	}
	return nil
}

// PublishUpstreamPolicy is called by the primary provider before it admits
// requests. Removal advances the revision without deleting IDs or occupancy.
func (p *Primary) PublishUpstreamPolicy(ctx context.Context, policy config.UpstreamKeyConcurrencyConfig) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	raw, digest, err := policy.EncodeLimits()
	if err != nil {
		return err
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO regional_upstream_key_policy(singleton,revision,digest,limits) VALUES(true,$1,$2,$3) ON CONFLICT DO NOTHING`, policy.Revision, digest, string(raw)); err != nil {
		return err
	}
	var revision int64
	var current string
	if err = tx.QueryRowContext(ctx, `SELECT revision,digest FROM regional_upstream_key_policy WHERE singleton FOR UPDATE`).Scan(&revision, &current); err != nil {
		return err
	}
	if revision > policy.Revision || (revision == policy.Revision && current != digest) {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE regional_upstream_key_policy SET revision=$1,digest=$2,limits=$3 WHERE singleton`, policy.Revision, digest, string(raw)); err != nil {
		return err
	}
	keys := make([]string, 0, len(policy.Limits))
	for fingerprint := range policy.Limits {
		keys = append(keys, fingerprint)
	}
	sort.Strings(keys)
	for _, fingerprint := range keys {
		if _, err = tx.ExecContext(ctx, `INSERT INTO regional_upstream_keys(fingerprint) VALUES($1) ON CONFLICT(fingerprint) DO NOTHING`, fingerprint); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func completeUpstreamResource(ctx context.Context, tx *sql.Tx, r *Resource) error {
	if r.Kind != "upstream_key" {
		return nil
	}
	return tx.QueryRowContext(ctx, `SELECT fingerprint FROM regional_upstream_keys WHERE id=$1 FOR SHARE`, r.ID).Scan(&r.Fingerprint)
}

func checkUpstreamResource(ctx context.Context, tx *sql.Tx, r Resource) error {
	actual := r
	if err := completeUpstreamResource(ctx, tx, &actual); err != nil {
		return err
	}
	if actual != r {
		return ErrConflict
	}
	return nil
}

func installUpstreamResource(ctx context.Context, tx *sql.Tx, r Resource) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Kind == "upstream_key" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO regional_upstream_keys(id,fingerprint) VALUES($1,$2) ON CONFLICT(id) DO NOTHING`, r.ID, r.Fingerprint); err != nil {
			return err
		}
	}
	return checkUpstreamResource(ctx, tx, r)
}

func readUpstreamPolicy(ctx context.Context, tx *sql.Tx) (config.UpstreamKeyConcurrencyConfig, string, error) {
	var policy config.UpstreamKeyConcurrencyConfig
	var digest string
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT revision,digest,limits FROM regional_upstream_key_policy WHERE singleton FOR SHARE`).Scan(&policy.Revision, &digest, &raw)
	if err == nil {
		err = json.Unmarshal(raw, &policy.Limits)
	}
	if err == nil {
		err = policy.Validate()
	}
	if err == nil {
		_, actual, encodeErr := policy.EncodeLimits()
		if encodeErr != nil {
			err = encodeErr
		} else if actual != digest {
			err = ErrConflict
		}
	}
	return policy, digest, err
}

func (p *Primary) ReadUpstreamPolicy(ctx context.Context) (UpstreamPolicy, error) {
	var snapshot UpstreamPolicy
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return snapshot, err
	}
	defer func() { _ = tx.Rollback() }()
	snapshot.Policy, _, err = readUpstreamPolicy(ctx, tx)
	if err != nil {
		return snapshot, err
	}
	snapshot.Resources = make(map[string]int64, len(snapshot.Policy.Limits))
	snapshot.Overdrawn = make(map[string]bool)
	for fingerprint, limit := range snapshot.Policy.Limits {
		var id int64
		var overdrawn bool
		if err = tx.QueryRowContext(ctx, `SELECT k.id,COALESCE(b.primary_used+b.granted,0)>$2 FROM regional_upstream_keys k LEFT JOIN regional_slot_budgets b ON b.kind='upstream_key' AND b.resource_id=k.id WHERE k.fingerprint=$1`, fingerprint, limit).Scan(&id, &overdrawn); err != nil {
			return snapshot, err
		}
		snapshot.Resources[fingerprint] = id
		if overdrawn {
			snapshot.Overdrawn[fingerprint] = true
		}
	}
	snapshot.IssuedAt, err = now(ctx, tx)
	if err != nil {
		return snapshot, err
	}
	snapshot.ExpiresAt = snapshot.IssuedAt.Add(UpstreamPolicyLifetime)
	if err = snapshot.Validate(); err != nil {
		return snapshot, err
	}
	return snapshot, tx.Commit()
}

// InstallUpstreamPolicy never extends a received expiry or replaces newer
// evidence with a delayed reply. Failed refreshes leave the old deadline intact.
func (l *Local) InstallUpstreamPolicy(ctx context.Context, snapshot UpstreamPolicy) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	_, digest, err := snapshot.Policy.EncodeLimits()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	tx, err := l.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := now(ctx, tx)
	if err != nil {
		return err
	}
	if n.Before(snapshot.IssuedAt) || !n.Before(snapshot.ExpiresAt) {
		return ErrUnavailable
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO regional_upstream_key_policy_cache(region_id,revision,digest,issued_at,expires_at,payload) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, l.Region, snapshot.Policy.Revision, digest, snapshot.IssuedAt, snapshot.ExpiresAt, string(payload)); err != nil {
		return err
	}
	var revision int64
	var previousDigest string
	var issued time.Time
	if err = tx.QueryRowContext(ctx, `SELECT revision,digest,issued_at FROM regional_upstream_key_policy_cache WHERE region_id=$1 FOR UPDATE`, l.Region).Scan(&revision, &previousDigest, &issued); err != nil {
		return err
	}
	if revision > snapshot.Policy.Revision || (revision == snapshot.Policy.Revision && previousDigest != digest) || issued.After(snapshot.IssuedAt) {
		return ErrConflict
	}
	for fingerprint, id := range snapshot.Resources {
		if err = installUpstreamResource(ctx, tx, Resource{Kind: "upstream_key", ID: id, Fingerprint: fingerprint}); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE regional_upstream_key_policy_cache SET revision=$2,digest=$3,issued_at=$4,expires_at=$5,payload=$6 WHERE region_id=$1`, l.Region, snapshot.Policy.Revision, digest, snapshot.IssuedAt, snapshot.ExpiresAt, string(payload)); err != nil {
		return err
	}
	return tx.Commit()
}

// UpstreamResourceTx verifies policy and resolves identity in the acquisition
// transaction. A missing rule is unrestricted only under a confirmed policy.
func (p *Primary) UpstreamResourceTx(ctx context.Context, tx *sql.Tx, revision int64, digest, fingerprint string) (Resource, bool, error) {
	policy, actualDigest, err := readUpstreamPolicy(ctx, tx)
	if err != nil {
		return Resource{}, false, err
	}
	if policy.Revision != revision || actualDigest != digest {
		return Resource{}, false, ErrConflict
	}
	if _, limited := policy.Limits[fingerprint]; !limited {
		return Resource{}, false, nil
	}
	r := Resource{Kind: "upstream_key", Fingerprint: fingerprint}
	err = tx.QueryRowContext(ctx, `SELECT id FROM regional_upstream_keys WHERE fingerprint=$1`, fingerprint).Scan(&r.ID)
	return r, true, err
}

func (l *Local) UpstreamResourceTx(ctx context.Context, tx *sql.Tx, revision int64, digest, fingerprint string) (Resource, bool, error) {
	var raw []byte
	var current int64
	var actualDigest string
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT payload,revision,digest,clock_timestamp()>=issued_at AND clock_timestamp()<expires_at FROM regional_upstream_key_policy_cache WHERE region_id=$1 FOR SHARE`, l.Region).Scan(&raw, &current, &actualDigest, &valid)
	if err != nil {
		return Resource{}, false, err
	}
	if !valid || current != revision || actualDigest != digest {
		return Resource{}, false, ErrUnavailable
	}
	var snapshot UpstreamPolicy
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return Resource{}, false, err
	}
	if err := snapshot.Validate(); err != nil {
		return Resource{}, false, err
	}
	_, payloadDigest, err := snapshot.Policy.EncodeLimits()
	if err != nil || snapshot.Policy.Revision != current || payloadDigest != actualDigest {
		return Resource{}, false, ErrConflict
	}
	if _, limited := snapshot.Policy.Limits[fingerprint]; !limited {
		return Resource{}, false, nil
	}
	r := Resource{Kind: "upstream_key", ID: snapshot.Resources[fingerprint], Fingerprint: fingerprint}
	if err := r.Validate(); err != nil {
		return Resource{}, false, err
	}
	if snapshot.Overdrawn[fingerprint] {
		return r, true, ErrUnavailable
	}
	return r, true, nil
}

func UpstreamLimit(ctx context.Context, tx *sql.Tx, r Resource) (int, error) {
	if r.Kind != "upstream_key" {
		return 0, ErrConflict
	}
	if err := checkUpstreamResource(ctx, tx, r); err != nil {
		return 0, err
	}
	policy, _, err := readUpstreamPolicy(ctx, tx)
	return policy.Limits[r.Fingerprint], err
}
