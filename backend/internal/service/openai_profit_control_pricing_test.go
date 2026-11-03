package service

// 请求级定价与利润门回归：请求级 pricingAt 定价上下文、门复用（failover 阈值稳定）、
// Responses 文本能力利润门、U 使用账号倍率且与探测新鲜度解耦、
// 用量记录定价时刻取值。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

func TestProfitControlFixedSaleAcrossWSTurns(t *testing.T) {
	group := profitControlTestGroup(946, 0, 0)
	svc := &OpenAIGatewayService{}
	ctx, firstPricingAt := svc.WithOpenAIRequestPricingContext(profitControlTestCtx(group), &group.ID)
	account := profitControlTestAccountWithRate(upstreamCostTestOAuthAccount(947), 0.2)
	vetoed, reason := OpenAIProfitControlVeto(ctx, account)
	require.False(t, vetoed)
	require.Empty(t, reason)
	markup := 2.0
	account.GroupRateMultiplier = &markup
	vetoed, reason = OpenAIProfitControlVeto(ctx, account)
	require.True(t, vetoed, "a change during a slot wait must not alter the sale price")
	require.Equal(t, "profit_account_sale_rate_mismatch", reason)
	next, nextPricingAt := svc.WithOpenAITurnPricingContext(ctx, &group.ID)
	require.False(t, nextPricingAt.IsZero())
	require.Equal(t, firstPricingAt, OpenAIPricingAtFromContext(ctx), "the preceding turn keeps its pricing time")
	vetoed, reason = OpenAIProfitControlVeto(next, account)
	require.True(t, vetoed)
	require.Equal(t, "profit_account_sale_rate_mismatch", reason)
	markup = 1
	vetoed, reason = OpenAIProfitControlVeto(next, account)
	require.False(t, vetoed)
	require.Empty(t, reason)
	require.Equal(t, 1.0, account.UserGroupRateMultiplier())
}

// WithOpenAIRequestPricingContext：装门 + 固定 pricingAt；显式抑制标记
// （媒体/count_tokens/live 等门范围外路径）跳门且防御性装门无法把门加回来。
func TestProfitControl_RequestPricingContext(t *testing.T) {
	svc := &OpenAIGatewayService{}
	groupID := int64(61)
	now := time.Now()
	expensive := upstreamCostTestAccount(1, UpstreamBillingProbeStatusOK, 0.8, now.Add(-time.Minute), 30*time.Minute)
	profitControlTestAccountWithRate(expensive, 0.8)

	t.Run("installs gate and pricing instant", func(t *testing.T) {
		base := profitControlTestCtx(profitControlTestGroup(groupID, 0.5, 0))
		ctx, pricingAt := svc.WithOpenAIRequestPricingContext(base, &groupID)
		require.False(t, pricingAt.IsZero())
		require.Equal(t, pricingAt, OpenAIPricingAtFromContext(ctx))
		vetoed, reason := OpenAIProfitControlVeto(ctx, expensive)
		require.True(t, vetoed)
		require.Equal(t, openAIProfitFilterReasonThreshold, reason)
	})

	t.Run("suppress marker skips gate everywhere", func(t *testing.T) {
		base := WithOpenAIProfitControlSuppressed(profitControlTestCtx(profitControlTestGroup(groupID, 0.5, 0)))
		ctx, pricingAt := svc.WithOpenAIRequestPricingContext(base, &groupID)
		require.False(t, pricingAt.IsZero(), "跳门时 pricingAt 仍需固定供计费共用")
		vetoed, _ := OpenAIProfitControlVeto(ctx, expensive)
		require.False(t, vetoed)
		// service 层防御性装门也必须被抑制标记挡住。
		reCtx := svc.withOpenAIProfitControlGate(ctx, &groupID)
		vetoed, _ = OpenAIProfitControlVeto(reCtx, expensive)
		require.False(t, vetoed)
	})
}

