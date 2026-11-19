package service

// 投影漏列回归（service 半程）：认证快照 build → L2 JSON 序列化
// → 反序列化 → 还原 apiKey.Group → 请求 ctx → 利润门解析，全链路保真。
// repository 半程（真实 GetByKeyForAuth 投影）见
// internal/repository/api_key_repo_profit_projection_integration_test.go。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func profitAuthTestAPIKey() *APIKey {
	groupID := int64(50)
	return &APIKey{
		ID:      82,
		UserID:  40,
		GroupID: &groupID,
		Name:    "profit-auth-roundtrip",
		Status:  StatusActive,
		User: &User{
			ID:          40,
			Email:       "profit@test.local",
			Status:      StatusActive,
			Concurrency: 5,
		},
		Group: &Group{
			ID:                   groupID,
			Name:                 "VIP-roundtrip",
			Platform:             PlatformOpenAI,
			Status:               StatusActive,
			Hydrated:             true,
			RateMultiplier:       0.06,
			SubscriptionType:     SubscriptionTypeStandard,
			PeakRateEnabled:      false,
			ProfitControlEnabled: true,
			ProfitMinMargin:      0.2,
			ProfitSafetyBuffer:   0.05,
		},
	}
}

// 快照构建 → L2 JSON 往返 → 还原 → 装门：利润字段必须全程保真，阈值与
// 计费同源（0.06 × (1−0.25) = 0.045）。
func TestAPIKeyAuthSnapshotProfitControlRoundtrip(t *testing.T) {
	svc := &APIKeyService{}
	apiKey := profitAuthTestAPIKey()

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	require.NotNil(t, snapshot)
	require.Equal(t, apiKeyAuthSnapshotVersion, snapshot.Version)
	require.Equal(t, apiKeyAuthSnapshotVersion, snapshot.Version, "认证快照版本必须与当前常量一致")

	// 模拟 L2 缓存的完整 JSON 往返（与 apiKeyCache.SetAuthCache/GetAuthCache 同构）。
	payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: snapshot})
	require.NoError(t, err)
	var restored APIKeyAuthCacheEntry
	require.NoError(t, json.Unmarshal(payload, &restored))

	materialized, used, err := svc.applyAuthCacheEntry(apiKey.Key, &restored)
	require.NoError(t, err)
	require.True(t, used)
	require.NotNil(t, materialized.Group)
	require.True(t, materialized.Group.Hydrated)
	require.True(t, materialized.Group.ProfitControlEnabled)
	require.InDelta(t, 0.2, materialized.Group.ProfitMinMargin, 1e-12)
	require.InDelta(t, 0.05, materialized.Group.ProfitSafetyBuffer, 1e-12)
	require.InDelta(t, 0.06, materialized.Group.RateMultiplier, 1e-12)

	// 中间件语义：materialized.Group 进请求 ctx → 门必须按快照配置装上。
	ctx := context.WithValue(context.Background(), ctxkey.Group, materialized.Group)
	gwSvc := &OpenAIGatewayService{}
	gate := gwSvc.resolveOpenAIProfitControlGate(ctx, materialized.GroupID)
	require.NotNil(t, gate, "还原后的认证分组必须能装门（投影漏列时本断言最先失败）")
	require.InDelta(t, 0.06*(1-0.25), gate.threshold, 1e-12)
}

// 旧版本快照（v16 及更早，无利润字段保真保证）必须被淘汰回源，不得复用。
func TestAPIKeyAuthSnapshotOldVersionEvicted(t *testing.T) {
	svc := &APIKeyService{}
	snapshot := svc.snapshotFromAPIKey(context.Background(), profitAuthTestAPIKey())
	require.NotNil(t, snapshot)
	snapshot.Version = 16

	materialized, used, err := svc.applyAuthCacheEntry("sk-old", &APIKeyAuthCacheEntry{Snapshot: snapshot})
	require.NoError(t, err)
	require.False(t, used, "版本不匹配的缓存条目必须淘汰并回源重建")
	require.Nil(t, materialized)
}

func TestProcurementPolicyAuthSnapshotRoundtrip(t *testing.T) {
	svc := &APIKeyService{}
	apiKey := profitAuthTestAPIKey()
	min, max := "0.02", "0.03"
	apiKey.Group.ProcurementPolicy = &ProcurementPolicy{Version: 1, Revision: 2, MinRate: &min, MaxRate: &max}
	snapshot := svc.snapshotFromAPIKey(t.Context(), apiKey)
	payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: snapshot})
	require.NoError(t, err)
	var restored APIKeyAuthCacheEntry
	require.NoError(t, json.Unmarshal(payload, &restored))
	materialized, used, err := svc.applyAuthCacheEntry(apiKey.Key, &restored)
	require.NoError(t, err)
	require.True(t, used)
	require.Equal(t, apiKey.Group.ProcurementPolicy, materialized.Group.ProcurementPolicy)
	*apiKey.Group.ProcurementPolicy.MinRate = "0"
	second := svc.snapshotToAPIKey(apiKey.Key, snapshot)
	require.Equal(t, "0.02", *second.Group.ProcurementPolicy.MinRate, "auth snapshots own their policy values")
	*second.Group.ProcurementPolicy.MinRate = "0.01"
	require.Equal(t, "0.02", *svc.snapshotToAPIKey(apiKey.Key, snapshot).Group.ProcurementPolicy.MinRate)
	snapshot.Version = 34
	_, used, err = svc.applyAuthCacheEntry(apiKey.Key, &APIKeyAuthCacheEntry{Snapshot: snapshot})
	require.NoError(t, err)
	require.False(t, used, "pre-procurement cached projections must reload")
}

func TestProcurementPolicySourcesAuthSnapshotRoundtrip(t *testing.T) {
	for _, ids := range []string{"null", "[]", "[901,902]"} {
		t.Run(ids, func(t *testing.T) {
			svc := &APIKeyService{}
			apiKey := profitAuthTestAPIKey()
			require.NoError(t, json.Unmarshal([]byte(`{"procurement_policy":{"version":2,"revision":2,"min_rate":null,"max_rate":null,"sources":{"account_ids":`+ids+`}}}`), apiKey.Group))
			snapshot := svc.snapshotFromAPIKey(t.Context(), apiKey)
			payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: snapshot})
			require.NoError(t, err)
			var restored APIKeyAuthCacheEntry
			require.NoError(t, json.Unmarshal(payload, &restored))
			materialized, used, err := svc.applyAuthCacheEntry(apiKey.Key, &restored)
			require.NoError(t, err)
			require.True(t, used)
			require.Equal(t, apiKey.Group.ProcurementPolicy, materialized.Group.ProcurementPolicy)
			if ids == "[901,902]" {
				materialized.Group.ProcurementPolicy.Sources.AccountIDs[0] = 1
				require.EqualValues(t, 901, apiKey.Group.ProcurementPolicy.Sources.AccountIDs[0])
				require.EqualValues(t, 901, svc.snapshotToAPIKey(apiKey.Key, snapshot).Group.ProcurementPolicy.Sources.AccountIDs[0])
			}
		})
	}
}
