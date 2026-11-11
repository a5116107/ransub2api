package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type schedulerLatencyAccountRepo struct{ schedulerTestOpenAIAccountRepo }

func (r schedulerLatencyAccountRepo) ListSchedulableByPlatform(ctx context.Context, platform string) ([]Account, error) {
	time.Sleep(20 * time.Millisecond)
	return r.schedulerTestOpenAIAccountRepo.ListSchedulableByPlatform(ctx, platform)
}

func (r schedulerLatencyAccountRepo) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]Account, error) {
	return r.ListSchedulableByPlatform(ctx, platform)
}

func TestOpenAISchedulerSelectReturnsRealLatency(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	for _, withAccount := range []bool{false, true} {
		name := "no_available_account"
		var accounts []Account
		if withAccount {
			name = "selected_account"
			accounts = []Account{{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5}}
		}
		t.Run(name, func(t *testing.T) {
			scheduler := &defaultOpenAIAccountScheduler{
				service: &OpenAIGatewayService{accountRepo: schedulerLatencyAccountRepo{schedulerTestOpenAIAccountRepo{accounts: accounts}}},
				stats:   newOpenAIAccountRuntimeStats(),
			}
			selection, decision, err := scheduler.Select(context.Background(), OpenAIAccountScheduleRequest{Platform: PlatformOpenAI})
			if withAccount {
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.Equal(t, int64(1), decision.SelectedAccountID)
				if selection.ReleaseFunc != nil {
					t.Cleanup(selection.ReleaseFunc)
				}
			} else {
				require.Error(t, err)
			}
			require.GreaterOrEqual(t, decision.LatencyMs, int64(20))
			metrics := scheduler.SnapshotMetrics()
			require.Equal(t, decision.LatencyMs, metrics.SchedulerLatencyMsTotal)
			require.Equal(t, float64(decision.LatencyMs), metrics.SchedulerLatencyMsAvg)
		})
	}
}

func TestOpenAISchedulerStickyHitRatioCountsSelectionOnce(t *testing.T) {
	scheduler := &defaultOpenAIAccountScheduler{stats: newOpenAIAccountRuntimeStats()}
	scheduler.metrics.recordSelect(OpenAIAccountScheduleDecision{StickyPreviousHit: true, StickySessionHit: true})
	snapshot := scheduler.SnapshotMetrics()
	require.Equal(t, int64(1), snapshot.SelectTotal)
	require.Equal(t, int64(1), snapshot.StickyPreviousHitTotal)
	require.Equal(t, int64(1), snapshot.StickySessionHitTotal)
	require.Equal(t, 1.0, snapshot.StickyHitRatio)

	scheduler.metrics.recordSelect(OpenAIAccountScheduleDecision{StickyPreviousHit: true})
	scheduler.metrics.recordSelect(OpenAIAccountScheduleDecision{StickySessionHit: true})
	scheduler.metrics.recordSelect(OpenAIAccountScheduleDecision{})
	snapshot = scheduler.SnapshotMetrics()
	require.Equal(t, int64(4), snapshot.SelectTotal)
	require.Equal(t, int64(2), snapshot.StickyPreviousHitTotal)
	require.Equal(t, int64(2), snapshot.StickySessionHitTotal)
	require.Equal(t, 0.75, snapshot.StickyHitRatio)
}

func TestOpenAIAccountRuntimeStatsTTFTExpiresIndependently(t *testing.T) {
	for _, tc := range []struct {
		name  string
		age   time.Duration
		known bool
	}{
		{"fresh", 9 * time.Minute, true},
		{"last_fresh_instant", 10*time.Minute - time.Nanosecond, true},
		{"expired", 10 * time.Minute, false},
		{"long_idle", 24 * time.Hour, false},
		{"future_sample", -time.Nanosecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stats := newOpenAIAccountRuntimeStats()
			now := time.Now()
			slow := 40000
			stats.reportAt(1, true, &slow, now)
			for _, missing := range []*int{nil, intPtrForTest(0), intPtrForTest(-1)} {
				stats.reportAt(1, true, missing, now.Add(tc.age))
			}
			_, latency, known := stats.snapshotAt(1, now.Add(tc.age))
			require.Equal(t, tc.known, known, "unmeasured results must not renew a TTFT sample")
			if tc.known {
				require.Equal(t, float64(slow), latency)
			} else {
				require.Zero(t, latency, "expired or future latency remains unknown")
			}
		})
	}
}

