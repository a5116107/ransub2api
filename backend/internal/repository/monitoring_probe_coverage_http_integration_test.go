//go:build integration

package repository_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

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
