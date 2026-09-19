package registry

import (
	"context"
	"strings"

	"gorm.io/gorm"
)

// Widen the two case enums without rewriting historical verdicts or evidence.
func (r *Registry) migrateCaseProof(ctx context.Context) error {
	for _, name := range []string{"chk_q_case_status", "chk_q_case_verdict"} {
		var definition string
		db := r.db.WithContext(ctx)
		var err error
		if db.Dialector.Name() == "postgres" {
			err = db.Raw("SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'q_case'::regclass AND conname = ?", name).Scan(&definition).Error
		} else {
			err = db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'q_case'").Scan(&definition).Error
			// Inspect each constraint separately: rebuilding one must not skip the other.
			if at := strings.Index(definition, name); at >= 0 {
				definition = strings.SplitN(definition[at:], ")", 2)[0]
			}
		}
		if err != nil {
			return err
		}
		if strings.Contains(definition, "'both_guilty'") {
			continue
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			if tx.Migrator().HasConstraint(&qCaseModel{}, name) {
				if err := tx.Migrator().DropConstraint(&qCaseModel{}, name); err != nil {
					return err
				}
			}
			if err := tx.Migrator().CreateConstraint(&qCaseModel{}, name); err != nil {
				return err
			}
			return tx.AutoMigrate(&qCaseModel{})
		}); err != nil {
			return err
		}
	}
	return nil
}
