//go:build integration

package repository_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestMonitoringProbeCoverageBillingScopesHTTPPostgresRecovery(t *testing.T) {
	db, _ := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra)
 SELECT 'scope-http-'||n,'openai','apikey','active',jsonb_build_object('api_key','stored-key-'||n,
 'base_url','https://scope-'||n||'.invalid/v1','header_override_enabled',true,
 'header_overrides',jsonb_build_object('api-key','scope-http-private-key')),
 '{"upstream_billing_probe_enabled":true}'::jsonb FROM generate_series(1,3) n`)
	require.NoError(t, err)
	accountSnapshot := func() string {
		t.Helper()
		var raw string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_agg(to_jsonb(a) ORDER BY id)::text FROM accounts a`).Scan(&raw))
		return raw
	}
	before := accountSnapshot()
	budget := repository.NewMonitoringProbeBudgetRepository(db)
	require.NoError(t, budget.SaveBillingProbeSettings(ctx, service.UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}))
	policy := service.DefaultMonitoringProbeBudgetSettings()
	policy.Enabled, policy.KeyRequestsPerMinute = true, 1
	request := func(method, path string, payload any) *httptest.ResponseRecorder {
		t.Helper()
		probe := service.NewUpstreamBillingProbeService(nil, nil, nil)
		probe.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(repository.NewMonitoringProbeBudgetRepository(db)))
		h := &admin.AccountHandler{}
		h.SetUpstreamBillingProbeService(probe)
		r := gin.New()
		r.POST("/budget/preview", h.PreviewMonitoringProbeCoverage)
		r.PUT("/budget", h.UpdateMonitoringProbeBudget)
		r.GET("/budget", h.GetMonitoringProbeBudget)
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	preview := request(http.MethodPost, "/budget/preview", policy)
	require.Equal(t, http.StatusOK, preview.Code)
	var envelope struct {
		Data service.MonitoringProbeCoverage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(preview.Body.Bytes(), &envelope))
	require.False(t, envelope.Data.CanSave)
	require.EqualValues(t, 6, envelope.Data.BillingScopes.MaxCredentialRequestsPerSweep)
	require.EqualValues(t, 4, envelope.Data.BillingScopes.CredentialGroups, "three distinct stored keys plus the shared allowed final Api-Key header")
	require.EqualValues(t, 3, envelope.Data.BillingScopes.Sites)
	for _, private := range []string{"scope-http-private-key", "stored-key-", ".invalid", "scope-http-"} {
		require.NotContains(t, preview.Body.String(), private)
	}
	rejected := request(http.MethodPut, "/budget", policy)
	require.Equal(t, http.StatusBadRequest, rejected.Code)
	require.Contains(t, rejected.Body.String(), "MONITORING_PROBE_COVERAGE_IMPOSSIBLE")
	require.Contains(t, rejected.Body.String(), "key_capacity")
	current, err := budget.GetSettings(ctx)
	require.NoError(t, err)
	require.Zero(t, current.Revision)
	policy.KeyRequestsPerMinute = 2
	repaired := request(http.MethodPut, "/budget", policy)
	require.Equal(t, http.StatusOK, repaired.Code)
	read := request(http.MethodGet, "/budget", nil)
	require.Equal(t, http.StatusOK, read.Code)
	var summary struct {
		Data service.MonitoringProbeBudgetSummary `json:"data"`
	}
	require.NoError(t, json.Unmarshal(read.Body.Bytes(), &summary))
	require.True(t, summary.Data.Coverage.CanSave)
	require.EqualValues(t, 10, summary.Data.Coverage.BillingScopes.CredentialSweepCapacity)
	require.EqualValues(t, 1, summary.Data.Settings.Revision)
	require.Equal(t, http.StatusConflict, request(http.MethodPut, "/budget", policy).Code)
	require.Equal(t, http.StatusConflict, request(http.MethodPost, "/budget/preview", policy).Code)
	policy = summary.Data.Settings
	policy.Enabled, policy.KeyRequestsPerMinute = false, 1
	require.Equal(t, http.StatusOK, request(http.MethodPut, "/budget", policy).Code, "disabling enforcement remains a repair path")
	current, err = repository.NewMonitoringProbeBudgetRepository(db).GetSettings(ctx)
	require.NoError(t, err)
	require.False(t, current.Enabled)
	require.EqualValues(t, 2, current.Revision)
	require.Equal(t, before, accountSnapshot(), "preflight/save must preserve all account fields")
	for _, table := range []string{"monitoring_probe_budget_counters", "monitoring_probe_budget_leases", "usage_logs"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count)
	}
}

