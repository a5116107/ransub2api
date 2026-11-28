package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestProcurementSourceMappingConclusionNeverGrantsDispatch(t *testing.T) {
	base := service.ProcurementSourceMapping{
		ID: 1, Platform: "openai", LocalGroupPlatform: "openai", LocalGroupID: 4, LocalGroupStatus: "active",
		AccountID: 9, AccountPlatform: "openai", AccountStatus: "active", InLocalGroup: true,
		Enabled: true, SourceGroupEnabled: true, RouteIdentity: "current",
	}
	tests := []struct {
		name       string
		mutate     func(*service.ProcurementSourceMapping)
		conclusion string
		reason     string
	}{
		{name: "current route still awaits fixed-group proof", mutate: func(*service.ProcurementSourceMapping) {}, conclusion: service.ProcurementSourceConclusionPending, reason: "fixed_group_selection_unverified"},
		{name: "route identity changed", mutate: func(m *service.ProcurementSourceMapping) { m.RouteIdentity = "changed" }, conclusion: service.ProcurementSourceConclusionStale, reason: "source_account_route_changed"},
		{name: "paused source group", mutate: func(m *service.ProcurementSourceMapping) { m.SourceGroupEnabled = false }, conclusion: service.ProcurementSourceConclusionPaused, reason: "source_mapping_paused"},
		{name: "paused account mapping", mutate: func(m *service.ProcurementSourceMapping) { m.Enabled = false }, conclusion: service.ProcurementSourceConclusionPaused, reason: "source_mapping_paused"},
		{name: "account deleted", mutate: func(m *service.ProcurementSourceMapping) { m.AccountDeleted = true }, conclusion: service.ProcurementSourceConclusionBlocked, reason: "source_account_deleted"},
		{name: "platform mismatch", mutate: func(m *service.ProcurementSourceMapping) { m.LocalGroupPlatform = "anthropic" }, conclusion: service.ProcurementSourceConclusionBlocked, reason: "source_platform_mismatch"},
		{name: "account left local group", mutate: func(m *service.ProcurementSourceMapping) { m.InLocalGroup = false }, conclusion: service.ProcurementSourceConclusionBlocked, reason: "source_account_not_in_local_group"},
		{name: "local group paused", mutate: func(m *service.ProcurementSourceMapping) { m.LocalGroupStatus = "disabled" }, conclusion: service.ProcurementSourceConclusionBlocked, reason: "local_group_inactive"},
		{name: "route identity unknown", mutate: func(m *service.ProcurementSourceMapping) { m.RouteIdentity = "unknown" }, conclusion: service.ProcurementSourceConclusionBlocked, reason: "source_account_route_unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := base
			item.Reasons = nil
			tt.mutate(&item)
			classifyProcurementSourceMapping(&item)
			require.Equal(t, tt.conclusion, item.Conclusion)
			require.Contains(t, item.Reasons, tt.reason)
			require.False(t, item.Dispatchable)
			require.Equal(t, "declared", item.SourceIdentity)
			require.Equal(t, "unverified", item.FixedGroupSelection)
		})
	}
}