// failover 重入复用同一门：请求中途分组配置变化不得改变本请求阈值。
func TestProfitControl_GateReuseKeepsThresholdAcrossFailover(t *testing.T) {
	svc := &OpenAIGatewayService{}
	groupID := int64(62)
	group := profitControlTestGroup(groupID, 0.5, 0)
	ctx := svc.withOpenAIProfitControlGate(profitControlTestCtx(group), &groupID)
	gate, ok := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.True(t, ok)
	require.InDelta(t, 0.5, gate.threshold, 1e-12)

	// 模拟请求进行中管理员改配置（ctx 分组为同一指针，与 auth 快照语义一致）。
	group.ProfitMinMargin = 0.9
	reCtx := svc.withOpenAIProfitControlGate(ctx, &groupID)
	reGate, ok := reCtx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.True(t, ok)
	require.Same(t, gate, reGate, "failover 重入必须复用同一门，阈值不得中途变化")

	// 换分组（composite/模型路由成员调度）重新解析；成员分组无门时必须清除
	// 父分组门，阈值不得跨组泄漏。
	otherID := int64(63)
	otherCtx := svc.withOpenAIProfitControlGate(reCtx, &otherID)
	otherGate, _ := otherCtx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.Nil(t, otherGate, "成员分组未启用利润控制时父分组门必须清除")
	now := time.Now()
	expensive := upstreamCostTestAccount(8, UpstreamBillingProbeStatusOK, 0.9, now.Add(-time.Minute), 30*time.Minute)
	vetoed, _ := openAIProfitControlVetoReason(otherCtx, expensive)
	require.False(t, vetoed)
}

// D 固定在 pricingAt：高峰因子按请求开始时刻计算，与"当前时刻"无关。
func TestProfitControl_PricingAtFixesDownstreamPeakFactor(t *testing.T) {
	svc := &OpenAIGatewayService{}
	groupID := int64(64)
	group := profitControlTestGroup(groupID, 0, 0)
	group.SubscriptionType = SubscriptionTypeSubscription
	group.PeakRateEnabled = true
	group.PeakRateMultiplier = 3.0

	pricingAt := time.Date(2026, time.January, 15, 8, 30, 0, 0, timezone.Location())
	outsideWindow := time.Date(2026, time.January, 15, 10, 30, 0, 0, timezone.Location())
	group.PeakStart = "08:00"
	group.PeakEnd = "09:00"
	require.Equal(t, 1.0, group.PeakMultiplierAt(outsideWindow), "构造前提：对照时刻不在窗口内")
	require.Equal(t, 3.0, group.PeakMultiplierAt(pricingAt), "构造前提：pricingAt 在窗口内")

	ctx := context.WithValue(profitControlTestCtx(group), openAIPricingAtCtxKey{}, pricingAt)
	gate := svc.resolveOpenAIProfitControlGate(ctx, &groupID)
	require.NotNil(t, gate)
	require.InDelta(t, 3.0, gate.threshold, 1e-9, "阈值必须用 pricingAt 时刻的高峰因子（1.0×3.0×(1-0)）")
	require.Equal(t, pricingAt, gate.pricingAt)
}

// U 只取账号倍率：探测快照内容和新鲜度不再直接参与利润判断。
func TestProfitControl_UsesAccountRateInsteadOfProbeSnapshot(t *testing.T) {
	gate := &openAIProfitControlGate{threshold: 0.5, pricingAt: time.Now().Add(-12 * time.Hour)}
	ctx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, gate)
	account := upstreamCostTestAccount(9, UpstreamBillingProbeStatusOK, 0.1, time.Now().Add(-3*time.Hour), 30*time.Minute)
	profitControlTestAccountWithRate(account, 0.8)
	vetoed, reason := openAIProfitControlVetoReason(ctx, account)
	require.True(t, vetoed)
	require.Equal(t, openAIProfitFilterReasonThreshold, reason)
}

