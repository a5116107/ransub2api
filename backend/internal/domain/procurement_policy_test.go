package domain

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcurementPolicyAvailabilityCompleteContract(t *testing.T) {
	const valid = `{"version":5,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"current_answer","availability":"current_state"}`
	for _, mode := range []string{"current_state", "not_required"} {
		raw := strings.Replace(valid, "current_state", mode, 1)
		var policy ProcurementPolicy
		require.NoError(t, json.Unmarshal([]byte(raw), &policy))
		require.NoError(t, policy.Validate())
		encoded, err := json.Marshal(policy.Clone())
		require.NoError(t, err)
		require.JSONEq(t, raw, string(encoded))
	}
	for name, raw := range map[string]string{
		"missing":         strings.Replace(valid, `,"availability":"current_state"`, "", 1),
		"null":            strings.Replace(valid, `"availability":"current_state"`, `"availability":null`, 1),
		"empty":           strings.Replace(valid, "current_state", "", 1),
		"unknown":         strings.Replace(valid, "current_state", "future", 1),
		"wrong case":      strings.Replace(valid, `"availability":`, `"Availability":`, 1),
		"duplicate":       strings.Replace(valid, `"availability":`, `"availability":"current_state","availability":`, 1),
		"old version":     strings.Replace(valid, `"version":5`, `"version":4`, 1),
		"future version":  strings.Replace(valid, `"version":5`, `"version":7`, 1),
		"missing quality": strings.Replace(valid, `,"quality":"current_answer"`, "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			policy := ProcurementPolicy{Version: 1, Revision: 9}
			before := policy.Clone()
			require.Error(t, json.Unmarshal([]byte(raw), &policy))
			require.Equal(t, before, &policy, "invalid persistence must not publish a partial policy")
		})
	}
}

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

func TestProcurementPolicyApprovedSourceGroupsJSON(t *testing.T) {
	base := `{"version":6,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"not_required","availability":"not_required","source_group_ids":GROUPS}`
	for _, value := range []string{"null", "[]", "[2,3]"} {
		var policy ProcurementPolicy
		require.NoError(t, json.Unmarshal([]byte(strings.Replace(base, "GROUPS", value, 1)), &policy))
		require.NoError(t, policy.Validate())
		encoded, err := json.Marshal(&policy)
		require.NoError(t, err)
		require.Contains(t, string(encoded), `"source_group_ids":`+value)
	}
	for _, value := range []string{"[0]", "[-1]", "[2,2]", "[9007199254740992]", "[1.2]", `{}`, `"2"`} {
		var policy ProcurementPolicy
		require.Error(t, json.Unmarshal([]byte(strings.Replace(base, "GROUPS", value, 1)), &policy))
	}
	var policy ProcurementPolicy
	require.Error(t, json.Unmarshal([]byte(strings.Replace(base, `,"source_group_ids":GROUPS`, "", 1)), &policy))
	require.Error(t, json.Unmarshal([]byte(strings.Replace(strings.Replace(base, "GROUPS", "null", 1), `"version":6`, `"version":5`, 1)), &policy))
	valid := strings.Replace(base, "GROUPS", "[3,2]", 1)
	for name, raw := range map[string]string{
		"duplicate":  strings.Replace(valid, `"source_group_ids":`, `"source_group_ids":null,"source_group_ids":`, 1),
		"case alias": strings.Replace(valid, `"source_group_ids":`, `"Source_Group_Ids":`, 1),
		"unknown":    strings.Replace(valid, `"source_group_ids":`, `"unknown":true,"source_group_ids":`, 1),
	} {
		t.Run(name, func(t *testing.T) { require.Error(t, json.Unmarshal([]byte(raw), &policy)) })
	}
	require.NoError(t, json.Unmarshal([]byte(valid), &policy))
	require.Equal(t, []int64{2, 3}, policy.WithRevision(2).SourceGroupIDs)
	policy.SourceGroupIDs = make([]int64, 65)
	for i := range policy.SourceGroupIDs {
		policy.SourceGroupIDs[i] = int64(i + 1)
	}
	require.Error(t, policy.Validate())
	policy.SourceGroupIDs = policy.SourceGroupIDs[:64]
	require.NoError(t, policy.Validate())
	legacy, err := json.Marshal(&ProcurementPolicy{Version: 1, Revision: 1})
	require.NoError(t, err)
	require.NotContains(t, string(legacy), "source_group_ids")
}

