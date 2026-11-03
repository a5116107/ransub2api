//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func gatewayProfitTestGroup(id int64, platform string) *Group {
	return &Group{
		ID:                   id,
		Name:                 "profit-" + platform,
		Platform:             platform,
		Status:               StatusActive,
		Hydrated:             true,
		RateMultiplier:       0.5,
		SubscriptionType:     SubscriptionTypeStandard,
		ProfitControlEnabled: true,
		ProfitMinMargin:      0,
		ProfitSafetyBuffer:   0,
	}
}

func gatewayProfitTestContext(group *Group) context.Context {
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	ctx, _ = WithGatewayTokenRequestPricing(ctx)
	return ctx
}

func gatewayProfitTestAccount(id int64, platform string, rate float64, groupID int64) Account {
	return Account{
		ID:             id,
		Name:           "account",
		Platform:       platform,
		Type:           AccountTypeAPIKey,
		Status:         StatusActive,
		Schedulable:    true,
		Concurrency:    2,
		Priority:       1,
		RateMultiplier: &rate,
		AccountGroups:  []AccountGroup{{AccountID: id, GroupID: groupID}},
		GroupIDs:       []int64{groupID},
	}
}

func TestGatewayProfitControlFixedSaleAcrossPlatforms(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformGrok, PlatformAntigravity} {
		t.Run(platform, func(t *testing.T) {
			group := gatewayProfitTestGroup(930, platform)
			svc := &GatewayService{}
			account := gatewayProfitTestAccount(931, platform, 0, group.ID)
			markup := 2.0
			account.GroupRateMultiplier = &markup
			ctx := svc.withGatewayProfitControlGate(gatewayProfitTestContext(group), &group.ID)
			vetoed, reason := OpenAIProfitControlVeto(ctx, &account)
			require.True(t, vetoed, "even a free upstream must not change the sale price")
			require.Equal(t, "profit_account_sale_rate_mismatch", reason)
			require.False(t, svc.isGatewayAccountProfitEligible(ctx, &account))
			_, vetoed, reason = svc.GatewayProfitControlVetoLatest(ctx, &account)
			require.True(t, vetoed)
			require.Equal(t, "profit_account_sale_rate_mismatch", reason)
			markup = 1
			require.True(t, svc.isGatewayAccountProfitEligible(ctx, &account), "restoring the configured price must restore admission")
			group.ProfitControlEnabled = false
			markup = 2
			plain := svc.withGatewayProfitControlGate(gatewayProfitTestContext(group), &group.ID)
			require.True(t, svc.isGatewayAccountProfitEligible(plain, &account), "known disabled groups preserve account pricing")
		})
	}
}

func TestGatewayProfitControlDiagnosticsCountEveryRejectedAccount(t *testing.T) {
	group := gatewayProfitTestGroup(940, PlatformAnthropic)
	account := gatewayProfitTestAccount(941, PlatformAnthropic, 0.2, group.ID)
	for _, tc := range []struct{ name, reason string }{
		{"sale price", "profit_account_sale_rate_mismatch"},
		{"group config", "profit_group_config_unavailable"},
		{"upstream evidence", "profit_upstream_rate_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := account
			gate := &openAIProfitControlGate{groupID: group.ID, platform: group.Platform, threshold: 0.5}
			switch tc.name {
			case "sale price":
				markup := 2.0
				candidate.GroupRateMultiplier = &markup
			case "group config":
				gate.configUnavailable = true
			case "upstream evidence":
				candidate.Extra = map[string]any{UpstreamBillingProbeEnabledExtraKey: true, UpstreamBillingRateSyncEnabledExtraKey: true}
			}
			ctx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, gate)
			svc := &GatewayService{}
			stats := svc.collectSelectionFailureStats(ctx, &group.ID, []Account{candidate}, "gpt-test", PlatformAnthropic, nil, false)
			require.Equal(t, 1, stats.Total)
			require.Zero(t, stats.Eligible)
			require.Contains(t, summarizeSelectionFailureStats(stats), tc.reason+"=1")
		})
	}
}

