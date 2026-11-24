package service

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCNMonitoringPriorityReviewDrainsBeforeUnobservedPeer(t *testing.T) {
	repo := &fakeCNCheckRepo{byPlatform: map[string][]Account{}}
	now := time.Now().UTC()
	for i := int64(1); i <= 8; i++ {
		a := Account{ID: i, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive,
			Credentials: map[string]any{"account_mode": AccountModeCoding, "base_url": "https://api.kimi.com/coding/v1"}, CreatedAt: now.Add(-24 * time.Hour)}
		if i >= 5 {
			a.Extra = map[string]any{"kimi_usage_updated_at": now.Add(-2 * time.Hour).Format(time.RFC3339Nano), "kimi_5h_used_percent": 10.0}
		}
		repo.byPlatform[PlatformKimi] = append(repo.byPlatform[PlatformKimi], a)
	}
	var sends, reviews atomic.Int32
	svc := NewCNProviderBalanceCheckService(repo, &CNProviderBalanceService{}, nil, &config.Config{}, time.Minute)
	budget := NewMonitoringProbeBudgetService(&monitoringBudgetRepoStub{})
	svc.SetMonitoringBudget(budget)
	svc.quotaService = cnQueueQuotaProber{query: func(ctx context.Context, id int64) (*CNProviderQuotaProbeResult, error) {
		if sends.Add(1) > 4 {
			denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day"}
			recordMonitoringProbeLocalError(ctx, denial)
			return nil, denial
		}
		if id >= 5 {
			reviews.Add(1)
		}
		return &CNProviderQuotaProbeResult{Success: true}, nil
	}}
	svc.runOnce(t.Context())
	require.EqualValues(t, 4, reviews.Load(), "same-age required reviews must use the first four admissions, not an unobserved low-ID prefix")
	// The next window must serve the older unserved scopes, even though the
	// successful review test double intentionally does not renew its evidence.
	sends.Store(0)
	reviews.Store(0)
	svc = NewCNProviderBalanceCheckService(repo, &CNProviderBalanceService{}, nil, &config.Config{}, time.Minute)
	svc.SetMonitoringBudget(budget)
	svc.quotaService = cnQueueQuotaProber{query: func(ctx context.Context, id int64) (*CNProviderQuotaProbeResult, error) {
		if sends.Add(1) > 4 {
			denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day"}
			recordMonitoringProbeLocalError(ctx, denial)
			return nil, denial
		}
		if id >= 5 {
			reviews.Add(1)
		}
		return &CNProviderQuotaProbeResult{Success: true}, nil
	}}
	svc.runOnce(t.Context())
	require.Zero(t, reviews.Load(), "recently served reviews must not jump older unserved scopes after executor rebuild")
}