func TestProcurementSourceSnapshotAuthorityAndClone(t *testing.T) {
	var policy ProcurementPolicy
	require.NoError(t, json.Unmarshal([]byte(`{"version":6,"revision":4,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"not_required","availability":"not_required","source_group_ids":[2,3]}`), &policy))
	source := &ProcurementSourceSnapshot{Version: 1, ProductGroupID: 7, PolicyRevision: 4, GroupIDs: []int64{2, 3}, AccountIDs: []int64{10, 11}}
	allowed, err := policy.AllowsSourceAccount(7, 10, source)
	require.NoError(t, err)
	require.True(t, allowed)
	allowed, err = policy.AllowsSourceAccount(7, 12, source)
	require.NoError(t, err)
	require.False(t, allowed)
	for _, mutate := range []func(*ProcurementSourceSnapshot){
		func(s *ProcurementSourceSnapshot) { s.Version = 2 }, func(s *ProcurementSourceSnapshot) { s.ProductGroupID = 8 }, func(s *ProcurementSourceSnapshot) { s.PolicyRevision = 3 },
		func(s *ProcurementSourceSnapshot) { s.GroupIDs = []int64{2} }, func(s *ProcurementSourceSnapshot) { s.AccountIDs = nil }, func(s *ProcurementSourceSnapshot) { s.AccountIDs = []int64{11, 10} }, func(s *ProcurementSourceSnapshot) { s.AccountIDs = []int64{10, 10} },
	} {
		broken := source.Clone()
		mutate(broken)
		_, err := policy.AllowsSourceAccount(7, 10, broken)
		require.Error(t, err)
	}
	cloned := policy.Clone()
	cloned.SourceGroupIDs[0] = 8
	require.Equal(t, int64(2), policy.SourceGroupIDs[0])
	copy := source.Clone()
	copy.AccountIDs[0] = 9
	require.Equal(t, int64(10), source.AccountIDs[0])
	_, err = policy.AllowsSourceAccount(7, 10, nil)
	require.Error(t, err)
	policy.SourceGroupIDs = []int64{}
	allowed, err = policy.AllowsSourceAccount(7, 10, nil)
	require.NoError(t, err)
	require.False(t, allowed)
	policy.SourceGroupIDs = nil
	allowed, err = policy.AllowsSourceAccount(7, 10, nil)
	require.NoError(t, err)
	require.True(t, allowed)
	policy.SourceGroupIDs = []int64{7}
	source.GroupIDs = []int64{7}
	require.Error(t, source.ValidateFor(7, &policy), "a product cannot approve itself")
	policy.SourceGroupIDs, source.GroupIDs = []int64{2, 3}, []int64{2, 3}
	source.AccountIDs = make([]int64, 10001)
	for i := range source.AccountIDs {
		source.AccountIDs[i] = int64(i + 1)
	}
	require.Error(t, source.ValidateFor(7, &policy))
	source.AccountIDs = source.AccountIDs[:10000]
	require.NoError(t, source.ValidateFor(7, &policy))
	encoded, err := json.Marshal(source)
	require.NoError(t, err)
	for _, raw := range []string{
		strings.Replace(string(encoded), `"account_ids":`, `"account_ids":[],"account_ids":`, 1),
		strings.Replace(string(encoded), `"group_ids":`, `"Group_Ids":`, 1),
		strings.Replace(string(encoded), `"version":1`, `"version":1,"unknown":true`, 1),
	} {
		var decoded ProcurementSourceSnapshot
		require.Error(t, json.Unmarshal([]byte(raw), &decoded))
	}
}

func TestProcurementEvidenceTemplateCompleteContract(t *testing.T) {
	raw := `{"version":7,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"current_answer","availability":"current_state","source_group_ids":[12],"evidence_template":{"version":1,"code":"answer_availability"}}`
	var policy ProcurementPolicy
	require.NoError(t, json.Unmarshal([]byte(raw), &policy))
	require.NoError(t, policy.Validate())
	encoded, err := json.Marshal(policy)
	require.NoError(t, err)
	require.JSONEq(t, raw, string(encoded))
}

func TestProcurementEvidenceTemplateRejectsDriftAndClones(t *testing.T) {
	const raw = `{"version":7,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":null},"quality":"current_answer","availability":"current_state","source_group_ids":[12],"evidence_template":{"version":1,"code":"answer_availability"}}`
	for _, change := range []struct{ from, to string }{
		{`,"evidence_template":{"version":1,"code":"answer_availability"}`, ``},
		{`"evidence_template":{"version":1,"code":"answer_availability"}`, `"evidence_template":null`},
		{`"version":7`, `"version":6`}, {`"version":1,"code"`, `"version":2,"code"`},
		{`"code":"answer_availability"`, `"code":"future"`},
		{`"code":"answer_availability"`, `"code":"answer_availability","code":"answer_availability"`},
		{`"code":"answer_availability"`, `"Code":"answer_availability"`},
		{`"code":"answer_availability"`, `"code":"answer_availability","extra":true`},
		{`"quality":"current_answer"`, `"quality":"not_required"`},
		{`"availability":"current_state"`, `"availability":"not_required"`},
		{`"source_group_ids":[12]`, `"source_group_ids":null`}, {`"source_group_ids":[12]`, `"source_group_ids":[]`},
	} {
		var policy ProcurementPolicy
		require.Error(t, json.Unmarshal([]byte(strings.Replace(raw, change.from, change.to, 1)), &policy), change.to)
	}
	var policy ProcurementPolicy
	require.NoError(t, json.Unmarshal([]byte(raw), &policy))
	copy := policy.WithRevision(2)
	copy.EvidenceTemplate.Code = "custom"
	copy.Quality = "not_required"
	copy.SourceGroupIDs = nil
	require.NoError(t, copy.Validate())
	require.Equal(t, "answer_availability", policy.EvidenceTemplate.Code)
	require.Equal(t, []int64{12}, policy.SourceGroupIDs)
	var old ProcurementPolicy
	require.NoError(t, json.Unmarshal([]byte(`{"version":1,"revision":1,"min_rate":null,"max_rate":null}`), &old))
	b, err := json.Marshal(old)
	require.NoError(t, err)
	require.NotContains(t, string(b), "evidence_template")
}