func TestGatewayProfitControlInstallsForFivePlatformsOnlyOnTokenRequests(t *testing.T) {
	for _, platform := range []string{
		PlatformOpenAI,
		PlatformAnthropic,
		PlatformGemini,
		PlatformGrok,
		PlatformAntigravity,
	} {
		t.Run(platform, func(t *testing.T) {
			group := gatewayProfitTestGroup(101, platform)
			groupID := group.ID
			svc := &GatewayService{}

			tokenCtx := svc.withGatewayProfitControlGate(gatewayProfitTestContext(group), &groupID)
			gate, _ := tokenCtx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
			require.NotNil(t, gate)
			require.Equal(t, platform, gate.platform)
			require.InDelta(t, 0.5, gate.threshold, 1e-12)

			metadataCtx := context.WithValue(context.Background(), ctxkey.Group, group)
			metadataCtx = svc.withGatewayProfitControlGate(metadataCtx, &groupID)
			gate, _ = metadataCtx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
			require.Nil(t, gate, "未显式标记为 token 请求的入口不得装门")
		})
	}
}

func TestGatewayProfitControlCompositeBillingUsesScheduledMemberConfig(t *testing.T) {
	billingGroup := &Group{
		ID:               201,
		Platform:         PlatformComposite,
		Status:           StatusActive,
		Hydrated:         true,
		RateMultiplier:   0.4,
		SubscriptionType: SubscriptionTypeStandard,
	}
	memberGroup := gatewayProfitTestGroup(202, PlatformAnthropic)
	memberGroup.RateMultiplier = 99
	memberGroup.ProfitMinMargin = 0.25

	ctx := context.WithValue(context.Background(), ctxkey.Group, billingGroup)
	ctx, pricingAt := WithGatewayTokenRequestPricing(ctx)
	svc := &GatewayService{
		schedulerSnapshot: NewSchedulerSnapshotService(
			nil,
			nil,
			nil,
			profitControlGroupRepo{group: memberGroup},
			nil,
		),
	}
	ctx = svc.withGatewayProfitControlGate(ctx, &memberGroup.ID)
	gate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.NotNil(t, gate)
	require.Equal(t, memberGroup.ID, gate.groupID)
	require.Equal(t, PlatformAnthropic, gate.platform)
	require.Equal(t, pricingAt, gate.pricingAt)
	require.InDelta(t, 0.4*(1-0.25), gate.threshold, 1e-12, "D 必须取 composite 计费父分组，margin 取被调度成员分组")
}

func TestGatewayProfitControlGroupLoadFailureRejectsAdmission(t *testing.T) {
	billingGroup := &Group{
		ID:               211,
		Platform:         PlatformComposite,
		Status:           StatusActive,
		Hydrated:         true,
		RateMultiplier:   0.4,
		SubscriptionType: SubscriptionTypeStandard,
	}
	targetGroupID := int64(212)
	ctx := gatewayProfitTestContext(billingGroup)
	ctx = context.WithValue(ctx, openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{
		groupID:   210,
		platform:  PlatformAnthropic,
		threshold: 0.1,
	})
	svc := &GatewayService{
		schedulerSnapshot: NewSchedulerSnapshotService(
			nil,
			nil,
			nil,
			profitControlFailingGroupRepo{},
			nil,
		),
	}

	ctx = svc.withGatewayProfitControlGate(ctx, &targetGroupID)
	gate, ok := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.True(t, ok)
	require.NotNil(t, gate, "加载新分组失败时必须保留拒绝准入的门")
	require.Equal(t, targetGroupID, gate.groupID, "不能复用其他分组的阈值")
	for _, rate := range []float64{0, 0.8} {
		account := gatewayProfitTestAccount(213, PlatformAnthropic, rate, targetGroupID)
		require.False(t, svc.isGatewayAccountProfitEligible(ctx, &account), "未知配置不能被免费账号绕过")
		vetoed, reason := OpenAIProfitControlVeto(ctx, &account)
		require.True(t, vetoed)
		require.Equal(t, "profit_group_config_unavailable", reason)
	}
}

