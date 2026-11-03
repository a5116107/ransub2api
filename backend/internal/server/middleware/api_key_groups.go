package middleware

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const pinnedAPIKeyGroupContextKey = "api_key.pinned_group_id"

func PinnedAPIKeyGroup(c *gin.Context) (int64, bool) {
	value, ok := c.Get(pinnedAPIKeyGroupContextKey)
	id, valid := value.(int64)
	return id, ok && valid && id > 0
}

func isMultiGroupBillingInfoRequest(c *gin.Context, key *service.APIKey) bool {
	_, pinned := PinnedAPIKeyGroup(c)
	return c.Request.URL.Path == "/v1/sub2api/billing" && len(key.ConfiguredGroupIDs()) > 1 && !pinned
}

func rejectAPIKeyRequestGroup(c *gin.Context, google bool, err error) (*service.APIKey, error) {
	status := infraerrors.Code(err)
	message, reason := infraerrors.Message(err), infraerrors.Reason(err)
	if status >= 500 {
		message = "No selected group is currently available for this request"
	}
	if google {
		abortWithGoogleError(c, status, message)
	} else {
		AbortWithError(c, status, reason, message)
	}
	return nil, err
}

func selectAPIKeyRequestGroup(c *gin.Context, keys *service.APIKeyService, subscriptions *service.SubscriptionService, cfg *config.Config, key *service.APIKey, google bool) (*service.APIKey, error) {
	var err error
	key, err = applySecurityPenalty(c, keys, key)
	if err != nil {
		if google {
			abortWithGoogleError(c, infraerrors.Code(err), infraerrors.Message(err))
		} else {
			AbortWithError(c, infraerrors.Code(err), infraerrors.Reason(err), infraerrors.Message(err))
		}
		return nil, err
	}
	if key == nil || c.IsAborted() {
		return nil, nil
	}
	requestedGroupID, err := service.ParseAPIKeyRequestGroup(c.Request.Header)
	if err != nil {
		return rejectAPIKeyRequestGroup(c, google, err)
	}
	if target, ok := c.Get("security_penalty.original_group_id"); ok && target != nil {
		if requestedGroupID != 0 {
			if key.GroupID == nil || *key.GroupID != requestedGroupID {
				return rejectAPIKeyRequestGroup(c, google, service.ErrGroupNotAllowed)
			}
			c.Set(pinnedAPIKeyGroupContextKey, requestedGroupID)
		}
		return key, nil
	}
	if requestedGroupID != 0 {
		if !slices.Contains(key.ConfiguredGroupIDs(), requestedGroupID) {
			return rejectAPIKeyRequestGroup(c, google, service.ErrGroupNotAllowed)
		}
		if c.Request.URL.Path == "/v1/usage" || isAsyncImageTaskRead(c.Request.Method, c.Request.URL.Path) {
			return rejectAPIKeyRequestGroup(c, google, infraerrors.BadRequest("API_KEY_GROUP_UNSUPPORTED", "Group selection is not supported for this resource"))
		}
		if c.Request.URL.Path == "/v1/sub2api/billing" || len(key.ConfiguredGroupIDs()) < 2 {
			selected, pinErr := keys.PinAPIKeyGroup(c.Request.Context(), key, requestedGroupID)
			if pinErr != nil {
				return rejectAPIKeyRequestGroup(c, google, pinErr)
			}
			c.Set(pinnedAPIKeyGroupContextKey, requestedGroupID)
			return selected, nil
		}
	}
	if len(key.ConfiguredGroupIDs()) < 2 || isMultiGroupBillingInfoRequest(c, key) || c.Request.URL.Path == "/v1/usage" || isAsyncImageTaskRead(c.Request.Method, c.Request.URL.Path) {
		return key, nil
	}
	var models []string
	var body []byte
	if model := groupModelAllowlistModelFromParams(c); model != "" {
		models = []string{model}
	}
	if c.Request.Method == http.MethodPost || c.Request.Method == http.MethodPut || c.Request.Method == http.MethodPatch {
		var ok bool
		body, ok = readAdmissionRequestBody(c)
		if !ok {
			return nil, nil
		}
		if len(models) == 0 {
			models = requestmodel.FromBodyCandidates(c.FullPath(), c.GetHeader("Content-Type"), body)
		}
	}
	if len(models) == 0 && c.Query("model") != "" {
		models = []string{c.Query("model")}
	}
	kind := "token"
	path := c.Request.URL.Path
	if strings.Contains(path, "/images/") {
		kind = "image"
	}
	if strings.Contains(path, "/videos") {
		kind = "video"
	}
	checkBilling := cfg.RunMode != config.RunModeSimple && c.Request.Method != http.MethodGet && !strings.HasSuffix(path, "/count_tokens")
	penaltyConfig, penaltyErr := keys.SecurityPenaltyConfig(c.Request.Context())
	if penaltyErr != nil {
		return nil, infraerrors.ServiceUnavailable("security_penalty_unavailable", "请求权限检查暂时不可用")
	}
	accepts := func(candidate *service.APIKey) bool {
		group := candidate.Group
		// Binding the restricted target must not make the normal cheapest-group
		// selector send healthy conversations there. Forced penalties returned
		// above already passed the normal target authorization checks.
		if penaltyConfig.Applies(key.UserID) && !penaltyConfig.ObserveOnly && penaltyConfig.Policy == "session_route_then_block" && penaltyConfig.TargetGroupID != nil && group.ID == *penaltyConfig.TargetGroupID {
			return false
		}
		if checkBilling && !group.IsSubscriptionType() && apiKeyBalanceBelowAuthThreshold(candidate.User.Balance, cfg) {
			return false
		}
		if google && group.Platform != service.PlatformGemini && group.Platform != service.PlatformAntigravity && group.Platform != service.PlatformComposite {
			return false
		}
		if strings.HasPrefix(path, "/antigravity/") && group.Platform != service.PlatformAntigravity {
			return false
		}
		if kind == "image" && !service.GroupAllowsImageGeneration(group) {
			return false
		}
		if group.StreamOnly && c.Request.Method == http.MethodPost {
			if streamOnlyJSONRoutes[c.FullPath()] && !jsonBodyRequestsStream(body) {
				return false
			}
			if streamOnlyGeminiRoutes[c.FullPath()] && strings.HasSuffix(c.Param("modelAction"), ":generateContent") {
				return false
			}
		}
		if group.BlockSubagents && service.IsCodexSubagentRequest(c, body) {
			return false
		}
		if group.CodexCLIOnly {
			policy := service.CodexRestrictionPolicy{EngineFingerprintSignals: openai.DefaultEngineFingerprintSignals}
			if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
				policy.EngineFingerprintSignals = nil
			}
			if !service.DetectCodexGroupClient(c, group, policy, body).Matched {
				return false
			}
		}
		return true
	}
	ctx := context.WithValue(c.Request.Context(), ctxkey.UserID, key.UserID)
	selected, err := keys.SelectRequestGroup(ctx, key, models, kind, subscriptions, accepts, checkBilling, requestedGroupID)
	if err != nil {
		return rejectAPIKeyRequestGroup(c, google, err)
	}
	if requestedGroupID != 0 {
		c.Set(pinnedAPIKeyGroupContextKey, requestedGroupID)
	}
	SetOpsFallbackAPIKey(c, selected)
	return selected, nil
}
