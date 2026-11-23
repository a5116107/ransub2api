package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type handlerMonitoringBudgetRepo struct {
	service.MonitoringProbeBudgetRepository
	settings service.MonitoringProbeBudgetSettings
}

type scheduledCoverageDeniedRepo struct{ reusePlanRepo }

func (r *scheduledCoverageDeniedRepo) Create(context.Context, *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	return nil, (&service.MonitoringProbeCoverage{Accounts: 999991, MinimumTotalRequestsPerDay: 777773}).ValidateSave()
}
func (r *scheduledCoverageDeniedRepo) Update(ctx context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	return r.Create(ctx, plan)
}

func TestScheduledMonitoringCoverageErrorKeepsReasonAndScope(t *testing.T) {
	for _, observer := range []bool{false, true} {
		for _, method := range []string{http.MethodPost, http.MethodPut} {
			repo := &scheduledCoverageDeniedRepo{reusePlanRepo{plan: &service.ScheduledTestPlan{ID: 7, AccountID: 42, ModelID: "test", CronExpression: "0 * * * *"}}}
			h := NewScheduledTestHandler(service.NewScheduledTestService(repo, nil))
			router := gin.New()
			if observer {
				router.Use(func(c *gin.Context) {
					c.Request = c.Request.WithContext(service.WithObserverScope(c.Request.Context(), []int64{9}))
					c.Next()
				})
			}
			router.POST("/plans", h.Create)
			router.PUT("/plans/:id", h.Update)
			path := "/plans"
			if method == http.MethodPut {
				path += "/7"
			}
			req := httptest.NewRequest(method, path, strings.NewReader(`{"account_id":42,"model_id":"test","cron_expression":"0 * * * *","enabled":false}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusBadRequest, w.Code)
			var body struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.Equal(t, "MONITORING_PROBE_COVERAGE_IMPOSSIBLE", body.Reason)
			require.Contains(t, body.Message, "999991")
			require.Contains(t, body.Message, "777773")
		}
	}
}

func (r *handlerMonitoringBudgetRepo) Summary(ctx context.Context) (*service.MonitoringProbeBudgetSummary, error) {
	coverage, err := r.PreviewCoverage(ctx, r.settings)
	return &service.MonitoringProbeBudgetSummary{Settings: r.settings, MinuteUsed: 7, Coverage: coverage}, err
}

func (r *handlerMonitoringBudgetRepo) PreviewCoverage(_ context.Context, s service.MonitoringProbeBudgetSettings, _ ...*service.MonitoringProbePlanDraft) (*service.MonitoringProbeCoverage, error) {
	if s.Revision != r.settings.Revision {
		return nil, service.ErrMonitoringProbeBudgetConflict
	}
	return service.CompileMonitoringProbeCoverage(s, service.UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 30}, nil, true, nil, true, nil, true, nil, true, time.Now()), nil
}

func TestAccountHandlerMonitoringProbeCoverage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	probe := service.NewUpstreamBillingProbeService(nil, nil, nil)
	repo := &handlerMonitoringBudgetRepo{settings: service.DefaultMonitoringProbeBudgetSettings()}
	probe.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(repo))
	ollama := service.NewOllamaCloudUsageService(nil, nil, nil, nil, false)
	t.Cleanup(ollama.Stop)
	probe.MonitoringBudget().SetOllamaUsage(ollama)
	tests := service.NewAccountTestService(nil, nil, nil, nil, nil, nil, nil, nil)
	probe.MonitoringBudget().SetResponsesCapability(tests)
	h := &AccountHandler{}
	h.SetUpstreamBillingProbeService(probe)
	router := gin.New()
	router.POST("/preview", h.PreviewMonitoringProbeCoverage)
	router.GET("/budget", h.GetMonitoringProbeBudget)
	raw, err := json.Marshal(repo.settings)
	require.NoError(t, err)
	for _, tc := range []struct {
		body   []byte
		status int
	}{
		{[]byte(`{`), 400}, {[]byte(`{"requests_per_minute":0}`), 400}, {raw, 200},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/preview", bytes.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		require.Equal(t, tc.status, w.Code)
		if tc.status == 200 {
			require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			require.Contains(t, w.Body.String(), `"inventory_complete":true`)
			require.Contains(t, w.Body.String(), `"ollama_usage":{`)
			require.Contains(t, w.Body.String(), ollama.MonitoringRuntime().InstanceID)
			require.Contains(t, w.Body.String(), `"last_scan":null`)
			require.Contains(t, w.Body.String(), `"responses_capability":{`)
			require.Contains(t, w.Body.String(), tests.ResponsesCapabilityRuntime().InstanceID)
			require.Contains(t, w.Body.String(), `"last_started_at":null`)
			require.Contains(t, w.Body.String(), `"started_events":0`)
		}
	}
	wGet := httptest.NewRecorder()
	router.ServeHTTP(wGet, httptest.NewRequest("GET", "/budget", nil))
	require.Equal(t, 200, wGet.Code)
	require.Equal(t, "no-store", wGet.Header().Get("Cache-Control"))
	var queueView struct {
		Data struct {
			Coverage struct {
				Queues *service.MonitoringProbeGroupQueues `json:"group_probe_queues"`
			} `json:"coverage"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(wGet.Body.Bytes(), &queueView))
	require.NotNil(t, queueView.Data.Coverage.Queues)
	require.Equal(t, "unavailable", queueView.Data.Coverage.Queues.V2.State)
	require.Equal(t, "unavailable", queueView.Data.Coverage.Queues.V3.State)
	require.Nil(t, queueView.Data.Coverage.Queues.V2.Cycle)
	require.Nil(t, queueView.Data.Coverage.Queues.V3.LastScan)
	require.Contains(t, wGet.Body.String(), ollama.MonitoringRuntime().InstanceID)
	require.Contains(t, wGet.Body.String(), `"last_scan":null`)
	require.Contains(t, wGet.Body.String(), tests.ResponsesCapabilityRuntime().InstanceID)
	require.Contains(t, wGet.Body.String(), `"last_finished_at":null`)
	require.EqualValues(t, 0, repo.settings.Revision, "preview is not a settings write")
	h.SetUpstreamBillingProbeService(nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/preview", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	require.Equal(t, 503, w.Code)
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
