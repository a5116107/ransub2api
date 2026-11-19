package domain

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcurementPolicySourcesValidationCloneAndRoundtrip(t *testing.T) {
	for _, ids := range [][]int64{nil, {}, {901, 902}, {MaxProcurementPolicyRevision}} {
		p := &ProcurementPolicy{Version: 2, Revision: 1, Sources: &ProcurementSources{AccountIDs: ids}}
		require.NoError(t, p.Validate())
		copy := p.Clone()
		require.Equal(t, p, copy)
		raw, err := json.Marshal(copy)
		require.NoError(t, err)
		var decoded ProcurementPolicy
		require.NoError(t, json.Unmarshal(raw, &decoded))
		require.Equal(t, p, &decoded, "null and empty sources must remain distinct")
		allowed, err := decoded.AllowsAccount(901)
		require.NoError(t, err)
		require.Equal(t, ids == nil || len(ids) == 2, allowed)
		if len(ids) > 0 {
			copy.Sources.AccountIDs[0] = 1
			require.NotEqual(t, p, copy, "clones own their source storage")
		}
	}
	for _, ids := range [][]int64{{0}, {-1}, {1, 1}, {MaxProcurementPolicyRevision + 1}, make([]int64, 1001)} {
		p := &ProcurementPolicy{Version: 2, Revision: 1, Sources: &ProcurementSources{AccountIDs: ids}}
		require.Error(t, p.Validate())
		_, err := p.AllowsAccount(1)
		require.Error(t, err)
	}
	p := &ProcurementPolicy{Version: 2, Revision: 1, Sources: &ProcurementSources{AccountIDs: []int64{902, 901}}}
	require.Equal(t, []int64{901, 902}, p.WithRevision(2).Sources.AccountIDs)
	require.Equal(t, []int64{902, 901}, p.Sources.AccountIDs)
	p.Version = 1
	require.Error(t, p.Validate(), "v1 must not silently carry source constraints")
	for _, sources := range []string{`{}`, `{"account_ids":null,"account_ids":null}`, `{"Account_ids":null}`, `{"account_ids":[],"unknown":true}`, `{"account_ids":false}`} {
		var stored ProcurementPolicy
		require.Error(t, json.Unmarshal([]byte(`{"version":2,"revision":1,"min_rate":null,"max_rate":null,"sources":`+sources+`}`), &stored), "corrupt persisted sources must not become the original pool")
	}
}

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
