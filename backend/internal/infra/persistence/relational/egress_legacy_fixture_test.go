package relational

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"time"
)

// Historical states are installed only by upgrade/preservation fixtures.
func seedLegacyEgressQuality(r *EgressRepository, ctx context.Context, id uint64, health float64, failureCount int, cooldownUntil *time.Time, lastError string, degradeCount int, lastDegradedAt *time.Time) error {
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", id).Updates(map[string]any{
		"health_revision": gorm.Expr("health_revision + 1"), "health": health, "failure_count": failureCount, "cooldown_until": cooldownUntil, "last_error": lastError,
		"degrade_count": degradeCount, "last_degraded_at": lastDegradedAt, "updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// seedLegacyEgressRotation installs historical bookkeeping for migration tests.
func seedLegacyEgressRotation(r *EgressRepository, ctx context.Context, id uint64, lastRotatedAt *time.Time, attempts int, lastError string) error {
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", id).Updates(map[string]any{
		"last_rotated_at":   gorm.Expr("COALESCE(?, last_rotated_at)", lastRotatedAt),
		"rotation_attempts": attempts, "last_rotation_error": lastError, "updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}
