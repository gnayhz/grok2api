package history

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	responseOwnershipCleanupBatchSize = 1000
	webResponseStateCleanupBatchSize  = 50
	responseCleanupMaxBatches         = 100
	ResponseCleanupInterval           = 5 * time.Minute
	responseCleanupBudget             = 30 * time.Second
	responseCleanupLockTTL            = 2 * time.Minute
)

const ConversationCleanupInterval = 10 * time.Minute

// Retention owns bounded history maintenance policy. The process lifecycle owns
// scheduling and cancellation; no goroutine or independent clock is created here.
type Retention struct {
	responses repository.ResponseRepository
	journal   repository.ConversationJournal
	lock      repository.DistributedLock
	logger    *slog.Logger
}

func NewRetention(responses repository.ResponseRepository, journal repository.ConversationJournal, lock repository.DistributedLock, logger *slog.Logger) *Retention {
	if logger == nil {
		logger = slog.Default()
	}
	return &Retention{responses: responses, journal: journal, lock: lock, logger: logger}
}
func (r *Retention) CleanupConversations(ctx context.Context, now time.Time) error {
	if r.journal == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := r.journal.Prune(cleanupCtx, now.UTC(), 100)
	return err
}

func (r *Retention) CleanupResponses(ctx context.Context, now time.Time) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, responseCleanupBudget)
	defer cancel()
	if r.lock != nil {
		release, acquired, err := r.lock.Acquire(cleanupCtx, "response-ownership-cleanup", responseCleanupLockTTL)
		if err != nil {
			return err
		}
		if !acquired {
			return nil
		}
		defer release()
	}
	var totalOwnership, totalWebState int64
	for range responseCleanupMaxBatches {
		if err := cleanupCtx.Err(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.recordResponseCleanup(totalOwnership, totalWebState, true)
			return nil
		}
		result, err := r.responses.DeleteExpired(cleanupCtx, now.UTC(), responseOwnershipCleanupBatchSize, webResponseStateCleanupBatchSize)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				r.recordResponseCleanup(totalOwnership, totalWebState, true)
				return nil
			}
			return err
		}
		totalOwnership += result.OwnershipDeleted
		totalWebState += result.WebStateDeleted
		if !result.HasMore {
			r.recordResponseCleanup(totalOwnership, totalWebState, false)
			return nil
		}
	}
	r.recordResponseCleanup(totalOwnership, totalWebState, true)
	return nil
}

func (r *Retention) recordResponseCleanup(ownershipDeleted, webStateDeleted int64, backlog bool) {
	outcome := "complete"
	if backlog {
		outcome = "backlog"
		r.logger.Warn("response_cleanup_backlog", "ownership_deleted", ownershipDeleted, "web_state_deleted", webStateDeleted)
	}
	labels := perfmetrics.Labels{Subsystem: "response", Operation: "cleanup", Outcome: outcome}
	perfmetrics.Default.Add("response_cleanup_ownership_rows", labels, ownershipDeleted)
	perfmetrics.Default.Add("response_cleanup_web_state_rows", labels, webStateDeleted)
}
