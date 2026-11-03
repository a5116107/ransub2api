//go:build unit

package handler

import (
	"errors"
	"fmt"
	"io"
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

func TestGrokSearchSharedKeyRefusalRemainsLocal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, xSearch := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			err    error
			status int
			kind   string
		}{
			{"capacity", service.ErrUpstreamKeyCapacity, http.StatusServiceUnavailable, "upstream_key_admission_unavailable"},
			{"policy", fmt.Errorf("wrapped: %w", service.ErrUpstreamKeyPolicy), http.StatusServiceUnavailable, "upstream_key_admission_unavailable"},
			{"route admission", &service.OpenAITurnAdmissionError{Reason: "account_binding_changed"}, http.StatusServiceUnavailable, "admission_unavailable"},
			{"transport", errors.New("fixture transport failure"), http.StatusBadGateway, "web_search_error"},
			{"supplier 503", nil, http.StatusBadGateway, "web_search_error"},
		} {
			t.Run(fmt.Sprintf("x_search=%v/%s", xSearch, tc.name), func(t *testing.T) {
				cfg := &config.Config{RunMode: config.RunModeSimple}
				group := &service.Group{ID: 24, Platform: service.PlatformGrok, Hydrated: true, Status: service.StatusActive}
				groups := &groupMapRepo{groups: map[int64]*service.Group{24: group}}
				repo := openAIImagesFailoverAccountRepo{accounts: []service.Account{{
					ID: 1, Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey, Status: service.StatusActive,
					Schedulable: true, Concurrency: 2, GroupIDs: []int64{24},
					Credentials: map[string]any{"api_key": "fixture-key"},
				}}}
				upstream := &grokMediaSlotUpstream{call: func(*http.Request, int64) (*http.Response, error) {
					if tc.err != nil {
						return nil, tc.err
					}
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(`{"error":"fixture supplier failure"}`))}, nil
				}}
				slots := &grokMediaSlotsCache{accounts: map[string]int64{}, users: map[string]int64{}}
				concurrency := service.NewConcurrencyService(slots)
				billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
				t.Cleanup(billing.Stop)
				gateway := service.NewGatewayService(repo, groups, nil, nil, nil, nil, nil, nil, cfg, nil, concurrency, nil, nil, billing,
					nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				h := &GatewayHandler{gatewayService: gateway, billingCacheService: billing,
					concurrencyHelper: NewConcurrencyHelper(concurrency, SSEPingFormatClaude, 0)}
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/web_search", strings.NewReader(`{"query":"fixture"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Set("grok_x_search_endpoint", xSearch)
				c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 20, UserID: 10, GroupID: &group.ID,
					Group: group, User: &service.User{ID: 10}})
				h.WebSearch(c)
				require.Equal(t, tc.status, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), tc.kind)
				require.Equal(t, 1, upstream.calls, "exhausted pool must not retry the same account")
				slots.assertReleased(t)
			})
		}
	}
}

func TestGrokSearchPreservesProfitGateUntilNativeSend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, xSearch := range []bool{false, true} {
		t.Run(fmt.Sprint(xSearch), func(t *testing.T) {
			cfg := &config.Config{RunMode: config.RunModeSimple}
			group := &service.Group{ID: 24, Platform: service.PlatformGrok, Hydrated: true, Status: service.StatusActive, RateMultiplier: 1, ProfitControlEnabled: true, ProfitMinMargin: 0.5}
			groups := &fakeGroupRepo{group: group}
			rate := 0.4
			account := &service.Account{ID: 1, Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 2, GroupIDs: []int64{24}, RateMultiplier: &rate, Credentials: map[string]any{"api_key": "fixture-key"}}
			snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account}}, nil, nil, groups, nil)
			upstream := &grokMediaSlotUpstream{call: func(*http.Request, int64) (*http.Response, error) {
				t.Error("missing final authority must not reach transport")
				return nil, nil
			}}
			slots := &grokMediaSlotsCache{accounts: map[string]int64{}, users: map[string]int64{}}
			concurrency := service.NewConcurrencyService(slots)
			billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			gateway := service.NewGatewayService(nil, groups, nil, nil, nil, nil, nil, nil, cfg, snapshot, concurrency, nil, nil, billing, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			h := &GatewayHandler{gatewayService: gateway, billingCacheService: billing, concurrencyHelper: NewConcurrencyHelper(concurrency, SSEPingFormatClaude, 0)}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/web_search", strings.NewReader(`{"query":"fixture"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set("grok_x_search_endpoint", xSearch)
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 20, UserID: 10, GroupID: &group.ID, Group: group, User: &service.User{ID: 10}})
			h.WebSearch(c)
			require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "admission_unavailable")
			require.Zero(t, upstream.calls)
			slots.assertReleased(t)
			_, exists := c.Get(service.OpsUpstreamErrorsKey)
			require.False(t, exists)
		})
	}
}
