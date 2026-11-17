package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type handlerMonitoringBudgetRepo struct {
	service.MonitoringProbeBudgetRepository
	settings service.MonitoringProbeBudgetSettings
}

func (r *handlerMonitoringBudgetRepo) Summary(context.Context) (*service.MonitoringProbeBudgetSummary, error) {
	return &service.MonitoringProbeBudgetSummary{Settings: r.settings, MinuteUsed: 7}, nil
}
func (r *handlerMonitoringBudgetRepo) SaveSettings(_ context.Context, s service.MonitoringProbeBudgetSettings) (service.MonitoringProbeBudgetSettings, error) {
	if s.Revision != r.settings.Revision {
		return s, service.ErrMonitoringProbeBudgetConflict
	}
	s.Revision++
	r.settings = s
	return s, nil
}

func TestAccountHandlerMonitoringProbeBudgetPolicyAndConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &AccountHandler{}
	probe := service.NewUpstreamBillingProbeService(nil, nil, nil)
	probe.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(&handlerMonitoringBudgetRepo{settings: service.DefaultMonitoringProbeBudgetSettings()}))
	handler.SetUpstreamBillingProbeService(probe)
	router := gin.New()
	router.GET("/budget", handler.GetMonitoringProbeBudget)
	router.PUT("/budget", handler.UpdateMonitoringProbeBudget)
	read := httptest.NewRecorder()
	router.ServeHTTP(read, httptest.NewRequest("GET", "/budget", nil))
	require.Equal(t, 200, read.Code)
	require.Contains(t, read.Body.String(), `"minute_used":7`)
	body, err := json.Marshal(service.DefaultMonitoringProbeBudgetSettings())
	require.NoError(t, err)
	for _, status := range []int{200, 409} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/budget", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, req)
		require.Equal(t, status, recorder.Code)
	}
	bad := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/budget", bytes.NewBufferString(`{"requests_per_minute":0}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(bad, req)
	require.Equal(t, 400, bad.Code)
	read = httptest.NewRecorder()
	router.ServeHTTP(read, httptest.NewRequest("GET", "/budget", nil))
	require.Contains(t, read.Body.String(), `"revision":1`)
	handler.SetUpstreamBillingProbeService(nil)
	read = httptest.NewRecorder()
	router.ServeHTTP(read, httptest.NewRequest("GET", "/budget", nil))
	require.Equal(t, 503, read.Code)
}

func setupUpstreamBillingProbeRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	handler := NewAccountHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	handler.SetUpstreamBillingProbeService(service.NewUpstreamBillingProbeService(nil, nil, nil))

	router := gin.New()
	router.GET("/admin/accounts/upstream-billing-probe/settings", handler.GetUpstreamBillingProbeSettings)
	router.POST("/admin/accounts/upstream-billing-probe/batch", handler.ProbeUpstreamBillingBatch)
	router.PUT("/admin/accounts/:id/upstream-billing-probe", handler.SetUpstreamBillingProbeEnabled)
	return router
}

func TestAccountHandlerGetUpstreamBillingProbeSettingsReturnsDefaults(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/accounts/upstream-billing-probe/settings", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Data service.UpstreamBillingProbeSettings `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Data.Enabled)
	require.Equal(t, 30, response.Data.IntervalMinutes)
}

func TestAccountHandlerProbeUpstreamBillingBatchValidatesIDs(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()

	for _, body := range []string{`{"account_ids":[]}`, `{"account_ids":[0]}`} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/admin/accounts/upstream-billing-probe/batch", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
	}
}

func TestAccountHandlerSetUpstreamBillingProbeEnabledRejectsInvalidID(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/admin/accounts/not-an-id/upstream-billing-probe", bytes.NewBufferString(`{"enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestAccountHandlerSetUpstreamBillingProbeEnabledRequiresValue(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/admin/accounts/1/upstream-billing-probe", bytes.NewBufferString(`{}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}