func TestMonitoringProbeCoverageActiveHTTPPostgresRecovery(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	settings := repository.NewSettingRepository(client)
	require.NoError(t, settings.SetMultiple(ctx, map[string]string{
		service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v1"}))
	budget := repository.NewMonitoringProbeBudgetRepository(db)
	policy := service.DefaultMonitoringProbeBudgetSettings()
	policy.Enabled, policy.RecoveryReservePercent, policy.RequestsPerDay = true, 0, 1000
	policy, err := budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	monitors := repository.NewChannelMonitorRepository(client, db)
	monitor := &service.ChannelMonitor{Name: "active-coverage", Provider: service.MonitorProviderOpenAI,
		Endpoint: "https://coverage.invalid/v1", APIKey: "coverage-fixture", PrimaryModel: "model-a",
		ExtraModels: []string{"model-b"}, IntervalSeconds: 60, Enabled: true}
	err = monitors.Create(ctx, monitor)
	require.ErrorContains(t, err, "MONITORING_PROBE_COVERAGE_IMPOSSIBLE",
		"two models plus HEAD need at least 4320 calls; V1 creation must not bypass the daily budget")
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM channel_monitors`).Scan(&count))
	require.Zero(t, count, "rejected creation must roll back its row")
	monitor.Enabled = false
	require.NoError(t, monitors.Create(ctx, monitor))
	run := func(payload any) *httptest.ResponseRecorder {
		t.Helper()
		svc := service.ProvideChannelMonitorService(repository.NewChannelMonitorRepository(client, db), v1BudgetMonitorEncryptor{},
			service.NewSettingService(repository.NewSettingRepository(client), nil), nil, nil,
			repository.NewMonitoringProbeBudgetRepository(db))
		router := gin.New()
		router.PUT("/monitors/:id", admin.NewChannelMonitorHandler(svc).Update)
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/monitors/%d", monitor.ID), strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, req)
		return recorder
	}
	rejected := run(map[string]any{"enabled": true})
	require.Equal(t, http.StatusBadRequest, rejected.Code, rejected.Body.String())
	require.Contains(t, rejected.Body.String(), "MONITORING_PROBE_COVERAGE_IMPOSSIBLE")
	stored, err := monitors.GetByID(ctx, monitor.ID)
	require.NoError(t, err)
	require.False(t, stored.Enabled)
	policy.RequestsPerDay = 10000
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	repaired := run(map[string]any{"enabled": true})
	require.Equal(t, http.StatusOK, repaired.Code, repaired.Body.String())
	coverage, err := repository.NewMonitoringProbeBudgetRepository(db).PreviewCoverage(ctx, policy)
	require.NoError(t, err)
	require.EqualValues(t, 4320, coverage.MinimumTotalRequestsPerDay)
	rejected = run(map[string]any{"interval_seconds": 15})
	require.Equal(t, http.StatusBadRequest, rejected.Code, rejected.Body.String())
	stored, err = monitors.GetByID(ctx, monitor.ID)
	require.NoError(t, err)
	require.Equal(t, 60, stored.IntervalSeconds)
	require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorEnabled, "false"))
	repaired = run(map[string]any{"interval_seconds": 15})
	require.Equal(t, http.StatusOK, repaired.Code, repaired.Body.String())
	require.ErrorContains(t, settings.Set(ctx, service.SettingKeyChannelMonitorEnabled, "true"), "MONITORING_PROBE_COVERAGE_IMPOSSIBLE")
	require.ErrorContains(t, settings.SetMultiple(ctx, map[string]string{
		service.SettingKeyChannelMonitorEnabled: "true", "v1_capacity_atomic_fixture": "not-committed"}), "MONITORING_PROBE_COVERAGE_IMPOSSIBLE")
	require.ErrorContains(t, settings.Delete(ctx, service.SettingKeyChannelMonitorEnabled), "MONITORING_PROBE_COVERAGE_IMPOSSIBLE",
		"deleting the switch restores default-on and must recheck capacity")
	require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorMode, "v2"))
	require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorEnabled, "true"))
	require.ErrorContains(t, settings.Set(ctx, service.SettingKeyChannelMonitorMode, "v1"), "MONITORING_PROBE_COVERAGE_IMPOSSIBLE")
	runtime := service.NewSettingService(settings, nil).GetChannelMonitorRuntime(ctx)
	require.True(t, runtime.Enabled)
	require.Equal(t, "v2", runtime.Mode)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key='v1_capacity_atomic_fixture'`).Scan(&count))
	require.Zero(t, count)
	for _, table := range []string{"channel_monitor_histories", "monitoring_probe_budget_counters", "monitoring_probe_budget_leases"} {
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count, "capacity validation must not run probes or update observations")
	}
}

