package investigator

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

type slowFirstExecutor struct {
	started chan uint64
	release <-chan struct{}
}

func (e slowFirstExecutor) Execute(ctx context.Context, task model.ProbeTask) (model.ProbeTaskResult, error) {
	e.started <- task.ID
	if task.ID == 1 {
		select {
		case <-ctx.Done():
			return model.ProbeTaskResult{Outcome: model.ProbeResultError}, ctx.Err()
		case <-e.release:
		}
	}
	return model.ProbeTaskResult{Outcome: model.ProbeResultClean}, nil
}

func TestFreeProbeWorkerClaimsPastSlowTask(t *testing.T) {
	store := newMemStore()
	for range 5 {
		if _, err := store.CreateProbeTask(context.Background(), model.ProbeTask{Direction: model.ProbeExitJury}); err != nil {
			t.Fatal(err)
		}
	}
	svc := New(store, &memRecorder{})
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	started := make(chan uint64, 5)
	done := make(chan error, 1)
	go func() { done <- svc.RunWorkers(ctx, slowFirstExecutor{started: started, release: release}, 2, nil) }()
	defer func() { cancel(); <-done }()
	// Five tasks start while task 1 is still blocked. A two-task batch would
	// leave task 3 unclaimed until task 1 returned.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	seen := map[uint64]bool{}
	for len(seen) < 5 {
		select {
		case id := <-started:
			seen[id] = true
		case <-deadline.C:
			t.Fatalf("slow sibling blocked replenishment: %v", seen)
		}
	}
	close(release)
}