// Responses 是端点能力，不代表媒体请求；原生远程压缩同样要求该能力，
// 因此唯一文本调度入口必须照常安装利润门。
func TestProfitControl_ResponsesCapabilityUsesTextGateAtScheduler(t *testing.T) {
	now := time.Now()
	expensive := upstreamCostTestAccount(51, UpstreamBillingProbeStatusOK, 0.8, now.Add(-time.Minute), 30*time.Minute)
	expensive.Status = StatusActive
	expensive.Schedulable = true
	expensive.Concurrency = 2
	svc := &OpenAIGatewayService{
		accountRepo:        stubOpenAIAccountRepo{accounts: []Account{*expensive}},
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(stubConcurrencyCache{}),
	}
	groupID := int64(77)
	ctx := profitControlTestCtx(profitControlTestGroup(groupID, 0.5, 0))

	_, _, err := svc.SelectAccountWithSchedulerForCapability(ctx, &groupID, "", "", "gpt-test", nil, OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true)
	require.ErrorIs(t, err, ErrNoAvailableAccounts, "文本能力必须过利润门")

	selection, _, err := svc.SelectAccountWithSchedulerForCapability(ctx, &groupID, "", "", "gpt-test", nil, OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityResponses, false, false, true)
	require.ErrorIs(t, err, ErrNoAvailableAccounts, "Responses 文本能力不得绕过利润门")
	require.Nil(t, selection)
}

// 账号倍率缺失一律视为非法保守拒绝；手工或同步维护了倍率的任意账号类型都按
// 同一阈值判断（OAuth 与 API Key 无差别）。
func TestProfitControl_AccountRateSemantics(t *testing.T) {
	now := time.Now()
	missing := upstreamCostTestOAuthAccount(2)
	manualOAuth := profitControlTestAccountWithRate(upstreamCostTestOAuthAccount(3), 0.3)
	expensive := profitControlTestAccountWithRate(upstreamCostTestAccount(4, UpstreamBillingProbeStatusOK, 0.1, now.Add(-3*time.Hour), 30*time.Minute), 0.8)

	group := profitControlTestGroup(77, 0.5, 0)
	group.RateMultiplier = 1
	base := context.WithValue(profitControlTestCtx(group), openAIPricingAtCtxKey{}, now)
	gate := (&OpenAIGatewayService{}).resolveOpenAIProfitControlGate(base, &group.ID)
	require.NotNil(t, gate)
	gateCtx := context.WithValue(base, openAIProfitControlGateCtxKey{}, gate)

	vetoed, reason := openAIProfitControlVetoReason(gateCtx, missing)
	require.True(t, vetoed, "缺失账号倍率必须保守拒绝")
	require.Equal(t, openAIProfitFilterReasonInvalidAccountRate, reason)

	vetoed, _ = openAIProfitControlVetoReason(gateCtx, manualOAuth)
	require.False(t, vetoed, "手工维护的 OAuth 倍率应正常准入")

	vetoed, reason = openAIProfitControlVetoReason(gateCtx, expensive)
	require.True(t, vetoed)
	require.Equal(t, openAIProfitFilterReasonThreshold, reason)
}

// 用量记录定价时刻：优先请求级 PricingAt，未装配回退记录时刻。
func TestOpenAIUsagePricingAt(t *testing.T) {
	fixed := time.Now().Add(-2 * time.Hour)
	require.Equal(t, fixed, openAIUsagePricingAt(&OpenAIRecordUsageInput{PricingAt: fixed}))
	fallback := openAIUsagePricingAt(&OpenAIRecordUsageInput{})
	require.WithinDuration(t, timezone.Now(), fallback, 5*time.Second)
	require.WithinDuration(t, timezone.Now(), openAIUsagePricingAt(nil), 5*time.Second)
}

func TestOpenAIProfitControlStickyBindingOccursOnlyAfterTerminalAdmission(t *testing.T) {
	groupID := int64(81)
	expensiveID := int64(901)
	cheapID := int64(902)
	const sessionHash = "profit-sticky"
	const cacheKey = "openai:" + sessionHash
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{cacheKey: expensiveID},
	}
	svc := &OpenAIGatewayService{cache: cache}
	ctx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{
		groupID:   groupID,
		platform:  PlatformOpenAI,
		threshold: 0.5,
	})

	require.NoError(t, svc.bindOpenAIStickySessionDuringSelection(ctx, &groupID, sessionHash, cheapID))
	require.Equal(t, expensiveID, cache.sessionBindings[cacheKey], "选号阶段不得覆盖原粘性绑定")

	require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctx, &groupID, sessionHash, cheapID))
	require.Equal(t, expensiveID, cache.sessionBindings[cacheKey], "终检通过的 fallback 账号不得覆盖原粘性绑定")

	cache.sessionBindings[cacheKey] = 0
	require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctx, &groupID, sessionHash, cheapID))
	require.Equal(t, cheapID, cache.sessionBindings[cacheKey], "无既有绑定时应在终检通过后建立粘性")
}

