package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const openAIAPIKeyHealthBreakerSettingsCacheTTL = 30 * time.Second

type cachedOpenAIAPIKeyHealthBreakerSettings struct {
	settings  OpenAIAPIKeyHealthBreakerSettings
	expiresAt time.Time
}

func normalizeOpenAIAPIKeyHealthBreakerSettings(settings *OpenAIAPIKeyHealthBreakerSettings) *OpenAIAPIKeyHealthBreakerSettings {
	if settings == nil {
		return DefaultOpenAIAPIKeyHealthBreakerSettings()
	}
	result := *settings
	if result.WindowMinutes < 1 {
		result.WindowMinutes = 1
	} else if result.WindowMinutes > 60 {
		result.WindowMinutes = 60
	}
	if result.FailureThreshold < 1 {
		result.FailureThreshold = 1
	} else if result.FailureThreshold > 10000 {
		result.FailureThreshold = 10000
	}
	if result.CooldownMinutes < 1 {
		result.CooldownMinutes = 1
	} else if result.CooldownMinutes > 60 {
		result.CooldownMinutes = 60
	}
	return &result
}

func (s *SettingService) GetOpenAIAPIKeyHealthBreakerSettings(ctx context.Context) (*OpenAIAPIKeyHealthBreakerSettings, error) {
	if s == nil || s.settingRepo == nil {
		return DefaultOpenAIAPIKeyHealthBreakerSettings(), nil
	}
	if cached, ok := s.openAIAPIKeyHealthBreakerCache.Load().(*cachedOpenAIAPIKeyHealthBreakerSettings); ok && cached != nil && time.Now().Before(cached.expiresAt) {
		result := cached.settings
		return &result, nil
	}
	s.openAIAPIKeyHealthBreakerMu.Lock()
	defer s.openAIAPIKeyHealthBreakerMu.Unlock()
	if cached, ok := s.openAIAPIKeyHealthBreakerCache.Load().(*cachedOpenAIAPIKeyHealthBreakerSettings); ok && cached != nil && time.Now().Before(cached.expiresAt) {
		result := cached.settings
		return &result, nil
	}

	settings := DefaultOpenAIAPIKeyHealthBreakerSettings()
	dbCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	value, err := s.settingRepo.GetValue(dbCtx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return nil, fmt.Errorf("get OpenAI API key health breaker settings: %w", err)
	}
	if err == nil && strings.TrimSpace(value) != "" {
		var stored OpenAIAPIKeyHealthBreakerSettings
		if json.Unmarshal([]byte(value), &stored) == nil {
			settings = normalizeOpenAIAPIKeyHealthBreakerSettings(&stored)
		}
	}
	s.openAIAPIKeyHealthBreakerCache.Store(&cachedOpenAIAPIKeyHealthBreakerSettings{
		settings:  *settings,
		expiresAt: time.Now().Add(openAIAPIKeyHealthBreakerSettingsCacheTTL),
	})
	result := *settings
	return &result, nil
}

// SetOpenAIAPIKeyHealthBreakerSettings keeps the hot-path cache consistent with
// persisted admin settings. Other instances refresh within the existing TTL.
func (s *SettingService) SetOpenAIAPIKeyHealthBreakerSettings(ctx context.Context, settings *OpenAIAPIKeyHealthBreakerSettings) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("settings repository is required")
	}
	if settings == nil {
		return fmt.Errorf("settings are required")
	}
	if settings.WindowMinutes < 1 || settings.WindowMinutes > 60 {
		return fmt.Errorf("window_minutes must be between 1-60")
	}
	if settings.FailureThreshold < 1 || settings.FailureThreshold > 10000 {
		return fmt.Errorf("failure_threshold must be between 1-10000")
	}
	if settings.CooldownMinutes < 1 || settings.CooldownMinutes > 60 {
		return fmt.Errorf("cooldown_minutes must be between 1-60")
	}
	saved := *settings
	data, err := json.Marshal(saved)
	if err != nil {
		return fmt.Errorf("marshal OpenAI API key health breaker settings: %w", err)
	}
	s.openAIAPIKeyHealthBreakerMu.Lock()
	defer s.openAIAPIKeyHealthBreakerMu.Unlock()
	if err := s.settingRepo.Set(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings, string(data)); err != nil {
		return err
	}
	s.openAIAPIKeyHealthBreakerCache.Store(&cachedOpenAIAPIKeyHealthBreakerSettings{
		settings: saved, expiresAt: time.Now().Add(openAIAPIKeyHealthBreakerSettingsCacheTTL),
	})
	return nil
}
