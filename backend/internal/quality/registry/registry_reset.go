package registry

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// stateTables is the explicit legacy maintenance reset scope. Startup and
// background reconciliation do not call this operation; archives, the degrade
// ledger and identity groups are outside its scope.
var stateTables = []string{
	"q_probe_projection",
	"q_account_state",
	"q_exit_state",
	"q_case",
	"q_case_party",
	"q_incident_closure",
	"q_observation",
	"q_probe_task",
}

// ResetQualityState explicitly resets the listed quality state and rebuilds
// this registry's cache. It is a maintenance operation, never an upgrade step.
func (r *Registry) ResetQualityState(ctx context.Context) error {
	if !r.inTransition {
		return r.withTransition(ctx, func(w *Registry) error { return w.ResetQualityState(ctx) })
	}
	if err := ResetQualityStateDB(ctx, r.db); err != nil {
		return err
	}
	return r.rebuildCache(ctx)
}

// ResetQualityStateDB performs the same explicit maintenance reset on a
// supplied database. Its caller owns cache refresh; production composition
// does not invoke this legacy entry point.
func ResetQualityStateDB(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, table := range stateTables {
			if err := tx.Exec("DELETE FROM " + table).Error; err != nil {
				return fmt.Errorf("清零 %s: %w", table, err)
			}
		}
		return nil
	})
}
