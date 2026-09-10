package registry

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// qNodeEpochModel is the current pointer, updated atomically with the append-only
// IP archive. Routine state operations never scan historical epochs.
type qNodeEpochModel struct {
	NodeID              uint64 `gorm:"primaryKey;autoIncrement:false"`
	Epoch               uint64 `gorm:"not null"`
	ObservationRevision uint64 `gorm:"not null;default:0"`
}

func (qNodeEpochModel) TableName() string { return "q_node_epoch" }

func (r *Registry) migrateCurrentEpochs(ctx context.Context) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&qStateRevisionModel{ID: 1}).Error; err != nil {
			return err
		}
		// The marker and backfill commit together. A failed upgrade is retryable;
		// concurrent starters cannot publish a partially initialized pointer set.
		claim := tx.Model(&qStateRevisionModel{}).Where("id = 1 AND epochs_initialized = ?", false).
			Updates(map[string]any{"epochs_initialized": true, "revision": gorm.Expr("revision + 1")})
		if claim.Error != nil || claim.RowsAffected == 0 {
			return claim.Error
		}
		return tx.Exec(`INSERT INTO q_node_epoch (node_id, epoch)
			SELECT node_id, MAX(epoch) FROM q_ip_epoch GROUP BY node_id
			ON CONFLICT (node_id) DO UPDATE SET epoch = excluded.epoch`).Error
	})
}
