package relational

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// lockProviderAccount obtains the account row write lock before reading on both
// SQLite and PostgreSQL. It does not depend on a process-local mutex.
func lockProviderAccount(tx *gorm.DB, id uint64, provider account.Provider) error {
	query := tx.Model(&accountModel{}).Where("id = ?", id)
	if provider != "" {
		query = query.Where("provider = ?", provider)
	}
	result := query.UpdateColumn("health_revision", gorm.Expr("health_revision"))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return repository.ErrNotFound
	}
	return nil
}
