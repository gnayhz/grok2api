package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// Keep this operation ID stable across application versions. Every pool edge
// or member writer and parent deletion takes it before reading or acquiring
// row locks. It protects missing targets as well as existing rows, which row
// locks alone cannot do.
// SQLite already serializes writers with BEGIN IMMEDIATE. No network call or
// runtime routing read holds this transaction-scoped administrative lock.
const egressPoolGraphLockID int64 = 0x47524f4b504f4f4c

func lockEgressPoolGraph(tx *gorm.DB) error {
	if tx.Dialector.Name() == "postgres" {
		return tx.Exec("SELECT pg_advisory_xact_lock(?)", egressPoolGraphLockID).Error
	}
	return nil
}

func validateEgressPoolGraph(tx *gorm.DB, value egress.Pool) error {
	if value.FallbackMode.Normalized() != egress.PoolFallbackPool {
		return nil
	}
	var rows []egressPoolModel
	if err := tx.Select("id", "fallback_mode", "fallback_pool_id").Find(&rows).Error; err != nil {
		return err
	}
	pools := make([]egress.Pool, len(rows))
	for i, row := range rows {
		pools[i] = toPoolDomain(row)
	}
	return egress.ValidatePoolFallback(value, pools)
}

func (r *EgressRepository) CreateEgressPool(ctx context.Context, value egress.Pool) (egress.Pool, error) {
	row := fromPoolDomain(value)
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockEgressPoolGraph(tx); err != nil {
			return err
		}
		if err := validateEgressPoolGraph(tx, value); err != nil {
			return err
		}
		return tx.Create(&row).Error
	})
	if err != nil {
		return egress.Pool{}, mapError(err)
	}
	return toPoolDomain(row), nil
}

func (r *EgressRepository) UpdateEgressPool(ctx context.Context, value egress.Pool) (egress.Pool, error) {
	row := fromPoolDomain(value)
	var saved egressPoolModel
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockEgressPoolGraph(tx); err != nil {
			return err
		}
		if err := tx.First(&saved, "id = ?", value.ID).Error; err != nil {
			return err
		}
		if err := validateEgressPoolGraph(tx, value); err != nil {
			return err
		}
		if err := tx.Model(&egressPoolModel{}).Where("id = ?", value.ID).Updates(map[string]any{
			"name": row.Name, "enabled": row.Enabled, "strategy": row.Strategy,
			"fallback_mode": row.FallbackMode, "fallback_pool_id": row.FallbackPoolID,
			"updated_at": time.Now().UTC(),
		}).Error; err != nil {
			return err
		}
		return tx.First(&saved, "id = ?", value.ID).Error
	})
	if err != nil {
		return egress.Pool{}, mapError(err)
	}
	return toPoolDomain(saved), nil
}

func (r *EgressRepository) DeleteEgressPool(ctx context.Context, id uint64) error {
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockEgressPoolGraph(tx); err != nil {
			return err
		}
		// Establish the singleton even before its first publication. All
		// routing writers then serialize before locking a pool/node row.
		if _, err := lockEgressOperationsConfig(tx); err != nil {
			return err
		}
		// Match node deletion's configuration-before-members order. Keep the
		// routing cleanup atomic with member, incoming-edge and pool deletion.
		if err := clearEgressRoutingPoolReferences(tx, id); err != nil {
			return err
		}
		if err := tx.Where("pool_id = ?", id).Delete(&egressPoolMemberModel{}).Error; err != nil {
			return err
		}
		if err := tx.Model(&egressPoolModel{}).Where("fallback_pool_id = ?", id).Updates(map[string]any{"fallback_mode": string(egress.PoolFallbackNone), "fallback_pool_id": 0}).Error; err != nil {
			return err
		}
		result := tx.Delete(&egressPoolModel{}, "id = ?", id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		return nil
	})
	return mapError(err)
}