type profitTurnLocalReader struct {
	LocalSchedulerSnapshotReader
	LocalSchedulerDetailReader
	group *Group
	err   error
	reads int
}

func (r *profitTurnLocalReader) GetLocalGroup(context.Context, int64) (*Group, error) {
	r.reads++
	return r.group, r.err
}

func TestProfitControlRequestUsesAuthoritativePolicy(t *testing.T) {
	testProfitControlRequestPolicy(t, false)
}

func TestGatewayProfitControlRequestUsesAuthoritativePolicy(t *testing.T) {
	testProfitControlRequestPolicy(t, true)
}

func testProfitControlRequestPolicy(t *testing.T, gateway bool) {
	t.Helper()
	platforms := []string{PlatformOpenAI, PlatformGrok}
	if gateway {
		platforms = []string{PlatformOpenAI, PlatformGrok, PlatformAnthropic, PlatformGemini, PlatformAntigravity}
	}
	for _, platform := range platforms {
		for _, local := range []bool{false, true} {
			for _, phase := range []string{"tightened", "disabled", "enabled", "missing", "error", "wrong group"} {
				t.Run(platform+map[bool]string{false: "/primary/", true: "/regional/"}[local]+phase, func(t *testing.T) {
					billing := profitControlTestGroup(967, 0.1, 0)
					billing.Platform = platform
					billing.ProfitControlEnabled = phase != "enabled" && phase != "missing" && phase != "error" && phase != "wrong group"
					fresh := *billing
					fresh.ProfitControlEnabled = phase != "disabled"
					fresh.ProfitMinMargin = 0.6
					fresh.RateMultiplier = 99
					repo := &profitControlGroupRepo{group: &fresh}
					reader := &profitTurnLocalReader{group: &fresh}
					snapshot := NewSchedulerSnapshotService(nil, nil, nil, repo, nil)
					switch phase {
					case "missing":
						repo.group, reader.group = nil, nil
					case "error":
						snapshot.groupRepo = profitControlFailingGroupRepo{}
						reader.err = errors.New("fixture regional unavailable")
					case "wrong group":
						fresh.ID++
					}
					if local {
						snapshot.localSnapshotReader = reader
					}
					svc := &OpenAIGatewayService{schedulerSnapshot: snapshot}
					install := svc.WithOpenAIRequestPricingContext
					reenter := svc.withOpenAIProfitControlGate
					if gateway {
						gatewaySvc := &GatewayService{schedulerSnapshot: snapshot}
						install = func(ctx context.Context, groupID *int64) (context.Context, time.Time) {
							ctx, at := WithGatewayTokenRequestPricing(ctx)
							return gatewaySvc.withGatewayProfitControlGate(ctx, groupID), at
						}
						reenter = gatewaySvc.withGatewayProfitControlGate
					}
					ctx, at := install(profitControlTestCtx(billing), &billing.ID)
					gate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
					if phase == "disabled" {
						require.Nil(t, gate)
					} else {
						require.NotNil(t, gate)
						unavailable := phase == "missing" || phase == "error" || phase == "wrong group"
						require.Equal(t, unavailable, gate.configUnavailable)
						if !unavailable {
							require.InDelta(t, 0.4, gate.threshold, 1e-12)
						}
					}
					for _, rate := range []float64{0, 0.5} {
						account := profitControlTestAccountWithRate(upstreamCostTestOAuthAccount(968), rate)
						blocked, reason := OpenAIProfitControlVeto(ctx, account)
						if phase == "missing" || phase == "error" || phase == "wrong group" {
							require.True(t, blocked)
							require.Equal(t, openAIProfitFilterReasonGroupConfigUnavailable, reason)
						} else {
							require.Equal(t, phase != "disabled" && rate > 0.4, blocked)
						}
					}
					// In-flight retries retain both enabled and disabled decisions,
					// even if the source changes or disappears after request admission.
					fresh.ProfitControlEnabled = !fresh.ProfitControlEnabled
					reader.err = errors.New("fixture changed after admission")
					snapshot.groupRepo = profitControlFailingGroupRepo{}
					retry := reenter(ctx, &billing.ID)
					require.Same(t, ctx, retry)
					if gateway {
						require.Equal(t, at, GatewayTokenRequestPricingAtFromContext(retry))
					} else {
						require.Equal(t, at, OpenAIPricingAtFromContext(retry))
					}
					require.Same(t, billing, retry.Value(ctxkey.Group))
					if local {
						require.Equal(t, 1, reader.reads)
					}
				})
			}
		}
	}
}

