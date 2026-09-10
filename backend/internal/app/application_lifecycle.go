package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	httpmiddleware "github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
)

const (
	shutdownDrainTimeout = 15 * time.Second
	shutdownJoinTimeout  = 10 * time.Second
)

func (a *Application) beginRun(ctx context.Context) (context.Context, error) {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if a.closing || a.runDone != nil {
		return nil, errors.New("application can only run once before Close")
	}
	ctx, a.runCancel = context.WithCancel(ctx)
	a.runDone = make(chan struct{})
	return ctx, nil
}

func (a *Application) drainHTTP() error {
	if a.requests == nil {
		return nil
	}
	started := time.Now()
	a.requests.beginDrain()
	ctx, cancel := context.WithTimeout(context.Background(), a.drainTimeout())
	err := a.server.Shutdown(ctx)
	if err == nil {
		err = waitStopped(ctx, a.requests.done)
	}
	cancel()
	// Cancellation and closing sockets are separate: a blocked HTTP write or a
	// hijacked read need not observe request cancellation until the socket closes.
	a.requests.force()
	closeErr := a.server.Close()
	if errors.Is(err, context.DeadlineExceeded) {
		a.logger.Warn("server_shutdown_drain_timeout", "error", err)
		err = nil // An expired grace period is expected; an unfinished handler is not.
	}
	joinCtx, joinCancel := context.WithTimeout(context.Background(), a.joinTimeout())
	defer joinCancel()
	if joinErr := waitStopped(joinCtx, a.requests.done); joinErr != nil {
		return errors.Join(err, closeErr, fmt.Errorf("wait for HTTP completion before closing dependencies: %w", joinErr))
	}
	a.logger.Info("server_stopped", "drain_ms", time.Since(started).Milliseconds())
	httpmiddleware.FlushAsyncAccessLogs()
	return errors.Join(err, closeErr)
}

// Close may be called while Run is active. It first requests the same drain as
// SIGTERM, then proves every producer stopped before touching its dependencies.
// A timeout preserves storage and allows Close to be retried after the producer
// releases its outstanding work.
func (a *Application) stopRun() error {
	a.lifecycleMu.Lock()
	a.closing = true
	cancelRun, runDone := a.runCancel, a.runDone
	a.lifecycleMu.Unlock()
	if cancelRun == nil {
		return nil // A constructed but never started (or partial) application.
	}
	cancelRun()
	// HTTP completion, Run workers, detached model sync and key display writes
	// each have their own join budget after the HTTP grace period.
	ctx, cancel := context.WithTimeout(context.Background(), a.drainTimeout()+4*a.joinTimeout())
	defer cancel()
	if err := waitStopped(ctx, runDone); err != nil {
		return fmt.Errorf("wait for application Run before closing dependencies: %w", err)
	}
	if a.requests != nil {
		if err := waitStopped(ctx, a.requests.done); err != nil {
			return fmt.Errorf("HTTP handlers still own dependencies: %w", err)
		}
	}
	if err := waitStopped(ctx, a.backgroundDone); err != nil {
		return fmt.Errorf("background workers still own dependencies: %w", err)
	}
	if err := waitStopped(ctx, a.serverDone); err != nil {
		return fmt.Errorf("HTTP listener is still running: %w", err)
	}
	return nil
}

func (a *Application) drainTimeout() time.Duration {
	if a.httpDrainTimeout > 0 {
		return a.httpDrainTimeout
	}
	return shutdownDrainTimeout
}

func (a *Application) joinTimeout() time.Duration {
	if a.shutdownJoinBudget > 0 {
		return a.shutdownJoinBudget
	}
	return shutdownJoinTimeout
}

func (a *Application) closeModelSync() error {
	if a.models == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.joinTimeout())
	defer cancel()
	if err := a.models.Close(ctx); err != nil {
		return fmt.Errorf("wait for detached model sync before closing dependencies: %w", err)
	}
	return nil
}

func (a *Application) closeClientKeyTouches() error {
	if a.clientKeys == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.joinTimeout())
	defer cancel()
	if err := a.clientKeys.Close(ctx); err != nil {
		return fmt.Errorf("wait for client key usage touches before closing dependencies: %w", err)
	}
	return nil
}
