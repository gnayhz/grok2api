package relational

import (
	"context"
	"errors"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MigrateLegacyQualityHolds coordinates the M07 health transition and M16
// restriction in one SQL transaction. Both schemas must already be installed.
// The existing once marker prevents upgrades from replaying or extending holds.
func (r *AccountRepository) MigrateLegacyQualityHolds(ctx context.Context, now time.Time) error {
	var changed []account.HealthState
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		insert := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&runtimeSettingsModel{
			Key: "guard_owned_restrictions_v1", ValueJSON: `{"migrated":true}`, Revision: 1, UpdatedAt: now.UTC()})
		if insert.Error != nil || insert.RowsAffected == 0 {
			return insert.Error
		}
		var ids []uint64
		if err := tx.Model(&accountModel{}).Where("last_error IN ?", account.LegacyQualityHealthMarkers()).Order("id").Pluck("id", &ids).Error; err != nil {
			return err
		}
		for _, id := range ids {
			current, err := readLockedHealth(tx, id, "")
			if errors.Is(err, repository.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			hold, migrate := model.PlanLegacyHold(current, now)
			if !migrate {
				continue
			}
			result, err := applyHealthTransition(tx, current, account.HealthEvent{Kind: account.HealthMigrateLegacyQuality}, now)
			if err != nil {
				return err
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&journal.RestrictionRow{
				Owner: hold.Owner, AccountID: id, Reason: hold.Reason, ExpiresAt: hold.ExpiresAt}).Error; err != nil {
				return err
			}
			changed = append(changed, result.State)
		}
		return nil
	})
	if err != nil {
		return mapError(err)
	}
	for _, state := range changed {
		r.notifyHealth(ctx, state)
	}
	return nil
}
