package investigator

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type maintenanceStore interface {
	ReclaimRunningProbes(context.Context, string) (int64, error)
	ReclaimStaleRunningProbes(context.Context, time.Duration) (int64, error)
	CancelOrphanProbes(context.Context, string) (int64, error)
}

// Run owns queue recovery and execution. Reconciliation has its own loop so a
// slow measurement cannot prevent expired leases and closed cases converging.
// The caller owns cancellation and waits for every loop by waiting for Run.
func (s *Service) Run(ctx context.Context, executor Executor, logger *slog.Logger) error {
	return s.run(ctx, executor, logger, 30*time.Second)
}

func (s *Service) run(ctx context.Context, executor Executor, logger *slog.Logger, interval time.Duration) error {
	store, ok := s.store.(maintenanceStore)
	if !ok || executor == nil || s.recorder == nil {
		return errors.New("probe lifecycle dependencies unavailable")
	}
	if logger == nil {
		logger = slog.Default()
	}
	// Startup cancels only expired or ownerless legacy work, preserving leases
	// held by live peers. Failure prevents this worker from starting claims.
	startupCtx, startupCancel := context.WithTimeout(ctx, 5*time.Second)
	_, err := store.ReclaimRunningProbes(startupCtx, "worker lease expired")
	startupCancel()
	if err != nil {
		return err
	}
	reconcileProbes(ctx, store, logger)
	reconcileCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-reconcileCtx.Done():
				return
			case <-ticker.C:
				reconcileProbes(reconcileCtx, store, logger)
			}
		}
	}()
	defer func() { cancel(); <-done }()
	return s.RunWorkers(ctx, executor, 8, func(err error) {
		logger.Warn("quality_probe_worker_failed", "error", err.Error())
	})
}

func reconcileProbes(ctx context.Context, store maintenanceStore, logger *slog.Logger) {
	workCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	n, err := store.ReclaimStaleRunningProbes(workCtx, 5*time.Minute)
	cancel()
	if err != nil {
		logger.Warn("quality_probe_reclaim_failed", "error", err.Error())
	} else if n > 0 {
		logger.Warn("quality_probe_stale_reclaimed", "count", n)
	}
	workCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
	n, err = store.CancelOrphanProbes(workCtx, "案件已结,取证中止")
	cancel()
	if err != nil {
		logger.Warn("quality_probe_orphan_cancel_failed", "error", err.Error())
	} else if n > 0 {
		logger.Warn("quality_probe_orphans_cancelled", "count", n)
	}
}
