package routes

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestChannelOperationsRoutesRequireExistingAdministratorAuthentication(t *testing.T) {
	router := gin.New()
	group := router.Group("/api/v1/admin")
	group.Use(gin.HandlerFunc(middleware.NewAdminAuthMiddleware(nil, nil, nil, nil)))
	// Exercise actual route registration (including static/wildcard coexistence).
	registerChannelRoutes(group, &handler.Handlers{Admin: &handler.AdminHandlers{Channel: admin.NewChannelHandler(nil, nil, nil)}})
	for _, tc := range []struct{ method, path string }{
		{"GET", "/upstream-operations/snapshot"}, {"GET", "/channels/7/operations"}, {"POST", "/channels/7/operations"},
		{"GET", "/channels/operations/snapshot"}, {"POST", "/channels"},
		{"PUT", "/channels/7"}, {"DELETE", "/channels/7"},
		{"GET", "/upstream-procurement/catalog"}, {"PUT", "/upstream-procurement/catalog"}, {"POST", "/upstream-procurement/preview"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(tc.method, "/api/v1/admin"+tc.path, nil))
			require.Equal(t, 401, w.Code)
		})
	}
}

func TestChannelOperationsRoutesDenyScopedObserverManagement(t *testing.T) {
	router := gin.New()
	group := router.Group("/api/v1/admin")
	group.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(service.WithObserverScope(c.Request.Context(), []int64{11}))
	})
	group.Use((&admin.AccountHandler{}).AuthorizeObserver)
	registerChannelRoutes(group, &handler.Handlers{Admin: &handler.AdminHandlers{Channel: admin.NewChannelHandler(nil, nil, nil)}})
	for _, tc := range []struct{ method, path string }{{"GET", "/upstream-operations/snapshot"}, {"GET", "/channels/7/operations"}, {"POST", "/channels/7/operations"}, {"GET", "/channels/operations/snapshot"}, {"GET", "/upstream-procurement/catalog"}, {"PUT", "/upstream-procurement/catalog"}, {"POST", "/upstream-procurement/preview"}} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(tc.method, "/api/v1/admin"+tc.path, nil))
		require.Equal(t, 403, w.Code)
	}
}

func TestUpstreamCacheProbeRoutesRequireAdministrator(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		router := gin.New()
		group := router.Group("/api/v1/admin")
		if scoped {
			group.Use(func(c *gin.Context) {
				c.Request = c.Request.WithContext(service.WithObserverScope(c.Request.Context(), []int64{11}))
			})
			group.Use((&admin.AccountHandler{}).AuthorizeObserver)
		} else {
			group.Use(gin.HandlerFunc(middleware.NewAdminAuthMiddleware(nil, nil, nil, nil)))
		}
		registerAccountRoutes(group, &handler.Handlers{Admin: &handler.AdminHandlers{Account: &admin.AccountHandler{}}}, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/admin/accounts/7/cache-probe", nil))
		status := 401
		if scoped {
			status = 403
		}
		require.Equal(t, status, w.Code)
	}
}

type upstreamProbeRoleUsers struct {
	service.UserRepository
	user *service.User
}

func (r upstreamProbeRoleUsers) GetByID(context.Context, int64) (*service.User, error) {
	return r.user, nil
}

func (upstreamProbeRoleUsers) GetUserAvatar(context.Context, int64) (*service.UserAvatar, error) {
	return nil, nil
}

func TestUpstreamCacheProbeRegisteredRoleBoundary(t *testing.T) {
	for _, role := range []string{service.RoleAdmin, service.RoleUser, service.RoleObserver} {
		t.Run(role, func(t *testing.T) {
			user := &service.User{ID: 9, Role: role, Status: service.StatusActive, TokenVersion: 2}
			cfg := &config.Config{JWT: config.JWTConfig{Secret: "role-boundary-fixture", ExpireHour: 1}}
			auth := service.NewAuthService(nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
			users := service.NewUserService(upstreamProbeRoleUsers{user: user}, nil, nil, nil)
			token, err := auth.GenerateToken(t.Context(), user)
			require.NoError(t, err)
			router := gin.New()
			group := router.Group("/api/v1/admin")
			group.Use(gin.HandlerFunc(middleware.NewAdminAuthMiddleware(auth, users, nil, nil)))
			registerAccountRoutes(group, &handler.Handlers{Admin: &handler.AdminHandlers{Account: &admin.AccountHandler{}}}, nil)
			request := httptest.NewRequest("POST", "/api/v1/admin/accounts/7/cache-probe", strings.NewReader(`{"model":"public-model","intervals_ms":[0],"prefix_blocks":16,"timeout_seconds":10}`))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, request)
			status := 403
			if role == service.RoleAdmin {
				status = 503 // Authenticated admin reaches the deliberately missing service.
			}
			require.Equal(t, status, w.Code)
		})
	}
}