func TestOpenAIAccountRuntimeStatsTTFTRecoveryAndLateReports(t *testing.T) {
	now := time.Now()
	slow, fast := 40000, 1000
	t.Run("first_fresh_sample_replaces_expired_ewma", func(t *testing.T) {
		stats := newOpenAIAccountRuntimeStats()
		stats.reportAt(1, true, &slow, now)
		stats.reportAt(1, true, &fast, now.Add(11*time.Minute))
		_, latency, known := stats.snapshotAt(1, now.Add(11*time.Minute))
		require.True(t, known)
		require.Equal(t, float64(fast), latency, "an expired slow sample must not contaminate recovery")
	})
	t.Run("late_sample_cannot_borrow_newer_result_time", func(t *testing.T) {
		stats := newOpenAIAccountRuntimeStats()
		stats.reportAt(1, true, nil, now.Add(11*time.Minute))
		stats.reportAt(1, true, &slow, now)
		_, latency, known := stats.snapshotAt(1, now.Add(11*time.Minute))
		require.False(t, known)
		require.Zero(t, latency)
	})
	t.Run("older_sample_cannot_overwrite_newer_latency", func(t *testing.T) {
		stats := newOpenAIAccountRuntimeStats()
		stats.reportAt(1, true, &fast, now.Add(11*time.Minute))
		stats.reportAt(1, true, &slow, now.Add(time.Minute))
		_, latency, known := stats.snapshotAt(1, now.Add(11*time.Minute))
		require.True(t, known)
		require.Equal(t, float64(fast), latency)
	})
	t.Run("fresh_samples_keep_existing_ewma", func(t *testing.T) {
		stats := newOpenAIAccountRuntimeStats()
		stats.reportAt(1, true, &slow, now)
		stats.reportAt(1, true, &fast, now.Add(time.Minute))
		_, latency, known := stats.snapshotAt(1, now.Add(time.Minute))
		require.True(t, known)
		require.Equal(t, 0.2*float64(fast)+0.8*float64(slow), latency)
	})
	t.Run("zero_timestamp_is_not_latency_evidence", func(t *testing.T) {
		stats := newOpenAIAccountRuntimeStats()
		stats.reportAt(1, true, &slow, time.Time{})
		_, latency, known := stats.snapshotAt(1, now)
		require.False(t, known)
		require.Zero(t, latency)
	})
}

func TestOpenAISchedulerTTFTFreshnessPreservesHealthyStickySession(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	for _, tc := range []struct {
		name     string
		age      time.Duration
		failures int
		sticky   bool
	}{
		{"expired_latency_keeps_binding", 11 * time.Minute, 0, true},
		{"fresh_slow_latency_escapes", 9 * time.Minute, 0, false},
		{"new_failures_still_escape", 11 * time.Minute, 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			groupID := int64(10101)
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:ttft-session": 1}}
			cfg := newSchedulerTestSubscriptionPriorityConfig()
			cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
			cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
			cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
			stats := newOpenAIAccountRuntimeStats()
			stats.now = func() time.Time { return now.Add(tc.age) }
			slow := 40000
			stats.reportAt(1, true, &slow, now)
			stats.reportAt(1, true, nil, now.Add(tc.age))
			for range tc.failures {
				stats.reportAt(1, false, nil, now.Add(tc.age))
			}
			svc := &OpenAIGatewayService{
				accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{
					{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1, GroupIDs: []int64{groupID}},
					{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID}},
				}},
				cache: cache, cfg: cfg, openaiAccountStats: stats,
				rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{1: true, 2: true}}),
			}
			selection, decision, err := svc.SelectAccountWithScheduler(t.Context(), &groupID, "", "ttft-session", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.NotNil(t, selection)
			if selection.ReleaseFunc != nil {
				t.Cleanup(selection.ReleaseFunc)
			}
			require.Equal(t, tc.sticky, decision.StickySessionHit)
			if tc.sticky {
				require.Equal(t, int64(1), selection.Account.ID)
				require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
				require.Equal(t, int64(1), cache.sessionBindings["openai:ttft-session"])
			} else {
				require.Equal(t, int64(2), selection.Account.ID)
				require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
			}
		})
	}
}
