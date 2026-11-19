package domain

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcurementCatalogStoredJSONRequiresCompleteContract(t *testing.T) {
	for name, raw := range map[string]string{
		"missing revision":      `{"version":1,"buckets":[]}`,
		"null revision":         `{"version":1,"revision":null,"buckets":[]}`,
		"duplicate revision":    `{"version":1,"revision":1,"revision":0,"buckets":[]}`,
		"wrong case":            `{"Version":1,"revision":1,"buckets":[]}`,
		"unknown field":         `{"version":1,"revision":1,"buckets":[],"owner":"unknown"}`,
		"missing minimum":       `{"version":1,"revision":1,"buckets":[{"code":"low","name":"Low","max_rate":"0.03"}]}`,
		"missing maximum":       `{"version":1,"revision":1,"buckets":[{"code":"low","name":"Low","min_rate":null}]}`,
		"overwritten maximum":   `{"version":1,"revision":1,"buckets":[{"code":"low","name":"Low","min_rate":null,"max_rate":"0.03","max_rate":null}]}`,
		"duplicate bucket list": `{"version":1,"revision":1,"buckets":[{"code":"low","name":"Low","min_rate":null,"max_rate":"0.03"}],"buckets":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			catalog := catalogFixture()
			before := catalog.Clone()
			require.Error(t, json.Unmarshal([]byte(raw), catalog), "all readers must preserve the complete catalog contract")
			require.Equal(t, before, catalog, "failed decoding must not publish a partial catalog")
		})
	}
	var empty ProcurementCatalog
	require.NoError(t, json.Unmarshal([]byte(`{"version":1,"revision":0,"buckets":[]}`), &empty))
	require.NoError(t, empty.ValidateDraft())
}

func catalogBound(v string) *string { return &v }

func catalogFixture() *ProcurementCatalog {
	return &ProcurementCatalog{Version: 1, Revision: 1, Buckets: []ProcurementBucket{
		{Code: "b2", Name: "Second", MinRate: catalogBound("0.03000000"), MaxRate: catalogBound("0.05")},
		{Code: "b1", Name: "First", MinRate: catalogBound("0.02"), MaxRate: catalogBound("0.03")},
	}}
}

func TestProcurementCatalogExactClassificationAndGaps(t *testing.T) {
	catalog := catalogFixture()
	for _, tc := range []struct {
		rate float64
		code string
	}{
		{0, ""}, {.02, "b1"}, {math.Nextafter(.03, 0), "b1"}, {.03, "b2"},
		{math.Nextafter(.03, 1), "b2"}, {math.Nextafter(.05, 0), "b2"}, {.05, ""},
	} {
		bucket, err := catalog.ClassifyRate(tc.rate)
		require.NoError(t, err)
		if tc.code == "" {
			require.Nil(t, bucket)
		} else {
			require.NotNil(t, bucket)
			require.Equal(t, tc.code, bucket.Code)
			bucket.Name = "not shared"
		}
	}
	for _, rate := range []float64{-1, math.NaN(), math.Inf(1)} {
		_, err := catalog.ClassifyRate(rate)
		require.Error(t, err)
	}
	canonical := catalog.WithRevision(2)
	require.Equal(t, "b1", canonical.Buckets[0].Code)
	require.Equal(t, "0.03", *canonical.Buckets[1].MinRate)
	require.Equal(t, "0.03000000", *catalog.Buckets[0].MinRate)
	require.Equal(t, "Second", catalog.Buckets[0].Name)
	gaps := canonical.Gaps()
	require.Equal(t, []ProcurementGap{{MinRate: catalogBound("0"), MaxRate: catalogBound("0.02")}, {MinRate: catalogBound("0.05")}}, gaps)
	canonical.Buckets[1].MinRate = catalogBound("0.04")
	require.Len(t, canonical.Gaps(), 3)
	canonical.Buckets[0].MinRate = nil
	canonical.Buckets[1].MaxRate = nil
	require.Len(t, canonical.Gaps(), 1)
	canonical.Buckets[1].MinRate = catalogBound("0.03")
	require.Empty(t, canonical.Gaps())
	bucket, err := canonical.ClassifyRate(0)
	require.NoError(t, err)
	require.Equal(t, "b1", bucket.Code)
}

func TestProcurementCatalogRejectsOverlapsAndMalformedCatalogs(t *testing.T) {
	for name, mutate := range map[string]func(*ProcurementCatalog){
		"version":           func(c *ProcurementCatalog) { c.Version = 2 },
		"negative revision": func(c *ProcurementCatalog) { c.Revision = -1 },
		"unsafe revision":   func(c *ProcurementCatalog) { c.Revision = MaxProcurementPolicyRevision + 1 },
		"nil buckets":       func(c *ProcurementCatalog) { c.Buckets = nil },
		"duplicate code":    func(c *ProcurementCatalog) { c.Buckets[0].Code = "b1" },
		"duplicate name":    func(c *ProcurementCatalog) { c.Buckets[0].Name = "First" },
		"control name":      func(c *ProcurementCatalog) { c.Buckets[0].Name = "A\nB" },
		"empty name":        func(c *ProcurementCatalog) { c.Buckets[0].Name = " " },
		"code":              func(c *ProcurementCatalog) { c.Buckets[0].Code = "B2" },
		"overlap":           func(c *ProcurementCatalog) { c.Buckets[0].MinRate = catalogBound("0.02999999") },
		"zero equivalence":  func(c *ProcurementCatalog) { c.Buckets[0].MinRate = nil },
		"open middle":       func(c *ProcurementCatalog) { c.Buckets[1].MaxRate = nil },
		"empty interval":    func(c *ProcurementCatalog) { c.Buckets[0].MaxRate = catalogBound("0.03") },
		"exponent":          func(c *ProcurementCatalog) { c.Buckets[0].MaxRate = catalogBound("5e-2") },
		"too precise":       func(c *ProcurementCatalog) { c.Buckets[0].MaxRate = catalogBound("0.050000001") },
		"too many":          func(c *ProcurementCatalog) { c.Buckets = make([]ProcurementBucket, 65) },
	} {
		t.Run(name, func(t *testing.T) {
			c := catalogFixture()
			mutate(c)
			require.Error(t, c.ValidateDraft())
			_, err := c.ClassifyRate(.025)
			require.Error(t, err)
		})
	}
	var absent *ProcurementCatalog
	require.Error(t, absent.ValidateDraft())
	empty := &ProcurementCatalog{Version: 1, Revision: 0, Buckets: []ProcurementBucket{}}
	require.NoError(t, empty.Validate())
	_, err := empty.ClassifyRate(.025)
	require.NoError(t, err)
	c := catalogFixture()
	c.Revision = 0
	require.Error(t, c.Validate())
	require.NoError(t, c.ValidateDraft())
}
