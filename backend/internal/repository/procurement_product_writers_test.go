//go:build unit

package repository

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestProcurementWriterNativeControls(t *testing.T) {
	account := func() service.Account {
		return service.Account{ID: 10, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Credentials: map[string]any{}, Extra: map[string]any{"openai_excel_bps": true, service.ExcelBPSAutoMoveOn403Key: true, service.ExcelBPS403TargetGroupIDKey: float64(0)}}
	}
	base := service.ProcurementWriter{}
	t.Run("403 clear-all, disabled, invalid and native applicability", func(t *testing.T) {
		a := account()
		row := procurementExcelWriter(a, base)
		require.Equal(t, "membership_conflict", row.Risk)
		require.True(t, row.WritesProduct)
		require.True(t, *row.Enabled)
		a.Extra[service.ExcelBPS403TargetGroupIDKey] = "0"
		row = procurementExcelWriter(a, base)
		require.Equal(t, "unknown", row.Risk)
		require.Nil(t, row.Enabled)
		a.Extra[service.ExcelBPSAutoMoveOn403Key] = false
		require.Nil(t, procurementExcelWriter(a, base))
		a.Extra[service.ExcelBPSAutoMoveOn403Key] = "future"
		require.Equal(t, "unknown", procurementExcelWriter(a, base).Risk)
		a = account()
		a.Credentials["plan_type"] = "free"
		require.Equal(t, "historical_only", procurementExcelWriter(a, base).Risk)
		a = account()
		a.ParentAccountID = &a.ID
		require.Equal(t, "historical_only", procurementExcelWriter(a, base).Risk)
	})
	t.Run("OAuth current settings and non-authoritative audit history", func(t *testing.T) {
		a := account()
		c := service.DefaultOAuthAutoConfig()
		c.DegradationAction.Enabled, c.DegradationAction.LimitGroups = true, true
		c.DegradationAction.GroupIDs = []int64{21}
		require.True(t, procurementOAuthWriter(a, base, 21, c, true).WritesProduct)
		require.Equal(t, "account_protection", procurementOAuthWriter(a, base, 22, c, true).Risk)
		require.Nil(t, procurementOAuthWriter(a, base, 21, c, false))
		a.Extra[service.OAuthDegradationActionExtraKey] = map[string]any{"revision": "old", "removed_group_ids": []int64{21}}
		row := procurementOAuthWriter(a, base, 21, c, true)
		require.Equal(t, "historical_only", row.Risk)
		require.False(t, row.WritesProduct)
		require.False(t, row.RetainedOwnership)
		a.Extra[service.OAuthDegradationActionExtraKey] = map[string]any{"revision": "old", "future": true}
		require.Equal(t, "unknown", procurementOAuthWriter(a, base, 21, c, true).Risk)
	})
	t.Run("Astra disabled does not relinquish saved membership ownership", func(t *testing.T) {
		a := account()
		raw := []byte(`{"mode":"groups","group_ids":[21],"memberships":[{"group_id":21,"before":null,"after":null}]}`)
		row := procurementAstraWriter(a, base, 21, config.AstraRoutingSettings{}, true, raw)
		require.Equal(t, "membership_conflict", row.Risk)
		require.True(t, row.RetainedOwnership)
		require.False(t, *row.Enabled)
		require.False(t, row.WritesProduct)
		row = procurementAstraWriter(a, base, 21, config.AstraRoutingSettings{}, false, raw)
		require.Equal(t, "unknown", row.Risk)
		require.Nil(t, row.Enabled)
		cfg := config.AstraRoutingSettings{AccountScheduling: true, SchedulingMode: "groups", SchedulingGroupIDs: []int64{21}, CookiePool: config.CodexGatewayPinConfig{TargetAccountIDs: []int64{a.ID}}}
		require.True(t, procurementAstraWriter(a, base, 21, cfg, true, nil).WritesProduct)
		cfg.SchedulingMode = "account"
		require.Equal(t, "account_protection", procurementAstraWriter(a, base, 21, cfg, true, nil).Risk)
		for _, mode := range []string{"account", "model"} {
			row := procurementAstraWriter(a, base, 21, config.AstraRoutingSettings{}, true, []byte(`{"mode":"`+mode+`","group_ids":[21]}`))
			require.Equal(t, "account_protection", row.Risk, "the native executor retains group selection metadata in non-group modes")
			require.True(t, row.RetainedOwnership)
			require.False(t, row.WritesProduct)
		}
		for _, raw := range []string{`null`, `{"mode":"future"}`, `{"mode":"groups","future":true}`, `{"mode":"groups","group_ids":[0]}`,
			`{"mode":"groups","group_ids":[21,21]}`, `{"mode":"model","removed":[{"group_id":21}]}`, `{"mode":"groups","memberships":[{"group_id":21,"before":{"group_id":22}}]}`} {
			require.Equal(t, "unknown", procurementAstraWriter(a, base, 21, cfg, true, []byte(raw)).Risk)
		}
	})
	t.Run("2FA pending, resumed, terminal and mismatched enrollment", func(t *testing.T) {
		a := account()
		policy := service.DefaultOpenAITwoFAAdmissionConfig()
		policy.Enabled, policy.IsolationGroupID = true, 21
		state := service.OpenAITwoFAAdmissionState{PlanID: 42, Policy: policy, Phase: "pending"}
		a.Extra[service.OpenAITwoFAAdmissionExtraKey] = state
		plan, err := json.Marshal(&service.PelicanTestConfig{Quality: &service.QualityPolicy{Action: service.QualityActionOpenAITwoFAAdmission, Admission: &policy}})
		require.NoError(t, err)
		require.True(t, procurementAdmissionWriter(a, base, sql.NullBool{Valid: true, Bool: true}, plan).WritesProduct)
		require.Equal(t, "account_protection", procurementAdmissionWriter(a, base, sql.NullBool{Valid: true}, plan).Risk)
		state.Phase = "isolation_pending"
		a.Extra[service.OpenAITwoFAAdmissionExtraKey] = state
		require.True(t, procurementAdmissionWriter(a, base, sql.NullBool{Valid: true, Bool: true}, plan).WritesProduct)
		state.Phase = "isolated"
		a.Extra[service.OpenAITwoFAAdmissionExtraKey] = state
		require.Equal(t, "historical_only", procurementAdmissionWriter(a, base, sql.NullBool{Valid: true, Bool: true}, plan).Risk)
		require.Equal(t, "unknown", procurementAdmissionWriter(a, base, sql.NullBool{}, nil).Risk)
		require.Equal(t, "unknown", procurementAdmissionWriter(a, base, sql.NullBool{Valid: true}, []byte(`null`)).Risk)
	})
}
