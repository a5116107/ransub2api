package service

// Shared fixtures must compile with and without the unit build tag.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// The queue fixture crosses the real physical budget boundary; it does not
// assume the runner's intended ordering or manufacture a successful snapshot.
type cnQueueQuotaProber struct {
	query func(context.Context, int64) (*CNProviderQuotaProbeResult, error)
}

func (p cnQueueQuotaProber) QueryUsage(ctx context.Context, id int64) (*CNProviderQuotaProbeResult, error) {
	return p.query(ctx, id)
}

func cnQueueAccounts(t *testing.T, count int) *fakeCNCheckRepo {
	t.Helper()
	repo := &fakeCNCheckRepo{byPlatform: map[string][]Account{}}
	for i := 1; i <= count; i++ {
		account := Account{ID: int64(i), Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive,
			Credentials: map[string]any{"account_mode": AccountModeCoding}}
		if i%3 == 1 {
			account = *mistralUsageTestAccount(int64(i), "")
			require.True(t, account.mistralUsageSupported())
		} else if i%3 == 2 {
			account = *clineTestAccount(int64(i))
			require.True(t, account.clineAccountAPISupported())
		}
		// Threshold-paused coding accounts still need recovery observations.
		account.Status, account.Schedulable = StatusActive, i%7 != 0
		repo.byPlatform[account.Platform] = append(repo.byPlatform[account.Platform], account)
	}
	return repo
}

type cnQueueInventoryRepo struct {
	*fakeCNCheckRepo
	list func(context.Context, string) ([]Account, error)
}

func (r cnQueueInventoryRepo) ListByPlatform(ctx context.Context, platform string) ([]Account, error) {
	return r.list(ctx, platform)
}

func mistralUsageTestAccount(id int64, apiKey string) *Account {
	if apiKey == "" {
		apiKey = "mstrl_test_key_0123456789abcdef"
	}
	return &Account{
		ID:          id,
		Name:        "mistral-test",
		Platform:    PlatformMistral,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":       apiKey,
			"base_url":      DefaultMistralBaseURL,
			"account_mode":  AccountModePayG,
			"api_protocol":  APIProtocolAdaptive,
			"api_base_urls": map[string]any{APIProtocolChatCompletions: DefaultMistralBaseURL},
		},
	}
}

// clineTestAccount 只带 api_key：端点与测试模型全部来自 provider profile。
func clineTestAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Name:        "cline",
		Platform:    PlatformCline,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "cline_test_key", "api_protocol": APIProtocolAdaptive},
	}
}
