package service

// Shared fixtures must compile with and without the unit build tag.

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func newLegacySchedulerDecisionTestAccounts(groupID int64, secondWSEnabled bool) []Account {
	return []Account{
		{
			ID: 38101, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0,
			GroupIDs: []int64{groupID},
			Extra:    map[string]any{"openai_apikey_responses_websockets_v2_enabled": true},
		},
		{
			ID: 38102, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5,
			GroupIDs: []int64{groupID},
			Extra:    map[string]any{"openai_apikey_responses_websockets_v2_enabled": secondWSEnabled},
		},
	}
}

func newLegacySchedulerDecisionTestService(accounts []Account, loadBatch bool, concurrency schedulerTestConcurrencyCache) *OpenAIGatewayService {
	cfg := newSchedulerTestOpenAIWSV2Config()
	cfg.Gateway.Scheduling.LoadBatchEnabled = loadBatch
	return &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(concurrency),
	}
}

func releaseLegacySchedulerDecisionSelection(selection *AccountSelectionResult) {
	if selection != nil && selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
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

func rawChatCompletionsTestConfig() *config.Config {
	return &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{
				Enabled:           false,
				AllowInsecureHTTP: true,
			},
		},
	}
}
