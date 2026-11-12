package service

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func priorityDiffRowsByAccount(rows []PrioritySchedulingDiffRow) map[int64]PrioritySchedulingDiffRow {
	out := make(map[int64]PrioritySchedulingDiffRow, len(rows))
	for _, row := range rows {
		out[row.AccountID] = row
	}
	return out
}

func TestDiffPrioritySnapshots(t *testing.T) {
	before := &PrioritySchedulingSnapshot{Candidates: []PrioritySchedulingScore{
		{AccountID: 1, AccountName: "A", Score: 80, SelectionWeight: 1.5, Tier: "eligible", PrioritySchedulingSignal: withAccountPerformanceFixture(PrioritySchedulingSignal{P90TTFTMs: 1000, Samples: 10, ErrorSamples: 1})},
		{AccountID: 2, AccountName: "B", Score: 70, SelectionWeight: 1.0, Tier: "insufficient", PrioritySchedulingSignal: withAccountPerformanceFixture(PrioritySchedulingSignal{P90TTFTMs: 5000, Samples: 5})},
		{AccountID: 4, AccountName: "D", Score: 50, SelectionWeight: 0.5, Tier: "degraded", PrioritySchedulingSignal: withAccountPerformanceFixture(PrioritySchedulingSignal{P90TTFTMs: 9000, Samples: 2})},
	}}
	after := &PrioritySchedulingSnapshot{Candidates: []PrioritySchedulingScore{
		{AccountID: 2, AccountName: "B", Score: 90, SelectionWeight: 2.0, Tier: "eligible", PrioritySchedulingSignal: withAccountPerformanceFixture(PrioritySchedulingSignal{P90TTFTMs: 2000, Samples: 8})},
		{AccountID: 1, AccountName: "A", Score: 60, SelectionWeight: 0.8, Tier: "degraded", PrioritySchedulingSignal: withAccountPerformanceFixture(PrioritySchedulingSignal{P90TTFTMs: 8000, Samples: 12, ErrorSamples: 3})},
		{AccountID: 3, AccountName: "C", Score: 85, SelectionWeight: 1.7, Tier: "eligible", PrioritySchedulingSignal: withAccountPerformanceFixture(PrioritySchedulingSignal{P90TTFTMs: 1500, Samples: 6})},
	}}
	rows := priorityDiffRowsByAccount(diffPrioritySnapshots(before, after))
	require.Len(t, rows, 4)

	require.Equal(t, 1, rows[1].OldRank)
	require.Equal(t, 2, rows[1].NewRank)
	require.True(t, rows[1].Changed)
	require.True(t, almostEqual(rows[1].NewP90TTFTMs, 8000, 0.001))

	require.Equal(t, 2, rows[2].OldRank)
	require.Equal(t, 1, rows[2].NewRank)
	require.True(t, rows[2].Changed)

	require.Equal(t, 3, rows[3].NewRank)
	require.Zero(t, rows[3].OldRank)
	require.True(t, rows[3].Changed)

	require.Equal(t, 3, rows[4].OldRank)
	require.Zero(t, rows[4].NewRank)
	require.True(t, rows[4].Changed)
}

func TestDiffPrioritySnapshotsUnchangedAndNil(t *testing.T) {
	snapshot := &PrioritySchedulingSnapshot{Candidates: []PrioritySchedulingScore{
		{AccountID: 1, AccountName: "A", Score: 80, SelectionWeight: 1.5, Tier: "eligible", PrioritySchedulingSignal: withAccountPerformanceFixture(PrioritySchedulingSignal{P90TTFTMs: 1000, Samples: 10})},
	}}
	for _, row := range diffPrioritySnapshots(snapshot, snapshot) {
		require.False(t, row.Changed)
	}
	require.Nil(t, diffPrioritySnapshots(snapshot, nil))
	for _, row := range diffPrioritySnapshots(nil, snapshot) {
		require.Zero(t, row.OldRank)
		require.True(t, row.Changed)
	}
}

