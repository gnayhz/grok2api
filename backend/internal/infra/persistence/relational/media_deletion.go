package relational

import (
	"context"
	"fmt"

	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Only the deletion facts are loaded; prompts and input payloads can be large.
func mediaJobsForDeletion(tx *gorm.DB) *gorm.DB {
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id", "status", "execution_phase", "quota_recorded_at", "usage_recorded_at")
}

func checkMediaJobDeletion(row mediaJobModel) error {
	if err := mediaJobToDomain(row).CheckDeletion(); err != nil {
		return fmt.Errorf("%w: %w", repository.ErrConflict, err)
	}
	return nil
}

// DeleteMediaJob rechecks M17's policy under the row lock. Ticket revocation and
// row removal commit together; asset retention/deletion remains owned by M17.
func (r *MediaJobRepository) DeleteMediaJob(ctx context.Context, id string) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row mediaJobModel
		if err := mediaJobsForDeletion(tx).Where("id = ?", id).First(&row).Error; err != nil {
			return mapError(err)
		}
		if err := checkMediaJobDeletion(row); err != nil {
			return err
		}
		return deleteMediaJobRows(tx, []string{row.ID})
	})
}

// The caller owns the key locks and the surrounding key-deletion transaction.
// Pages bound memory; a later conflict/error rolls back all earlier pages.
func deleteClientKeyMediaJobs(tx *gorm.DB, keyIDs []uint64) error {
	const pageSize = 200
	for {
		var rows []mediaJobModel
		if err := mediaJobsForDeletion(tx).Where("client_key_id IN ?", keyIDs).
			Order("id ASC").Limit(pageSize).Find(&rows).Error; err != nil {
			return err
		}
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			if err := checkMediaJobDeletion(row); err != nil {
				return err
			}
			ids = append(ids, row.ID)
		}
		if len(ids) == 0 {
			return nil
		}
		if err := deleteMediaJobRows(tx, ids); err != nil {
			return err
		}
		if len(rows) < pageSize {
			return nil
		}
	}
}

func deleteMediaJobRows(tx *gorm.DB, ids []string) error {
	if err := tx.Where("job_id IN ?", ids).Delete(&mediaUploadTicketModel{}).Error; err != nil {
		return err
	}
	return tx.Where("id IN ?", ids).Delete(&mediaJobModel{}).Error
}
