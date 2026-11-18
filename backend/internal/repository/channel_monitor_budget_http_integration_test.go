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
