package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

const generatedMediaJobPredicate = "(execution_phase = 'generated' OR (execution_phase = '' AND status = 'completed'))"

func (r *MediaJobRepository) ListUnrecordedMediaJobQuotas(ctx context.Context, afterID string, limit int) ([]media.Job, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var rows []mediaJobModel
	err := r.db.db.WithContext(ctx).Omit("input_json").Where("id > ? AND quota_recorded_at IS NULL AND "+generatedMediaJobPredicate, afterID).Order("id ASC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	values := make([]media.Job, 0, len(rows))
	for _, row := range rows {
		values = append(values, mediaJobToDomain(row))
	}
	return values, nil
}

func (r *MediaJobRepository) MarkMediaJobQuotaRecorded(ctx context.Context, value media.Job, recordedAt time.Time) error {
	query := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).Where("id = ? AND quota_mode = ? AND quota_snapshot_version = ? AND quota_account_id = ? AND "+generatedMediaJobPredicate, value.ID, value.Quota.Mode, value.Quota.SnapshotVersion, value.Quota.AccountID)
	if value.AccountID == 0 {
		query = query.Where("account_id IS NULL")
	} else {
		query = query.Where("account_id = ?", value.AccountID)
	}
	result := query.UpdateColumn("quota_recorded_at", gorm.Expr("COALESCE(quota_recorded_at, ?)", recordedAt))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return repository.ErrConflict
	}
	return nil
}