func TestProfitControlTurnRefreshIgnoresStaleAuthPolicy(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformGrok} {
		for _, local := range []bool{false, true} {
			t.Run(platform+map[bool]string{false: "/primary", true: "/regional"}[local], func(t *testing.T) {
				billing := profitControlTestGroup(963, 0.1, 0)
				billing.Platform = platform
				repo := &profitControlGroupRepo{group: billing}
				reader := &profitTurnLocalReader{group: billing}
				snapshot := NewSchedulerSnapshotService(nil, nil, nil, repo, nil)
				if local {
					snapshot.localSnapshotReader = reader
				}
				svc := &OpenAIGatewayService{schedulerSnapshot: snapshot}
				ctx, initialAt := svc.WithOpenAIRequestPricingContext(profitControlTestCtx(billing), &billing.ID)
				initialGate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
				require.NotNil(t, initialGate)
				account := profitControlTestAccountWithRate(upstreamCostTestOAuthAccount(964), 0.5)
				for _, phase := range []struct {
					name             string
					margin           float64
					enabled, blocked bool
				}{
					{"tightened", 0.6, true, true},
					{"relaxed", 0.1, true, false},
					{"disabled", 0.6, false, false},
					{"re-enabled", 0.6, true, true},
				} {
					fresh := *billing
					fresh.ProfitMinMargin, fresh.ProfitControlEnabled = phase.margin, phase.enabled
					fresh.RateMultiplier = 99 // Policy refresh must not silently reprice the billing group.
					repo.group, reader.group = &fresh, &fresh
					next, at := svc.WithOpenAITurnPricingContext(ctx, &billing.ID)
					vetoed, reason := OpenAIProfitControlVeto(next, account)
					require.Equal(t, phase.blocked, vetoed, phase.name)
					if phase.blocked {
						require.Equal(t, openAIProfitFilterReasonThreshold, reason)
					}
					require.Same(t, billing, next.Value(ctxkey.Group))
					require.Equal(t, 1.0, billing.RateMultiplier)
					require.Equal(t, at, OpenAIPricingAtFromContext(next))
					gate, _ := next.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
					if phase.enabled {
						require.NotNil(t, gate)
						require.InDelta(t, 1-phase.margin, gate.threshold, 1e-12)
					} else {
						require.Nil(t, gate)
					}
					ctx = next
				}
				require.InDelta(t, 0.9, initialGate.threshold, 1e-12)
				require.Equal(t, initialAt, initialGate.pricingAt)
				if local {
					require.Equal(t, 5, reader.reads)
				}
			})
		}
	}
}

