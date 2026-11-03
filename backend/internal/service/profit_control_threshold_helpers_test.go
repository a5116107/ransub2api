package service

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestClampProfitControlThreshold 钉死阈值清洗口径：非有限或为负一律按 0
// （等价于只放行免费上游），合法值原样保留。
func TestClampProfitControlThreshold(t *testing.T) {
	require.Equal(t, 0.0, clampProfitControlThreshold(math.NaN()))
	require.Equal(t, 0.0, clampProfitControlThreshold(math.Inf(1)))
	require.Equal(t, 0.0, clampProfitControlThreshold(math.Inf(-1)))
	require.Equal(t, 0.0, clampProfitControlThreshold(-0.5))
	require.Equal(t, 0.0, clampProfitControlThreshold(0))
	require.Equal(t, 0.7, clampProfitControlThreshold(0.7))
}

// TestProfitControlOverThreshold 钉死「越线」的唯一定义：相对 epsilon 吸收
// decimal(10,4) 落库与浮点乘法的边界误差，U == 阈值 判定为合格。
//
// 线上否决点（openAIProfitControlVetoReason）与 profit-preview 现在共用这个
// 函数与 clampProfitControlThreshold；此前各复制了一份，会随线上口径漂移，
// 导致预演结果与真实调度不一致。
func TestProfitControlOverThreshold(t *testing.T) {
	require.False(t, profitControlOverThreshold(0.7, 0.7), "U == 阈值 必须判定为合格")
	require.False(t, profitControlOverThreshold(0.6999, 0.7))
	require.True(t, profitControlOverThreshold(0.7001, 0.7))

	// 0.3*3 的浮点尾数不得被判成越线。
	require.False(t, profitControlOverThreshold(0.1*3, 0.3))

	// 阈值为 0 时只放行 0 成本上游。
	require.False(t, profitControlOverThreshold(0, 0))
	require.True(t, profitControlOverThreshold(0.0001, 0))
}

func profitControlSyncedTestAccount(at time.Time) *Account {
	account := profitControlTestAccountWithRate(upstreamCostTestAccount(1, UpstreamBillingProbeStatusOK, 0.4, at.Add(-time.Minute), 30*time.Minute), 0.4)
	account.Extra[UpstreamBillingProbeEnabledExtraKey] = true
	account.Extra[UpstreamBillingRateSyncEnabledExtraKey] = true
	return account
}