func TestCNMonitoringPriorityClassifiesOnlyNativeUsableDueObservations(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 123456789, time.UTC)
	const interval = 10 * time.Minute
	for _, provider := range []string{PlatformKimi, PlatformZhipu, PlatformMiniMax, PlatformCommandCode, PlatformCline, PlatformMistral} {
		t.Run(provider, func(t *testing.T) {
			a := &Account{Platform: provider, Type: AccountTypeAPIKey, Credentials: map[string]any{"account_mode": AccountModeCoding}}
			switch provider {
			case PlatformKimi:
				a.Credentials["base_url"] = "https://api.kimi.com/coding/v1"
			case PlatformZhipu:
				a.Credentials["base_url"] = "https://open.bigmodel.cn/api/coding/paas/v4"
			case PlatformMiniMax:
				a.Credentials["base_url"] = "https://api.minimax.io/anthropic"
			case PlatformCommandCode:
				a.Credentials = commandCodeTestAccount(4).Credentials
			case PlatformCline:
				a = clineTestAccount(4)
			case PlatformMistral:
				a = mistralUsageTestAccount(4, "")
			}
			require.Equal(t, provider, a.GetCodingPlanProvider())
			stamp := now.Add(-2 * interval)
			a.Extra = map[string]any{cnExtraKey(provider, cnExtraSuffixUsageUpdated): stamp.Format(time.RFC3339Nano), cnExtraKey(provider, cnExtraSuffixWeeklyUsed): "35.5",
				cnExtraKey(provider, cnExtraSuffixWeeklyReset): now.Add(-time.Hour).Format(time.RFC3339Nano)}
			target := accountProbeTarget{account: a}
			require.Equal(t, stamp.Add(interval), cnMonitoringReviewAt(target, interval, now), "elapsed quota reset does not erase a real cadence review")
			a.Extra[cnExtraKey(provider, cnExtraSuffixWeeklyReset)] = now.Add(24 * time.Hour).Format(time.RFC3339Nano)
			require.Equal(t, stamp.Add(interval), cnMonitoringReviewAt(target, interval, now), "quota reset is not a freshness lease")
			a.Extra[cnExtraKey(provider, cnExtraSuffixUsageUpdated)] = now.Add(-interval + time.Nanosecond).Format(time.RFC3339Nano)
			require.True(t, cnMonitoringReviewAt(target, interval, now).IsZero(), "do not round or advance the actual due gate")
			a.Extra[cnExtraKey(provider, cnExtraSuffixUsageUpdated)] = now.Add(-interval).Format(time.RFC3339Nano)
			require.Equal(t, now, cnMonitoringReviewAt(target, interval, now))
		})
	}
	for _, test := range []struct {
		name         string
		stamp, value any
	}{
		{"missing", nil, 20.0}, {"malformed", "bad", 20.0}, {"zero", "0001-01-01T00:00:00Z", 20.0},
		{"future", now.Add(time.Hour).Format(time.RFC3339Nano), 20.0}, {"noncanonical", now.Unix(), 20.0},
		{"timestamp_only", now.Add(-time.Hour).Format(time.RFC3339Nano), nil}, {"nan", now.Add(-time.Hour).Format(time.RFC3339Nano), math.NaN()},
		{"infinity", now.Add(-time.Hour).Format(time.RFC3339Nano), "+Inf"}, {"negative_usage", now.Add(-time.Hour).Format(time.RFC3339Nano), -1.0},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := &Account{Platform: PlatformKimi, Credentials: map[string]any{"account_mode": AccountModeCoding, "base_url": "https://api.kimi.com/coding/v1"}, Extra: map[string]any{"kimi_usage_updated_at": test.stamp, "kimi_5h_used_percent": test.value}}
			require.True(t, cnMonitoringReviewAt(accountProbeTarget{account: a}, interval, now).IsZero())
		})
	}
	a := &Account{Platform: PlatformDeepseek, Extra: map[string]any{"deepseek_balance_updated_at": now.Add(-time.Hour).Format(time.RFC3339Nano), "deepseek_balance": -2.0}}
	require.Equal(t, now.Add(-time.Hour+interval), cnMonitoringReviewAt(accountProbeTarget{account: a, balance: true}, interval, now), "negative balance is a valid low-balance observation")
	require.True(t, cnMonitoringReviewAt(accountProbeTarget{account: a, balance: true}, 0, now).IsZero())
	delete(a.Extra, "deepseek_balance_updated_at")
	a.Extra["kimi_balance_updated_at"] = now.Add(-time.Hour).Format(time.RFC3339Nano)
	require.True(t, cnMonitoringReviewAt(accountProbeTarget{account: a, balance: true}, interval, now).IsZero(), "other provider evidence cannot borrow priority")
	a = clineTestAccount(4)
	a.Extra = map[string]any{"cline_usage_updated_at": now.Add(-time.Hour).Format(time.RFC3339Nano), "cline_5h_used_percent": 10.0,
		"cline_balance_updated_at": now.Add(-2 * time.Hour).Format(time.RFC3339Nano), "cline_balance": 10.0}
	require.Equal(t, now.Add(-2*time.Hour+interval), cnMonitoringReviewAt(accountProbeTarget{account: a}, interval, now), "a combined query uses its oldest due valid component")
	a.Extra["cline_balance_updated_at"] = "broken"
	require.Equal(t, now.Add(-time.Hour+interval), cnMonitoringReviewAt(accountProbeTarget{account: a}, interval, now), "one damaged component does not erase another due observation")
	a = &Account{Platform: PlatformKimi, Credentials: map[string]any{"account_mode": AccountModeCoding, "base_url": "https://api.kimi.com/coding/v1"},
		Extra: map[string]any{"kimi_usage_updated_at": now.Add(-time.Hour).Format(time.RFC3339Nano), "kimi_monthly_used_percent": 20.0}}
	require.True(t, cnMonitoringReviewAt(accountProbeTarget{account: a}, interval, now).IsZero(), "unsupported window names must not manufacture native observations")
}

