package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetPriorityScheduling(c *gin.Context) {
	cfg, err := h.settingService.GetPrioritySchedulingConfig(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "Priority scheduling configuration unavailable")
		return
	}
	response.Success(c, cfg)
}
func (h *SettingHandler) SavePriorityScheduling(c *gin.Context) {
	defaults := service.DefaultPrioritySchedulingConfig()
	var request struct {
		service.PrioritySchedulingConfig
		TargetTPS                  *float64 `json:"target_tps"`
		ErrorRateWeight            *float64 `json:"error_rate_weight"`
		ErrorRateWindowMinutes     *int     `json:"error_rate_window_minutes"`
		ErrorRatePriorSamples      *int     `json:"error_rate_prior_samples"`
		OAuthQuotaPriority         *bool    `json:"oauth_quota_priority"`
		OAuthQuotaThreshold        *int     `json:"oauth_quota_threshold"`
		BalanceProtocols           *bool    `json:"balance_protocols"`
		PreserveSessionAffinity    *bool    `json:"preserve_session_affinity"`
		LatencyPenaltyMode         *string  `json:"latency_penalty_mode"`
		LatencyPenaltyMinutes      *int     `json:"latency_penalty_minutes"`
		LatencyPercentile          *int     `json:"latency_percentile"`
		LatencyMeasurement         *string  `json:"latency_measurement"`
		RecoveryExplorationPercent *int     `json:"recovery_exploration_percent"`
	}
	request.PrioritySchedulingConfig = defaults
	if c.ShouldBindJSON(&request) != nil {
		response.BadRequest(c, "Invalid scheduling configuration")
		return
	}
	cfg := request.PrioritySchedulingConfig
	preserve := make([]string, 0, 13)
	if request.TargetTPS != nil {
		cfg.TargetTPS = *request.TargetTPS
	} else {
		preserve = append(preserve, "target_tps")
	}
	if request.ErrorRateWeight != nil {
		cfg.ErrorRateWeight = *request.ErrorRateWeight
	} else {
		preserve = append(preserve, "error_rate_weight")
	}
	if request.ErrorRateWindowMinutes != nil {
		cfg.ErrorRateWindowMinutes = *request.ErrorRateWindowMinutes
	} else {
		preserve = append(preserve, "error_rate_window_minutes")
	}
	if request.ErrorRatePriorSamples != nil {
		cfg.ErrorRatePriorSamples = *request.ErrorRatePriorSamples
	} else {
		preserve = append(preserve, "error_rate_prior_samples")
	}
	if request.OAuthQuotaPriority != nil {
		cfg.OAuthQuotaPriority = *request.OAuthQuotaPriority
	} else {
		preserve = append(preserve, "oauth_quota_priority")
	}
	if request.OAuthQuotaThreshold != nil {
		cfg.OAuthQuotaThreshold = *request.OAuthQuotaThreshold
	} else {
		preserve = append(preserve, "oauth_quota_threshold")
	}
	if request.BalanceProtocols != nil {
		cfg.BalanceProtocols = *request.BalanceProtocols
	} else {
		preserve = append(preserve, "balance_protocols")
	}
	if request.PreserveSessionAffinity != nil {
		cfg.PreserveSessionAffinity = *request.PreserveSessionAffinity
	} else {
		preserve = append(preserve, "preserve_session_affinity")
	}
	if request.LatencyPenaltyMode != nil {
		if *request.LatencyPenaltyMode == "" {
			response.BadRequest(c, "Invalid latency penalty mode")
			return
		}
		cfg.LatencyPenaltyMode = *request.LatencyPenaltyMode
	} else {
		preserve = append(preserve, "latency_penalty_mode")
	}
	if request.LatencyPenaltyMinutes != nil {
		cfg.LatencyPenaltyMinutes = *request.LatencyPenaltyMinutes
	} else {
		preserve = append(preserve, "latency_penalty_minutes")
	}

	if request.LatencyPercentile != nil {
		cfg.LatencyPercentile = *request.LatencyPercentile
	} else {
		preserve = append(preserve, "latency_percentile")
	}
	if request.LatencyMeasurement != nil {
		cfg.LatencyMeasurement = *request.LatencyMeasurement
	} else {
		preserve = append(preserve, "latency_measurement")
	}
	if request.RecoveryExplorationPercent != nil {
		cfg.RecoveryExplorationPercent = *request.RecoveryExplorationPercent
	} else {
		preserve = append(preserve, "recovery_exploration_percent")
	}
	if err := service.ValidatePrioritySchedulingConfig(cfg); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if err := h.settingService.SavePrioritySchedulingConfig(c.Request.Context(), &cfg, preserve...); err != nil {
		if errors.Is(err, service.ErrPrioritySchedulingConfigConflict) {
			response.ErrorFrom(c, err)
			return
		}
		response.Error(c, http.StatusServiceUnavailable, "Could not save scheduling configuration")
		return
	}
	response.Success(c, cfg)
}
func (h *AccountHandler) PrioritySchedulingSnapshot(c *gin.Context) {
	response.Success(c, gin.H{"snapshot": h.openAIGatewayService.PrioritySchedulingSnapshot()})
}

