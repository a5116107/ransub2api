package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestOpenAIAPIKeyHealthCommittedStreamFailures(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, terminal := range []string{
			`{"type":"response.failed","response":{"status":"failed","error":{"code":"upstream_http2_stream_error","message":"Upstream HTTP/2 stream failed"},"usage":{"input_tokens":3,"output_tokens":2}}}`,
			`{"type":"error","error":{"code":"internal_server_error","type":"server_error","message":"siwc executor: stream ended without terminal event"}}`,
			"{\"type\":\"error\",\"error\":{\"code\":\"internal_server_error\",\"message\":\"pending upstream failure\"}}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"upstream_error\",\"message\":\"final upstream failure\"}}}",
		} {
			t.Run(fmt.Sprintf("route=%s/%s", route, terminal[:24]), func(t *testing.T) {
				settings := NewSettingService(&openAIAPIKeyHealthSettingRepo{value: `{"enabled":true,"window_minutes":5,"failure_threshold":3,"cooldown_minutes":1}`}, &config.Config{})
				cache := &openAIAPIKeyHealthCacheStub{tripped: true}
				repo := &openAIAPIKeyHealthAccountRepo{}
				rateLimit := NewRateLimitService(repo, nil, &config.Config{}, nil, cache)
				rateLimit.SetSettingService(settings)
				rateLimit.SetOpenAIAPIKeyHealthCache(cache)
				svc := &OpenAIGatewayService{cfg: &config.Config{}, rateLimitService: rateLimit, toolCorrector: NewCodexToolCorrector()}
				account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				wire := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n" + "data: " + terminal + "\n\n"
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}
				var err error
				switch route {
				case "chat":
					_, err = svc.handleChatStreamingResponse(resp, c, account, "gpt-6.1-sol", "gpt-6.1-sol", "gpt-6.1-sol", time.Now(), 0)
				case "messages":
					_, err = svc.handleAnthropicStreamingResponse(resp, c, account, "gpt-6.1-sol", "gpt-6.1-sol", "gpt-6.1-sol", time.Now())
				case "passthrough":
					_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-6.1-sol", "gpt-6.1-sol")
				default:
					_, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-6.1-sol", "gpt-6.1-sol")
				}
				require.ErrorContains(t, err, "upstream response failed")
				require.Contains(t, rec.Body.String(), "partial", "the test must commit output before failing")
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover), "committed output must never become replayable")
				require.True(t, svc.ObserveOpenAIAccountHealthFailure(c.Request.Context(), account, err), "an explicit upstream failure must reach the enabled health breaker")
				require.Equal(t, 1, cache.recordCalls, "count one failed physical stream, not each event")
				require.Equal(t, 1, repo.setCalls)
			})
		}
	}
}

func TestOpenAIUpstreamTerminalErrorHealthExclusions(t *testing.T) {
	for _, tt := range []struct {
		name, payload string
		eligible      bool
	}{
		{"server", `{"type":"response.failed","response":{"error":{"code":"upstream_error","message":"upstream unavailable"},"output":[{"text":"private generated output"}]}}`, true},
		{"capacity", `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"Please try again later"}}}`, false},
		{"invalid request", `{"type":"error","error":{"type":"invalid_request_error","message":"bad input"}}`, false},
		{"encrypted history", `{"type":"error","error":{"code":"invalid_encrypted_content","message":"history rejected"}}`, false},
		{"nested encrypted history", `{"type":"response.failed","response":{"error":{"code":"invalid_encrypted_content","message":"history rejected"}}}`, false},
		{"cyber", `{"type":"error","error":{"code":"cyber_policy","message":"request blocked"}}`, false},
		{"session", `{"type":"error","error":{"code":"session_blocked_by_cyber_policy","message":"session blocked"}}`, false},
		{"credential", `{"type":"error","error":{"code":"invalid_api_key","message":"credential revoked"}}`, false},
		{"context", `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"maximum context length exceeded"}}}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := newOpenAIUpstreamTerminalError([]byte(tt.payload), extractOpenAISSEErrorMessage([]byte(tt.payload)))
			_, body, eligible := classifyOpenAIAPIKeyHealthFailure(fmt.Errorf("wrapped: %w", err))
			require.Equal(t, tt.eligible, eligible)
			require.NotContains(t, string(body), "private generated output")
			var replay *UpstreamFailoverError
			require.NotErrorAs(t, err, &replay)
		})
	}
}
