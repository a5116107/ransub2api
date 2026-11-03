package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// withGatewayProfitControlGate installs the gate only for explicitly marked
// token requests. This keeps media, metadata, and models-list paths outside
// the profit-control surface by construction.
func (s *GatewayService) withGatewayProfitControlGate(ctx context.Context, groupID *int64) context.Context {
	pricingAt, tokenRequest := gatewayTokenRequestPricingAtFromContext(ctx)
	if !tokenRequest || groupID == nil || *groupID <= 0 {
		return ctx
	}
	if existing, ok := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate); ok && existing != nil && existing.groupID == *groupID {
		return ctx
	}
	if disabledGroup, ok := ctx.Value(openAIProfitControlDisabledGroupCtxKey{}).(int64); ok && disabledGroup == *groupID {
		return withProfitControlDisabledGroup(ctx, groupID)
	}

	group, err := s.resolveProfitControlGroup(ctx, *groupID)
	if err != nil || group == nil || group.ID != *groupID {
		slog.Warn("profit_control_group_load_failed", "group_id", *groupID, "error", err)
		gate := profitControlUnavailableGroupGate(*groupID, pricingAt)
		openAIProfitControlObserverInstance.recordInstall(gate.groupID, gate.platform, gate.threshold)
		return context.WithValue(ctx, openAIProfitControlGateCtxKey{}, gate)
	}
	if !group.ProfitControlEnabled || !profitControlPlatformSupported(group.Platform) {
		return withProfitControlDisabledGroup(ctx, groupID)
	}

	billingGroup := gatewayTokenRequestBillingGroupFromContext(ctx)
	if billingGroup == nil {
		if ctxGroup, ok := ctx.Value(ctxkey.Group).(*Group); ok && IsGroupContextValid(ctxGroup) {
			billingGroup = ctxGroup
		} else {
			billingGroup = group
		}
	}

	downstream := billingGroup.RateMultiplier
	if userID, _ := ctx.Value(ctxkey.UserID).(int64); userID > 0 {
		downstream = s.ResolveUserGroupRateMultiplier(ctx, userID, billingGroup.ID, billingGroup.RateMultiplier)
	}
	downstream *= billingGroup.PeakMultiplierAt(pricingAt)
	threshold := clampProfitControlThreshold(downstream * (1 - group.ProfitMinMargin - group.ProfitSafetyBuffer))

	gate := &openAIProfitControlGate{
		groupID:   group.ID,
		platform:  group.Platform,
		threshold: threshold,
		pricingAt: pricingAt,
	}
	openAIProfitControlObserverInstance.recordInstall(gate.groupID, gate.platform, gate.threshold)
	return context.WithValue(ctx, openAIProfitControlGateCtxKey{}, gate)
}

func (s *GatewayService) resolveProfitControlGroup(ctx context.Context, groupID int64) (*Group, error) {
	if s.schedulerSnapshot != nil {
		// Lite 读取：门只用平台/倍率/利润/高峰字段，不需要账号计数聚合。
		return s.schedulerSnapshot.GetGroupByIDLite(ctx, groupID)
	}
	if group, ok := ctx.Value(ctxkey.Group).(*Group); ok && IsGroupContextValid(group) && group.ID == groupID {
		return group, nil
	}
	if s.groupRepo == nil {
		return nil, errors.New("profit control group repository unavailable")
	}
	return s.resolveGroupByID(ctx, groupID)
}

// GatewayProfitControlVetoLatest performs the terminal post-slot check against
// the latest scheduler snapshot. When a gate is active, refresh failures veto
// admission because the selected account's current rate cannot be verified.
func (s *GatewayService) GatewayProfitControlVetoLatest(ctx context.Context, selected *Account) (*Account, bool, string) {
	return profitControlVetoLatest(ctx, selected, s.schedulerSnapshot)
}

func profitControlVetoLatest(ctx context.Context, selected *Account, snapshot *SchedulerSnapshotService) (*Account, bool, string) {
	gate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	if gate == nil || selected == nil {
		return selected, false, ""
	}
	if gate.configUnavailable {
		vetoed, reason := openAIProfitControlVetoReason(ctx, selected)
		return selected, vetoed, reason
	}
	latest := selected
	if snapshot != nil {
		refreshed, err := snapshot.GetAccount(ctx, selected.ID)
		if err != nil || refreshed == nil {
			slog.Warn("profit_control_account_refresh_failed", "group_id", gate.groupID, "platform", gate.platform, "account_id", selected.ID, "error", err)
			openAIProfitControlObserverInstance.recordRefreshFailure(gate.groupID, gate.platform, gate.threshold)
			return selected, true, openAIProfitFilterReasonAccountRefreshFailed
		} else if !refreshed.UpdatedAt.Before(selected.UpdatedAt) {
			// 选号路径可能已做过 DB recheck，selected 比缓存快照更新鲜；只有
			// 快照不落后时才替换，避免终检把新鲜账号换回较旧的缓存对象。
			latest = refreshed
		}
	}
	vetoed, reason := openAIProfitControlVetoReason(ctx, latest)
	return latest, vetoed, reason
}

func (s *GatewayService) isGatewayAccountProfitEligible(ctx context.Context, account *Account) bool {
	vetoed, _ := openAIProfitControlVetoReason(ctx, account)
	return !vetoed
}

func gatewayProfitControlGateActive(ctx context.Context) bool {
	gate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	return gate != nil
}