func TestMonitoringProbeCoverageCandyHTTPPostgresRecovery(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	var group int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,status) VALUES('candy-http','openai','active') RETURNING id`).Scan(&group))
	settings := repository.NewSettingRepository(client)
	require.NoError(t, settings.SetMultiple(ctx, map[string]string{service.SettingKeyChannelMonitorEnabled: "true", service.SettingKeyChannelMonitorMode: "v2"}))
	v2 := repository.NewChannelMonitorV2Repository(db)
	cfg, err := v2.GetConfig(ctx)
	require.NoError(t, err)
	cfg.Enabled = false
	cfg.Platforms = []service.ChannelMonitorV2PlatformConfig{{Platform: service.PlatformOpenAI, Enabled: true}}
	cfg.CandyProbes = []service.ChannelMonitorV2CandyProbe{{GroupID: group, Enabled: true, Model: "model-a", ReasoningEffort: "medium", IntervalMinutes: 1}}
	cfg, err = v2.UpdateConfig(ctx, *cfg, cfg.Version)
	require.NoError(t, err)
	budget := repository.NewMonitoringProbeBudgetRepository(db)
	policy := service.DefaultMonitoringProbeBudgetSettings()
	policy.Enabled, policy.RequestsPerDay, policy.RecoveryReservePercent = true, 1000, 0
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	newRouter := func() *gin.Engine {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
		})
		h := handler.NewChannelMonitorV2Handler(service.NewChannelMonitorV2Service(repository.NewChannelMonitorV2Repository(db)), nil)
		r.PUT("/config", h.UpdateConfig)
		r.GET("/config", h.GetConfig)
		m := handler.NewChannelMonitorV3Handler(nil, nil, service.NewSettingService(repository.NewSettingRepository(client), nil))
		r.PUT("/mode", m.SetMode)
		return r
	}
	request := func(method, path string, payload any) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		newRouter().ServeHTTP(w, req)
		return w
	}
	cfg.Enabled = true
	rejected := request(http.MethodPut, "/config", cfg)
	require.Equal(t, http.StatusBadRequest, rejected.Code, rejected.Body.String())
	require.Contains(t, rejected.Body.String(), "MONITORING_PROBE_COVERAGE_IMPOSSIBLE")
	read := request(http.MethodGet, "/config", nil)
	require.Equal(t, http.StatusOK, read.Code)
	var envelope struct {
		Data service.ChannelMonitorV2Config `json:"data"`
	}
	require.NoError(t, json.Unmarshal(read.Body.Bytes(), &envelope))
	require.Equal(t, cfg.Version, envelope.Data.Version)
	require.False(t, envelope.Data.Enabled)
	require.Equal(t, cfg.CandyProbes, envelope.Data.CandyProbes)
	cfg.CandyProbes[0].IntervalMinutes = 2
	repaired := request(http.MethodPut, "/config", cfg)
	require.Equal(t, http.StatusOK, repaired.Code, repaired.Body.String())
	require.Equal(t, http.StatusConflict, request(http.MethodPut, "/config", cfg).Code, "stale versions still conflict")
	require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorMode, "v1"))
	cfg, err = repository.NewChannelMonitorV2Repository(db).GetConfig(ctx)
	require.NoError(t, err)
	cfg.CandyProbes[0].IntervalMinutes = 1
	require.Equal(t, http.StatusOK, request(http.MethodPut, "/config", cfg).Code, "inactive V2 is editable")
	rejected = request(http.MethodPut, "/mode", map[string]string{"mode": "v2"})
	require.Equal(t, http.StatusBadRequest, rejected.Code, rejected.Body.String())
	require.Contains(t, rejected.Body.String(), "MONITORING_PROBE_COVERAGE_IMPOSSIBLE")
	runtime := service.NewSettingService(repository.NewSettingRepository(client), nil).GetChannelMonitorRuntime(ctx)
	require.Equal(t, "v1", runtime.Mode)
	require.NoError(t, settings.Set(ctx, service.SettingKeyChannelMonitorEnabled, "false"))
	require.Equal(t, http.StatusOK, request(http.MethodPut, "/mode", map[string]string{"mode": "v2"}).Code)
	for _, table := range []string{"channel_monitor_v2_candy_results", "monitoring_probe_budget_counters", "monitoring_probe_budget_leases"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count, "configuration and preview must not execute probes")
	}
}
