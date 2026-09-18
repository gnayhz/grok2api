package registry

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func accountCheckDatabase(t *testing.T, driver string) (Options, *gorm.DB) {
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

func TestAccountCheckUpgradePreservesTasksAndIndexes(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			opts, db := accountCheckDatabase(t, driver)
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
				if _, err := NewProbeTaskStore(r).CreateAccountCheck(context.Background(), checkTask(73001), 1); err != nil {
					t.Fatal(err)
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func checkTask(account uint64) model.ProbeTask {
	return model.ProbeTask{Direction: model.ProbeAccountCheck, DefendantAccountID: account, Experiment: model.ProbeExperiment{Version: model.AccountCheckVersion, Sample: "brief-confirmation", Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "grok-4.6", RuleVersion: "fictional-rule"}}}
}

func TestAccountCheckDurableQueueDoesNotChangeCourtEvidence(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			opts, _ := accountCheckDatabase(t, driver)
			first, err := Open(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			second, err := Open(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stores := []*ProbeTaskStore{NewProbeTaskStore(first), NewProbeTaskStore(second)}
			var wg sync.WaitGroup
			ids := make([]uint64, 2)
			errs := make([]error, 2)
			for i := range stores {
				wg.Add(1)
				go func(i int) { defer wg.Done(); ids[i], errs[i] = stores[i].CreateAccountCheck(ctx, checkTask(73001), 1) }(i)
			}
			wg.Wait()
			if errs[0] != nil || errs[1] != nil || ids[0] == 0 || ids[0] != ids[1] {
				t.Fatalf("ids=%v errs=%v", ids, errs)
			}
			if _, err := stores[0].CreateAccountCheck(ctx, checkTask(73002), 1); !errors.Is(err, model.ErrCheckQueueFull) {
				t.Fatalf("capacity: %v", err)
			}
			if n, err := second.CancelOrphanProbes(ctx, "orphan"); err != nil || n != 0 {
				t.Fatalf("manual check cancelled: %d %v", n, err)
			}
			tasks, err := stores[1].ClaimPendingProbeTasks(ctx, 1)
			if err != nil || len(tasks) != 1 || tasks[0].ID != ids[0] {
				t.Fatalf("claim=%v err=%v", tasks, err)
			}
			report := model.AssessAccountCheck([]model.AccountCheckSample{{Outcome: "clean"}, {Outcome: "clean"}, {Outcome: "clean"}})
			result := model.ProbeTaskResult{Outcome: model.ProbeResultClean, Detail: report.Reason, AccountCheck: &report}
			if err := stores[0].CompleteProbeTask(ctx, ids[0], model.ProbeDone, result, time.Now()); !errors.Is(err, model.ErrProbeAlreadySettled) {
				t.Fatalf("foreign owner accepted: %v", err)
			}
			if err := stores[1].CompleteProbeTask(ctx, ids[0], model.ProbeDone, result, time.Now()); err != nil {
				t.Fatal(err)
			}
			checks, err := stores[0].ListAccountChecks(ctx, 73001)
			if err != nil || len(checks) != 1 || checks[0].Report == nil || checks[0].Report.Outcome != "clean" {
				t.Fatalf("checks=%+v err=%v", checks, err)
			}
			if n, err := stores[0].BackfillProbeProjections(ctx, 10); err != nil || n != 0 {
				t.Fatalf("manual projection repaired: %d %v", n, err)
			}
			var count int64
			if err := first.DB().Model(&qProbeProjectionModel{}).Where("task_id = ?", ids[0]).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("projected check into court evidence: %d %v", count, err)
			}
			if count, err := first.CountCases(ctx); err != nil || count != 0 {
				t.Fatalf("check created cases: %d %v", count, err)
			}
			id, err := stores[0].CreateAccountCheck(ctx, checkTask(73001), 1)
			if err != nil || id == ids[0] {
				t.Fatalf("new measurement rejected: %d %v", id, err)
			}
			if err := first.DB().Model(&qProbeTaskModel{}).Where("id = ?", id).Update("created_at", time.Now().UTC().Add(-11*time.Minute)).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := first.CancelOrphanProbes(ctx, "orphan"); err != nil {
				t.Fatal(err)
			}
			checks, err = stores[0].ListAccountChecks(ctx, 73001)
			if err != nil || checks[0].State != model.ProbeCancelled {
				t.Fatalf("pending deadline lost: %+v %v", checks, err)
			}
		})
	}
}