func TestProfitControlTurnRefreshFailureAndRecoveryWithAuthGroup(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "primary", true: "regional"}[local], func(t *testing.T) {
			billing := profitControlTestGroup(965, 0.1, 0)
			billing.ProfitControlEnabled = false // Old auth snapshot says off.
			repo := &profitControlGroupRepo{group: billing}
			reader := &profitTurnLocalReader{}
			snapshot := NewSchedulerSnapshotService(nil, nil, nil, repo, nil)
			if local {
				snapshot.localSnapshotReader = reader
			}
			svc := &OpenAIGatewayService{schedulerSnapshot: snapshot}
			ctx := profitControlTestCtx(billing)
			for _, phase := range []string{"missing", "error", "wrong group", "recovered"} {
				fresh := profitControlTestGroup(billing.ID, 0.6, 0)
				repo.group, reader.group, reader.err = fresh, fresh, nil
				snapshot.groupRepo = repo
				switch phase {
				case "missing":
					repo.group, reader.group = nil, nil
				case "error":
					snapshot.groupRepo = profitControlFailingGroupRepo{}
					reader.err = errors.New("fixture regional unavailable")
				case "wrong group":
					fresh.ID++
				}
				next, _ := svc.WithOpenAITurnPricingContext(ctx, &billing.ID)
				gate, _ := next.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
				require.NotNil(t, gate, phase)
				require.Equal(t, billing.ID, gate.groupID)
				require.Equal(t, phase != "recovered", gate.configUnavailable, phase)
				for _, rate := range []float64{0, 0.5} {
					account := profitControlTestAccountWithRate(upstreamCostTestOAuthAccount(966), rate)
					blocked, reason := OpenAIProfitControlVeto(next, account)
					if phase == "recovered" {
						require.Equal(t, rate > 0.4, blocked)
					} else {
						require.True(t, blocked)
						require.Equal(t, openAIProfitFilterReasonGroupConfigUnavailable, reason)
					}
				}
				require.Same(t, billing, next.Value(ctxkey.Group))
				ctx = next
			}
			if local {
				require.Equal(t, 4, reader.reads)
			}
		})
	}
}

func TestProfitControlDisabledMemberKeepsTurnScope(t *testing.T) {
	member := profitControlTestGroup(969, 0.1, 0)
	member.ProfitControlEnabled = false
	billing := profitControlTestGroup(970, 0.1, 0)
	billing.Platform = PlatformComposite
	reader := &profitTurnLocalReader{group: member}
	snapshot := NewSchedulerSnapshotService(nil, nil, nil, nil, nil)
	snapshot.localSnapshotReader = reader
	svc := &OpenAIGatewayService{schedulerSnapshot: snapshot}
	ctx, _ := svc.WithOpenAIRequestPricingContext(profitControlTestCtx(billing), &member.ID)
	require.Nil(t, ctx.Value(openAIProfitControlGateCtxKey{}))
	active := *member
	active.ProfitControlEnabled = true
	active.ProfitMinMargin = 0.6
	reader.group = &active
	retry := svc.withOpenAIProfitControlGate(ctx, &member.ID)
	require.Same(t, ctx, retry)
	require.Equal(t, 1, reader.reads)
	next, _ := svc.WithOpenAITurnPricingContext(ctx, &billing.ID)
	gate, _ := next.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.NotNil(t, gate)
	require.False(t, gate.configUnavailable)
	require.Equal(t, member.ID, gate.groupID)
	require.InDelta(t, 0.4, gate.threshold, 1e-12)
	require.Same(t, billing, next.Value(ctxkey.Group))
	require.Equal(t, 2, reader.reads)
	// A disabled member must clear an active foreign gate when revisited.
	foreign := context.WithValue(ctx, openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: billing.ID})
	cleared := svc.withOpenAIProfitControlGate(foreign, &member.ID)
	clearedGate, _ := cleared.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.Nil(t, clearedGate)
	require.Equal(t, 2, reader.reads)
}

