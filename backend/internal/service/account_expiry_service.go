package service

import (
	"context"
	"log"
	"sync"
	"time"
)

// grokRateLimitRecoverySweeper 只清理已到期的 Grok OAuth 限流记录。
// 保持为可选接口，避免把后台恢复能力扩散到所有账号仓储实现。
type grokRateLimitRecoverySweeper interface {
	RecoverExpiredGrokRateLimits(ctx context.Context, now time.Time) (int64, error)
}

// AccountExpiryService periodically pauses expired accounts when auto-pause is enabled.
type AccountExpiryService struct {
	accountRepo AccountRepository
	interval    time.Duration
	stopCh      chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
}

func NewAccountExpiryService(accountRepo AccountRepository, interval time.Duration) *AccountExpiryService {
	return &AccountExpiryService{
		accountRepo: accountRepo,
		interval:    interval,
		stopCh:      make(chan struct{}),
	}
}

func (s *AccountExpiryService) Start() {
	if s == nil || s.accountRepo == nil || s.interval <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		s.runOnce()
		for {
			select {
			case <-ticker.C:
				s.runOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *AccountExpiryService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *AccountExpiryService) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	now := time.Now()
	updated, err := s.accountRepo.AutoPauseExpiredAccounts(ctx, now)
	if err != nil {
		log.Printf("[AccountExpiry] Auto pause expired accounts failed: %v", err)
	} else if updated > 0 {
		log.Printf("[AccountExpiry] Auto paused %d expired accounts", updated)
	}

	recoveryRepo, ok := s.accountRepo.(grokRateLimitRecoverySweeper)
	if !ok {
		return
	}
	recovered, err := recoveryRepo.RecoverExpiredGrokRateLimits(ctx, now)
	if err != nil {
		log.Printf("[GrokQuotaRecovery] Recover expired rate limits failed: %v", err)
		return
	}
	if recovered > 0 {
		log.Printf("[GrokQuotaRecovery] Recovered %d expired Grok OAuth rate limits without active model probes", recovered)
	}
}
