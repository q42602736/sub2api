package service

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

type cachedOpenAIOverLimitSettings struct {
	enabled         bool
	cooldownSeconds int
	parallelEnabled bool
	expiresAt       int64
}

var openAIOverLimitSettingsCache atomic.Value // *cachedOpenAIOverLimitSettings
var openAIOverLimitSettingsSF singleflight.Group

const openAIOverLimitSettingsCacheTTL = 60 * time.Second
const openAIOverLimitSettingsErrorTTL = 5 * time.Second
const openAIOverLimitSettingsDBTimeout = 5 * time.Second

func normalizeOpenAIOverLimitCooldownSeconds(value int) int {
	if value < 1 {
		return 15
	}
	if value > 600 {
		return 600
	}
	return value
}

func (s *SettingService) GetOpenAIOverLimitModeSettings(ctx context.Context) (bool, int, bool, error) {
	nowUnix := time.Now().UnixNano()
	if cached, ok := openAIOverLimitSettingsCache.Load().(*cachedOpenAIOverLimitSettings); ok && cached != nil && cached.expiresAt > nowUnix {
		return cached.enabled, cached.cooldownSeconds, cached.parallelEnabled, nil
	}

	value, err, _ := openAIOverLimitSettingsSF.Do("openai_over_limit_settings", func() (any, error) {
		readCtx := context.Background()
		if ctx != nil {
			readCtx = context.WithoutCancel(ctx)
		}
		readCtx, cancel := context.WithTimeout(readCtx, openAIOverLimitSettingsDBTimeout)
		defer cancel()

		values, readErr := s.settingRepo.GetMultiple(readCtx, []string{
			SettingKeyOpenAIOverLimitModeEnabled,
			SettingKeyOpenAIOverLimitCooldownSeconds,
			SettingKeyOpenAIOverLimitParallelEnabled,
		})
		if readErr != nil {
			openAIOverLimitSettingsCache.Store(&cachedOpenAIOverLimitSettings{
				enabled:         false,
				cooldownSeconds: 15,
				parallelEnabled: false,
				expiresAt:       time.Now().Add(openAIOverLimitSettingsErrorTTL).UnixNano(),
			})
			return nil, readErr
		}

		cooldownSeconds, parseErr := strconv.Atoi(strings.TrimSpace(values[SettingKeyOpenAIOverLimitCooldownSeconds]))
		if parseErr != nil {
			cooldownSeconds = 15
		}
		resolved := &cachedOpenAIOverLimitSettings{
			enabled:         values[SettingKeyOpenAIOverLimitModeEnabled] == "true",
			cooldownSeconds: normalizeOpenAIOverLimitCooldownSeconds(cooldownSeconds),
			parallelEnabled: values[SettingKeyOpenAIOverLimitParallelEnabled] == "true",
			expiresAt:       time.Now().Add(openAIOverLimitSettingsCacheTTL).UnixNano(),
		}
		openAIOverLimitSettingsCache.Store(resolved)
		return resolved, nil
	})
	if err != nil {
		return false, 15, false, err
	}

	resolved, _ := value.(*cachedOpenAIOverLimitSettings)
	if resolved == nil {
		return false, 15, false, nil
	}
	return resolved.enabled, resolved.cooldownSeconds, resolved.parallelEnabled, nil
}