// A synced base rate alone is insufficient evidence for admitting a route.
// Exercise the public online gate and preview together so their rules stay aligned.
func TestProfitControlSyncedAdmission(t *testing.T) {
	now := time.Now().UTC()
	const rejectedProbe = "rejected_upstream_rate_unavailable"
	tests := []struct {
		name   string
		mutate func(*Account, map[string]any, map[string]any)
		class  string
		reason string
		rate   float64
	}{
		{name: "fresh legacy snapshot without synced field", class: ProfitPreviewClassAdmitted},
		{name: "missing snapshot", mutate: func(a *Account, _, _ map[string]any) { delete(a.Extra, UpstreamBillingProbeExtraKey) }, class: rejectedProbe},
		{name: "failed snapshot with retained data", mutate: func(_ *Account, s, _ map[string]any) { s["status"] = UpstreamBillingProbeStatusFailed }, class: rejectedProbe},
		{name: "unsupported snapshot", mutate: func(_ *Account, s, _ map[string]any) { s["status"] = UpstreamBillingProbeStatusUnsupported }, class: rejectedProbe},
		{name: "malformed snapshot", mutate: func(_ *Account, s, _ map[string]any) { s["received_at"] = "invalid" }, class: rejectedProbe},
		{name: "expired snapshot", mutate: func(_ *Account, s, _ map[string]any) { s["fresh_until"] = now.Add(-time.Second) }, class: rejectedProbe},
		{name: "expiry exactly at evaluation time", mutate: func(_ *Account, s, _ map[string]any) { s["fresh_until"] = now }, class: rejectedProbe},
		{name: "missing expiry", mutate: func(_ *Account, s, _ map[string]any) { delete(s, "fresh_until") }, class: rejectedProbe},
		{name: "missing received time", mutate: func(_ *Account, s, _ map[string]any) { delete(s, "received_at") }, class: rejectedProbe},
		{name: "zero received time", mutate: func(_ *Account, s, _ map[string]any) { s["received_at"] = time.Time{} }, class: rejectedProbe},
		{name: "future received time", mutate: func(_ *Account, s, _ map[string]any) { s["received_at"] = now.Add(time.Hour) }, class: rejectedProbe},
		{name: "missing attempt time", mutate: func(_ *Account, s, _ map[string]any) { delete(s, "last_attempt_at") }, class: rejectedProbe},
		{name: "future attempt time", mutate: func(_ *Account, s, _ map[string]any) { s["last_attempt_at"] = now.Add(time.Hour) }, class: rejectedProbe},
		{name: "non token billing", mutate: func(_ *Account, _, d map[string]any) { d["billing_scope"] = "request" }, class: rejectedProbe},
		{name: "new API group snapshot is not token evidence", mutate: func(_ *Account, _, d map[string]any) { d["provider"] = "new_api"; d["billing_scope"] = "group" }, class: rejectedProbe},
		{name: "multiple groups cannot use minimum", mutate: func(_ *Account, _, d map[string]any) { d["billing_mode"] = "multi_group" }, class: rejectedProbe},
		{name: "zero declaration rejected by sync", mutate: func(_ *Account, _, d map[string]any) { d["resolved_rate_multiplier"] = 0.0 }, class: rejectedProbe},
		{name: "rounds to zero", mutate: func(_ *Account, _, d map[string]any) { d["resolved_rate_multiplier"] = 0.00001 }, class: rejectedProbe},
		{name: "over sync limit", mutate: func(_ *Account, _, d map[string]any) { d["resolved_rate_multiplier"] = 101.0 }, class: rejectedProbe},
		{name: "nonfinite declaration", mutate: func(_ *Account, _, d map[string]any) { d["resolved_rate_multiplier"] = math.NaN() }, class: rejectedProbe},
		{name: "missing peak configuration", mutate: func(_ *Account, _, d map[string]any) { delete(d, "peak_rate_enabled") }, class: rejectedProbe},
		{name: "invalid peak timezone", mutate: func(_ *Account, _, d map[string]any) {
			d["peak_rate_enabled"] = true
			d["peak_start"] = "00:00"
			d["peak_end"] = "12:00"
			d["timezone"] = "invalid-zone"
			d["peak_rate_multiplier"] = 2.0
		}, class: rejectedProbe},
		{name: "invalid peak window", mutate: func(_ *Account, _, d map[string]any) {
			d["peak_rate_enabled"] = true
			d["peak_start"] = "12:00"
			d["peak_end"] = "00:00"
			d["timezone"] = "UTC"
			d["peak_rate_multiplier"] = 2.0
		}, class: rejectedProbe},
		{name: "negative peak coefficient", mutate: func(_ *Account, _, d map[string]any) {
			d["peak_rate_enabled"] = true
			d["peak_start"] = "00:00"
			d["peak_end"] = "12:00"
			d["timezone"] = "UTC"
			d["peak_rate_multiplier"] = -1.0
		}, class: rejectedProbe},
		{name: "higher declaration before write back", mutate: func(_ *Account, _, d map[string]any) { d["resolved_rate_multiplier"] = 0.9 }, class: ProfitPreviewClassRejectedThreshold, reason: openAIProfitFilterReasonThreshold, rate: 0.9},
		{name: "saved conservative rate is retained", mutate: func(a *Account, _, _ map[string]any) { r := 0.9; a.RateMultiplier = &r }, class: ProfitPreviewClassRejectedThreshold, reason: openAIProfitFilterReasonThreshold, rate: 0.9},
		{name: "invalid saved rate", mutate: func(a *Account, _, _ map[string]any) { a.RateMultiplier = nil }, class: ProfitPreviewClassRejectedInvalidRate, reason: openAIProfitFilterReasonInvalidAccountRate},
		{name: "probe only is manual", mutate: func(a *Account, s, _ map[string]any) {
			a.Extra[UpstreamBillingRateSyncEnabledExtraKey] = false
			s["status"] = UpstreamBillingProbeStatusFailed
		}, class: ProfitPreviewClassAdmitted},
		{name: "sync alone is inactive", mutate: func(a *Account, s, _ map[string]any) {
			a.Extra[UpstreamBillingProbeEnabledExtraKey] = false
			s["status"] = UpstreamBillingProbeStatusFailed
		}, class: ProfitPreviewClassAdmitted},
		{name: "oauth uses manual rate", mutate: func(a *Account, s, _ map[string]any) {
			a.Type = AccountTypeOAuth
			s["status"] = UpstreamBillingProbeStatusFailed
		}, class: ProfitPreviewClassAdmitted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := profitControlSyncedTestAccount(now)
			snapshot, ok := account.Extra[UpstreamBillingProbeExtraKey].(map[string]any)
			require.True(t, ok)
			data, ok := snapshot["data"].(map[string]any)
			require.True(t, ok)
			if tt.mutate != nil {
				tt.mutate(account, snapshot, data)
			}
			ctx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{threshold: 0.7, pricingAt: now})
			vetoed, reason := OpenAIProfitControlVeto(ctx, account)
			wantReason := tt.reason
			if tt.class == rejectedProbe {
				wantReason = "profit_upstream_rate_unavailable"
			}
			require.Equal(t, tt.class != ProfitPreviewClassAdmitted, vetoed)
			require.Equal(t, wantReason, reason)
			verdict := previewAccountProfitAdmission(account, true, 0.7, 0.3, now)
			require.Equal(t, tt.class, verdict.Class)
			if tt.class == rejectedProbe || tt.class == ProfitPreviewClassRejectedInvalidRate {
				require.Nil(t, verdict.EffectiveRate)
			} else {
				wantRate := tt.rate
				if wantRate == 0 {
					wantRate = 0.4
				}
				require.NotNil(t, verdict.EffectiveRate)
				require.InDelta(t, wantRate, *verdict.EffectiveRate, 1e-12)
			}
			if tt.class == ProfitPreviewClassAdmitted {
				require.True(t, verdict.RejectedUnderMinD)
			}
			// Opt-in probe failures must never install a gate for a disabled group.
			vetoed, reason = OpenAIProfitControlVeto(context.Background(), account)
			require.False(t, vetoed)
			require.Empty(t, reason)
			require.Equal(t, ProfitPreviewClassAdmitted, previewAccountProfitAdmission(account, false, 0.7, 0.3, now).Class)
		})
	}
}

