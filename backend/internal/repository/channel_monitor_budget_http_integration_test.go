//go:build integration

package repository_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type v1BudgetMonitorEncryptor struct{}

func (v1BudgetMonitorEncryptor) Encrypt(v string) (string, error) { return v, nil }
func (v1BudgetMonitorEncryptor) Decrypt(v string) (string, error) { return v, nil }

func TestChannelMonitorBudgetHTTPPostgresActualUsageAndRecovery(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	settings := repository.NewSettingRepository(client)
	require.NoError(t, settings.SetMultiple(ctx, map[string]string{
		service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1",
	}))
	budget := repository.NewMonitoringProbeBudgetRepository(db)
	policy := service.DefaultMonitoringProbeBudgetSettings()
	policy.Enabled, policy.RecoveryReservePercent, policy.KeyConcurrency = true, 0, 4
	policy.TokensPerDay = 5
	policy, err := budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	var heads, generations atomic.Int32
	var omitUsage atomic.Bool
	ready := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodHead {
			heads.Add(1)
			require.Equal(t, "/", req.URL.Path)
			require.Empty(t, req.Header.Get("Authorization"), "origin ping must not disclose the configured Key")
			require.Empty(t, req.Header.Get("X-Api-Key"))
			return
		}
		require.Equal(t, "/tenant/v1/chat/completions", req.URL.Path)
		require.Equal(t, "Bearer v1-final-fixture", req.Header.Get("Authorization"))
		n := generations.Add(1)
		// Both models are physically admitted before either reports usage.
		// Their returned totals may exceed the daily budget in flight.
		if n == 2 {
			close(ready)
		}
		select {
		case <-ready:
		case <-req.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if omitUsage.Load() {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(service.SetChannelMonitorHTTPClientForTesting(upstream.Client()))
	monitors := repository.NewChannelMonitorRepository(client, db)
	monitor := &service.ChannelMonitor{
		Name: "v1-budget-http", Provider: service.MonitorProviderOpenAI, APIMode: service.MonitorAPIModeChatCompletions,
		Endpoint: upstream.URL + "/tenant/v1", APIKey: "unused-v1-fixture", Enabled: true, IntervalSeconds: 60,
		PrimaryModel: "model-a", ExtraModels: []string{"model-b"}, ExtraHeaders: map[string]string{"authorization": "Bearer v1-final-fixture"},
		BodyOverrideMode: service.MonitorBodyOverrideModeReplace,
		BodyOverride:     map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}},
	}
	require.NoError(t, monitors.Create(ctx, monitor))
	type checkResult struct {
		Skipped        bool                                 `json:"skipped"`
		Status         string                               `json:"status"`
		LatencyMs      *int                                 `json:"latency_ms"`
		BudgetDeferral *service.MonitoringProbeBudgetDenial `json:"budget_deferral"`
	}
	run := func() []checkResult {
		t.Helper()
		// Reconstruct settings, repositories and service for each admin call.
		// Persistence, not a process-local counter, must stop the later sends.
		svc := service.ProvideChannelMonitorService(repository.NewChannelMonitorRepository(client, db), v1BudgetMonitorEncryptor{},
			service.NewSettingService(repository.NewSettingRepository(client), nil), &config.Config{}, nil,
			repository.NewMonitoringProbeBudgetRepository(db))
		router := gin.New()
		router.POST("/monitors/:id/run", admin.NewChannelMonitorHandler(svc).Run)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/monitors/%d/run", monitor.ID), nil))
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.NotContains(t, recorder.Body.String(), "v1-final-fixture")
		var envelope struct {
			Data struct {
				Results []checkResult `json:"results"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
		require.Len(t, envelope.Data.Results, 2)
		return envelope.Data.Results
	}
	for _, result := range run() {
		require.False(t, result.Skipped)
		require.Equal(t, service.MonitorStatusOperational, result.Status)
		require.Nil(t, result.BudgetDeferral)
	}
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 14, summary.DayTokens, "only the two actual returned usages are charged, including in-flight excess")
	require.EqualValues(t, 3, summary.DayUsed, "one unauthenticated HEAD and two model sends")
	require.EqualValues(t, 2, summary.UsageReported)
	require.Zero(t, summary.UsageUnknown)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.ActiveRequests)
	before, err := monitors.GetByID(ctx, monitor.ID)
	require.NoError(t, err)
	require.NotNil(t, before.LastCheckedAt)
	for _, result := range run() {
		require.True(t, result.Skipped)
		require.Nil(t, result.LatencyMs)
		require.NotNil(t, result.BudgetDeferral)
		require.Equal(t, "token_limit", result.BudgetDeferral.Reason)
		require.Equal(t, "global_day", result.BudgetDeferral.Scope)
		require.True(t, result.BudgetDeferral.RetryAt.After(time.Now()))
	}
	after, err := monitors.GetByID(ctx, monitor.ID)
	require.NoError(t, err)
	require.Equal(t, before.LastCheckedAt, after.LastCheckedAt, "local deferral must not refresh earlier passing evidence")
	for _, model := range []string{"model-a", "model-b"} {
		history, err := monitors.ListHistory(ctx, monitor.ID, model, 20)
		require.NoError(t, err)
		require.Len(t, history, 1)
	}
	require.EqualValues(t, 2, generations.Load())
	policy.TokensPerDay = 50
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	for _, result := range run() {
		require.False(t, result.Skipped)
		require.Equal(t, service.MonitorStatusOperational, result.Status)
	}
	omitUsage.Store(true)
	for _, result := range run() {
		require.False(t, result.Skipped, "missing usage is tracked as unknown, not guessed or presented as zero consumption")
	}
	summary, err = repository.NewMonitoringProbeBudgetRepository(db).Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 28, summary.DayTokens)
	require.EqualValues(t, 10, summary.DayUsed)
	require.EqualValues(t, 4, summary.UsageReported)
	require.EqualValues(t, 2, summary.UsageUnknown)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.ActiveRequests)
	require.EqualValues(t, 4, heads.Load())
	require.EqualValues(t, 6, generations.Load())
	var downstreamUsage int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM usage_logs").Scan(&downstreamUsage))
	require.Zero(t, downstreamUsage, "monitoring accounting must not create downstream business charges")
}

func TestChannelMonitorQuotaBudgetHTTPPostgresRestartAndRecovery(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	settings := repository.NewSettingRepository(client)
	require.NoError(t, settings.SetMultiple(ctx, map[string]string{
		service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1",
	}))
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		require.Equal(t, http.MethodGet, req.Method)
		require.Equal(t, "/tenant/user/balance", req.URL.Path)
		require.Equal(t, "Bearer quota-final-fixture", req.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10"}]}`)
	}))
	t.Cleanup(upstream.Close)
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
		AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	accounts := repository.NewAccountRepository(client, db, nil)
	monitors := repository.NewChannelMonitorRepository(client, db)
	create := func(name string) (*service.Account, *service.ChannelMonitor) {
		a := &service.Account{Name: name, Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Concurrency: 2, Schedulable: true,
			Credentials: map[string]any{"api_key": "quota-final-fixture", "base_url": upstream.URL + "/tenant",
				"account_mode": service.AccountModePayG}}
		require.NoError(t, accounts.Create(ctx, a))
		m := &service.ChannelMonitor{Name: name, Provider: service.MonitorProviderDeepseek,
			Endpoint: upstream.URL, APIKey: "unused-quota-monitor-fixture",
			CheckMode: service.MonitorCheckModeQuota, AccountID: &a.ID, PrimaryModel: "quota", Enabled: true, IntervalSeconds: 60}
		require.NoError(t, monitors.Create(ctx, m))
		return a, m
	}
	firstAccount, first := create("quota-budget-first")
	aliasAccount, alias := create("quota-budget-alias")
	accountRow := func(id int64) string {
		var raw string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&raw))
		return raw
	}
	aliasBefore := accountRow(aliasAccount.ID)
	firstBefore := accountRow(firstAccount.ID)
	budget := repository.NewMonitoringProbeBudgetRepository(db)
	policy := service.DefaultMonitoringProbeBudgetSettings()
	policy.Enabled, policy.RecoveryReservePercent, policy.KeyRequestsPerMinute = true, 0, 1
	policy, err := budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	type quotaResult struct {
		Status         string                               `json:"status"`
		Skipped        bool                                 `json:"skipped"`
		Quota          json.RawMessage                      `json:"quota"`
		BudgetDeferral *service.MonitoringProbeBudgetDenial `json:"budget_deferral"`
	}
	run := func(id int64) quotaResult {
		t.Helper()
		// Recreate every adapter and budget reader. SQL, not a process counter,
		// must coordinate aliases of the same actual outgoing Key.
		accountRepo := repository.NewAccountRepository(client, db, nil)
		httpUpstream := repository.NewHTTPUpstream(cfg)
		svc := service.ProvideChannelMonitorService(repository.NewChannelMonitorRepository(client, db), v1BudgetMonitorEncryptor{},
			service.NewSettingService(settings, nil), cfg, nil, repository.NewMonitoringProbeBudgetRepository(db))
		svc.SetQuotaFetcher(service.NewChannelMonitorQuotaFetcher(nil, nil,
			service.NewCNProviderBalanceService(accountRepo, nil, httpUpstream, cfg), accountRepo, cfg))
		router := gin.New()
		router.POST("/monitors/:id/run", admin.NewChannelMonitorHandler(svc).Run)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/monitors/%d/run", id), nil))
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.NotContains(t, recorder.Body.String(), "quota-final-fixture")
		var envelope struct {
			Data struct {
				Results []quotaResult `json:"results"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
		require.Len(t, envelope.Data.Results, 1)
		return envelope.Data.Results[0]
	}
	result := run(first.ID)
	require.False(t, result.Skipped)
	require.Equal(t, service.MonitorStatusOperational, result.Status)
	require.Contains(t, string(result.Quota), `"balance":10`)
	require.NotEqual(t, firstBefore, accountRow(firstAccount.ID))
	result = run(alias.ID)
	require.True(t, result.Skipped)
	require.NotNil(t, result.BudgetDeferral)
	require.Equal(t, "request_limit", result.BudgetDeferral.Reason)
	require.Equal(t, "key_minute", result.BudgetDeferral.Scope)
	require.Empty(t, result.Quota)
	require.Equal(t, aliasBefore, accountRow(aliasAccount.ID), "local budget denial must not write supplier quota state")
	after, err := monitors.GetByID(ctx, alias.ID)
	require.NoError(t, err)
	require.Nil(t, after.LastCheckedAt)
	history, err := monitors.ListHistory(ctx, alias.ID, "quota", 20)
	require.NoError(t, err)
	require.Empty(t, history)
	require.EqualValues(t, 1, calls.Load())
	policy.KeyRequestsPerMinute = 3
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	result = run(alias.ID)
	require.False(t, result.Skipped)
	require.Equal(t, service.MonitorStatusOperational, result.Status)
	require.EqualValues(t, 2, calls.Load())
	history, err = monitors.ListHistory(ctx, alias.ID, "quota", 20)
	require.NoError(t, err)
	require.Len(t, history, 1)
	summary, err := repository.NewMonitoringProbeBudgetRepository(db).Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, summary.DayUsed)
	require.Zero(t, summary.DayTokens)
	require.Zero(t, summary.UsageUnknown)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.ActiveRequests)
	var downstreamUsage int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&downstreamUsage))
	require.Zero(t, downstreamUsage)
}
