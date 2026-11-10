package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// RegionalAPIKeyAuth runs only identity/permission checks using a local replica.
// It performs no last-used writes, subscription refresh, Redis admission, billing
// or ops persistence. Serving still requires independent funds admission.
type regionalAPIKeyAuthenticator interface {
	GetRegionalByKey(context.Context, string) (*service.APIKey, error)
}

func RegionalAPIKeyAuth(svc regionalAPIKeyAuthenticator, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if apiKeyHeadersTooLarge(c) {
			AbortWithError(c, http.StatusUnauthorized, "INVALID_API_KEY", "Invalid API key")
			return
		}
		// Never accept credentials in URLs, including native Gemini preview routes.
		if c.Query("key") != "" || c.Query("api_key") != "" {
			AbortWithError(c, http.StatusBadRequest, "api_key_in_query_deprecated", "Use an API key header")
			return
		}
		var key string
		if parts := strings.SplitN(c.GetHeader("Authorization"), " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			key = strings.TrimSpace(parts[1])
		}
		if key == "" {
			key = c.GetHeader("x-api-key")
		}
		if key == "" {
			key = c.GetHeader("x-goog-api-key")
		}
		if key == "" || len(key) > service.MaxAPIKeyCredentialBytes {
			AbortWithError(c, http.StatusUnauthorized, "INVALID_API_KEY", "Invalid API key")
			return
		}
		auth, err := svc.GetRegionalByKey(c.Request.Context(), key)
		if err != nil {
			if errors.Is(err, service.ErrAPIKeyNotFound) {
				AbortWithError(c, http.StatusUnauthorized, "INVALID_API_KEY", "Invalid API key")
			} else {
				AbortWithError(c, http.StatusServiceUnavailable, "REGIONAL_AUTH_UNAVAILABLE", "Regional authorization unavailable")
			}
			return
		}
		if len(auth.ConfiguredGroupIDs()) > 1 {
			AbortWithError(c, http.StatusServiceUnavailable, "MULTI_GROUP_ROUTING_UNAVAILABLE", "Multi-group routing is unavailable on this regional gateway")
			return
		}
		requestedGroupID, err := service.ParseAPIKeyRequestGroup(c.Request.Header)
		if err != nil {
			_, _ = rejectAPIKeyRequestGroup(c, false, err)
			return
		}
		if requestedGroupID != 0 {
			if auth.GroupID == nil || *auth.GroupID != requestedGroupID {
				_, _ = rejectAPIKeyRequestGroup(c, false, service.ErrGroupNotAllowed)
				return
			}
			c.Set(pinnedAPIKeyGroupContextKey, requestedGroupID)
		}
		if len(auth.IPWhitelist) > 0 || len(auth.IPBlacklist) > 0 {
			allowed, _ := ip.CheckIPRestrictionWithCompiledRules(ip.GetSecurityClientIP(c, cfg.TrustForwardedIPForAPIKeyACL()), auth.CompiledIPWhitelist, auth.CompiledIPBlacklist)
			if !allowed {
				AbortWithError(c, http.StatusForbidden, "ACCESS_DENIED", "Access denied")
				return
			}
		}
		c.Set(string(ContextKeyAPIKey), auth)
		c.Set(string(ContextKeyUser), AuthSubject{UserID: auth.User.ID, Concurrency: auth.User.Concurrency})
		c.Set(string(ContextKeyUserRole), auth.User.Role)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.UserID, auth.User.ID))
		setGroupContext(c, auth.Group)
		nextWithAPIKeyAdmissionLookup(c, svc.GetRegionalByKey, key, ip.GetSecurityClientIP(c, cfg.TrustForwardedIPForAPIKeyACL()), auth, strings.HasPrefix(c.Request.URL.Path, "/v1beta/"))
	}
}

// RegionalVideoRequest classifies registered video routes for the authenticated
// group. Classification never authorizes spending: creation must reserve its
// immutable bound, and lookups require the original task's permanent funds.
func RegionalVideoRequest(c *gin.Context) (service.GrokMediaEndpoint, string) {
	if c == nil || c.Request == nil {
		return "", ""
	}
	key, ok := GetAPIKeyFromContext(c)
	if !ok || key == nil || key.Group == nil || key.GroupID == nil || *key.GroupID <= 0 || *key.GroupID != key.Group.ID {
		return "", ""
	}
	platform, path := key.Group.Platform, c.FullPath()
	videoPath := strings.TrimPrefix(path, "/v1")
	if c.Request.Method == http.MethodPost {
		switch videoPath {
		case "/videos", "/videos/generations":
			switch platform {
			case service.PlatformGrok, service.PlatformComposite:
				return service.GrokMediaEndpointVideosGenerations, ""
			case service.PlatformMiniMax:
				return service.MiniMaxH3EndpointCreate, ""
			}
		case "/videos/edits":
			if platform == service.PlatformGrok || platform == service.PlatformComposite {
				return service.GrokMediaEndpointVideosEdits, ""
			}
		case "/videos/extensions":
			if platform == service.PlatformGrok || platform == service.PlatformComposite {
				return service.GrokMediaEndpointVideosExtensions, ""
			}
		}
		if platform == service.PlatformOpenAI || platform == service.PlatformComposite {
			switch path {
			case "/contents/generations/tasks", "/v1/contents/generations/tasks", "/v3/contents/generations/tasks", "/api/v3/contents/generations/tasks":
				return service.SeedanceEndpointCreate, ""
			}
		}
		return "", ""
	}
	videoLookup := false
	switch videoPath {
	case "/videos/:request_id", "/videos/generations/:request_id", "/videos/edits/:request_id", "/videos/extensions/:request_id",
		"/videos/:request_id/content", "/videos/generations/:request_id/content", "/videos/edits/:request_id/content", "/videos/extensions/:request_id/content":
		videoLookup = true
	}
	if c.Request.Method == http.MethodGet && c.Param("request_id") != "" && videoLookup {
		switch platform {
		case service.PlatformGrok, service.PlatformComposite:
			if strings.HasSuffix(path, "/content") {
				return service.GrokMediaEndpointVideoContent, c.Param("request_id")
			}
			return service.GrokMediaEndpointVideoStatus, c.Param("request_id")
		case service.PlatformMiniMax:
			if strings.HasSuffix(path, "/content") {
				return service.MiniMaxH3EndpointContent, service.MiniMaxH3TaskKey(c.Param("request_id"))
			}
			return service.MiniMaxH3EndpointStatus, service.MiniMaxH3TaskKey(c.Param("request_id"))
		}
	}
	seedanceLookup := false
	switch path {
	case "/contents/generations/tasks/:task_id", "/v1/contents/generations/tasks/:task_id", "/v3/contents/generations/tasks/:task_id", "/api/v3/contents/generations/tasks/:task_id":
		seedanceLookup = true
	}
	if (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodDelete) && c.Param("task_id") != "" && seedanceLookup && (platform == service.PlatformOpenAI || platform == service.PlatformComposite) {
		if c.Request.Method == http.MethodDelete {
			return service.SeedanceEndpointDelete, service.SeedanceTaskKey(c.Param("task_id"))
		}
		return service.SeedanceEndpointStatus, service.SeedanceTaskKey(c.Param("task_id"))
	}
	return "", ""
}
