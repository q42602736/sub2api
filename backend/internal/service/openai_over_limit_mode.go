package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

type openAIOverLimitModeSettings struct {
	enabled         bool
	cooldown        time.Duration
	parallelEnabled bool
}

func (s *OpenAIGatewayService) getOpenAIOverLimitModeSettings(ctx context.Context) openAIOverLimitModeSettings {
	if s == nil || s.settingService == nil {
		return openAIOverLimitModeSettings{}
	}
	enabled, cooldownSeconds, parallelEnabled, err := s.settingService.GetOpenAIOverLimitModeSettings(ctx)
	if err != nil {
		slog.Warn("load openai over-limit settings failed", "error", err)
		return openAIOverLimitModeSettings{}
	}
	return openAIOverLimitModeSettings{
		enabled:         enabled,
		cooldown:        time.Duration(cooldownSeconds) * time.Second,
		parallelEnabled: parallelEnabled,
	}
}

func normalizeOpenAIOverLimitCooldownModel(requestedModel string) string {
	trimmed := strings.TrimSpace(requestedModel)
	if trimmed == "" {
		return "*"
	}
	return NormalizeOpenAICompatRequestedModel(trimmed)
}

func openAIOverLimitCooldownKey(accountID int64, requestedModel string) string {
	return fmt.Sprintf("%d:%s", accountID, normalizeOpenAIOverLimitCooldownModel(requestedModel))
}

func openAIRequestedModelFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	model, _ := ctx.Value(ctxkey.Model).(string)
	return strings.TrimSpace(model)
}

func (s *OpenAIGatewayService) IsOpenAIOverLimitModeEnabled(ctx context.Context) bool {
	return s.getOpenAIOverLimitModeSettings(ctx).enabled
}

func (s *OpenAIGatewayService) ShouldPreserveOpenAIStickyBindingAfterFailover(ctx context.Context, failedAccountIDs map[int64]struct{}) bool {
	return len(failedAccountIDs) > 0 && s.IsOpenAIOverLimitModeEnabled(ctx)
}

func (s *OpenAIGatewayService) shouldPreserveOpenAIStickyBindingDuringOverLimitCooldown(ctx context.Context, accountID int64, requestedModel string) bool {
	if accountID <= 0 {
		return false
	}
	settings := s.getOpenAIOverLimitModeSettings(ctx)
	return settings.enabled && s.isOpenAIAccountInOverLimitCooldown(accountID, requestedModel)
}

func (s *OpenAIGatewayService) shouldSkipOpenAIStickyHitDuringOverLimitCooldown(ctx context.Context, account *Account, requestedModel string) bool {
	return account != nil &&
		account.IsOpenAI() &&
		s.shouldPreserveOpenAIStickyBindingDuringOverLimitCooldown(ctx, account.ID, requestedModel)
}

func (s *OpenAIGatewayService) markOpenAIOverLimitCooldown(accountID int64, requestedModel string, cooldown time.Duration) {
	if s == nil || accountID <= 0 || cooldown <= 0 {
		return
	}
	s.openAIOverLimitCooldownUntil.Store(openAIOverLimitCooldownKey(accountID, requestedModel), time.Now().Add(cooldown))
}

func (s *OpenAIGatewayService) isOpenAIAccountInOverLimitCooldown(accountID int64, requestedModel string) bool {
	if s == nil || accountID <= 0 {
		return false
	}
	key := openAIOverLimitCooldownKey(accountID, requestedModel)
	value, ok := s.openAIOverLimitCooldownUntil.Load(key)
	if !ok {
		return false
	}
	until, ok := value.(time.Time)
	if !ok {
		s.openAIOverLimitCooldownUntil.Delete(key)
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	s.openAIOverLimitCooldownUntil.Delete(key)
	return false
}

func (s *OpenAIGatewayService) shouldClearOpenAIStickySession(ctx context.Context, account *Account, requestedModel string) bool {
	if !shouldClearStickySession(account, requestedModel) {
		return false
	}
	settings := s.getOpenAIOverLimitModeSettings(ctx)
	if !settings.enabled || account == nil || !account.IsOpenAI() {
		return true
	}
	now := time.Now()
	if account.Status != StatusError &&
		account.Status != StatusDisabled &&
		account.Schedulable &&
		(account.TempUnschedulableUntil == nil || !now.Before(*account.TempUnschedulableUntil)) &&
		account.RateLimitResetAt != nil &&
		now.Before(*account.RateLimitResetAt) &&
		!s.isOpenAIAccountInOverLimitCooldown(account.ID, requestedModel) {
		return false
	}
	return true
}

func (s *OpenAIGatewayService) isOpenAIAccountSelectable(account *Account, requestedModel string, settings openAIOverLimitModeSettings) bool {
	if account == nil || !account.IsOpenAI() {
		return false
	}
	if !account.IsActive() || !account.Schedulable {
		return false
	}
	now := time.Now()
	if account.AutoPauseOnExpired && account.ExpiresAt != nil && !now.Before(*account.ExpiresAt) {
		return false
	}
	if account.OverloadUntil != nil && now.Before(*account.OverloadUntil) {
		return false
	}
	if account.TempUnschedulableUntil != nil && now.Before(*account.TempUnschedulableUntil) {
		return false
	}
	if account.RateLimitResetAt != nil && now.Before(*account.RateLimitResetAt) {
		if !settings.enabled || s.isOpenAIAccountInOverLimitCooldown(account.ID, requestedModel) {
			return false
		}
	}
	if remaining := account.GetRateLimitRemainingTimeWithContext(context.Background(), requestedModel); remaining > 0 {
		return false
	}
	if account.IsAPIKeyOrBedrock() && account.IsQuotaExceeded() {
		return false
	}
	return true
}

func isUngroupedOpenAIAccount(account *Account) bool {
	if account == nil {
		return false
	}
	return len(account.GroupIDs) == 0 && len(account.Groups) == 0 && len(account.AccountGroups) == 0
}
