package registry

import (
	"context"
	"strings"

	"gorm.io/gorm"
)

// Widen only the task direction constraint. GORM's SQLite table reconstruction
// copies every existing column; PostgreSQL replaces the check in the same schema
// transaction. Old reports, leases and projection identities are retained.
func (r *Registry) migrateResourceCheckDirection(ctx context.Context) error {
	var definition string
	db := r.db.WithContext(ctx)
	var err error
	if db.Dialector.Name() == "postgres" {
		err = db.Raw("SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'q_probe_task'::regclass AND conname = 'chk_q_probe_task_direction'").Scan(&definition).Error
	} else {
		err = db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'q_probe_task'").Scan(&definition).Error
	}
	if err != nil || strings.Contains(definition, "'case_proof'") && strings.Contains(definition, "'resource_check'") {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if tx.Migrator().HasConstraint(&qProbeTaskModel{}, "chk_q_probe_task_direction") {
			if err := tx.Migrator().DropConstraint(&qProbeTaskModel{}, "chk_q_probe_task_direction"); err != nil {
				return err
			}
		}
		if err := tx.Migrator().CreateConstraint(&qProbeTaskModel{}, "chk_q_probe_task_direction"); err != nil {
			return err
		}
		// SQLite rebuilds drop indexes; restore all indexes declared by this
		// table's model before committing the replacement constraint.
		return tx.AutoMigrate(&qProbeTaskModel{})
	})
}
