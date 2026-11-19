package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ProcurementPolicy narrows the original pool by administrator-owned price and
// source constraints. It does not certify quality/cache or own group bindings.
type ProcurementPolicy struct {
	Version  int                 `json:"version"`
	Revision int64               `json:"revision"`
	MinRate  *string             `json:"min_rate"`
	MaxRate  *string             `json:"max_rate"`
	Sources  *ProcurementSources `json:"sources,omitempty"`
	Buckets  *ProcurementBuckets `json:"buckets,omitempty"`
}

// A persisted policy and a command have the same complete JSON contract. Plain
// struct decoding accepts missing bounds, case aliases and last-value wins,
// which could turn a damaged snapshot into less restrictive admission.
func (p *ProcurementPolicy) UnmarshalJSON(raw []byte) error {
	fields, err := procurementJSONFields(raw, []string{"version", "revision", "min_rate", "max_rate"}, "sources", "buckets")
	if err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(fields["revision"]), []byte("null")) {
		return errors.New("procurement policy requires an explicit revision")
	}
	type contract ProcurementPolicy
	var next contract
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	policy := ProcurementPolicy(next)
	if err := policy.ValidateDraft(); err != nil {
		return err
	}
	if policy.Version == 1 && fields["sources"] != nil || policy.Version < 3 && fields["buckets"] != nil {
		return errors.New("procurement fields do not match the policy version")
	}
	*p = policy
	return nil
}

func procurementJSONFields(raw []byte, required []string, optional ...string) (map[string]json.RawMessage, error) {
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, name := range required {
		allowed[name] = true
	}
	for _, name := range optional {
		allowed[name] = true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("procurement configuration must be an object")
	}
	fields := make(map[string]json.RawMessage, len(allowed))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok || !allowed[name] || fields[name] != nil {
			return nil, errors.New("duplicate or unknown procurement field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("invalid procurement object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("trailing procurement data")
	}
	for _, name := range required {
		if fields[name] == nil {
			return nil, errors.New("missing procurement field")
		}
	}
	return fields, nil
}

// Version 3 requires an explicit codes list. Nil preserves the original pool;
// an empty list admits no bucket. Codes refer to the shared catalog, not groups.
type ProcurementBuckets struct {
	Codes []string `json:"codes"`
}

func (b *ProcurementBuckets) UnmarshalJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') || !decoder.More() {
		return errors.New("procurement buckets require an explicit codes field")
	}
	if token, err := decoder.Token(); err != nil || token != "codes" {
		return errors.New("unknown procurement buckets field")
	}
	var codes []string
	if err := decoder.Decode(&codes); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("duplicate or unknown procurement buckets field")
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return errors.New("invalid procurement buckets object")
	}
	b.Codes = codes
	return nil
}

// Version 2 requires the object. A null list preserves the original pool; an
// empty list explicitly admits none. IDs refer only to local upstream accounts.
type ProcurementSources struct {
	AccountIDs []int64 `json:"account_ids"`
}

// Persisted JSON and scheduler/auth snapshots must distinguish an explicit
// null list from a missing field. Otherwise a damaged object becomes an
// unrestricted pool before Validate sees it.
func (s *ProcurementSources) UnmarshalJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') || !decoder.More() {
		return errors.New("procurement sources require an explicit account_ids field")
	}
	if token, err := decoder.Token(); err != nil || token != "account_ids" {
		return errors.New("unknown procurement sources field")
	}
	var ids []int64
	if err := decoder.Decode(&ids); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("duplicate or unknown procurement sources field")
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return errors.New("invalid procurement sources object")
	}
	s.AccountIDs = ids
	return nil
}

const MaxProcurementPolicyRevision int64 = 9007199254740991

var procurementRatePattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,7})(\.[0-9]{1,8})?$`)

func procurementBound(value *string) (*big.Rat, error) {
	if value == nil {
		return nil, nil
	}
	if !procurementRatePattern.MatchString(*value) {
		return nil, errors.New("procurement bounds require nonnegative decimals with up to 8 integer and fractional digits")
	}
	n, ok := new(big.Rat).SetString(*value)
	if !ok {
		return nil, errors.New("invalid procurement bound")
	}
	return n, nil
}

func (p *ProcurementPolicy) ValidateDraft() error {
	if p == nil || p.Version < 1 || p.Version > 3 || p.Revision < 0 || p.Revision > MaxProcurementPolicyRevision {
		return errors.New("invalid procurement policy version or revision")
	}
	if p.Version == 1 && p.Sources != nil || p.Version >= 2 && p.Sources == nil {
		return errors.New("procurement source contract does not match its version")
	}
	if p.Version < 3 && p.Buckets != nil || p.Version == 3 && p.Buckets == nil {
		return errors.New("procurement bucket contract does not match its version")
	}
	if p.Buckets != nil {
		if len(p.Buckets.Codes) > 64 {
			return errors.New("too many allowed procurement buckets")
		}
		seen := make(map[string]bool, len(p.Buckets.Codes))
		for _, code := range p.Buckets.Codes {
			if !procurementBucketCode.MatchString(code) || seen[code] {
				return errors.New("procurement bucket codes must be valid and unique")
			}
			seen[code] = true
		}
	}
	if p.Sources != nil {
		if len(p.Sources.AccountIDs) > 1000 {
			return errors.New("too many approved procurement sources")
		}
		seen := make(map[int64]bool, len(p.Sources.AccountIDs))
		for _, id := range p.Sources.AccountIDs {
			if id < 1 || id > MaxProcurementPolicyRevision || seen[id] {
				return errors.New("procurement source IDs must be unique positive safe integers")
			}
			seen[id] = true
		}
	}
	min, err := procurementBound(p.MinRate)
	if err != nil {
		return err
	}
	max, err := procurementBound(p.MaxRate)
	if err != nil {
		return err
	}
	if min == nil {
		min = new(big.Rat)
	}
	if max != nil && min.Cmp(max) >= 0 {
		return errors.New("procurement maximum must exceed its inclusive minimum")
	}
	return nil
}

func (p *ProcurementPolicy) Validate() error {
	if err := p.ValidateDraft(); err != nil {
		return err
	}
	if p.Revision == 0 {
		return errors.New("stored procurement policy requires a positive revision")
	}
	return nil
}

func (p *ProcurementPolicy) Clone() *ProcurementPolicy {
	if p == nil {
		return nil
	}
	next := *p
	if p.Buckets != nil {
		next.Buckets = &ProcurementBuckets{Codes: slices.Clone(p.Buckets.Codes)}
	}
	if p.Sources != nil {
		next.Sources = &ProcurementSources{}
		if p.Sources.AccountIDs != nil {
			next.Sources.AccountIDs = make([]int64, len(p.Sources.AccountIDs))
			copy(next.Sources.AccountIDs, p.Sources.AccountIDs)
		}
	}
	if p.MinRate != nil {
		min := *p.MinRate
		next.MinRate = &min
	}
	if p.MaxRate != nil {
		max := *p.MaxRate
		next.MaxRate = &max
	}
	return &next
}

// WithRevision canonicalizes only validated decimals; it never rounds a bound.
func (p *ProcurementPolicy) WithRevision(revision int64) *ProcurementPolicy {
	next := p.Clone()
	next.Revision = revision
	if next.Buckets != nil {
		slices.Sort(next.Buckets.Codes)
	}
	if next.Sources != nil {
		slices.Sort(next.Sources.AccountIDs)
	}
	for _, bound := range []**string{&next.MinRate, &next.MaxRate} {
		if *bound != nil && strings.Contains(**bound, ".") {
			value := strings.TrimRight(strings.TrimRight(**bound, "0"), ".")
			*bound = &value
		}
	}
	return next
}

func (p *ProcurementPolicy) AllowsAccount(id int64) (bool, error) {
	if p == nil {
		return true, nil
	}
	if err := p.Validate(); err != nil {
		return false, err
	}
	if p.Sources == nil || p.Sources.AccountIDs == nil {
		return true, nil
	}
	return slices.Contains(p.Sources.AccountIDs, id), nil
}

func (p *ProcurementPolicy) ContainsRate(rate float64) (bool, error) {
	if p == nil {
		return true, nil
	}
	if err := p.Validate(); err != nil {
		return false, err
	}
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
		return false, errors.New("procurement rate unavailable")
	}
	// The existing authoritative multiplier is a float64. Its shortest decimal
	// representation preserves 0.03 as 0.03 rather than comparing binary tails,
	// and does not round a higher-precision observed rate into this interval.
	actual, ok := new(big.Rat).SetString(strconv.FormatFloat(rate, 'f', -1, 64))
	if !ok {
		return false, errors.New("procurement rate unavailable")
	}
	min, _ := procurementBound(p.MinRate)
	max, _ := procurementBound(p.MaxRate)
	return (min == nil || actual.Cmp(min) >= 0) && (max == nil || actual.Cmp(max) < 0), nil
}