func (h *AccountHandler) PrioritySchedulingAnalysis(c *gin.Context) {
	q := service.PriorityAnalysisQuery{Model: strings.TrimSpace(c.Query("model")), Minutes: 60}
	if raw, exists := c.GetQuery("minutes"); exists {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 5 || value > 1440 {
			response.BadRequest(c, "minutes must be between 5 and 1440")
			return
		}
		q.Minutes = value
	}
	if raw, exists := c.GetQuery("group_id"); exists {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			response.BadRequest(c, "group_id must be a positive integer")
			return
		}
		q.GroupID = &value
	}
	if q.Model == "" || len(q.Model) > 200 {
		response.BadRequest(c, "model is required (maximum 200 bytes)")
		return
	}
	ctx := c.Request.Context()
	if subject, ok := middleware.GetAuthSubjectFromContext(c); ok {
		role, _ := middleware.GetUserRoleFromContext(c)
		identity, _ := json.Marshal([]any{subject.UserID, role, c.GetString("auth_method"), adminaccess.FromContext(ctx)})
		ctx = service.WithPriorityAnalysisPrincipal(ctx, string(identity))
	}
	result, err := h.openAIGatewayService.PriorityAnalysis(ctx, q)
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "Priority analysis read failed")
		return
	}
	response.Success(c, result)
}

func (h *AccountHandler) PrioritySchedulingLatency(c *gin.Context) {
	minutes := 60
	if raw := strings.TrimSpace(c.Query("minutes")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 5 || value > 1440 {
			response.BadRequest(c, "minutes must be between 5 and 1440")
			return
		}
		minutes = value
	}
	bucketMinutes := 5
	if raw := strings.TrimSpace(c.Query("bucket_minutes")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 60 {
			response.BadRequest(c, "bucket_minutes must be between 1 and 60")
			return
		}
		bucketMinutes = value
	}
	series, err := h.openAIGatewayService.PriorityLatencySeries(c.Request.Context(), minutes, bucketMinutes)
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "Priority latency series unavailable")
		return
	}
	response.Success(c, series)
}

func (h *AccountHandler) RecomputePriorityScheduling(c *gin.Context) {
	result, err := h.openAIGatewayService.RecomputePriorityScheduling(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "Priority scheduling recompute unavailable")
		return
	}
	response.Success(c, result)
}

func (h *AccountHandler) PrioritySchedulingModels(c *gin.Context) {
	var group *int64
	if raw, exists := c.GetQuery("group_id"); exists {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			response.BadRequest(c, "group_id must be a positive integer")
			return
		}
		group = &value
	}
	models, err := h.openAIGatewayService.PriorityAnalysisModels(c.Request.Context(), group)
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "Priority model catalog read failed")
		return
	}
	response.Success(c, gin.H{"models": models})
}
