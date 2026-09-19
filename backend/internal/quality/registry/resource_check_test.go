package registry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func resourceTask(kind string, id uint64) model.ProbeTask {
	task := checkTask(id)
	task.Direction = model.ProbeResourceCheck
	task.Experiment.Version = model.ResourceCheckVersion
	task.Experiment.Sample = "token-short"
	task.Experiment.ResourceCheck = &model.ResourceCheckPlan{Kind: kind, ResourceID: id, Accounts: []uint64{801, 802, 803}, Nodes: []uint64{901, 902, 903}}
	if kind == "node" {
		task.DefendantAccountID = 0
		task.DefendantNodeID = id
	}
	return task
}
func TestResourceCheckQueueOwnershipProgressAndCompatibility(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			opts, _ := accountCheckDatabase(t, driver)
			r, err := Open(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			a, b := NewProbeTaskStore(r), NewProbeTaskStore(r)
			first, err := a.CreateResourceCheck(ctx, resourceTask("account", 11), 4)
			if err != nil {
				t.Fatal(err)
			}
			duplicate, err := b.CreateResourceCheck(ctx, resourceTask("account", 11), 4)
			if err != nil || duplicate != first {
				t.Fatalf("duplicate %d %v", duplicate, err)
			}
			for _, id := range []uint64{12, 13} {
				if _, err := b.CreateResourceCheck(ctx, resourceTask("node", id), 4); err != nil {
					t.Fatal(err)
				}
			}
			claimed, err := a.ClaimPendingProbeTasks(ctx, 8)
			if err != nil || len(claimed) != 2 {
				t.Fatalf("claimed=%v %v", claimed, err)
			}
			if more, err := b.ClaimPendingProbeTasks(ctx, 8); err != nil || len(more) != 0 {
				t.Fatalf("cross-owner capacity %v %v", more, err)
			}
			report := model.ResourceCheckReport{Version: model.ResourceCheckVersion, Kind: "account", ResourceID: 11, Revision: 1, MaxCalls: 5, Calls: 1, Outcome: "inconclusive", Groups: []model.ResourceCheckGroup{}}
			if err := b.SaveResourceCheckProgress(ctx, first, report); !errors.Is(err, model.ErrProbeAlreadySettled) {
				t.Fatalf("foreign progress %v", err)
			}
			if err := a.SaveResourceCheckProgress(ctx, first, report); err != nil {
				t.Fatal(err)
			}
			rows, err := b.ListResourceChecks(ctx, "account", []uint64{11})
			if err != nil || len(rows) != 1 || rows[0].Report == nil || rows[0].Report.Calls != 1 {
				t.Fatalf("progress %+v %v", rows, err)
			}
			if _, err := r.CancelOrphanProbes(ctx, "fictional-orphan-sweep"); err != nil {
				t.Fatal(err)
			}
			if err := a.CompleteProbeTask(ctx, first, model.ProbeDone, model.ProbeTaskResult{ResourceCheck: &report, Outcome: model.ProbeResultError}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := a.SaveResourceCheckProgress(ctx, first, report); !errors.Is(err, model.ErrProbeAlreadySettled) {
				t.Fatalf("late progress %v", err)
			}
			if more, err := b.ClaimPendingProbeTasks(ctx, 8); err != nil || len(more) != 1 {
				t.Fatalf("released capacity %v %v", more, err)
			}
			views, err := a.ListProbeTasks(ctx, 100)
			if err != nil || len(views) != 0 {
				t.Fatalf("manual leaked to court %+v %v", views, err)
			}
			var count int64
			if err := r.DB().Model(&qProbeProjectionModel{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatal("manual check projected to court")
			}
		})
	}
}

func TestResourceCheckUpgradeFromAccountOnlyConstraint(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			opts, db := accountCheckDatabase(t, driver)
			if err := db.AutoMigrate(&qProbeTaskModel{}); err != nil {
				t.Fatal(err)
			}
			// Simulate the previous deployed constraint while retaining reports.
			if driver == "postgres" {
				if err := db.Exec("ALTER TABLE q_probe_task DROP CONSTRAINT chk_q_probe_task_direction").Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Exec("ALTER TABLE q_probe_task ADD CONSTRAINT chk_q_probe_task_direction CHECK (direction IN ('account_differential','exit_jury','account_check'))").Error; err != nil {
					t.Fatal(err)
				}
			} else {
				// Recreate only this empty disposable fixture with the exact old
				// constraint. The real migration below must retain its report.
				var schema string
				if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'q_probe_task'").Scan(&schema).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Migrator().DropTable(&qProbeTaskModel{}); err != nil {
					t.Fatal(err)
				}
				schema = strings.ReplaceAll(schema, ",'resource_check'", "")
				if err := db.Exec(schema).Error; err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC()
			old := qProbeTaskModel{Direction: "account_check", State: "done", CheckReportJSON: `{"version":"account-quality-check-v1","outcome":"inconclusive","samples":[]}`, CreatedAt: now, UpdatedAt: now}
			if err := db.Create(&old).Error; err != nil {
				t.Fatal(err)
			}
			// The old resource protocol did not have batch capacity or CAS columns.
			for _, column := range []string{"ManualSlots", "CheckRevision"} {
				if err := db.Migrator().DropColumn(&qProbeTaskModel{}, column); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				r, err := Open(context.Background(), opts)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := NewProbeTaskStore(r).CreateResourceCheck(context.Background(), resourceTask("node", 77), 32); err != nil {
					t.Fatal(err)
				}
				var retained qProbeTaskModel
				if err := r.DB().First(&retained, old.ID).Error; err != nil || retained.CheckReportJSON != old.CheckReportJSON || retained.ManualSlots != 1 || retained.CheckRevision != 0 {
					t.Fatal("old report lost", err)
				}
				for _, index := range []string{"idx_q_probe_task_case", "idx_q_probe_task_state_updated", "idx_q_probe_task_lease_owner", "idx_q_probe_task_lease_until"} {
					if !r.DB().Migrator().HasIndex(&qProbeTaskModel{}, index) {
						t.Fatal("index lost", index)
					}
				}
				r.Close()
			}
		})
	}
}