func TestOpenAIProfitControlGroupLoadFailureRejectsAdmission(t *testing.T) {
	for _, turn := range []bool{false, true} {
		name := "http"
		if turn {
			name = "websocket turn"
		}
		t.Run(name, func(t *testing.T) {
			billingGroup := profitControlTestGroup(901, 0, 0)
			billingGroup.Platform = PlatformComposite
			targetID := int64(902)
			ctx := profitControlTestCtx(billingGroup)
			previous := &openAIProfitControlGate{groupID: 903, threshold: 1}
			if turn {
				previous.groupID = targetID
			}
			ctx = context.WithValue(ctx, openAIProfitControlGateCtxKey{}, previous)
			svc := &OpenAIGatewayService{schedulerSnapshot: NewSchedulerSnapshotService(nil, nil, nil, profitControlFailingGroupRepo{}, nil)}
			var next context.Context
			var pricingAt time.Time
			if turn {
				next, pricingAt = svc.WithOpenAITurnPricingContext(ctx, &targetID)
			} else {
				next, pricingAt = svc.WithOpenAIRequestPricingContext(ctx, &targetID)
			}
			gate, _ := next.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
			require.NotNil(t, gate)
			require.NotSame(t, previous, gate)
			require.Equal(t, targetID, gate.groupID)
			require.Equal(t, pricingAt, gate.pricingAt)
			require.Same(t, previous, ctx.Value(openAIProfitControlGateCtxKey{}), "不修改先前请求/轮次的门")
			require.Same(t, billingGroup, next.Value(ctxkey.Group), "不重写计费分组")
			for _, rate := range []float64{0, 0.8} {
				account := &Account{ID: 904, RateMultiplier: &rate}
				vetoed, reason := OpenAIProfitControlVeto(next, account)
				require.True(t, vetoed)
				require.Equal(t, "profit_group_config_unavailable", reason)
				latest, vetoed, reason := svc.ProfitControlVetoLatest(next, account)
				require.Same(t, account, latest)
				require.True(t, vetoed)
				require.Equal(t, "profit_group_config_unavailable", reason, "账号刷新不得掩盖配置失败")
			}
		})
	}
}

func TestProfitControlGroupReadOutcomes(t *testing.T) {
	const targetID int64 = 912
	enabled := profitControlTestGroup(targetID, 0.5, 0)
	disabled := *enabled
	disabled.ProfitControlEnabled = false
	wrong := *enabled
	wrong.ID++
	for _, tc := range []struct {
		name    string
		repo    GroupRepository
		blocked bool
	}{
		{"load error", profitControlFailingGroupRepo{}, true},
		{"missing group", profitControlGroupRepo{}, true},
		{"wrong group", profitControlGroupRepo{group: &wrong}, true},
		{"missing repository", nil, true},
		{"known disabled", profitControlGroupRepo{group: &disabled}, false},
		{"known enabled", profitControlGroupRepo{group: enabled}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, shared := range []bool{false, true} {
				name := "openai"
				if shared {
					name = "shared gateway"
				}
				t.Run(name, func(t *testing.T) {
					billing := profitControlTestGroup(911, 0, 0)
					billing.Platform = PlatformComposite
					ctx := profitControlTestCtx(billing)
					ctx = context.WithValue(ctx, openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 910, threshold: 0.01})
					snapshot := NewSchedulerSnapshotService(nil, nil, nil, tc.repo, nil)
					groupID := targetID
					if shared {
						ctx, _ = WithGatewayTokenRequestPricing(ctx)
						ctx = (&GatewayService{schedulerSnapshot: snapshot}).withGatewayProfitControlGate(ctx, &groupID)
					} else {
						ctx, _ = (&OpenAIGatewayService{schedulerSnapshot: snapshot}).WithOpenAIRequestPricingContext(ctx, &groupID)
					}
					rate := 0.2
					account := &Account{ID: 913, RateMultiplier: &rate}
					vetoed, reason := OpenAIProfitControlVeto(ctx, account)
					require.Equal(t, tc.blocked, vetoed)
					if tc.blocked {
						require.Equal(t, "profit_group_config_unavailable", reason)
					} else {
						require.Empty(t, reason, "不得继承外部分组的阈值或修改已确认的关闭开关")
					}
					selection := attachSelectionProfitGate(ctx, &AccountSelectionResult{})
					replayed := ContextWithSelectionProfitGate(context.Background(), selection)
					vetoed, replayReason := OpenAIProfitControlVeto(replayed, account)
					require.Equal(t, tc.blocked, vetoed)
					require.Equal(t, reason, replayReason)
					require.Same(t, billing, ctx.Value(ctxkey.Group))
				})
			}
		})
	}
}

