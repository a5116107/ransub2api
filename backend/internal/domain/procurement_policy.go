package domain

import (
	"errors"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// ProcurementPolicy is an administrator-owned price constraint, not a quality
// or cache qualification. Bounds are exact decimal multipliers, [min, max).
type ProcurementPolicy struct {
	Version  int     `json:"version"`
	Revision int64   `json:"revision"`
	MinRate  *string `json:"min_rate"`
	MaxRate  *string `json:"max_rate"`
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
	if p == nil || p.Version != 1 || p.Revision < 0 || p.Revision > MaxProcurementPolicyRevision {
		return errors.New("invalid procurement policy version or revision")
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
	for _, bound := range []**string{&next.MinRate, &next.MaxRate} {
		if *bound != nil && strings.Contains(**bound, ".") {
			value := strings.TrimRight(strings.TrimRight(**bound, "0"), ".")
			*bound = &value
		}
	}
	return next
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
