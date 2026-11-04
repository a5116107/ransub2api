//go:build unit

package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type availabilityReportRepo struct {
	service.ScheduledTestPlanRepository
	err   error
	reads int
}

func (r *availabilityReportRepo) ReadAvailability(context.Context, int64) (*service.AvailabilityReport, error) {
	r.reads++
	return &service.AvailabilityReport{PlanID: 7, AccountID: 42, Configured: true, Enabled: true, Models: []service.AvailabilityModelState{{ModelID: "target", Status: "unknown", Reason: "route_changed"}}}, r.err
}

func (r *availabilityReportRepo) GetByID(context.Context, int64) (*service.ScheduledTestPlan, error) {
	return nil, errors.New("outside-observer-scope")
}

func TestAvailabilityHandlerReportsAndSanitizesFailures(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		err      error
		status   int
	}{
		{"report", "7", nil, http.StatusOK},
		{"bad_id", "invalid", nil, http.StatusBadRequest},
		{"negative_id", "-1", nil, http.StatusBadRequest},
		{"missing", "7", sql.ErrNoRows, http.StatusNotFound},
		{"database_error", "7", errors.New("fixture-private-database-secret"), http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &availabilityReportRepo{err: tc.err}
			h := NewScheduledTestHandler(service.NewScheduledTestService(repo, nil))
			router := gin.New()
			router.GET("/plans/:id/availability", h.Availability)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/plans/"+tc.id+"/availability", nil))
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			require.NotContains(t, w.Body.String(), "fixture-private-database-secret")
			if tc.status == http.StatusOK {
				require.Contains(t, w.Body.String(), `"status":"unknown"`)
				require.Contains(t, w.Body.String(), `"reason":"route_changed"`)
			}
		})
	}
}

func TestAvailabilityHandlerObserverGuardPreventsStateRead(t *testing.T) {
	repo := &availabilityReportRepo{}
	h := NewScheduledTestHandler(service.NewScheduledTestService(repo, nil))
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(service.WithObserverScope(c.Request.Context(), []int64{}))
	})
	router.GET("/plans/:id/availability", h.ObserverGuard(nil), h.Availability)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/plans/7/availability", nil))
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Zero(t, repo.reads)
}