func TestProfitControlUnavailableGroupRecovery(t *testing.T) {
	targetID := int64(922)
	billing := profitControlTestGroup(921, 0, 0)
	billing.Platform = PlatformComposite
	repo := &profitControlGroupRepo{}
	svc := &OpenAIGatewayService{schedulerSnapshot: NewSchedulerSnapshotService(nil, nil, nil, repo, nil)}
	failed, _ := svc.WithOpenAIRequestPricingContext(profitControlTestCtx(billing), &targetID)
	account := gatewayProfitTestAccount(923, PlatformOpenAI, 0.2, targetID)
	vetoed, reason := OpenAIProfitControlVeto(failed, &account)
	require.True(t, vetoed)
	require.Equal(t, "profit_group_config_unavailable", reason)
	repo.group = profitControlTestGroup(targetID, 0.5, 0)
	recovered, _ := svc.WithOpenAITurnPricingContext(failed, &billing.ID)
	vetoed, reason = OpenAIProfitControlVeto(recovered, &account)
	require.False(t, vetoed)
	require.Empty(t, reason)
	gate, _ := recovered.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.NotNil(t, gate)
	require.Equal(t, targetID, gate.groupID, "WS 恢复仍读取实际调度分组而非计费父组")
	require.Same(t, billing, recovered.Value(ctxkey.Group))
	repo.group.ProfitControlEnabled = false
	disabled, _ := svc.WithOpenAITurnPricingContext(recovered, &billing.ID)
	gate, _ = disabled.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.Nil(t, gate, "已确认关闭才能卸门")
	vetoed, reason = OpenAIProfitControlVeto(failed, &account)
	require.True(t, vetoed, "恢复不修改先前失败轮次")
	require.Equal(t, "profit_group_config_unavailable", reason)
}

func TestProfitControlGroupConfigFailureObservation(t *testing.T) {
	observer := &openAIProfitControlObserver{}
	observer.recordVeto(931, "unknown", 0, openAIProfitFilterReasonGroupConfigUnavailable)
	stats := observer.stats(931, "unknown")
	require.Equal(t, int64(1), stats.vetoGroupConfig.Load())
	require.Zero(t, stats.vetoThreshold.Load())
	require.Zero(t, stats.vetoInvalidRate.Load())
	require.Zero(t, stats.refreshFailures.Load())
}

func TestGatewayProfitControlLegacyMixedAndRoutedSelection(t *testing.T) {
	t.Run("legacy single-platform selection", func(t *testing.T) {
		group := gatewayProfitTestGroup(111, PlatformGrok)
		cheap := gatewayProfitTestAccount(1, PlatformGrok, 0.2, group.ID)
		expensive := gatewayProfitTestAccount(2, PlatformGrok, 0.8, group.ID)
		repo := &mockAccountRepoForPlatform{
			accounts:     []Account{expensive, cheap},
			accountsByID: map[int64]*Account{cheap.ID: &cheap, expensive.ID: &expensive},
		}
		svc := &GatewayService{
			accountRepo: repo,
			cache:       &mockGatewayCacheForPlatform{},
			cfg:         testConfig(),
		}

		selected, err := svc.SelectAccountForModelWithExclusions(
			gatewayProfitTestContext(group), &group.ID, "", "", nil,
		)
		require.NoError(t, err)
		require.Equal(t, cheap.ID, selected.ID)

		_, err = svc.SelectAccountForModelWithExclusions(
			gatewayProfitTestContext(group), &group.ID, "", "", map[int64]struct{}{cheap.ID: {}},
		)
		require.Error(t, err)
		require.ErrorIs(t, err, ErrNoAvailableAccounts)
	})

	t.Run("mixed routing filters the routed account", func(t *testing.T) {
		group := gatewayProfitTestGroup(112, PlatformAnthropic)
		group.ModelRoutingEnabled = true
		group.ModelRouting = map[string][]int64{"claude-test": {2, 1}}
		cheap := gatewayProfitTestAccount(1, PlatformAntigravity, 0.2, group.ID)
		cheap.Extra = map[string]any{"mixed_scheduling": true}
		cheap.Credentials = map[string]any{"model_mapping": map[string]any{"claude-test": "claude-test"}}
		expensive := gatewayProfitTestAccount(2, PlatformAnthropic, 0.8, group.ID)
		repo := &mockAccountRepoForPlatform{
			accounts:     []Account{expensive, cheap},
			accountsByID: map[int64]*Account{cheap.ID: &cheap, expensive.ID: &expensive},
		}
		svc := &GatewayService{
			accountRepo: repo,
			cache:       &mockGatewayCacheForPlatform{},
			cfg:         testConfig(),
		}

		selected, err := svc.SelectAccountForModelWithExclusions(
			gatewayProfitTestContext(group), &group.ID, "", "claude-test", nil,
		)
		require.NoError(t, err)
		require.Equal(t, cheap.ID, selected.ID)
	})
}

