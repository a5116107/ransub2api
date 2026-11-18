package domain

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcurementPolicyDecimalBoundaries(t *testing.T) {
	min, max := "0.02000000", "0.03"
	p := &ProcurementPolicy{Version: 1, Revision: 1, MinRate: &min, MaxRate: &max}
	for _, tc := range []struct {
		rate    float64
		allowed bool
	}{
		{0, false}, {0.019999999999999997, false}, {0.02, true}, {0.029999999999999995, true},
		{0.03, false}, {math.Nextafter(0.03, 0), true}, {math.Nextafter(0.03, 1), false},
	} {
		got, err := p.ContainsRate(tc.rate)
		require.NoError(t, err)
		require.Equal(t, tc.allowed, got, "%.18f", tc.rate)
	}
	for _, rate := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := p.ContainsRate(rate)
		require.Error(t, err)
	}
	copy := p.WithRevision(2)
	require.Equal(t, "0.02", *copy.MinRate)
	*copy.MinRate = "0"
	require.Equal(t, "0.02000000", *p.MinRate)
	p.MinRate = nil
	allowed, err := p.ContainsRate(0)
	require.NoError(t, err)
	require.True(t, allowed, "explicit free rates may enter an interval starting at zero")
	p.MaxRate = nil
	allowed, err = p.ContainsRate(99999)
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestProcurementPolicyRejectsMalformedBoundaries(t *testing.T) {
	for _, invalid := range []string{"", "-0.01", "+0.01", ".02", "00.02", "0.123456789", "100000000", "1e-2", "NaN", "Inf", " 0.02", "0.02 "} {
		p := &ProcurementPolicy{Version: 1, Revision: 1, MinRate: &invalid}
		require.Error(t, p.Validate(), invalid)
	}
	zero := "0"
	require.Error(t, (&ProcurementPolicy{Version: 1, Revision: 1, MaxRate: &zero}).Validate())
	require.Error(t, (&ProcurementPolicy{Version: 2, Revision: 1}).Validate())
	require.Error(t, (&ProcurementPolicy{Version: 1, Revision: 0}).Validate())
	require.NoError(t, (&ProcurementPolicy{Version: 1, Revision: 0}).ValidateDraft())
	require.Error(t, (&ProcurementPolicy{Version: 1, Revision: MaxProcurementPolicyRevision + 1}).Validate())
}