func TestResourceCheckBatchSharesOwnerAndCountsResourceSlots(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			opts, _ := accountCheckDatabase(t, driver)
			a, err := Open(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			b, err := Open(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			first, second := NewProbeTaskStore(a), NewProbeTaskStore(b)
			items, err := first.CreateResourceCheckBatch(ctx, []model.ProbeTask{resourceTask("account", 41), resourceTask("account", 42)}, 2)
			if err != nil || len(items) != 2 || items[0].ID == 0 || items[0].ID != items[1].ID {
				t.Fatalf("batch %+v %v", items, err)
			}
			if _, err := second.CreateAccountCheck(ctx, checkTask(43), 2); !errors.Is(err, model.ErrCheckQueueFull) {
				t.Fatal("batch bypassed resource capacity", err)
			}
			duplicates, err := second.CreateResourceCheckBatch(ctx, []model.ProbeTask{resourceTask("account", 42)}, 2)
			if err != nil || duplicates[0].ID != items[0].ID {
				t.Fatal("duplicate batch", duplicates, err)
			}
			mismatch := resourceTask("account", 42)
			mismatch.Experiment.Baseline.Model = "fictional-other-model"
			wrong, err := second.CreateResourceCheckBatch(ctx, []model.ProbeTask{mismatch}, 2)
			if err != nil || wrong[0].Error != "active_spec_conflict" {
				t.Fatal("wrong model reused", wrong, err)
			}
			tasks, err := first.ClaimPendingProbeTasks(ctx, 8)
			if err != nil || len(tasks) != 1 || len(tasks[0].Experiment.ResourceCheck.Targets) != 2 || tasks[0].Experiment.ResourceCheck.MaxCalls != 6 {
				t.Fatal(tasks, err)
			}
			r := model.ResourceCheckReport{Version: model.ResourceCheckVersion, Revision: 1, MaxCalls: 6, Calls: 1, Results: []model.ResourceProof{{ResourceTarget: model.ResourceTarget{Kind: "account", ResourceID: 41}, Outcome: "healthy", Reason: "proved_normal"}, {ResourceTarget: model.ResourceTarget{Kind: "account", ResourceID: 42}, Outcome: "inconclusive", Reason: "insufficient_controls"}}}
			expanded := r
			expanded.MaxCalls = 28
			if err := first.SaveResourceCheckProgress(ctx, tasks[0].ID, expanded); !errors.Is(err, model.ErrProbeAlreadySettled) {
				t.Fatal("writer expanded frozen budget", err)
			}
			if err := first.SaveResourceCheckProgress(ctx, tasks[0].ID, r); err != nil {
				t.Fatal(err)
			}
			if err := first.SaveResourceCheckProgress(ctx, tasks[0].ID, r); !errors.Is(err, model.ErrProbeAlreadySettled) {
				t.Fatal("stale revision accepted", err)
			}
			rows, err := second.ListResourceChecks(ctx, "account", []uint64{41, 42})
			if err != nil || len(rows) != 2 || rows[0].Report.Outcome != "healthy" || rows[1].Report.Outcome != "inconclusive" {
				t.Fatal("per-target projection", rows, err)
			}
			if err := a.DB().Model(&qProbeTaskModel{}).Where("id = ?", tasks[0].ID).Update("lease_until", time.Now().UTC().Add(-time.Second)).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := b.ReclaimStaleRunningProbes(ctx, time.Second); err != nil {
				t.Fatal(err)
			}
			if next, err := second.ClaimPendingProbeTasks(ctx, 8); err != nil || len(next) != 0 {
				t.Fatal("replayed lost batch", next, err)
			}
			r.Revision++
			if err := first.SaveResourceCheckProgress(ctx, tasks[0].ID, r); !errors.Is(err, model.ErrProbeAlreadySettled) {
				t.Fatal("expired owner wrote", err)
			}
		})
	}
}
