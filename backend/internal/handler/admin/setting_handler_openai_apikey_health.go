package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetOpenAIAPIKeyHealthBreakerSettings(c *gin.Context) {
	settings, err := h.settingService.GetOpenAIAPIKeyHealthBreakerSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

func (h *SettingHandler) UpdateOpenAIAPIKeyHealthBreakerSettings(c *gin.Context) {
	var req struct {
		Enabled          *bool `json:"enabled"`
		WindowMinutes    *int  `json:"window_minutes"`
		FailureThreshold *int  `json:"failure_threshold"`
		CooldownMinutes  *int  `json:"cooldown_minutes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if req.Enabled == nil || req.WindowMinutes == nil || req.FailureThreshold == nil || req.CooldownMinutes == nil {
		response.BadRequest(c, "enabled, window_minutes, failure_threshold and cooldown_minutes are required")
		return
	}
	settings := service.OpenAIAPIKeyHealthBreakerSettings{
		Enabled: *req.Enabled, WindowMinutes: *req.WindowMinutes,
		FailureThreshold: *req.FailureThreshold, CooldownMinutes: *req.CooldownMinutes,
	}
	if err := h.settingService.SetOpenAIAPIKeyHealthBreakerSettings(c.Request.Context(), &settings); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, settings)
}
