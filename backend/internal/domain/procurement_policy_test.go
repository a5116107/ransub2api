package domain

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcurementPolicyQualityCompleteContract(t *testing.T) {
	for _, quality := range []string{`"not_required"`, `"current_answer"`} {
		raw := `{"version":4,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":` + quality + `}`
		var p ProcurementPolicy
		require.NoError(t, json.Unmarshal([]byte(raw), &p))
		require.NoError(t, p.Validate())
		encoded, err := json.Marshal(p.Clone())
		require.NoError(t, err)
		require.JSONEq(t, raw, string(encoded))
	}
	for _, quality := range []string{`null`, `true`, `""`, `"future"`} {
		var p ProcurementPolicy
		require.Error(t, json.Unmarshal([]byte(`{"version":4,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":`+quality+`}`), &p))
	}
	for _, raw := range []string{
		`{"version":4,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null}}`,
		`{"version":3,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"current_answer"}`,
		`{"version":4,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"Quality":"current_answer"}`,
		`{"version":4,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"current_answer","quality":"not_required"}`,
	} {
		var p ProcurementPolicy
		require.Error(t, json.Unmarshal([]byte(raw), &p))
	}
}

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

func TestProcurementPolicyStoredJSONRequiresCompleteContract(t *testing.T) {
	for name, raw := range map[string]string{
		"missing minimum":     `{"version":1,"revision":1,"max_rate":null}`,
		"missing maximum":     `{"version":1,"revision":1,"min_rate":null}`,
		"null revision":       `{"version":1,"revision":null,"min_rate":null,"max_rate":null}`,
		"duplicate version":   `{"version":1,"version":1,"revision":1,"min_rate":null,"max_rate":null}`,
		"overwritten maximum": `{"version":1,"revision":1,"min_rate":null,"max_rate":"0.03","max_rate":null}`,
		"wrong case":          `{"Version":1,"revision":1,"min_rate":null,"max_rate":null}`,
		"unknown field":       `{"version":1,"revision":1,"min_rate":null,"max_rate":null,"owner":"unknown"}`,
		"v1 sources null":     `{"version":1,"revision":1,"min_rate":null,"max_rate":null,"sources":null}`,
		"v2 buckets null":     `{"version":2,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":null}`,
		"overwritten sources": `{"version":2,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":[9]},"sources":{"account_ids":null}}`,
		"overwritten buckets": `{"version":3,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":["low"]},"buckets":{"codes":null}}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := &ProcurementPolicy{Version: 1, Revision: 7}
			before := p.Clone()
			require.Error(t, json.Unmarshal([]byte(raw), p), "storage/snapshot reads must not silently remove a constraint")
			require.Equal(t, before, p, "failed decoding must not publish a partial policy")
		})
	}
	for _, version := range []int{1, 2, 3} {
		p := &ProcurementPolicy{Version: version, Revision: 0}
		if version >= 2 {
			p.Sources = &ProcurementSources{}
		}
		if version >= 3 {
			p.Buckets = &ProcurementBuckets{}
		}
		raw, err := json.Marshal(p)
		require.NoError(t, err)
		var draft ProcurementPolicy
		require.NoError(t, json.Unmarshal(raw, &draft), "explicit null and revision-zero commands remain supported")
		require.Equal(t, p, &draft)
	}
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

func TestProcurementPolicyBucketsValidationCloneAndRoundtrip(t *testing.T) {
	for _, codes := range [][]string{nil, {}, {"low", "next"}} {
		p := &ProcurementPolicy{Version: 3, Revision: 1, Sources: &ProcurementSources{}, Buckets: &ProcurementBuckets{Codes: codes}}
		require.NoError(t, p.Validate())
		cloned := p.Clone()
		raw, err := json.Marshal(cloned)
		require.NoError(t, err)
		var decoded ProcurementPolicy
		require.NoError(t, json.Unmarshal(raw, &decoded))
		require.Equal(t, p, &decoded)
		if len(codes) > 0 {
			cloned.Buckets.Codes[0] = "other"
			require.NotEqual(t, p, cloned)
		}
	}
	for _, raw := range []string{`null`, `{}`, `{"codes":false}`, `{"codes":["LOW"]}`, `{"codes":["low","low"]}`, `{"codes":[""]}`, `{"codes":[1]}`, `{"codes":null,"codes":null}`, `{"Codes":null}`, `{"codes":[],"unknown":true}`} {
		var decoded ProcurementPolicy
		err := json.Unmarshal([]byte(`{"version":3,"revision":1,"sources":{"account_ids":null},"buckets":`+raw+`}`), &decoded)
		require.True(t, err != nil || decoded.Validate() != nil, raw)
	}
	p := &ProcurementPolicy{Version: 3, Revision: 1, Sources: &ProcurementSources{}, Buckets: &ProcurementBuckets{Codes: []string{"next", "low"}}}
	require.Equal(t, []string{"low", "next"}, p.WithRevision(2).Buckets.Codes)
	require.Equal(t, []string{"next", "low"}, p.Buckets.Codes)
	p.Version = 2
	require.Error(t, p.Validate())
	p.Version = 3
	p.Sources = nil
	require.Error(t, p.Validate())
	p.Sources = &ProcurementSources{}
	p.Buckets = nil
	require.Error(t, p.Validate())
	p.Buckets = &ProcurementBuckets{Codes: make([]string, 65)}
	require.Error(t, p.Validate())
}
