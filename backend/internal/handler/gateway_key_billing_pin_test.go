//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type pinnedBillingRouter struct{}

func (pinnedBillingRouter) ProbeAPIKeyGroup(context.Context, *service.Group, string, string) (service.APIKeyGroupAvailability, error) {
	return service.APIKeyGroupAvailability{Available: true}, nil
}

func TestPinnedGroupBillingAndBusinessUseSameGroupOverHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	original := &service.Group{ID: 1, Platform: service.PlatformOpenAI, Status: service.StatusActive, RateMultiplier: 0.2}
	selected := &service.Group{ID: 2, Platform: service.PlatformOpenAI, Status: service.StatusActive, RateMultiplier: 0.8}
	key := &service.APIKey{ID: 19, UserID: 7, Key: "pinned-billing-fixture", Status: service.StatusActive,
		GroupID: &original.ID, GroupIDs: []int64{1, 2}, Group: original,
		User: &service.User{ID: 7, Role: service.RoleUser, Status: service.StatusActive, Balance: 10}}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	keys := service.NewAPIKeyService(&wsRevocationKeyRepo{initial: key}, nil, &fakeGroupRepo{group: selected}, nil, nil, nil, cfg)
	keys.SetGroupRouter(pinnedBillingRouter{})
	router := gin.New()
	router.Use(gin.HandlerFunc(middleware2.NewAPIKeyAuthMiddleware(keys, nil, cfg)))
	router.GET("/v1/sub2api/billing", newKeyBillingHandler(nil).KeyBillingInfo)
	router.POST("/v1/responses", func(c *gin.Context) {
		actual, ok := middleware2.GetAPIKeyFromContext(c)
		if !ok {
			c.Status(http.StatusUnauthorized)
			return
		}
		c.JSON(http.StatusOK, gin.H{"selected_group_id": *actual.GroupID, "effective_rate_multiplier": actual.Group.RateMultiplier})
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	for _, path := range []string{"/v1/sub2api/billing", "/v1/responses"} {
		method := http.MethodGet
		if path == "/v1/responses" {
			method = http.MethodPost
		}
		req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(`{"model":"gpt-fixture","input":"hello"}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+key.Key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(service.APIKeyGroupHeader, "2")
		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		var got struct {
			SelectedGroupID int64   `json:"selected_group_id"`
			Rate            float64 `json:"effective_rate_multiplier"`
		}
		err = json.NewDecoder(resp.Body).Decode(&got)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, int64(2), got.SelectedGroupID)
		require.Equal(t, 0.8, got.Rate)
	}
	require.Equal(t, int64(1), *key.GroupID)
	require.Equal(t, []int64{1, 2}, key.GroupIDs)
}
