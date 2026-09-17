package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func TestPeerStartupPreservesLiveProbeAndFencesForeignSettlement(t *testing.T) {
	r := newReconcileRegistry(t)
	ctx := context.Background()
	first, second := NewProbeTaskStore(r), NewProbeTaskStore(r)
	id, err := first.CreateProbeTask(ctx, model.ProbeTask{Direction: model.ProbeExitJury, JurorAccountID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if tasks, err := first.ClaimPendingProbeTasks(ctx, 1); err != nil || len(tasks) != 1 {
		t.Fatalf("claim=%+v err=%v", tasks, err)
	}
	if n, err := r.ReclaimRunningProbes(ctx, "peer startup"); err != nil || n != 0 {
		t.Fatalf("live peer cancelled: %d %v", n, err)
	}
	if err := second.RenewProbeTask(ctx, id); !errors.Is(err, model.ErrProbeAlreadySettled) {
		t.Fatalf("foreign heartbeat accepted: %v", err)
	}
	result := model.ProbeTaskResult{Outcome: model.ProbeResultClean}
	if err := second.CompleteProbeTask(ctx, id, model.ProbeDone, result, time.Now()); !errors.Is(err, model.ErrProbeAlreadySettled) {
		t.Fatalf("foreign result accepted: %v", err)
	}
	if err := first.RenewProbeTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := first.CompleteProbeTask(ctx, id, model.ProbeDone, result, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredProbeCannotRenewOrCommitBeforeReclaimer(t *testing.T) {
	r := newReconcileRegistry(t)
	ctx := context.Background()
	store := NewProbeTaskStore(r)
	id, err := store.CreateProbeTask(ctx, model.ProbeTask{Direction: model.ProbeExitJury})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimPendingProbeTasks(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.DB().Model(&qProbeTaskModel{}).Where("id = ?", id).Update("lease_until", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.RenewProbeTask(ctx, id); !errors.Is(err, model.ErrProbeAlreadySettled) {
		t.Fatalf("expired lease revived: %v", err)
	}
	if err := store.CompleteProbeTask(ctx, id, model.ProbeDone, model.ProbeTaskResult{Outcome: model.ProbeResultClean}, time.Now()); !errors.Is(err, model.ErrProbeAlreadySettled) {
		t.Fatalf("expired result accepted: %v", err)
	}
	if n, err := r.ReclaimRunningProbes(ctx, "expired"); err != nil || n != 1 {
		t.Fatalf("expired not reclaimed: %d %v", n, err)
	}
}

func TestProbeProjectionAtomicIntentAndRestartRetry(t *testing.T) {
	r := newReconcileRegistry(t)
	ctx := context.Background()
	store := NewProbeTaskStore(r)
	id, err := store.CreateProbeTask(ctx, model.ProbeTask{Direction: model.ProbeExitJury, JurorAccountID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimPendingProbeTasks(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.DB().Exec("CREATE TRIGGER fail_projection BEFORE INSERT ON q_probe_projection BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	result := model.ProbeTaskResult{Outcome: model.ProbeResultClean}
	if err := store.CompleteProbeTask(ctx, id, model.ProbeDone, result, time.Now()); err == nil {
		t.Fatal("result committed without projection intent")
	}
	var task qProbeTaskModel
	if err := r.DB().First(&task, id).Error; err != nil || task.State != "running" {
		t.Fatalf("partial completion: %+v %v", task, err)
	}
	if err := r.DB().Exec("DROP TRIGGER fail_projection").Error; err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteProbeTask(ctx, id, model.ProbeDone, result, time.Now()); err != nil {
		t.Fatal(err)
	}
	archive, err := evidence.New(ctx, r.DB(), model.DefaultEvidenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	// Persist a projection, then fail before acknowledging it, as on a crash.
	if _, err := store.ProcessProbeProjections(ctx, 1, func(ctx context.Context, obs model.Observation) error {
		if err := archive.Record(ctx, obs); err != nil {
			return err
		}
		return errors.New("lost acknowledgement")
	}); err == nil {
		t.Fatal("fault invisible")
	}
	if err := r.DB().Model(&qProbeProjectionModel{}).Where("task_id = ?", id).Update("ready_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	// The task/case may already have been retained elsewhere or removed; the
	// pending payload is independently replayable and needs no physical call.
	if err := r.DB().Delete(&qProbeTaskModel{}, id).Error; err != nil {
		t.Fatal(err)
	}
	restarted := NewProbeTaskStore(r)
	if n, err := restarted.ProcessProbeProjections(ctx, 1, archive.Record); err != nil || n != 1 {
		t.Fatalf("recovery=%d %v", n, err)
	}
	if count, err := archive.Count(ctx); err != nil || count != 1 {
		t.Fatalf("duplicate/missing projection: %d %v", count, err)
	}
	if n, err := restarted.ProcessProbeProjections(ctx, 1, archive.Record); err != nil || n != 0 {
		t.Fatalf("ack missing: %d %v", n, err)
	}
}

func TestUpgradeRepairsMissingProjectionWithoutDuplicatingLegacyObservation(t *testing.T) {
	r := newReconcileRegistry(t)
	ctx := context.Background()
	store := NewProbeTaskStore(r)
	archive, err := evidence.New(ctx, r.DB(), model.DefaultEvidenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := uint64(1); i <= 2; i++ {
		task := model.ProbeTask{ID: i, Direction: model.ProbeExitJury, JurorAccountID: i}
		if err := r.DB().Create(&qProbeTaskModel{ID: i, Direction: "exit_jury", JurorAccountID: i, State: "done", Result: "clean", CreatedAt: now, UpdatedAt: now, FinishedAt: &now}).Error; err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			obs := model.ProbeObservation(task, model.ProbeTaskResult{Outcome: model.ProbeResultClean}, now)
			obs.EventID = "" // Pre-upgrade projections did not have a stable ID.
			if err := archive.Record(ctx, obs); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := store.ProcessProbeProjections(ctx, 100, archive.Record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProcessProbeProjections(ctx, 100, archive.Record); err != nil {
		t.Fatal(err)
	}
	if count, err := archive.Count(ctx); err != nil || count != 2 {
		t.Fatalf("upgrade count=%d err=%v", count, err)
	}
}