func TestDiffPrioritySnapshotsMeanLatency(t *testing.T) {
	for _, tc := range []struct {
		name           string
		old, fresh     *float64
		oldP90, newP90 float64
		changed        bool
	}{
		{name: "selected_changed", old: new(950.0), fresh: new(1500.0), oldP90: 500, newP90: 500, changed: true},
		{name: "became_unknown", old: new(950.0), oldP90: 500, newP90: 500, changed: true},
		{name: "recovered", fresh: new(1500.0), oldP90: 500, newP90: 500, changed: true},
		{name: "both_unknown", oldP90: 500, newP90: 500},
		{name: "unchanged", old: new(950.0), fresh: new(950.0), oldP90: 500, newP90: 500},
		{name: "within_tolerance", old: new(950.0), fresh: new(950.5), oldP90: 500, newP90: 500},
		{name: "other_percentile_changed", old: new(950.0), fresh: new(950.0), oldP90: 500, newP90: 800},
		{name: "zero_is_unknown", old: new(0.0), oldP90: 500, newP90: 500},
		{name: "negative_is_unknown", old: new(-1.0), oldP90: 500, newP90: 500},
		{name: "nan_is_unknown", old: new(math.NaN()), oldP90: 500, newP90: 500},
		{name: "infinite_is_unknown", old: new(math.Inf(1)), oldP90: 500, newP90: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := &PrioritySchedulingSnapshot{LatencyPercentile: 95, LatencyMeasurement: "attributed_v1", Candidates: []PrioritySchedulingScore{
				{AccountID: 1, PrioritySchedulingSignal: PrioritySchedulingSignal{SelectedTTFTMs: tc.old, P90TTFTMs: tc.oldP90}},
			}}
			after := &PrioritySchedulingSnapshot{LatencyPercentile: 95, LatencyMeasurement: "attributed_v1", Candidates: []PrioritySchedulingScore{
				{AccountID: 1, PrioritySchedulingSignal: PrioritySchedulingSignal{SelectedTTFTMs: tc.fresh, P90TTFTMs: tc.newP90}},
			}}
			rows := diffPrioritySnapshots(before, after)
			require.Len(t, rows, 1)
			require.Equal(t, tc.changed, rows[0].Changed, "the shared mean, not an unrelated P90, must drive the diff")
			raw, err := json.Marshal(rows[0])
			require.NoError(t, err)
			var body map[string]any
			require.NoError(t, json.Unmarshal(raw, &body))
			for field, value := range map[string]*float64{"old_avg_ttft_ms": tc.old, "new_avg_ttft_ms": tc.fresh} {
				require.Contains(t, body, field, "unknown mean latency must be explicitly null")
				if value == nil || *value <= 0 || math.IsNaN(*value) || math.IsInf(*value, 0) {
					require.Nil(t, body[field])
				} else {
					require.Equal(t, *value, body[field])
				}
			}
			require.Equal(t, tc.oldP90, body["old_p90_ttft_ms"], "legacy P90 fields keep their actual independent values")
			require.Equal(t, tc.newP90, body["new_p90_ttft_ms"])
			if tc.old != nil && *tc.old > 0 && !math.IsInf(*tc.old, 0) {
				*tc.old = 99
			}
			if tc.fresh != nil && *tc.fresh > 0 && !math.IsInf(*tc.fresh, 0) {
				*tc.fresh = 99
			}
			owned, err := json.Marshal(rows[0])
			require.NoError(t, err)
			require.JSONEq(t, string(raw), string(owned), "published diff values must not alias mutable source pointers")
		})
	}
}

func TestDiffPrioritySnapshotsMeanDiagnosticsAndMembership(t *testing.T) {
	before := &PrioritySchedulingSnapshot{LatencyPercentile: 95, LatencyMeasurement: "attributed_v1", Candidates: []PrioritySchedulingScore{
		{AccountID: 1, PrioritySchedulingSignal: PrioritySchedulingSignal{SelectedTTFTMs: new(900.0)}},
	}}
	for _, field := range []string{"percentile", "measurement"} {
		t.Run(field, func(t *testing.T) {
			after := *before
			if field == "percentile" {
				after.LatencyPercentile = 70
			} else {
				after.LatencyMeasurement = "legacy_forward"
			}
			rows := diffPrioritySnapshots(before, &after)
			require.Len(t, rows, 1)
			require.False(t, rows[0].Changed, "diagnostic percentile settings do not change shared mean scoring")
		})
	}
	after := &PrioritySchedulingSnapshot{LatencyPercentile: 95, LatencyMeasurement: "attributed_v1", Candidates: []PrioritySchedulingScore{
		{AccountID: 2, PrioritySchedulingSignal: PrioritySchedulingSignal{SelectedTTFTMs: new(1500.0)}},
	}}
	rows := priorityDiffRowsByAccount(diffPrioritySnapshots(before, after))
	require.Len(t, rows, 2)
	require.Equal(t, float64(900), *rows[1].OldAvgTTFTMs)
	require.Nil(t, rows[1].NewAvgTTFTMs)
	require.Nil(t, rows[2].OldAvgTTFTMs)
	require.Equal(t, float64(1500), *rows[2].NewAvgTTFTMs)
	require.True(t, rows[1].Changed)
	require.True(t, rows[2].Changed)
	first := diffPrioritySnapshots(nil, after)
	require.Len(t, first, 1)
	require.Nil(t, first[0].OldAvgTTFTMs)
	require.Equal(t, float64(1500), *first[0].NewAvgTTFTMs)

	// The removal path owns the old value just as the ordinary row path does.
	*before.Candidates[0].SelectedTTFTMs = 42
	require.Equal(t, float64(900), *rows[1].OldAvgTTFTMs)
}
