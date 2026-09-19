package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func resourceCheckDatabase(t *testing.T, driver string) (Options, *gorm.DB) {
	t.Helper()
	if driver == "postgres" {
		return isolatedQualityPostgres(t)
	}
	opts := Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "checks.db")}
	db, err := gorm.Open(sqlite.Open(opts.SQLitePath), qualityGormConfig())
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return opts, db
}

func TestResourceCheckUpgradePreservesTasksAndIndexes(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			opts, db := resourceCheckDatabase(t, driver)
			if err := db.AutoMigrate(&legacyEvolutionProbeTask{}); err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&qProbeTaskModel{}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			row := qProbeTaskModel{Direction: "exit_jury", State: "running", LeaseOwner: "fictional-owner", LeaseUntil: &now, ProjectionVersion: 1, ExperimentJSON: `{"version":"fictional"}`, AttemptJSON: `{"id":"fictional-attempt"}`, ControlAttemptJSON: `{"id":"fictional-control"}`, CreatedAt: now, UpdatedAt: now}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			for range 2 {
				r, err := Open(context.Background(), opts)
				if err != nil {
					t.Fatal(err)
				}
				var got qProbeTaskModel
				if err := r.DB().First(&got, row.ID).Error; err != nil {
					t.Fatal(err)
				}
				if got.LeaseOwner != row.LeaseOwner || got.AttemptJSON != row.AttemptJSON || got.ControlAttemptJSON != row.ControlAttemptJSON || got.ExperimentJSON != row.ExperimentJSON || got.ProjectionVersion != 1 {
					t.Fatalf("lost old task: %+v", got)
				}
				for _, index := range []string{"idx_q_probe_task_case", "idx_q_probe_task_state_updated", "idx_q_probe_task_lease_owner", "idx_q_probe_task_lease_until", "idx_q_probe_task_projection_version"} {
					if !r.DB().Migrator().HasIndex(&qProbeTaskModel{}, index) {
						t.Errorf("lost index %s", index)
					}
				}
				if _, err := createResourceCheck(NewProbeTaskStore(r), context.Background(), resourceTask("account", 73001), 1); err != nil {
					t.Fatal(err)
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
