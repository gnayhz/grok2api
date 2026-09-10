package investigator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

type lifecycleExecutor struct{ started chan uint64 }

func (e lifecycleExecutor) Execute(ctx context.Context, task model.ProbeTask) (model.ProbeTaskResult, error) {
	e.started <- task.ID
	<-ctx.Done()
	return model.ProbeTaskResult{Outcome: model.ProbeResultError}, ctx.Err()
}

func TestProbeLifecycleRecoveryAndIndependentReaper(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			opts := registry.Options{Driver: dialect, SQLitePath: filepath.Join(t.TempDir(), "lifecycle.db")}
			if dialect == "postgres" {
				opts.PostgresDSN = os.Getenv("TEST_POSTGRES_DSN")
				if opts.PostgresDSN == "" {
					t.Skip("isolated PostgreSQL DSN not set")
				}
			}
			ctx := context.Background()
			reg, err := registry.Open(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reg.Close() })
			peer, err := registry.Open(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = peer.Close() })
			first, second := registry.NewProbeTaskStore(reg), registry.NewProbeTaskStore(peer)
			caseID, err := reg.CreateCase(ctx, time.Now(), "{}")
			if err != nil {
				t.Fatal(err)
			}
			create := func(caseID uint64) uint64 {
				t.Helper()
				id, err := first.CreateProbeTask(ctx, model.ProbeTask{CaseID: caseID, Direction: model.ProbeExitJury})
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			live := create(caseID)
			if tasks, err := first.ClaimPendingProbeTasks(ctx, 1); err != nil || len(tasks) != 1 || tasks[0].ID != live {
				t.Fatalf("peer claim=%v err=%v", tasks, err)
			}
			expired := create(caseID)
			if _, err := first.ClaimPendingProbeTasks(ctx, 1); err != nil {
				t.Fatal(err)
			}
			if err := reg.DB().Table("q_probe_task").Where("id = ?", expired).Update("lease_until", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
				t.Fatal(err)
			}
			orphan := create(0)
			own := create(caseID)
			service := New(DefaultConfig(), second, &memRecorder{})
			runCtx, cancel := context.WithCancel(ctx)
			started := make(chan uint64, 8)
			done := make(chan error, 1)
			go func() {
				done <- service.run(runCtx, lifecycleExecutor{started}, slog.New(slog.NewTextHandler(io.Discard, nil)), 20*time.Millisecond)
			}()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(3 * time.Second):
					t.Error("probe loops outlived cancellation")
				}
			}()
			select {
			case id := <-started:
				if id != own {
					t.Fatalf("started invalid task %d", id)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("worker never started")
			}
			state := func(id uint64) string {
				t.Helper()
				var row struct{ State string }
				if err := reg.DB().Table("q_probe_task").Select("state").Where("id = ?", id).Take(&row).Error; err != nil {
					t.Fatal(err)
				}
				return row.State
			}
			if state(live) != string(model.ProbeRunning) || state(expired) != string(model.ProbeCancelled) || state(orphan) != string(model.ProbeCancelled) {
				t.Fatalf("startup states live=%s expired=%s orphan=%s", state(live), state(expired), state(orphan))
			}
			// Expire a peer while this worker's measurement stays blocked. Its
			// recovery must proceed independently of generation completion.
			if err := reg.DB().Table("q_probe_task").Where("id = ?", live).Update("lease_until", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for state(live) != string(model.ProbeCancelled) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if state(live) != string(model.ProbeCancelled) || state(own) != string(model.ProbeRunning) {
				t.Fatal("slow measurement blocked independent recovery")
			}
		})
	}
}

type blockingMaintenanceStore struct {
	*memStore
	startupErr error
	calls      atomic.Int32
	blocked    chan struct{}
	finished   chan struct{}
}

func (s *blockingMaintenanceStore) ReclaimRunningProbes(ctx context.Context, _ string) (int64, error) {
	if _, ok := ctx.Deadline(); !ok {
		return 0, errors.New("startup has no deadline")
	}
	return 0, s.startupErr
}
func (s *blockingMaintenanceStore) ReclaimStaleRunningProbes(ctx context.Context, _ time.Duration) (int64, error) {
	if _, ok := ctx.Deadline(); !ok {
		return 0, errors.New("recovery has no deadline")
	}
	if s.calls.Add(1) == 2 {
		close(s.blocked)
		<-ctx.Done()
		close(s.finished)
		return 0, ctx.Err()
	}
	return 0, nil
}
func (s *blockingMaintenanceStore) CancelOrphanProbes(ctx context.Context, _ string) (int64, error) {
	return 0, ctx.Err()
}

func TestProbeLifecycleWaitsForMaintenanceCancellation(t *testing.T) {
	store := &blockingMaintenanceStore{memStore: newMemStore(), blocked: make(chan struct{}), finished: make(chan struct{})}
	svc := New(DefaultConfig(), store, &memRecorder{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- svc.run(ctx, lifecycleExecutor{make(chan uint64, 1)}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	}()
	select {
	case <-store.blocked:
	case <-time.After(time.Second):
		t.Fatal("maintenance never entered")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance cancellation did not complete")
	}
	select {
	case <-store.finished:
	default:
		t.Fatal("Run returned before maintenance ended")
	}
}

func TestProbeLifecycleRejectsStartupRecoveryFailure(t *testing.T) {
	store := &blockingMaintenanceStore{memStore: newMemStore(), startupErr: errors.New("database unavailable")}
	svc := New(DefaultConfig(), store, &memRecorder{})
	if err := svc.Run(context.Background(), lifecycleExecutor{make(chan uint64, 1)}, nil); !errors.Is(err, store.startupErr) {
		t.Fatalf("startup failure=%v", err)
	}
	if store.calls.Load() != 0 {
		t.Fatal("workers started after failed startup recovery")
	}
}
