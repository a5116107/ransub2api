package admin

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUpstreamCacheProbeRequestBoundary(t *testing.T) {
	valid := `{"model":"public-model","intervals_ms":[0,1000],"prefix_blocks":16,"timeout_seconds":10,"reasoning_effort":"","service_tier":""}`
	for _, tc := range []struct {
		name, id, body string
		status         int
	}{
		{"valid-service-unavailable", "7", valid, 503},
		{"negative-id", "-1", valid, 400},
		{"invalid-id", "invalid", valid, 400},
		{"empty-body", "7", "", 400},
		{"null", "7", "null", 400},
		{"null-field", "7", strings.Replace(valid, `"service_tier":""`, `"service_tier":null`, 1), 400},
		{"duplicate", "7", strings.Replace(valid, `"model":`, `"model":"other","model":`, 1), 400},
		{"alias", "7", strings.Replace(valid, `"model":`, `"Model":`, 1), 400},
		{"url", "7", strings.TrimSuffix(valid, "}") + `,"base_url":"https://untrusted.invalid"}`, 400},
		{"credential", "7", strings.TrimSuffix(valid, "}") + `,"api_key":"untrusted"}`, 400},
		{"prompt", "7", strings.TrimSuffix(valid, "}") + `,"prompt":"private"}`, 400},
		{"second-document", "7", valid + "{}", 400},
		{"oversized", "7", `{"model":"` + strings.Repeat("a", 8193) + `"}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.POST("/accounts/:id/cache-probe", (&AccountHandler{}).UpstreamCacheProbe)
			w := httptest.NewRecorder()
			request := httptest.NewRequest("POST", "/accounts/"+tc.id+"/cache-probe", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, request)
			require.Equal(t, tc.status, w.Code)
			require.NotContains(t, w.Body.String(), "untrusted")
			require.NotContains(t, w.Body.String(), "private")
		})
	}
}
