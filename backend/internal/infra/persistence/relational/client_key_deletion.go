package relational

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *ClientKeyRepository) Delete(ctx context.Context, id uint64) error {
	deleted, err := r.deleteKeys(ctx, []uint64{id})
	if err != nil {
		return err
	}
	if deleted == 0 {
		return repository.ErrNotFound
	}
	r.notifyInvalidation(ctx, id)
	return nil
}

func (r *ClientKeyRepository) DeleteMany(ctx context.Context, ids []uint64) (int64, error) {
	deleted, err := r.deleteKeys(ctx, ids)
	if err == nil && deleted > 0 {
		r.notifyInvalidation(ctx, 0)
	}
	return deleted, err
}

// Key, permissions, reservations, and eligible media jobs are one transaction.
// PostgreSQL key locks fence concurrent media-job FK inserts; SQLite uses its
// configured BEGIN IMMEDIATE. Deterministic ordering also serializes overlapping
// batches with billing's key-row lock. No object I/O occurs in this transaction.
func (r *ClientKeyRepository) deleteKeys(ctx context.Context, ids []uint64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []clientKeyModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").
			Where("id IN ? AND internal_kind IS NULL", ids).Order("id ASC").Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		keyIDs := make([]uint64, 0, len(rows))
		for _, row := range rows {
			keyIDs = append(keyIDs, row.ID)
		}
		if err := deleteClientKeyMediaJobs(tx, keyIDs); err != nil {
			return err
		}
		result := tx.Where("id IN ?", keyIDs).Delete(&clientKeyModel{})
		deleted = result.RowsAffected
		return result.Error
	})
	if err != nil {
		return 0, mapError(err)
	}
	return deleted, nil
}
