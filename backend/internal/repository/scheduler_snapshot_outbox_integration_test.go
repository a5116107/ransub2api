//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSchedulerSnapshotOutboxReplay(t *testing.T) {
	ctx := context.Background()
	rdb := testRedis(t)
	client := testEntClient(t)

	_, _ = integrationDB.ExecContext(ctx, "TRUNCATE scheduler_outbox")

	accountRepo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	outboxRepo := NewSchedulerOutboxRepository(integrationDB)
	cache := NewSchedulerCache(rdb)

	cfg := &config.Config{
		RunMode: config.RunModeStandard,
		Gateway: config.GatewayConfig{
			Scheduling: config.GatewaySchedulingConfig{
				OutboxPollIntervalSeconds:  1,
				FullRebuildIntervalSeconds: 0,
				DbFallbackEnabled:          true,
			},
		},
	}

	account := &service.Account{
		Name:        "outbox-replay-" + time.Now().Format("150405.000000"),
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 3,
		Priority:    1,
		Credentials: map[string]any{},
		Extra:       map[string]any{},
	}
	require.NoError(t, accountRepo.Create(ctx, account))
	require.NoError(t, cache.SetAccount(ctx, account))

	svc := service.NewSchedulerSnapshotService(cache, outboxRepo, accountRepo, nil, cfg)
	svc.Start()
	t.Cleanup(svc.Stop)

	require.NoError(t, accountRepo.UpdateLastUsed(ctx, account.ID))
	updated, err := accountRepo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.LastUsedAt)
	expectedUnix := updated.LastUsedAt.Unix()

	require.Eventually(t, func() bool {
		cached, err := cache.GetAccount(ctx, account.ID)
		if err != nil || cached == nil || cached.LastUsedAt == nil {
			return false
		}
		return cached.LastUsedAt.Unix() == expectedUnix
	}, 5*time.Second, 100*time.Millisecond)
}

func TestSchedulerSnapshotCooldownRecoveryWithoutRebuild(t *testing.T) {
	for _, kind := range []string{"temporary", "overload", "rate_limit"} {
		for _, grouped := range []bool{false, true} {
			name := kind + "/ungrouped"
			if grouped {
				name = kind + "/grouped"
			}
			t.Run(name, func(t *testing.T) {
				ctx := t.Context()
				client := testEntClient(t)
				repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
				rdb := testRedis(t)
				cache := NewSchedulerCache(rdb)
				account := &service.Account{
					Name: "cooldown-recovery", Platform: service.PlatformOpenAI,
					Type: service.AccountTypeAPIKey, Status: service.StatusActive,
					Schedulable: true, Concurrency: 3,
					Credentials: map[string]any{}, Extra: map[string]any{},
				}
				require.NoError(t, repo.Create(ctx, account))
				t.Cleanup(func() { require.NoError(t, repo.Delete(context.Background(), account.ID)) })
				var groupID *int64
				if grouped {
					group := &service.Group{
						Name: "cooldown-recovery-" + kind, Platform: service.PlatformOpenAI,
						Status: service.StatusActive, RateMultiplier: 1,
						SubscriptionType: service.SubscriptionTypeStandard,
					}
					groups := newGroupRepositoryWithSQL(client, integrationDB)
					require.NoError(t, groups.Create(ctx, group))
					t.Cleanup(func() { require.NoError(t, groups.Delete(context.Background(), group.ID)) })
					require.NoError(t, repo.AddToGroup(ctx, account.ID, group.ID, 1))
					groupID = &group.ID
				}
				until := time.Now().Add(2 * time.Second)
				switch kind {
				case "temporary":
					require.NoError(t, repo.SetTempUnschedulable(ctx, account.ID, until, "cooldown-recovery"))
				case "overload":
					require.NoError(t, repo.SetOverloaded(ctx, account.ID, until))
				case "rate_limit":
					require.NoError(t, repo.SetRateLimited(ctx, account.ID, until))
				}
				writerCfg := &config.Config{RunMode: config.RunModeStandard}
				writerCfg.Gateway.Scheduling.DbFallbackEnabled = true
				writer := service.NewSchedulerSnapshotService(cache, nil, repo, nil, writerCfg)
				blocked, _, err := writer.ListSchedulableAccounts(ctx, groupID, service.PlatformOpenAI, false)
				require.NoError(t, err)
				for _, candidate := range blocked {
					require.NotEqual(t, account.ID, candidate.ID, "a cooldown must still block requests")
				}
				bucket := service.SchedulerBucket{Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
				if groupID != nil {
					bucket.GroupID = *groupID
				}
				cached, hit, err := cache.GetSnapshot(ctx, bucket)
				require.NoError(t, err)
				require.True(t, hit)
				cachedIDs := make([]int64, 0, len(cached))
				for _, candidate := range cached {
					cachedIDs = append(cachedIDs, candidate.ID)
				}
				require.Contains(t, cachedIDs, account.ID, "cooldown must not erase durable snapshot membership")
				activeKey := schedulerBucketKey(schedulerActivePrefix, bucket)
				version, err := rdb.Get(ctx, activeKey).Result()
				require.NoError(t, err)
				readerCfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}}
				reader := service.NewSchedulerSnapshotService(cache, nil, nil, nil, readerCfg)
				require.Eventually(t, func() bool {
					return !time.Now().Before(until)
				}, 3*time.Second, 10*time.Millisecond)
				recovered, _, err := reader.ListSchedulableAccounts(ctx, groupID, service.PlatformOpenAI, false)
				require.NoError(t, err)
				recoveredIDs := make([]int64, 0, len(recovered))
				for _, candidate := range recovered {
					recoveredIDs = append(recoveredIDs, candidate.ID)
					require.True(t, candidate.IsSchedulable())
				}
				require.Contains(t, recoveredIDs, account.ID, "a reader with no database or rebuild worker must recover at expiry")
				after, err := rdb.Get(ctx, activeKey).Result()
				require.NoError(t, err)
				require.Equal(t, version, after, "recovery must not require publishing another snapshot")
			})
		}
	}
}