func TestCNMonitoringPriorityWaitsForReviewPhaseWithoutLosingParallelism(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := cnQueueAccounts(t, 12)
		for platform, accounts := range repo.byPlatform {
			for i := range accounts {
				if accounts[i].ID > 4 {
					continue
				}
				if platform == PlatformKimi {
					accounts[i].Credentials["base_url"] = "https://api.kimi.com/coding/v1"
				}
				accounts[i].Extra = map[string]any{cnExtraKey(platform, cnExtraSuffixUsageUpdated): time.Now().Add(-time.Hour).Format(time.RFC3339Nano), cnExtraKey(platform, cnExtraSuffix5hUsed): 10.0}
			}
		}
		var calls, early atomic.Int32
		entered, release, done := make(chan struct{}, 4), make(chan struct{}), make(chan struct{})
		var once sync.Once
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(func() { cancel(); once.Do(func() { close(release) }); <-done })
		svc := NewCNProviderBalanceCheckService(repo, &CNProviderBalanceService{}, nil, &config.Config{}, time.Minute)
		svc.quotaService = cnQueueQuotaProber{query: func(_ context.Context, id int64) (*CNProviderQuotaProbeResult, error) {
			calls.Add(1)
			if id <= 4 {
				entered <- struct{}{}
				<-release
			} else {
				select {
				case <-release:
				default:
					early.Add(1)
				}
			}
			return &CNProviderQuotaProbeResult{Success: true}, nil
		}}
		go func() { defer close(done); svc.runOnce(ctx) }()
		for range 4 {
			<-entered
		}
		synctest.Wait()
		require.EqualValues(t, 4, calls.Load(), "four distinct review scopes may run together")
		require.Zero(t, early.Load())
		once.Do(func() { close(release) })
		<-done
		require.EqualValues(t, 12, calls.Load())
		require.EqualValues(t, 4, svc.MonitoringRuntime().LastScan.ReviewTargets)
		require.EqualValues(t, 8, svc.MonitoringRuntime().LastScan.RoutineTargets)
	})
}

