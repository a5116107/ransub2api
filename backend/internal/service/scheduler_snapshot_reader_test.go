package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// Unimplemented cache/repository operations panic: readers must not reach any
// writer lifecycle, token capture, snapshot publication, or database operation.
type snapshotReaderCache struct {
	SchedulerCache
	accounts []*Account
	hit      bool
	err      error
	feedback *Account
}

func (c *snapshotReaderCache) GetSnapshot(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	return c.accounts, c.hit, c.err
}
func (c *snapshotReaderCache) GetAccount(context.Context, int64) (*Account, error) {
	if len(c.accounts) > 0 {
		return c.accounts[0], c.err
	}
	return nil, c.err
}
func (c *snapshotReaderCache) GetAccounts(context.Context, []int64) (map[int64]*Account, error) {
	out := make(map[int64]*Account)
	for _, account := range c.accounts {
		out[account.ID] = account
	}
	return out, c.err
}
func (c *snapshotReaderCache) SetAccount(_ context.Context, account *Account) error {
	c.feedback = account
	return nil
}

func snapshotReaderConfig() *config.Config {
	cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}}
	// An operator's legacy fallback flag must not undo the reader boundary.
	cfg.Gateway.Scheduling.DbFallbackEnabled = true
	return cfg
}

func TestSchedulerSnapshotReaderDoesNotStartOrManuallyRebuild(t *testing.T) {
	cache := &snapshotReaderCache{}
	svc, err := ProvideSchedulerSnapshotService(cache, nil, nil, nil, snapshotReaderConfig(), nil)
	require.NoError(t, err)
	defer svc.Stop()
	svc.pollOutbox()
	require.ErrorIs(t, svc.triggerFullRebuild("reader-test"), ErrSchedulerCacheNotReady)
	require.ErrorIs(t, svc.rebuildFullSnapshot(context.Background(), "reader-test"), ErrSchedulerCacheNotReady)
	_, err = svc.ExportRegionalScheduler(context.Background(), nil, nil)
	require.Error(t, err)
}

func TestSchedulerSnapshotReaderUsesHitsAndFailsClosedOnMissOrFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hit      bool
		accounts []*Account
		err      error
		success  bool
	}{
		{name: "hit", hit: true, accounts: []*Account{{ID: 1, Status: StatusActive, Schedulable: true}}, success: true},
		{name: "miss"},
		{name: "cache_error", err: errors.New("cache unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := &snapshotReaderCache{accounts: tc.accounts, hit: tc.hit, err: tc.err}
			svc := NewSchedulerSnapshotService(cache, nil, nil, nil, snapshotReaderConfig())
			defer svc.Stop()
			accounts, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
			if tc.success {
				require.NoError(t, err)
				require.Len(t, accounts, 1)
			} else {
				require.ErrorIs(t, err, ErrSchedulerCacheNotReady)
				require.Nil(t, accounts)
			}
			account, err := svc.GetAccount(context.Background(), 1)
			if tc.success {
				require.NoError(t, err)
				require.Equal(t, int64(1), account.ID)
			} else {
				require.ErrorIs(t, err, ErrSchedulerCacheNotReady)
				require.Nil(t, account)
			}
			batch, err := svc.GetAccounts(context.Background(), []int64{1})
			if tc.success {
				require.NoError(t, err)
				require.Len(t, batch, 1)
			} else {
				require.ErrorIs(t, err, ErrSchedulerCacheNotReady)
				require.Nil(t, batch)
			}
		})
	}
}

func TestSchedulerSnapshotReaderPreservesRequestAccountFeedback(t *testing.T) {
	cache := &snapshotReaderCache{}
	svc := NewSchedulerSnapshotService(cache, nil, nil, nil, snapshotReaderConfig())
	defer svc.Stop()
	account := &Account{ID: 1, Status: "error"}
	require.NoError(t, svc.UpdateAccountInCache(context.Background(), account))
	require.Same(t, account, cache.feedback)
}

func TestSchedulerSnapshotReaderChecksLiveEligibilityWithoutMutatingCache(t *testing.T) {
	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Minute)
	for _, tc := range []struct {
		name   string
		change func(*Account)
		allow  bool
	}{
		{"healthy", func(*Account) {}, true},
		{"temporary_cooldown", func(a *Account) { a.TempUnschedulableUntil = &future }, false},
		{"temporary_recovered", func(a *Account) { a.TempUnschedulableUntil = &past }, true},
		{"overloaded", func(a *Account) { a.OverloadUntil = &future }, false},
		{"overload_recovered", func(a *Account) { a.OverloadUntil = &past }, true},
		{"rate_limited", func(a *Account) { a.RateLimitResetAt = &future }, false},
		{"rate_limit_recovered", func(a *Account) { a.RateLimitResetAt = &past }, true},
		{"expired", func(a *Account) { a.ExpiresAt, a.AutoPauseOnExpired = &past, true }, false},
		{"expiry_pause_disabled", func(a *Account) { a.ExpiresAt = &past }, true},
		{"manual_pause", func(a *Account) { a.Schedulable = false }, false},
		{"disabled", func(a *Account) { a.Status = StatusDisabled }, false},
		{"performance_paused", func(a *Account) { a.PerformancePaused = true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 1, Status: StatusActive, Schedulable: true}
			tc.change(account)
			cache := &snapshotReaderCache{accounts: []*Account{account}, hit: true}
			svc := NewSchedulerSnapshotService(cache, nil, nil, nil, snapshotReaderConfig())
			for range 2 {
				accounts, _, err := svc.ListSchedulableAccounts(t.Context(), nil, PlatformOpenAI, false)
				require.NoError(t, err)
				if tc.allow {
					require.Len(t, accounts, 1)
				} else {
					require.Empty(t, accounts)
				}
				require.Len(t, cache.accounts, 1, "request filtering must not delete the stored member")
				require.Same(t, account, cache.accounts[0])
			}
		})
	}
}

func TestSchedulerSnapshotReaderRequiresAllRequestedAccountDetails(t *testing.T) {
	cache := &snapshotReaderCache{accounts: []*Account{{ID: 1}}}
	svc := NewSchedulerSnapshotService(cache, nil, nil, nil, snapshotReaderConfig())
	defer svc.Stop()
	accounts, err := svc.GetAccounts(context.Background(), []int64{1, 2})
	require.ErrorIs(t, err, ErrSchedulerCacheNotReady)
	require.Nil(t, accounts, "partial cache hits must not make an incomplete request eligible")
}

func TestFullSnapshotReaderDisablesBulkWorkWithoutDisablingOtherJobs(t *testing.T) {
	cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleFull, SchedulerSnapshotMode: config.SchedulerSnapshotModeReader}}
	require.True(t, cfg.RunsBackgroundJobs())
	svc := NewSchedulerSnapshotService(&snapshotReaderCache{}, nil, nil, nil, cfg)
	svc.Start()
	defer svc.Stop()
	require.ErrorIs(t, svc.guardFallback(context.Background()), ErrSchedulerCacheNotReady)
	require.ErrorIs(t, svc.triggerFullRebuild("reader-test"), ErrSchedulerCacheNotReady)
}
