package journal

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// Sweep drains expired data in short transactions until caught up or its
// deadline expires. The caller supplies a time budget, not a fixed row quota.
// An unresolved completion obligation keeps all related facts available.
func (s *Store) Sweep(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cutoff := now.Add(-7 * 24 * time.Hour)
	for {
		removed := 0
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var ids []string
			if err := tx.Table("q_guard_outbox o").Select("o.event_id").
				Joins("JOIN q_guard_event e ON e.id = o.event_id").
				Where("o.processed_at < ? AND NOT EXISTS (SELECT 1 FROM q_guard_completion c WHERE c.attempt_id = e.attempt_id)", cutoff).
				Order("o.processed_at, o.event_id").Limit(1000).Pluck("o.event_id", &ids).Error; err != nil {
				return err
			}
			if len(ids) > 0 {
				if err := tx.Where("id IN ?", ids).Delete(&EventRow{}).Error; err != nil {
					return err
				}
				if err := tx.Where("event_id IN ?", ids).Delete(&OutboxRow{}).Error; err != nil {
					return err
				}
			}
			var owners []string
			if err := tx.Model(&RestrictionRow{}).Where("expires_at < ? AND reason <> ?", cutoff, "legacy_disabled_review").Limit(1000).Pluck("owner", &owners).Error; err != nil {
				return err
			}
			if len(owners) > 0 {
				if err := tx.Where("owner IN ?", owners).Delete(&RestrictionRow{}).Error; err != nil {
					return err
				}
			}
			removed = len(ids) + len(owners)
			return nil
		})
		if err != nil {
			return err
		}
		if removed == 0 {
			return nil
		}
	}
}
