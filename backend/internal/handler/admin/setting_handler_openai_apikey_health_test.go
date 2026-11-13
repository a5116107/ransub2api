package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIAPIKeyHealthBreakerAdminValidationAndPersistence(t *testing.T) {
	repo := &pr612AdminSettingRepo{settingHandlerRepoStub{values: map[string]string{}}}
	h := NewSettingHandler(service.NewSettingService(repo, nil), nil, nil, nil, nil, nil, nil)
	call := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/openai-apikey-health-breaker", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdateOpenAIAPIKeyHealthBreakerSettings(c)
		return rec
	}
	for _, body := range []string{
		`{}`, `null`, `{"enabled":true}`, `{"enabled":null,"window_minutes":5,"failure_threshold":3,"cooldown_minutes":1}`,
		`{"enabled":true,"window_minutes":0,"failure_threshold":3,"cooldown_minutes":1}`,
		`{"enabled":true,"window_minutes":61,"failure_threshold":3,"cooldown_minutes":1}`,
		`{"enabled":true,"window_minutes":5,"failure_threshold":0,"cooldown_minutes":1}`,
		`{"enabled":true,"window_minutes":5,"failure_threshold":10001,"cooldown_minutes":1}`,
		`{"enabled":true,"window_minutes":5,"failure_threshold":3,"cooldown_minutes":0}`,
		`{"enabled":true,"window_minutes":5,"failure_threshold":3,"cooldown_minutes":61}`,
		`{"enabled":false,"window_minutes":5,"failure_threshold":1.5,"cooldown_minutes":1}`,
	} {
		require.Equal(t, http.StatusBadRequest, call(body).Code, body)
	}
	require.Empty(t, repo.values, "invalid requests must not persist")
	for _, body := range []string{
		`{"enabled":true,"window_minutes":5,"failure_threshold":3,"cooldown_minutes":1}`,
		`{"enabled":false,"window_minutes":5,"failure_threshold":3,"cooldown_minutes":1}`,
	} {
		rec := call(body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.JSONEq(t, body, repo.values[service.SettingKeyOpenAIAPIKeyHealthBreakerSettings])
		get := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(get)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		h.GetOpenAIAPIKeyHealthBreakerSettings(c)
		require.Equal(t, http.StatusOK, get.Code)
		require.Contains(t, get.Body.String(), `"window_minutes":5`)
	}
	// A fresh process must read the stored opt-out, not just the old cache.
	fresh, err := service.NewSettingService(repo, nil).GetOpenAIAPIKeyHealthBreakerSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, &service.OpenAIAPIKeyHealthBreakerSettings{Enabled: false, WindowMinutes: 5, FailureThreshold: 3, CooldownMinutes: 1}, fresh)
}
