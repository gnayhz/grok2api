package quotarecovery

import (
	"context"
	"log/slog"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/port/lifecycle"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	defaultRecoveryWorkers = 25
	recoveryClaimLease     = 2 * time.Minute
	recoveryProbeTimeout   = 30 * time.Second
	recoveryReconcileEvery = time.Minute
	recoveryReconcileLimit = 1000
)

type quotaSynchronizer interface {
	ProbeQuotaMode(ctx context.Context, accountID uint64, mode string) (accountdomain.QuotaWindow, error)
	ListDueQuotaWindows(ctx context.Context, now time.Time, limit int, after *repository.QuotaWindowCursor) ([]accountdomain.QuotaWindow, error)
}

type Service struct {
	logger   *slog.Logger
	queue    repository.QuotaRecoveryQueue
	syncer   quotaSynchronizer
	mu       sync.RWMutex
	base     time.Duration
	max      time.Duration
	bulkPool *batch.Pool
	// Run owns the scan cursor. Advance over every inspected candidate so
	// non-routing windows and still-exhausted accounts cannot starve the tail.
	reconcileAfter *repository.QuotaWindowCursor
}

func NewService(logger *slog.Logger, queue repository.QuotaRecoveryQueue, syncer quotaSynchronizer, base, max time.Duration) *Service {
	return &Service{logger: logger, queue: queue, syncer: syncer, base: base, max: max, bulkPool: batch.NewPool(defaultRecoveryWorkers)}
}

func (s *Service) SetBulkPool(pool *batch.Pool) {
	if pool != nil {
		s.bulkPool = pool
	}
}

func (s *Service) UpdateConfig(base, max time.Duration) {
	s.mu.Lock()
	s.base, s.max = base, max
	s.mu.Unlock()
}

func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	reconcileTicker := time.NewTicker(recoveryReconcileEvery)
	defer reconcileTicker.Stop()
	s.reconcileDue(ctx, time.Now().UTC())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.runDue(ctx, now.UTC())
		case now := <-reconcileTicker.C:
			s.reconcileDue(ctx, now.UTC())
		}
	}
}

func (s *Service) runDue(ctx context.Context, now time.Time) {
	workers := s.bulkPool.Limit()
	values, err := s.queue.ClaimDueQuotaRecoveries(ctx, now, workers, recoveryClaimLease)
	if err != nil {
		s.logger.Warn("quota_recovery_claim_failed", "error", err)
		return
	}
	results, summary, runErr := batch.Run(ctx, values, batch.Options{Workers: workers, Pool: s.bulkPool}, func(workCtx context.Context, value accountdomain.QuotaRecoveryEvent) error {
		s.runOne(workCtx, now, value)
		return nil
	})
	for index, result := range results {
		if panicErr, ok := result.Err.(*batch.PanicError); ok {
			s.logger.Error("quota_recovery_panicked", "account_id", values[index].AccountID, "mode", values[index].Mode, "error", panicErr, "stack", string(panicErr.Stack))
		}
	}
	if runErr != nil {
		s.logger.Warn("quota_recovery_batch_canceled", "submitted", summary.Submitted, "completed", summary.Completed, "error", runErr)
	}
}

func (s *Service) runOne(ctx context.Context, now time.Time, value accountdomain.QuotaRecoveryEvent) {
	probeCtx, cancel := context.WithTimeout(ctx, recoveryProbeTimeout)
	window, probeErr := s.syncer.ProbeQuotaMode(probeCtx, value.AccountID, value.Mode)
	cancel()
	if probeErr == nil && window.Remaining > 0 {
		if err := s.queue.AckQuotaRecovery(ctx, value); err != nil {
			s.logger.Warn("quota_recovery_ack_failed", "account_id", value.AccountID, "mode", value.Mode, "error", err)
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	value.Attempts++
	if probeErr == nil && window.ResetAt != nil && window.ResetAt.After(now) {
		value.DueAt = *window.ResetAt
	} else if probeErr == nil && accountdomain.IsConsoleUsageQuotaMode(value.Mode) {
		// Console usage currently exposes no reset timestamp. A healthy zero
		// result is therefore rechecked after the fixed 24-hour prediction
		// window owned by domain/account; transport failures still use bounded
		// exponential backoff.
		value.DueAt = now.Add(accountdomain.ConsolePredictedQuotaProbeDelay)
	} else if probeErr == nil && window.WindowSeconds > 0 {
		// Preserve Provider-specific upstream window semantics. In particular,
		// Grok Web can report a duration without an absolute reset timestamp.
		value.DueAt = now.Add(time.Duration(window.WindowSeconds) * time.Second)
	} else {
		value.DueAt = now.Add(s.backoff(value.Attempts))
	}
	if err := s.queue.RescheduleQuotaRecovery(ctx, value); err != nil {
		s.logger.Warn("quota_recovery_reschedule_failed", "account_id", value.AccountID, "mode", value.Mode, "error", err)
	}
}

func (s *Service) reconcileDue(ctx context.Context, now time.Time) {
	windows, err := s.syncer.ListDueQuotaWindows(ctx, now, recoveryReconcileLimit, s.reconcileAfter)
	if err != nil {
		if !lifecycle.IsShutdownCancellation(ctx, err) {
			s.logger.Warn("quota_recovery_reconcile_failed", "error", err)
		}
		return
	}
	for _, window := range windows {
		// 恢复事件的资格归 domain owner：Console 的非用量窗口(billing 等)
		// 不参与路由控制，也就不应产生恢复事件。此前该巡检 ticker 只依赖
		// 存储层的"已耗尽且已到期"过滤，使旧 Console 账单窗口仍能排入队列，
		// 绕过启动回放与刷新路径共用的资格规则。
		if !accountdomain.QuotaWindowControlsRouting(window.Provider, window.Mode) {
			continue
		}
		if err := s.queue.EnsureQuotaRecovery(ctx, accountdomain.QuotaRecoveryEvent{AccountID: window.AccountID, Mode: window.Mode, DueAt: now}); err != nil {
			s.logger.Warn("quota_recovery_reconcile_schedule_failed", "account_id", window.AccountID, "mode", window.Mode, "error", err)
			return // Retry this page; successful Ensure calls are idempotent.
		}
	}
	s.reconcileAfter = nil
	if len(windows) == recoveryReconcileLimit {
		last := windows[len(windows)-1]
		if last.ResetAt != nil {
			s.reconcileAfter = &repository.QuotaWindowCursor{ResetAt: *last.ResetAt, AccountID: last.AccountID, Mode: last.Mode}
		}
	}
}

func (s *Service) backoff(attempt int) time.Duration {
	s.mu.RLock()
	base, maximum := s.base, s.max
	s.mu.RUnlock()
	if base <= 0 {
		base = 30 * time.Second
	}
	if maximum < base {
		maximum = 30 * time.Minute
	}
	value := base
	for index := 1; index < attempt && value < maximum; index++ {
		value *= 2
	}
	if value > maximum {
		return maximum
	}
	return value
}
