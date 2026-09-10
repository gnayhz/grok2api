package registry

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// migrateLegacyDirectStates converts rows written by the retired multi-stage
// tribunal before the finite loop became the only runtime architecture.
// A legacy final account remains sentenced; a legacy bail is an inconclusive
// result and therefore becomes released. The old columns may remain in an
// existing database for SQLite compatibility, but no runtime code reads or
// writes their semantics.
func (r *Registry) migrateLegacyDirectStates(ctx context.Context) error {
	accountTable := r.db.Migrator().HasTable("q_account_state")
	partyTable := r.db.Migrator().HasTable("q_case_party")
	if !accountTable && !partyTable {
		return nil
	}
	now := time.Now().UTC()
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if accountTable {
			if err := tx.Exec("UPDATE q_account_state SET state = 'sentenced', updated_at = ? WHERE state = 'final'", now).Error; err != nil {
				return fmt.Errorf("migrate legacy final account states: %w", err)
			}
		}
		if partyTable {
			if err := tx.Exec("UPDATE q_case_party SET disposition = 'released', updated_at = ? WHERE disposition = 'bail'", now).Error; err != nil {
				return fmt.Errorf("migrate legacy bail parties: %w", err)
			}
		}
		if accountTable {
			if err := tx.Exec("DELETE FROM q_account_state WHERE state = 'bail'").Error; err != nil {
				return fmt.Errorf("migrate legacy bail account states: %w", err)
			}
		}
		return nil
	})
}
