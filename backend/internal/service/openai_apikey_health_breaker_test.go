package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type openAIAPIKeyHealthSettingRepo struct {
	SettingRepository
	value    string
	getCalls int
	setErr   error
}

func (r *openAIAPIKeyHealthSettingRepo) GetValue(context.Context, string) (string, error) {
	r.getCalls++
	return r.value, nil
}

func (r *openAIAPIKeyHealthSettingRepo) Set(_ context.Context, _ string, value string) error {
	if r.setErr != nil {
		return r.setErr
	}
	r.value = value
	return nil
}

type openAIAPIKeyHealthAccountRepo struct {
	AccountRepository
	setCalls int
	reason   string
}

func (r *openAIAPIKeyHealthAccountRepo) SetTempUnschedulable(_ context.Context, _ int64, _ time.Time, reason string) error {
	r.setCalls++
	r.reason = reason
	return nil
}

type openAIAPIKeyHealthCacheStub struct {
	TempUnschedCache
	recordCalls int
	setCalls    int
	tripped     bool
}

func (c *openAIAPIKeyHealthCacheStub) RecordOpenAIAPIKeyHealthFailure(context.Context, int64, int, int) (int64, bool, error) {
	c.recordCalls++
	return int64(c.recordCalls), c.tripped, nil
}

func (c *openAIAPIKeyHealthCacheStub) SetTempUnsched(context.Context, int64, *TempUnschedState) error {
	c.setCalls++
	return nil
}

type openAIAPIKeyHealthRuntimeBlocker struct{ calls int }

func (b *openAIAPIKeyHealthRuntimeBlocker) BlockAccountScheduling(*Account, time.Time, string) {
	b.calls++
}
func (*openAIAPIKeyHealthRuntimeBlocker) ClearAccountSchedulingBlock(int64) {}

func openAIHealthPoolAccount() *Account {
	return &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}
}

func TestOpenAIAPIKeyHealthBreakerDoesNotDependOnSameAccountRetryMode(t *testing.T) {
	for _, account := range []*Account{
		openAIHealthPoolAccount(),
		{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
	} {
		require.True(t, isOpenAIAPIKeyHealthBreakerAccount(account), "explicit health protection also covers ordinary API keys")
	}
	for _, account := range []*Account{nil, {Platform: PlatformOpenAI, Type: AccountTypeOAuth}, {Platform: PlatformAnthropic, Type: AccountTypeAPIKey}} {
		require.False(t, isOpenAIAPIKeyHealthBreakerAccount(account))
	}
}

func TestClassifyOpenAIAPIKeyHealthFailureExclusions(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		eligible bool
	}{
		{name: "account attributed 502", err: &UpstreamFailoverError{StatusCode: http.StatusBadGateway}, eligible: true},
		{name: "account attributed 503", err: &UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable}, eligible: true},
		{name: "submitted response header timeout", err: &UpstreamFailoverError{StatusCode: http.StatusGatewayTimeout, HeaderTimeoutSubmitted: true, Stage: GatewayFailureStageInference, Scope: GatewayFailureScopeRequest}, eligible: true},
		{name: "wrapped submitted response header timeout", err: fmt.Errorf("wrapped: %w", &UpstreamFailoverError{StatusCode: http.StatusGatewayTimeout, HeaderTimeoutSubmitted: true, Scope: GatewayFailureScopeRequest}), eligible: true},
		{name: "other request scoped timeout", err: &UpstreamFailoverError{StatusCode: http.StatusGatewayTimeout, Scope: GatewayFailureScopeRequest}},
		{name: "explicit request transient timeout", err: &UpstreamFailoverError{StatusCode: http.StatusGatewayTimeout, HeaderTimeoutSubmitted: true, RequestScopedTransient: true, Scope: GatewayFailureScopeRequest}},
		{name: "client canceled", err: fmt.Errorf("wrapped: %w", context.Canceled)},
		{name: "client deadline", err: fmt.Errorf("wrapped: %w", context.DeadlineExceeded)},
		{name: "local shared key capacity", err: upstreamKeyAdmissionFailover(ErrUpstreamKeyCapacity)},
		{name: "wrapped local shared key policy", err: fmt.Errorf("wrapped: %w", upstreamKeyAdmissionFailover(ErrUpstreamKeyPolicy))},
		{name: "request scoped capacity", err: &UpstreamFailoverError{StatusCode: 529, RequestScopedTransient: true}},
		{name: "provider scoped overload", err: &UpstreamFailoverError{StatusCode: 529, Scope: GatewayFailureScopeProvider}},
		{name: "dedicated same account retry", err: &UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, RetryableOnSameAccount: true}},
		{name: "credential disable path", err: &UpstreamFailoverError{StatusCode: http.StatusUnauthorized, Stage: GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeAccount}},
		{name: "client request", err: &UpstreamFailoverError{StatusCode: http.StatusBadRequest}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, eligible := classifyOpenAIAPIKeyHealthFailure(tt.err)
			require.Equal(t, tt.eligible, eligible)
		})
	}
}

func TestOpenAIAPIKeyHealthBreakerDefaultDisabled(t *testing.T) {
	settings := NewSettingService(&openAIAPIKeyHealthSettingRepo{}, &config.Config{})
	cache := &openAIAPIKeyHealthCacheStub{tripped: true}
	svc := NewRateLimitService(&openAIAPIKeyHealthAccountRepo{}, nil, &config.Config{}, nil, cache)
	svc.SetSettingService(settings)
	svc.SetOpenAIAPIKeyHealthCache(cache)

	require.False(t, svc.ObserveOpenAIAPIKeyHealthFailure(context.Background(), openAIHealthPoolAccount(), &UpstreamFailoverError{StatusCode: http.StatusBadGateway}))
	require.Zero(t, cache.recordCalls)
}

