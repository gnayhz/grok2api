package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// Fixture setup only: some legacy tests seed account rows without credentials.
func seedHealthFixture(repo *AccountRepository, ctx context.Context, id uint64, provider account.Provider, count int, until *time.Time, reason string, success bool) error {
	now := time.Now().UTC()
	updates := map[string]any{"failure_count": count, "cooldown_until": until, "last_error": reason, "health_revision": gorm.Expr("health_revision + 1"), "cooldown_marked_at": nil}
	if until != nil {
		updates["cooldown_marked_at"] = now
	}
	if success {
		updates["last_used_at"] = now
	}
	result := repo.db.db.WithContext(ctx).Model(&accountModel{}).Where("id = ? AND provider = ?", id, provider).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return repository.ErrNotFound
	}
	return nil
}
