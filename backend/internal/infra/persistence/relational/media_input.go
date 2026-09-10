package relational

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// This fixed operation ID must remain stable across live application versions.
// Only input registrations use it; no file I/O or upstream request runs inside
// the transaction. SQLite's immediate transaction already serializes writers.
const mediaInputCapacityLockID int64 = 0x47524b494e505554

func (r *MediaAssetRepository) CreateMediaInputAsset(ctx context.Context, value media.Asset, capacityLimit int64) error {
	if value.ExpiresAt == nil || !media.IsInputAssetID(value.ID) || value.SourceJobID != "" || (value.Kind != "image" && value.Kind != "video") || value.SizeBytes <= 0 {
		return repository.ErrInvalidRecord
	}
	if capacityLimit <= 0 || value.SizeBytes > capacityLimit {
		return repository.ErrLimitExceeded
	}
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", mediaInputCapacityLockID).Error; err != nil {
				return err
			}
		}
		var total int64
		if err := tx.Model(&mediaAssetModel{}).Select("COALESCE(SUM(size_bytes), 0)").Scan(&total).Error; err != nil {
			return err
		}
		if total > capacityLimit-value.SizeBytes {
			return repository.ErrLimitExceeded
		}
		row := mediaAssetModel{ID: value.ID, Kind: value.Kind, StorageKey: value.StorageKey, MIMEType: value.MIMEType, SizeBytes: value.SizeBytes, SHA256: value.SHA256, ExpiresAt: value.ExpiresAt, CreatedAt: value.CreatedAt}
		return tx.Create(&row).Error
	})
}