func TestOpenAIAPIKeyHealthBreakerTripsPersistedAndRuntimeState(t *testing.T) {
	encoded, err := json.Marshal(OpenAIAPIKeyHealthBreakerSettings{Enabled: true, WindowMinutes: 1, FailureThreshold: 3, CooldownMinutes: 5})
	require.NoError(t, err)
	settings := NewSettingService(&openAIAPIKeyHealthSettingRepo{value: string(encoded)}, &config.Config{})
	cache := &openAIAPIKeyHealthCacheStub{tripped: true}
	repo := &openAIAPIKeyHealthAccountRepo{}
	blocker := &openAIAPIKeyHealthRuntimeBlocker{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, cache)
	svc.SetSettingService(settings)
	svc.SetOpenAIAPIKeyHealthCache(cache)
	svc.SetAccountRuntimeBlocker(blocker)
	account := openAIHealthPoolAccount()
	for _, local := range []error{ErrUpstreamKeyCapacity, ErrUpstreamKeyPolicy} {
		require.False(t, svc.ObserveOpenAIAPIKeyHealthFailure(context.Background(), account, fmt.Errorf("wrapped: %w", upstreamKeyAdmissionFailover(local))))
	}
	require.Zero(t, cache.recordCalls)
	require.Zero(t, cache.setCalls)
	require.Zero(t, repo.setCalls)
	require.Zero(t, blocker.calls)
	require.Nil(t, account.TempUnschedulableUntil)

	require.True(t, svc.ObserveOpenAIAPIKeyHealthFailure(context.Background(), account, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, ResponseBody: []byte(`{"error":"upstream"}`)}))
	require.Equal(t, 1, cache.recordCalls)
	require.Equal(t, 1, cache.setCalls)
	require.Equal(t, 1, repo.setCalls)
	require.Equal(t, 1, blocker.calls)
	require.NotNil(t, account.TempUnschedulableUntil)
	require.Contains(t, repo.reason, openAIAPIKeyHealthBreakerReason)
}

func TestOpenAIAPIKeyHealthSuccessDoesNotTouchSettingsOrCache(t *testing.T) {
	encoded, err := json.Marshal(OpenAIAPIKeyHealthBreakerSettings{Enabled: true, WindowMinutes: 1, FailureThreshold: 3, CooldownMinutes: 5})
	require.NoError(t, err)
	settingRepo := &openAIAPIKeyHealthSettingRepo{value: string(encoded)}
	settings := NewSettingService(settingRepo, &config.Config{})
	cache := &openAIAPIKeyHealthCacheStub{}
	svc := NewRateLimitService(&openAIAPIKeyHealthAccountRepo{}, nil, &config.Config{}, nil, cache)
	svc.SetSettingService(settings)
	svc.SetOpenAIAPIKeyHealthCache(cache)

	svc.ObserveOpenAIAPIKeyHealthSuccess(context.Background(), openAIHealthPoolAccount())
	svc.ObserveOpenAIAPIKeyHealthSuccess(context.Background(), &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey})
	require.Zero(t, settingRepo.getCalls)
	require.Zero(t, cache.recordCalls)
}

func TestOpenAIAPIKeyHealthBreakerSubmittedTimeoutThresholdAndRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settingRepo := &openAIAPIKeyHealthSettingRepo{value: `{"enabled":true,"window_minutes":5,"failure_threshold":3,"cooldown_minutes":1}`}
		cache := &openAIAPIKeyHealthCacheStub{}
		repo := &openAIAPIKeyHealthAccountRepo{}
		blocker := &openAIAPIKeyHealthRuntimeBlocker{}
		svc := NewRateLimitService(repo, nil, &config.Config{}, nil, cache)
		svc.SetSettingService(NewSettingService(settingRepo, &config.Config{}))
		svc.SetOpenAIAPIKeyHealthCache(cache)
		svc.SetAccountRuntimeBlocker(blocker)
		account := openAIHealthPoolAccount()
		account.Credentials = nil // ordinary production API key; no same-account retry mode
		account.Status, account.Schedulable = StatusActive, true
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		failure := handleOpenAIHTTPPhaseError(c, account, &openAIHTTPPhaseTimeout{phase: "response_header_timeout"})
		require.True(t, account.IsSchedulable())
		for attempt := 1; attempt <= 3; attempt++ {
			cache.tripped = attempt == 3
			require.Equal(t, attempt == 3, svc.ObserveOpenAIAPIKeyHealthFailure(t.Context(), account, failure))
			// A concurrent successful request must not erase rolling failures.
			svc.ObserveOpenAIAPIKeyHealthSuccess(t.Context(), account)
		}
		require.Equal(t, 3, cache.recordCalls)
		require.Equal(t, 1, repo.setCalls)
		require.Equal(t, 1, blocker.calls)
		require.False(t, account.IsSchedulable())
		require.True(t, account.Schedulable, "temporary cooldown must not overwrite manual state")
		var state TempUnschedState
		require.NoError(t, json.Unmarshal([]byte(repo.reason), &state))
		require.EqualValues(t, 3, state.TriggerCount)
		require.Equal(t, 3, state.TriggerThreshold)
		require.Equal(t, 5, state.TriggerWindowMinutes)
		time.Sleep(61 * time.Second)
		require.True(t, account.IsSchedulable(), "eligible account recovers after cooldown")
		account.Schedulable = false
		require.False(t, account.IsSchedulable(), "cooldown expiry must preserve manual disable")
	})
}