func TestGatewayProfitControlLoadAwareSelectionAndFailover(t *testing.T) {
	group := gatewayProfitTestGroup(121, PlatformGrok)
	cheap := gatewayProfitTestAccount(1, PlatformGrok, 0.2, group.ID)
	expensive := gatewayProfitTestAccount(2, PlatformGrok, 0.8, group.ID)
	repo := &mockAccountRepoForPlatform{
		accounts:     []Account{expensive, cheap},
		accountsByID: map[int64]*Account{cheap.ID: &cheap, expensive.ID: &expensive},
	}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &GatewayService{
		accountRepo:        repo,
		cache:              &mockGatewayCacheForPlatform{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(stubConcurrencyCache{}),
	}

	result, err := svc.SelectAccountWithLoadAwareness(
		gatewayProfitTestContext(group), &group.ID, "", "", nil, "", 0,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, cheap.ID, result.Account.ID)
	if result.ReleaseFunc != nil {
		result.ReleaseFunc()
	}

	result, err = svc.SelectAccountWithLoadAwareness(
		gatewayProfitTestContext(group),
		&group.ID,
		"",
		"",
		map[int64]struct{}{cheap.ID: {}},
		"",
		0,
	)
	require.Nil(t, result)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
}

func TestGatewayProfitControlStickyVetoKeepsBindingUntilRateRecovers(t *testing.T) {
	group := gatewayProfitTestGroup(131, PlatformAnthropic)
	expensive := gatewayProfitTestAccount(1, PlatformAnthropic, 0.8, group.ID)
	cheap := gatewayProfitTestAccount(2, PlatformAnthropic, 0.2, group.ID)
	repo := &mockAccountRepoForPlatform{
		accounts:     []Account{expensive, cheap},
		accountsByID: map[int64]*Account{expensive.ID: &expensive, cheap.ID: &cheap},
	}
	cache := &mockGatewayCacheForPlatform{
		sessionBindings: map[string]int64{"sticky-profit": expensive.ID},
	}
	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}
	ctx := gatewayProfitTestContext(group)

	selected, err := svc.SelectAccountForModelWithExclusions(ctx, &group.ID, "sticky-profit", "", nil)
	require.NoError(t, err)
	require.Equal(t, cheap.ID, selected.ID)
	require.Equal(t, expensive.ID, cache.sessionBindings["sticky-profit"], "候选过滤不得覆盖旧粘性绑定")

	require.NoError(t, svc.BindStickySessionAfterProfitAdmission(
		svc.withGatewayProfitControlGate(ctx, &group.ID),
		&group.ID,
		"sticky-profit",
		cheap.ID,
	))
	require.Equal(t, expensive.ID, cache.sessionBindings["sticky-profit"], "终检通过的 fallback 账号也不得覆盖旧绑定")
	require.Zero(t, cache.deletedSessions["sticky-profit"])

	recovered := expensive
	recoveredRate := 0.2
	recovered.RateMultiplier = &recoveredRate
	repo.accounts[0] = recovered
	repo.accountsByID[recovered.ID] = &repo.accounts[0]

	selected, err = svc.SelectAccountForModelWithExclusions(ctx, &group.ID, "sticky-profit", "", nil)
	require.NoError(t, err)
	require.Equal(t, recovered.ID, selected.ID, "倍率恢复后应重新命中原粘性账号")
	require.Zero(t, cache.deletedSessions["sticky-profit"])
}

type gatewayProfitSnapshotCache struct {
	SchedulerCache
	account *Account
	err     error
}

func (c *gatewayProfitSnapshotCache) GetAccount(context.Context, int64) (*Account, error) {
	return c.account, c.err
}

type gatewayProfitAccountRepo struct {
	AccountRepository
	account *Account
	err     error
}

func (r gatewayProfitAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, r.err
}

