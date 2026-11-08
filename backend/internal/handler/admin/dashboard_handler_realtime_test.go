package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDashboardRealtimeMetricsFailsClosedWithoutSource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/admin/dashboard/realtime", NewDashboardHandler(nil, nil).GetRealtimeMetrics)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/admin/dashboard/realtime", nil))

	require.Equal(t, http.StatusNotImplemented, recorder.Code)
	require.Contains(t, recorder.Body.String(), "Realtime dashboard metrics are not available")
}
