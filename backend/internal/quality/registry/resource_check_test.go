package registry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func resourceTask(kind string, id uint64) model.ProbeTask {
	task := model.ProbeTask{DefendantAccountID: id, Experiment: model.ProbeExperiment{Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "fictional-model", RuleVersion: "fictional-rule"}}}
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
func TestResourceCheckQueueOwnershipAndProgress(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			opts, _ := resourceCheckDatabase(t, driver)
			r, err := Open(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			a, b := NewProbeTaskStore(r), NewProbeTaskStore(r)
			first, err := createResourceCheck(a, ctx, resourceTask("account", 11), 4)
			if err != nil {
				t.Fatal(err)
			}
			duplicate, err := createResourceCheck(b, ctx, resourceTask("account", 11), 4)
			if err != nil || duplicate != first {
				t.Fatalf("duplicate %d %v", duplicate, err)
			}
			for _, id := range []uint64{12, 13} {
				if _, err := createResourceCheck(b, ctx, resourceTask("node", id), 4); err != nil {
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
			report := model.ResourceCheckReport{Version: model.ResourceCheckVersion, Kind: "account", ResourceID: 11, Revision: 1, MaxCalls: 5, Calls: 1, Outcome: "inconclusive"}
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

func TestResourceCheckConcurrentBatchSubmissionSharesAcceptedWork(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			opts, _ := resourceCheckDatabase(t, driver)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stores := make([]*ProbeTaskStore, 2)
			for i := range stores {
				r, err := Open(ctx, opts)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				stores[i] = NewProbeTaskStore(r)
			}
			var wg sync.WaitGroup
			results := make([][]model.ResourceSubmission, len(stores))
			errs := make([]error, len(stores))
			for i := range stores {
				wg.Add(1)
				go func() {
					defer wg.Done()
					results[i], errs[i] = stores[i].CreateResourceCheckBatch(ctx, []model.ProbeTask{resourceTask("account", 41), resourceTask("account", 42)}, 1)
				}()
			}
			wg.Wait()
			for i := range stores {
				if errs[i] != nil || len(results[i]) != 2 || results[i][0].ID == 0 || results[i][1].Error != "queue_full" {
					t.Fatalf("results=%+v errors=%v", results, errs)
				}
			}
			if results[0][0].ID != results[1][0].ID {
				t.Fatal("simultaneous submission duplicated accepted work")
			}
			var count int64
			if err := stores[0].registry.DB().Model(&qResourceCheckTargetModel{}).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("partial submission leaked targets: %d %v", count, err)
			}
		})
	}
}

func TestResourceCheckBatchSharesOwnerAndCountsResourceSlots(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			opts, _ := resourceCheckDatabase(t, driver)
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
			if rejected, err := second.CreateResourceCheckBatch(ctx, []model.ProbeTask{resourceTask("account", 43)}, 2); err != nil || rejected[0].Error != "queue_full" {
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

func createResourceCheck(s *ProbeTaskStore, ctx context.Context, task model.ProbeTask, capacity int) (uint64, error) {
	items, err := s.CreateResourceCheckBatch(ctx, []model.ProbeTask{task}, capacity)
	if err != nil {
		return 0, err
	}
	if len(items) != 1 {
		return 0, errors.New("resource batch missing result")
	}
	if items[0].Error == "queue_full" {
		return 0, errors.New("queue_full")
	}
	if items[0].Error != "" {
		return 0, errors.New(items[0].Error)
	}
	return items[0].ID, nil
}
