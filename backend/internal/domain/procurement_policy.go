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

// ProcurementPolicy narrows the original pool by administrator-owned price,
// sources and explicit evidence requirements. It does not certify model/cache
// quality or own group bindings.
type ProcurementPolicy struct {
	Version        int                 `json:"version"`
	Revision       int64               `json:"revision"`
	MinRate        *string             `json:"min_rate"`
	MaxRate        *string             `json:"max_rate"`
	Sources        *ProcurementSources `json:"sources,omitempty"`
	Buckets        *ProcurementBuckets `json:"buckets,omitempty"`
	Quality        string              `json:"quality,omitempty"`
	Availability   string              `json:"availability,omitempty"`
	SourceGroupIDs []int64             `json:"source_group_ids,omitempty"`
}

// Explicit null and empty lists remain distinct in v6; older contracts never
// acquire the new field just because they are encoded by a newer application.
func (p ProcurementPolicy) MarshalJSON() ([]byte, error) {
	type contract ProcurementPolicy
	var groups json.RawMessage
	if p.Version >= 6 {
		var err error
		groups, err = json.Marshal(p.SourceGroupIDs)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(struct {
		contract
		SourceGroupIDs json.RawMessage `json:"source_group_ids,omitempty"`
	}{contract(p), groups})
}

// A persisted policy and a command have the same complete JSON contract. Plain
// struct decoding accepts missing bounds, case aliases and last-value wins,
// which could turn a damaged snapshot into less restrictive admission.
func (p *ProcurementPolicy) UnmarshalJSON(raw []byte) error {
	fields, err := procurementJSONFields(raw, []string{"version", "revision", "min_rate", "max_rate"}, "sources", "buckets", "quality", "availability", "source_group_ids")
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
	if policy.Version == 1 && fields["sources"] != nil || policy.Version < 3 && fields["buckets"] != nil || policy.Version < 4 && fields["quality"] != nil || policy.Version < 5 && fields["availability"] != nil || policy.Version < 6 && fields["source_group_ids"] != nil || policy.Version >= 6 && fields["source_group_ids"] == nil {
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
	if p == nil || p.Version < 1 || p.Version > 6 || p.Revision < 0 || p.Revision > MaxProcurementPolicyRevision {
		return errors.New("invalid procurement policy version or revision")
	}
	if p.Version == 1 && p.Sources != nil || p.Version >= 2 && p.Sources == nil {
		return errors.New("procurement source contract does not match its version")
	}
	if p.Version < 3 && p.Buckets != nil || p.Version >= 3 && p.Buckets == nil {
		return errors.New("procurement bucket contract does not match its version")
	}
	if p.Version < 4 && p.Quality != "" || p.Version >= 4 && p.Quality != "not_required" && p.Quality != "current_answer" {
		return errors.New("procurement quality contract does not match its version")
	}
	if p.Version < 5 && p.Availability != "" || p.Version >= 5 && p.Availability != "not_required" && p.Availability != "current_state" {
		return errors.New("procurement availability contract does not match its version")
	}
	if p.Version < 6 && p.SourceGroupIDs != nil || len(p.SourceGroupIDs) > 64 {
		return errors.New("invalid procurement source groups")
	}
	seenGroups := make(map[int64]bool, len(p.SourceGroupIDs))
	for _, id := range p.SourceGroupIDs {
		if id < 1 || id > MaxProcurementPolicyRevision || seenGroups[id] {
			return errors.New("procurement source group IDs must be unique positive safe integers")
		}
		seenGroups[id] = true
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
	next.SourceGroupIDs = slices.Clone(p.SourceGroupIDs)
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
	slices.Sort(next.SourceGroupIDs)
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

// ProcurementSourceSnapshot is an internal, bounded authority projection. It
// travels only in encrypted regional scheduler documents, never user DTOs.
type ProcurementSourceSnapshot struct {
	Version        int     `json:"version"`
	ProductGroupID int64   `json:"product_group_id"`
	PolicyRevision int64   `json:"policy_revision"`
	GroupIDs       []int64 `json:"group_ids"`
	AccountIDs     []int64 `json:"account_ids"`
}

func (s *ProcurementSourceSnapshot) UnmarshalJSON(raw []byte) error {
	if _, err := procurementJSONFields(raw, []string{"version", "product_group_id", "policy_revision", "group_ids", "account_ids"}); err != nil {
		return err
	}
	type contract ProcurementSourceSnapshot
	var next contract
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	*s = ProcurementSourceSnapshot(next)
	return nil
}

func (s *ProcurementSourceSnapshot) Clone() *ProcurementSourceSnapshot {
	if s == nil {
		return nil
	}
	next := *s
	next.GroupIDs, next.AccountIDs = slices.Clone(s.GroupIDs), slices.Clone(s.AccountIDs)
	return &next
}

func (s *ProcurementSourceSnapshot) ValidateFor(groupID int64, p *ProcurementPolicy) error {
	if s == nil || p == nil || p.Validate() != nil || p.Version < 6 || p.SourceGroupIDs == nil ||
		s.Version != 1 || groupID < 1 || s.ProductGroupID != groupID || s.PolicyRevision != p.Revision ||
		s.GroupIDs == nil || !slices.Equal(s.GroupIDs, p.SourceGroupIDs) || s.AccountIDs == nil || len(s.AccountIDs) > 10000 {
		return errors.New("procurement source authority unavailable")
	}
	for _, id := range s.GroupIDs {
		if id == groupID {
			return errors.New("product cannot approve itself as a source")
		}
	}
	for i, id := range s.AccountIDs {
		if id < 1 || id > MaxProcurementPolicyRevision || i > 0 && id <= s.AccountIDs[i-1] {
			return errors.New("invalid procurement source members")
		}
	}
	return nil
}

func (p *ProcurementPolicy) AllowsSourceAccount(groupID, accountID int64, source *ProcurementSourceSnapshot) (bool, error) {
	if p == nil {
		return true, nil
	}
	if err := p.Validate(); err != nil {
		return false, err
	}
	if p.Version < 6 || p.SourceGroupIDs == nil {
		return true, nil
	}
	if len(p.SourceGroupIDs) == 0 {
		return false, nil
	}
	if err := source.ValidateFor(groupID, p); err != nil {
		return false, err
	}
	return slices.Contains(source.AccountIDs, accountID), nil
}