func TestProfitControlSyncedPeakUsesAttemptTime(t *testing.T) {
	now := time.Now().UTC()
	account := profitControlSyncedTestAccount(now)
	snapshot, ok := account.Extra[UpstreamBillingProbeExtraKey].(map[string]any)
	require.True(t, ok)
	data, ok := snapshot["data"].(map[string]any)
	require.True(t, ok)
	zone := "UTC"
	if now.Hour() >= 12 {
		zone = "Etc/GMT-12"
	}
	data["peak_rate_enabled"] = true
	data["peak_start"] = "00:00"
	data["peak_end"] = "12:00"
	data["timezone"] = zone
	data["peak_rate_multiplier"] = 2.0
	// D was frozen before this peak window. U still follows the current attempt.
	ctx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{threshold: 0.7, pricingAt: now.Add(-12 * time.Hour)})
	vetoed, reason := OpenAIProfitControlVeto(ctx, account)
	require.True(t, vetoed)
	require.Equal(t, openAIProfitFilterReasonThreshold, reason)
	verdict := previewAccountProfitAdmission(account, true, 0.7, 0.3, now)
	require.Equal(t, ProfitPreviewClassRejectedThreshold, verdict.Class)
	require.Equal(t, 0.4, *verdict.AccountRate)
	require.InDelta(t, 0.8, *verdict.EffectiveRate, 1e-12)
	require.Equal(t, 0.4, *account.RateMultiplier, "admission must not write back the peak")
	data["peak_rate_enabled"] = false
	vetoed, reason = OpenAIProfitControlVeto(ctx, account)
	require.False(t, vetoed)
	require.Empty(t, reason)
}

func TestProfitControlSyncedSnapshotRecoveryAndObserver(t *testing.T) {
	now := time.Now().UTC()
	account := profitControlSyncedTestAccount(now)
	snapshot, ok := account.Extra[UpstreamBillingProbeExtraKey].(map[string]any)
	require.True(t, ok)
	gate := &openAIProfitControlGate{groupID: 972311, platform: PlatformOpenAI, threshold: 0.7, pricingAt: now}
	ctx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, gate)
	stats := openAIProfitControlObserverInstance.stats(gate.groupID, gate.platform)
	before := stats.vetoUpstreamRate.Load()
	snapshot["status"] = UpstreamBillingProbeStatusFailed
	vetoed, reason := OpenAIProfitControlVeto(ctx, account)
	require.True(t, vetoed)
	require.Equal(t, openAIProfitFilterReasonUpstreamRateUnavailable, reason)
	require.Equal(t, before+1, stats.vetoUpstreamRate.Load())
	verdict := previewAccountProfitAdmission(account, true, 0.7, 0.3, now)
	require.Contains(t, verdict.Warnings, ProfitPreviewWarningProbeFailed)
	snapshot["status"] = UpstreamBillingProbeStatusOK
	vetoed, reason = OpenAIProfitControlVeto(ctx, account)
	require.False(t, vetoed)
	require.Empty(t, reason)
	require.Equal(t, before+1, stats.vetoUpstreamRate.Load(), "recovery must not increment rejection counts")
	verdict = previewAccountProfitAdmission(account, true, 0.7, 0.3, now)
	require.Equal(t, ProfitPreviewClassAdmitted, verdict.Class)
	require.Empty(t, verdict.Warnings)
}