func TestGatewayProfitControlTerminalRefreshUsesReplacementObject(t *testing.T) {
	selected := gatewayProfitTestAccount(141, PlatformGemini, 0.2, 1)
	replacement := selected
	expensiveRate := 0.8
	replacement.RateMultiplier = &expensiveRate

	snapshot := NewSchedulerSnapshotService(
		&gatewayProfitSnapshotCache{account: &replacement},
		nil,
		gatewayProfitAccountRepo{},
		nil,
		nil,
	)
	ctx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{
		groupID:   1,
		platform:  PlatformGemini,
		threshold: 0.5,
	})

	latest, vetoed, reason := profitControlVetoLatest(ctx, &selected, snapshot)
	require.Same(t, &replacement, latest)
	require.True(t, vetoed)
	require.Equal(t, openAIProfitFilterReasonThreshold, reason)
	require.InDelta(t, 0.2, *selected.RateMultiplier, 1e-12, "测试必须替换缓存对象，不能原地修改旧指针")
}

func TestGatewayProfitControlTerminalRefreshFallsBackFromCacheToDatabase(t *testing.T) {
	selected := gatewayProfitTestAccount(145, PlatformAnthropic, 0.2, 1)
	replacement := selected
	expensiveRate := 0.8
	replacement.RateMultiplier = &expensiveRate

	snapshot := NewSchedulerSnapshotService(
		&gatewayProfitSnapshotCache{err: errors.New("cache unavailable")},
		nil,
		gatewayProfitAccountRepo{account: &replacement},
		nil,
		nil,
	)
	ctx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{
		groupID:   1,
		platform:  PlatformAnthropic,
		threshold: 0.5,
	})

	latest, vetoed, reason := profitControlVetoLatest(ctx, &selected, snapshot)
	require.Same(t, &replacement, latest)
	require.True(t, vetoed, "缓存读取失败时必须继续从数据库重读，不能直接使用选号旧对象")
	require.Equal(t, openAIProfitFilterReasonThreshold, reason)
}

func TestGatewayProfitControlTerminalRefreshFailureRejectsAdmission(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "cache and database unavailable", err: errors.New("database unavailable")},
		{name: "account no longer exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := gatewayProfitTestAccount(151, PlatformAntigravity, 0.2, 1)
			snapshot := NewSchedulerSnapshotService(
				&gatewayProfitSnapshotCache{err: errors.New("cache unavailable")},
				nil,
				gatewayProfitAccountRepo{err: tc.err},
				nil,
				nil,
			)
			ctx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{
				groupID:   1,
				platform:  PlatformAntigravity,
				threshold: 0.5,
			})

			latest, vetoed, reason := profitControlVetoLatest(ctx, &selected, snapshot)
			require.Same(t, &selected, latest)
			require.True(t, vetoed, "an old low rate must not admit an account whose current rate cannot be verified")
			require.Equal(t, "profit_account_refresh_failed", reason)
		})
	}
}

func TestGatewayProfitControlTerminalRefreshWithoutGatePreservesAdmission(t *testing.T) {
	selected := gatewayProfitTestAccount(152, PlatformAntigravity, 0.2, 1)
	snapshot := NewSchedulerSnapshotService(
		&gatewayProfitSnapshotCache{err: errors.New("cache unavailable")},
		nil,
		gatewayProfitAccountRepo{err: errors.New("database unavailable")},
		nil,
		nil,
	)

	latest, vetoed, reason := profitControlVetoLatest(context.Background(), &selected, snapshot)
	require.Same(t, &selected, latest)
	require.False(t, vetoed)
	require.Empty(t, reason)
}

// 选号结果携带门：门安装在调度栈局部 ctx 上，handler 必须经
// ContextWithSelectionProfitGate 重放后终检与准入后绑定才可见（评审修复回归）。
func TestGatewayProfitControlSelectionCarriesGateToHandlerContext(t *testing.T) {
	group := gatewayProfitTestGroup(1, PlatformAnthropic)
	svc := &GatewayService{}
	expensive := gatewayProfitTestAccount(161, PlatformAnthropic, 0.9, group.ID)

	gateCtx := svc.withGatewayProfitControlGate(gatewayProfitTestContext(group), &group.ID)
	selection, err := svc.newSelectionResult(gateCtx, &expensive, true, nil, nil)
	require.NoError(t, err)
	require.True(t, selection.ProfitGateActive(), "选号结果必须携带调度栈内生效的门")

	// 修复前的缺陷形态：handler 原始 ctx 不含门，终检退化为空操作。
	_, vetoed, _ := svc.GatewayProfitControlVetoLatest(context.Background(), &expensive)
	require.False(t, vetoed, "对照组：不重放门时终检确实看不到门")

	handlerCtx := ContextWithSelectionProfitGate(context.Background(), selection)
	latest, vetoed, reason := svc.GatewayProfitControlVetoLatest(handlerCtx, &expensive)
	require.True(t, vetoed, "重放门后终检必须真实生效")
	require.Equal(t, openAIProfitFilterReasonThreshold, reason)
	require.NotNil(t, latest)

	// 无门选号不携带门，重放为无操作。
	plain, err := svc.newSelectionResult(context.Background(), &expensive, true, nil, nil)
	require.NoError(t, err)
	require.False(t, plain.ProfitGateActive())
	require.Equal(t, context.Background(), ContextWithSelectionProfitGate(context.Background(), plain))
}

