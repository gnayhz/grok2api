package gateway

import (
	"context"
	"errors"
	"sync"
)

var errRoutingLoadInterrupted = errors.New("routing snapshot load interrupted")

type routingLoad struct {
	done          chan struct{}
	value         any
	err           error
	ownerCanceled bool
}

// routingLoadGroup only shares work in flight. The winning request runs the
// loader synchronously, so its caller also owns storage cleanup and application
// drain. Cache contents and invalidation remain Selector's responsibility.
type routingLoadGroup struct {
	mu    sync.Mutex
	loads map[string]*routingLoad
}

func (g *routingLoadGroup) Do(ctx context.Context, key string, loader func() (any, error)) (any, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		g.mu.Lock()
		pending := g.loads[key]
		if pending == nil {
			pending = &routingLoad{done: make(chan struct{})}
			if g.loads == nil {
				g.loads = make(map[string]*routingLoad)
			}
			g.loads[key] = pending
			g.mu.Unlock()
			return g.run(ctx, key, pending, loader)
		}
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pending.done:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if pending.ownerCanceled {
				// Re-elect a live caller after the canceled owner's loader has
				// exited. Never share another request's cancellation or detach
				// that request's unfinished work from application drain.
				continue
			}
			return pending.value, pending.err
		}
	}
}

func (g *routingLoadGroup) run(ctx context.Context, key string, pending *routingLoad, loader func() (any, error)) (value any, err error) {
	completed := false
	defer func() {
		if !completed {
			// Also runs on panic/Goexit: wake waiters without swallowing the
			// original panic or leaving this key permanently occupied.
			pending.err = errRoutingLoadInterrupted
		}
		if ctx.Err() != nil {
			pending.ownerCanceled = true
			value, err = nil, ctx.Err()
		}
		g.mu.Lock()
		delete(g.loads, key)
		close(pending.done)
		g.mu.Unlock()
	}()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	pending.value, pending.err = loader()
	completed = true
	return pending.value, pending.err
}
