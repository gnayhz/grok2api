package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer serializes the slog writer (task goroutine) against readers
// polling the captured output (test goroutine).
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A task still in flight when shutdown arrives surfaces context.Canceled from
// its blocked I/O. That is lifecycle noise, not a task failure: it must not be
// logged as <name>_failed.
func TestRunPeriodicTaskSuppressesShutdownCancellation(t *testing.T) {
	output := &lockedBuffer{}
	app := &Application{logger: slog.New(slog.NewTextHandler(output, nil))}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		app.runPeriodicTask(ctx, time.Hour, "demo_task", func(runCtx context.Context) error {
			// Model a call blocked on I/O until the lifecycle context tears down.
			<-runCtx.Done()
			return runCtx.Err()
		})
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runPeriodicTask did not return after context cancellation")
	}

	if strings.Contains(output.String(), "demo_task_failed") {
		t.Fatalf("shutdown cancellation must not be logged as task failure, got: %s", output.String())
	}
}

func awaitLog(t *testing.T, output *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(output.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("expected log %q, got: %s", want, output.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Per-run failures that are not lifecycle cancellation stay visible.
func TestRunPeriodicTaskLogsGenuineFailures(t *testing.T) {
	output := &lockedBuffer{}
	app := &Application{logger: slog.New(slog.NewTextHandler(output, nil))}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		app.runPeriodicTask(ctx, time.Millisecond, "demo_task", func(runCtx context.Context) error {
			return errors.New("disk full")
		})
		close(done)
	}()

	awaitLog(t, output, "demo_task_failed")
	cancel()
	<-done
}

// A task whose own per-run deadline expires still reports the failure even
// though the parent lifecycle context may also be winding down.
func TestRunPeriodicTaskKeepsDeadlineFailures(t *testing.T) {
	output := &lockedBuffer{}
	app := &Application{logger: slog.New(slog.NewTextHandler(output, nil))}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		app.runPeriodicTask(ctx, time.Millisecond, "demo_task", func(runCtx context.Context) error {
			return context.DeadlineExceeded
		})
		close(done)
	}()

	awaitLog(t, output, "demo_task_failed")
	cancel()
	<-done
}
