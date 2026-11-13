package routes

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGatewayRoutesLongContextBlocksBeforeHandler(t *testing.T) {
	for _, path := range []string{
		"/v1/responses", "/v1/responses/compact", "/responses", "/responses/compact",
		"/backend-api/codex/responses", "/backend-api/codex/responses/compact",
		"/v1/chat/completions", "/chat/completions", "/v1/messages", "/v1/systemone", "/antigravity/v1/messages",
	} {
		t.Run(path, func(t *testing.T) {
			router := newGatewayRoutesTestRouterWithGroup(&service.Group{Platform: service.PlatformOpenAI})
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-5.4","input":"hi","previous_response_id":"resp_fixture"}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), "context_length_exceeded")
		})
	}
}

func TestGatewayRoutesLongContextIncludesNativeGeminiAndAliases(t *testing.T) {
	source, err := os.ReadFile("gateway.go")
	require.NoError(t, err)
	for _, chain := range []string{"gateway", "gemini", "antigravityV1", "antigravityV1Beta"} {
		require.Contains(t, string(source), chain+".Use(apiKeyLongContext)", "missing admission on %s", chain)
	}
	owner := strings.Index(string(source), "gateway.Use(apiKeyLongContext)")
	pod := strings.Index(string(source), "gateway.Use(podRoute)")
	require.GreaterOrEqual(t, owner, 0)
	require.Greater(t, pod, owner, "owner must enforce admission before proxying, including through the regional funds gate")
	require.Contains(t, string(source), "gateway.Use(compositeTarget)\n\tgateway.Use(apiKeyLongContext)", "resolved model thresholds need a second admission")
	rootStart := strings.Index(string(source), "rootRoute := func(")
	require.GreaterOrEqual(t, rootStart, 0)
	root := string(source)[rootStart:]
	policy := strings.Index(root, "groupStreamOnly, apiKeyLongContext}")
	forward := strings.Index(root, "handlers = append(handlers, podRoute, compositeTarget, apiKeyLongContext,")
	require.GreaterOrEqual(t, policy, 0)
	require.Greater(t, forward, policy, "aliases must enforce admission before pod proxying and after model resolution")
	require.Contains(t, root, "r.Handle(method, path, handlers...)")
	for _, line := range strings.Split(string(source), "\n") {
		if strings.Contains(line, "codexDirect.Use(") {
			require.Contains(t, line, "apiKeyLongContext, podRoute", "aliases must enforce admission before pod proxying")
			require.Contains(t, line, "compositeTarget, apiKeyLongContext", "aliases must enforce resolved model thresholds")
		}
	}
}
