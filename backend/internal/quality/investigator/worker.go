package investigator

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

type projectionStore interface {
	ProcessProbeProjections(context.Context, int, func(context.Context, model.Observation) error) (int, error)
}

// RunWorkers continuously replenishes each free slot. Each task owns its own
// deadline; neither a slow sibling nor court review delays the next claim.
func (s *Service) RunWorkers(ctx context.Context, executor Executor, concurrency int, report func(error)) error {
	if executor == nil || s.store == nil || s.recorder == nil {
		return errors.New("probe worker dependencies unavailable")
	}
	if concurrency < 1 || concurrency > 8 {
		concurrency = 8
	}
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for ctx.Err() == nil {
				taskCtx, cancel := context.WithTimeout(ctx, model.ResourceCheckTimeout)
				count, err := s.runDue(taskCtx, executor, 1)
				cancel()
				if err != nil && ctx.Err() == nil && report != nil {
					report(err)
				}
				if count == 0 || err != nil {
					timer := time.NewTimer(time.Second)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}
		}()
	}
	workers.Wait()
	return nil
}

func (s *Service) heartbeat(ctx context.Context, id uint64, cancel context.CancelFunc) func() {
	store, ok := s.store.(interface {
		RenewProbeTask(context.Context, uint64) error
	})
	if !ok {
		return func() {}
	}
	leaseCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				writeCtx, writeCancel := context.WithTimeout(leaseCtx, 5*time.Second)
				err := store.RenewProbeTask(writeCtx, id)
				writeCancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return func() { stop(); <-done }
}

// RunProjections remains active when no new probes run. Restarting it retries
// committed result projections without repeating the physical experiment.
func (s *Service) RunProjections(ctx context.Context) error {
	store, ok := s.store.(projectionStore)
	if !ok {
		<-ctx.Done()
		return nil
	}
	for {
		workCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		count, err := store.ProcessProbeProjections(workCtx, 100, s.recorder.Record)
		cancel()
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if count == 100 {
			continue
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
