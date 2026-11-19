package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProcurementCatalog classifies inventory for explicit product bucket filters.
// It never owns group membership, credentials or model-quality qualifications.
type ProcurementCatalog struct {
	Version  int                 `json:"version"`
	Revision int64               `json:"revision"`
	Buckets  []ProcurementBucket `json:"buckets"`
}

func (c *ProcurementCatalog) UnmarshalJSON(raw []byte) error {
	fields, err := procurementJSONFields(raw, []string{"version", "revision", "buckets"})
	if err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(fields["revision"]), []byte("null")) {
		return errors.New("procurement catalog requires an explicit revision")
	}
	var buckets []json.RawMessage
	if err := json.Unmarshal(fields["buckets"], &buckets); err != nil {
		return err
	}
	for _, bucket := range buckets {
		if _, err := procurementJSONFields(bucket, []string{"code", "name", "min_rate", "max_rate"}); err != nil {
			return err
		}
	}
	type contract ProcurementCatalog
	var next contract
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	catalog := ProcurementCatalog(next)
	if err := catalog.ValidateDraft(); err != nil {
		return err
	}
	*c = catalog
	return nil
}

type ProcurementBucket struct {
	Code    string  `json:"code"`
	Name    string  `json:"name"`
	MinRate *string `json:"min_rate"`
	MaxRate *string `json:"max_rate"`
}

type ProcurementGap struct {
	MinRate *string `json:"min_rate"`
	MaxRate *string `json:"max_rate"`
}

var procurementBucketCode = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func (b ProcurementBucket) policy() *ProcurementPolicy {
	return &ProcurementPolicy{Version: 1, Revision: 1, MinRate: b.MinRate, MaxRate: b.MaxRate}
}

func (c *ProcurementCatalog) ValidateDraft() error {
	if c == nil || c.Version != 1 || c.Revision < 0 || c.Revision > MaxProcurementPolicyRevision || c.Buckets == nil || len(c.Buckets) > 64 {
		return errors.New("invalid procurement catalog version, revision or bucket count")
	}
	codes, names := map[string]bool{}, map[string]bool{}
	for _, bucket := range c.Buckets {
		if !procurementBucketCode.MatchString(bucket.Code) || codes[bucket.Code] || names[bucket.Name] ||
			!utf8.ValidString(bucket.Name) || bucket.Name == "" || strings.TrimSpace(bucket.Name) != bucket.Name ||
			utf8.RuneCountInString(bucket.Name) > 100 || strings.IndexFunc(bucket.Name, unicode.IsControl) >= 0 {
			return errors.New("procurement bucket codes and names must be valid and unique")
		}
		if err := bucket.policy().Validate(); err != nil {
			return err
		}
		codes[bucket.Code], names[bucket.Name] = true, true
	}
	sorted := c.sortedBuckets()
	for i := 1; i < len(sorted); i++ {
		previous, current := sorted[i-1], sorted[i]
		if previous.MaxRate == nil || compareProcurementBounds(current.MinRate, previous.MaxRate) < 0 {
			return errors.New("procurement bucket intervals overlap")
		}
	}
	return nil
}

func (c *ProcurementCatalog) Validate() error {
	if err := c.ValidateDraft(); err != nil {
		return err
	}
	if c.Revision == 0 && len(c.Buckets) != 0 {
		return errors.New("stored procurement catalog requires a positive revision")
	}
	return nil
}

// Nil lower bounds represent zero. Only callers checking lower bounds or a
// known finite upper bound use this helper.
func compareProcurementBounds(a, b *string) int {
	zero := "0"
	if a == nil {
		a = &zero
	}
	if b == nil {
		b = &zero
	}
	x, _ := procurementBound(a)
	y, _ := procurementBound(b)
	return x.Cmp(y)
}

func (c *ProcurementCatalog) sortedBuckets() []ProcurementBucket {
	buckets := slices.Clone(c.Buckets)
	slices.SortFunc(buckets, func(a, b ProcurementBucket) int { return compareProcurementBounds(a.MinRate, b.MinRate) })
	return buckets
}

// WithRevision is called only after validation; it returns independent bounds
// in stable interval order, using the same decimal canonicalization as Group.
func (c *ProcurementCatalog) WithRevision(revision int64) *ProcurementCatalog {
	next := &ProcurementCatalog{Version: c.Version, Revision: revision, Buckets: make([]ProcurementBucket, 0, len(c.Buckets))}
	for _, b := range c.sortedBuckets() {
		p := b.policy().WithRevision(1)
		b.MinRate, b.MaxRate = p.MinRate, p.MaxRate
		next.Buckets = append(next.Buckets, b)
	}
	return next
}

func (c *ProcurementCatalog) Clone() *ProcurementCatalog {
	if c == nil {
		return nil
	}
	next := *c
	next.Buckets = slices.Clone(c.Buckets)
	for i := range next.Buckets {
		b := &next.Buckets[i]
		if b.MinRate != nil {
			value := *b.MinRate
			b.MinRate = &value
		}
		if b.MaxRate != nil {
			value := *b.MaxRate
			b.MaxRate = &value
		}
	}
	return &next
}

func (c *ProcurementCatalog) ClassifyRate(rate float64) (*ProcurementBucket, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	// Validate the observed rate even for an empty catalog.
	if _, err := (&ProcurementPolicy{Version: 1, Revision: 1}).ContainsRate(rate); err != nil {
		return nil, err
	}
	for _, b := range c.Buckets {
		contains, err := b.policy().ContainsRate(rate)
		if err != nil {
			return nil, err
		}
		if contains {
			p := b.policy().Clone()
			b.MinRate, b.MaxRate = p.MinRate, p.MaxRate
			return &b, nil
		}
	}
	return nil, nil
}

// Gaps reports uncovered prices, including the leading and trailing ranges.
// Empty catalogs are displayed as not configured, rather than a fake bucket.
func (c *ProcurementCatalog) Gaps() []ProcurementGap {
	gaps := []ProcurementGap{}
	if c.Validate() != nil || len(c.Buckets) == 0 {
		return gaps
	}
	canonical := c.WithRevision(c.Revision)
	zero := "0"
	previous := &zero
	for _, b := range canonical.Buckets {
		if compareProcurementBounds(previous, b.MinRate) < 0 {
			gaps = append(gaps, ProcurementGap{MinRate: previous, MaxRate: b.MinRate})
		}
		previous = b.MaxRate
	}
	if previous != nil {
		gaps = append(gaps, ProcurementGap{MinRate: previous})
	}
	return gaps
}
