//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

type adminUserUsageStatsRepoStub struct {
	usageBatchLogRepoStub
	stats  *usagestats.UsageStats
	err    error
	userID int64
	start  time.Time
	end    time.Time
}

func (r *adminUserUsageStatsRepoStub) GetUserStatsAggregated(_ context.Context, userID int64, start, end time.Time) (*usagestats.UsageStats, error) {
	r.userID = userID
	r.start = start
	r.end = end
	return r.stats, r.err
}

func TestAdminServiceGetUserUsageStatsUsesAggregatedRepository(t *testing.T) {
	repo := &adminUserUsageStatsRepoStub{stats: &usagestats.UsageStats{
		TotalRequests:     7,
		TotalTokens:       1234,
		TotalCost:         1.25,
		TotalActualCost:   0.75,
		AverageDurationMs: 245.5,
	}}
	svc := &adminServiceImpl{usageLogs: repo}

	value, err := svc.GetUserUsageStats(context.Background(), 42, "month")
	require.NoError(t, err)
	stats, ok := value.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "month", stats["period"])
	require.Equal(t, int64(7), stats["total_requests"])
	require.Equal(t, int64(1234), stats["total_tokens"])
	require.Equal(t, 1.25, stats["total_cost"])
	require.Equal(t, 0.75, stats["total_actual_cost"])
	require.Equal(t, 245.5, stats["avg_duration_ms"])
	require.Equal(t, int64(42), repo.userID)
	require.True(t, repo.start.Before(repo.end))
	duration := repo.end.Sub(repo.start)
	require.GreaterOrEqual(t, duration, 28*24*time.Hour)
	require.LessOrEqual(t, duration, 32*24*time.Hour)
}

func TestAdminServiceGetUserUsageStatsAcceptsSupportedPeriods(t *testing.T) {
	for _, period := range []string{"today", "day", "week", "month", "year"} {
		t.Run(period, func(t *testing.T) {
			repo := &adminUserUsageStatsRepoStub{stats: &usagestats.UsageStats{}}
			svc := &adminServiceImpl{usageLogs: repo}

			value, err := svc.GetUserUsageStats(context.Background(), 1, period)
			require.NoError(t, err)
			stats := value.(map[string]any)
			require.Equal(t, period, stats["period"])
			require.True(t, repo.start.Before(repo.end))
		})
	}
}

func TestAdminServiceGetUserUsageStatsRejectsInvalidPeriod(t *testing.T) {
	svc := &adminServiceImpl{usageLogs: &adminUserUsageStatsRepoStub{stats: &usagestats.UsageStats{}}}

	_, err := svc.GetUserUsageStats(context.Background(), 1, "quarter")
	require.Error(t, err)
	require.True(t, infraerrors.IsBadRequest(err))
	require.Equal(t, "INVALID_USAGE_PERIOD", infraerrors.Reason(err))
}

func TestAdminServiceGetUserUsageStatsFailsClosedWithoutRepository(t *testing.T) {
	_, err := (&adminServiceImpl{}).GetUserUsageStats(context.Background(), 1, "month")
	require.Error(t, err)
	require.Equal(t, 501, infraerrors.Code(err))
	require.Equal(t, "USER_USAGE_STATS_UNAVAILABLE", infraerrors.Reason(err))
}

func TestAdminServiceGetUserUsageStatsReturnsRepositoryError(t *testing.T) {
	svc := &adminServiceImpl{usageLogs: &adminUserUsageStatsRepoStub{err: errors.New("db unavailable")}}

	_, err := svc.GetUserUsageStats(context.Background(), 1, "week")
	require.ErrorContains(t, err, "db unavailable")
}