// 生图意图不关门（H1/H2 回归锚点）：/v1/responses 混合请求即使带生图声明，
// token 定价上下文照常装配，共享门照常安装并否决越线账号。
func TestGatewayProfitControlImageIntentDoesNotDisableGate(t *testing.T) {
	group := gatewayProfitTestGroup(2, PlatformAnthropic)
	svc := &GatewayService{}
	expensive := gatewayProfitTestAccount(162, PlatformAnthropic, 0.9, group.ID)

	ctx := gatewayProfitTestContext(group)
	ctx = WithOpenAIImageGenerationIntent(ctx)
	gateCtx := svc.withGatewayProfitControlGate(ctx, &group.ID)
	require.False(t, svc.isGatewayAccountProfitEligible(gateCtx, &expensive),
		"请求体里的生图声明（含被动 image_gen namespace）不得关闭利润门")
}

// 无门时准入后绑定回退官方 eager 语义；门下读失败保守不写（评审 M-Bind 回归）。
func TestGatewayProfitControlAfterAdmissionBindSemantics(t *testing.T) {
	groupID := int64(3)
	expensiveID := int64(171)
	cheapID := int64(172)

	t.Run("eager without gate", func(t *testing.T) {
		cache := &mockGatewayCacheForPlatform{sessionBindings: map[string]int64{"s": expensiveID}}
		svc := &GatewayService{cache: cache}
		require.NoError(t, svc.BindStickySessionAfterProfitAdmission(context.Background(), &groupID, "s", cheapID))
		require.Equal(t, cheapID, cache.sessionBindings["s"], "无门时保持既有 eager 绑定行为")
	})

	t.Run("gated read failure is conservative", func(t *testing.T) {
		// mock 的 miss 返回非 sentinel 错误，等价于 Redis 读失败：门下保守不写。
		cache := &mockGatewayCacheForPlatform{sessionBindings: map[string]int64{}}
		svc := &GatewayService{cache: cache}
		gate := &openAIProfitControlGate{groupID: groupID, platform: PlatformAnthropic, threshold: 0.5}
		gateCtx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, gate)
		require.NoError(t, svc.BindStickySessionAfterProfitAdmission(gateCtx, &groupID, "absent", cheapID))
		require.NotContains(t, cache.sessionBindings, "absent")
	})

	t.Run("gated sentinel miss binds", func(t *testing.T) {
		cache := &sentinelMissGatewayCache{mockGatewayCacheForPlatform: &mockGatewayCacheForPlatform{sessionBindings: map[string]int64{}}}
		svc := &GatewayService{cache: cache}
		gate := &openAIProfitControlGate{groupID: groupID, platform: PlatformAnthropic, threshold: 0.5}
		gateCtx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, gate)
		require.NoError(t, svc.BindStickySessionAfterProfitAdmission(gateCtx, &groupID, "fresh", cheapID))
		require.Equal(t, cheapID, cache.sessionBindings["fresh"], "门下无既有绑定（sentinel miss）应建立粘性")
	})
}

// sentinelMissGatewayCache 让 miss 返回与真实仓库一致的 ErrStickySessionNotFound。
type sentinelMissGatewayCache struct {
	*mockGatewayCacheForPlatform
}

func (c *sentinelMissGatewayCache) GetSessionAccountID(ctx context.Context, groupID int64, sessionHash string) (int64, error) {
	if id, ok := c.sessionBindings[sessionHash]; ok {
		return id, nil
	}
	return 0, ErrStickySessionNotFound
}