// WithOpenAITurnPricingContext：长连接 turn 边界重新冻结 pricingAt 并按当前
// 配置重装门（区别于请求级同门复用）；已装门时以门所属调度分组为准。
func TestProfitControl_TurnPricingContext(t *testing.T) {
	svc := &OpenAIGatewayService{}
	groupID := int64(63)
	expensive := upstreamCostTestAccount(3, UpstreamBillingProbeStatusOK, 0.8, time.Now().Add(-time.Minute), 30*time.Minute)
	profitControlTestAccountWithRate(expensive, 0.8)

	t.Run("refreshes instant and re-resolves gate config", func(t *testing.T) {
		group := profitControlTestGroup(groupID, 0.5, 0)
		base := profitControlTestCtx(group)
		connCtx, connAt := svc.WithOpenAIRequestPricingContext(base, &groupID)
		vetoed, _ := OpenAIProfitControlVeto(connCtx, expensive)
		require.True(t, vetoed)

		// 连接中途运营者放宽 margin：turn 级重装必须生效（请求级复用不生效）。
		group.ProfitMinMargin = 0.1
		turnCtx, turnAt := svc.WithOpenAITurnPricingContext(connCtx, &groupID)
		require.False(t, turnAt.Before(connAt))
		require.Equal(t, turnAt, OpenAIPricingAtFromContext(turnCtx))
		vetoed, _ = OpenAIProfitControlVeto(turnCtx, expensive)
		require.False(t, vetoed, "turn 级重装应采用最新分组配置")
	})

	t.Run("keeps scheduled group of the existing gate", func(t *testing.T) {
		scheduledGroupID := int64(64)
		scheduled := profitControlTestGroup(scheduledGroupID, 0.5, 0)
		connCtx, _ := svc.WithOpenAIRequestPricingContext(profitControlTestCtx(scheduled), &scheduledGroupID)
		// 入口分组与调度分组不同（composite 成员分组场景）：turn 重装取门的分组。
		entryGroupID := int64(65)
		turnCtx, _ := svc.WithOpenAITurnPricingContext(connCtx, &entryGroupID)
		gate, ok := turnCtx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
		require.True(t, ok)
		require.NotNil(t, gate)
		require.Equal(t, scheduledGroupID, gate.groupID)
	})

	t.Run("suppress marker only refreshes instant", func(t *testing.T) {
		base := WithOpenAIProfitControlSuppressed(profitControlTestCtx(profitControlTestGroup(groupID, 0.5, 0)))
		turnCtx, turnAt := svc.WithOpenAITurnPricingContext(base, &groupID)
		require.False(t, turnAt.IsZero())
		vetoed, _ := OpenAIProfitControlVeto(turnCtx, expensive)
		require.False(t, vetoed)
	})

	t.Run("clears gate when group disables profit control mid-connection", func(t *testing.T) {
		group := profitControlTestGroup(groupID, 0.5, 0)
		connCtx, _ := svc.WithOpenAIRequestPricingContext(profitControlTestCtx(group), &groupID)
		group.ProfitControlEnabled = false
		turnCtx, _ := svc.WithOpenAITurnPricingContext(connCtx, &groupID)
		vetoed, _ := OpenAIProfitControlVeto(turnCtx, expensive)
		require.False(t, vetoed, "关门后 turn 级复核应放行")
	})
}

// 无门时准入后绑定回退官方 eager 语义：等待/抢槽路径不得因利润控制关闭而
// 失去粘性绑定（评审 M-Bind 回归锚点）。
func TestOpenAIProfitControlAfterAdmissionBindEagerWithoutGate(t *testing.T) {
	groupID := int64(82)
	expensiveID := int64(903)
	cheapID := int64(904)
	const sessionHash = "no-gate-sticky"
	const cacheKey = "openai:" + sessionHash
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{cacheKey: expensiveID},
	}
	svc := &OpenAIGatewayService{cache: cache}

	require.NoError(t, svc.BindStickySessionAfterProfitAdmission(context.Background(), &groupID, sessionHash, cheapID))
	require.Equal(t, cheapID, cache.sessionBindings[cacheKey], "无门时保持既有 eager 绑定行为")
}

type profitControlFailingGroupRepo struct {
	GroupRepository
}

func (profitControlFailingGroupRepo) GetByIDLite(context.Context, int64) (*Group, error) {
	return nil, errors.New("group cache unavailable")
}

// 见 profitControlGroupRepo.GetByID：利润门必须走不带账号计数聚合的 lite 读取。
func (profitControlFailingGroupRepo) GetByID(context.Context, int64) (*Group, error) {
	panic("profit control gate must read groups via GetByIDLite (no account-count aggregation)")
}
