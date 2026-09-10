package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func (r *MediaJobRepository) StartMediaJobExecutionLimits(ctx context.Context, id, claim string, limits media.ExecutionLimits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if limits.Version != 1 || limits.Reserved != 0 || limits.Confirmed != 0 {
		return media.ErrInvalidExecutionLimits
	}
	if id == "" || claim == "" {
		return repository.ErrConflict
	}
	result := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).
		Where("id = ? AND claim_token = ? AND status = ? AND limits_version = 0", id, claim, media.StatusInProgress).
		Updates(map[string]any{"limits_version": limits.Version, "execution_deadline": limits.Deadline.UTC(), "physical_limit": limits.PhysicalLimit})
	return mediaLimitsWriteResult(result)
}

// ReserveMediaJobPhysicalCall is the only permit allocation path. SQL fences
// stale workers and exhausted budgets before the transport may submit bytes.
func (r *MediaJobRepository) ReserveMediaJobPhysicalCall(ctx context.Context, id, claim string, now time.Time) error {
	if id == "" || claim == "" {
		return repository.ErrConflict
	}
	result := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).
		Where("id = ? AND claim_token = ? AND status = ? AND limits_version = 1 AND execution_deadline > ? AND physical_reserved < physical_limit", id, claim, media.StatusInProgress, now.UTC()).
		UpdateColumn("physical_reserved", gorm.Expr("physical_reserved + 1"))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	var row mediaJobModel
	if err := r.db.db.WithContext(ctx).Select("claim_token", "status", "execution_deadline", "physical_limit", "physical_reserved").Where("id = ?", id).First(&row).Error; err != nil {
		return mapError(err)
	}
	if row.ClaimToken != claim || row.Status != string(media.StatusInProgress) {
		return repository.ErrConflict
	}
	if row.ExecutionDeadline != nil && !row.ExecutionDeadline.After(now) {
		return context.DeadlineExceeded
	}
	if row.PhysicalLimit > 0 && row.PhysicalReserved >= row.PhysicalLimit {
		return media.ErrPhysicalBudgetExhausted
	}
	return repository.ErrConflict
}

// Confirmation is an idempotent CAS after the physical journal acknowledges a
// bounded batch. A lost database response cannot increment the counter twice.
func (r *MediaJobRepository) ConfirmMediaJobPhysicalCalls(ctx context.Context, id, claim string, previous, confirmed uint32) error {
	if id == "" || claim == "" || confirmed <= previous {
		return repository.ErrConflict
	}
	result := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).
		Where("id = ? AND claim_token = ? AND limits_version = 1 AND physical_confirmed IN ? AND physical_reserved >= ?", id, claim, []uint32{previous, confirmed}, confirmed).
		UpdateColumn("physical_confirmed", confirmed)
	return mediaLimitsWriteResult(result)
}
func mediaLimitsWriteResult(result *gorm.DB) error {
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return repository.ErrConflict
	}
	return nil
}