func TestCNMonitoringPriorityCrossesBalanceAndQuotaWithoutPhaseLeak(t *testing.T) {
	for _, balanceReview := range []bool{false, true} {
		t.Run(map[bool]string{false: "quota_reviews", true: "balance_reviews"}[balanceReview], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repo := cnQueueAccounts(t, 4)
				for platform, accounts := range repo.byPlatform {
					for i := range accounts {
						if platform == PlatformKimi {
							accounts[i].Credentials["base_url"] = "https://api.kimi.com/coding/v1"
						}
						if !balanceReview {
							accounts[i].Extra = map[string]any{cnExtraKey(platform, cnExtraSuffixUsageUpdated): time.Now().Add(-time.Hour).Format(time.RFC3339Nano), cnExtraKey(platform, cnExtraSuffix5hUsed): 10.0}
						}
					}
				}
				for id := int64(101); id <= 104; id++ {
					a := Account{ID: id, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
					if balanceReview {
						a.Extra = map[string]any{"deepseek_balance_updated_at": time.Now().Add(-time.Hour).Format(time.RFC3339Nano), "deepseek_balance": 10.0}
					}
					repo.byPlatform[PlatformDeepseek] = append(repo.byPlatform[PlatformDeepseek], a)
				}
				var finished, early, calls, activePayg, maxPayg atomic.Int32
				check := func(balance bool) {
					calls.Add(1)
					if balance {
						n := activePayg.Add(1)
						for old := maxPayg.Load(); n > old && !maxPayg.CompareAndSwap(old, n); old = maxPayg.Load() {
						}
						defer activePayg.Add(-1)
					}
					if balance == balanceReview {
						time.Sleep(time.Millisecond)
						finished.Add(1)
					} else {
						if finished.Load() != 4 {
							early.Add(1)
						}
						time.Sleep(time.Millisecond)
					}
				}
				balance := cnQueueBalanceRepo{load: func(context.Context, int64) (*Account, error) {
					check(true)
					return nil, errors.New("fixture unavailable balance")
				}}
				svc := NewCNProviderBalanceCheckService(repo, NewCNProviderBalanceService(balance, nil, nil, nil), nil, &config.Config{}, time.Minute)
				svc.quotaService = cnQueueQuotaProber{query: func(context.Context, int64) (*CNProviderQuotaProbeResult, error) {
					check(false)
					return &CNProviderQuotaProbeResult{Success: true}, nil
				}}
				svc.runOnce(t.Context())
				require.Zero(t, early.Load(), "routine work from the other native lane cannot race unfinished required reviews")
				require.EqualValues(t, 8, calls.Load())
				require.EqualValues(t, 1, maxPayg.Load(), "priority cannot raise payg's original serial limit")
				require.EqualValues(t, 4, svc.MonitoringRuntime().LastScan.ReviewTargets)
			})
		})
	}
}

func TestCNMonitoringPriorityOrdersDueReviewsAndRoutineCreation(t *testing.T) {
	for _, review := range []bool{false, true} {
		t.Run(map[bool]string{false: "routine_created_at", true: "review_due_at"}[review], func(t *testing.T) {
			repo := &fakeCNCheckRepo{byPlatform: map[string][]Account{}}
			now := time.Now().UTC()
			for id := int64(1); id <= 8; id++ {
				a := Account{ID: id, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive,
					Credentials: map[string]any{"account_mode": AccountModeCoding, "base_url": "https://api.kimi.com/coding/v1"}, CreatedAt: now.Add(-time.Hour)}
				older := now.Add(-time.Duration(id) * time.Hour)
				if review {
					a.Extra = map[string]any{"kimi_usage_updated_at": older.Format(time.RFC3339Nano), "kimi_5h_used_percent": 10.0}
				} else {
					a.CreatedAt = older
				}
				repo.byPlatform[PlatformKimi] = append(repo.byPlatform[PlatformKimi], a)
			}
			var admitted atomic.Int32
			var seen atomic.Uint64
			svc := NewCNProviderBalanceCheckService(repo, &CNProviderBalanceService{}, nil, &config.Config{}, time.Minute)
			svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(&monitoringBudgetRepoStub{}))
			svc.quotaService = cnQueueQuotaProber{query: func(ctx context.Context, id int64) (*CNProviderQuotaProbeResult, error) {
				if admitted.Add(1) > 4 {
					denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day"}
					recordMonitoringProbeLocalError(ctx, denial)
					return nil, denial
				}
				seen.Or(1 << uint(id))
				return &CNProviderQuotaProbeResult{Success: true}, nil
			}}
			svc.runOnce(t.Context())
			require.EqualValues(t, (1<<5)|(1<<6)|(1<<7)|(1<<8), seen.Load(), "oldest due/created times must precede lower IDs in the same unserved turn")
		})
	}
}
