package account

import (
	"context"
	"errors"
	"sync"
)

type accountOperation struct {
	value         any
	done          chan struct{}
	err           error
	ownerCanceled bool
}

// OperationGroup shares in-flight work within M07, including the initial
// account-sync pipeline. Each use case owns its key,
// result and commit policy. The owner executes synchronously, keeping Provider,
// SQL and lock cleanup in its caller's drain. Waiters cancel independently.
// Successful results remain valid when a use case has deliberately committed
// after cancellation (for example a rotated refresh token).
type OperationGroup[K comparable] struct {
	mu     sync.Mutex
	active map[K]*accountOperation
}

func (g *OperationGroup[K]) Do(ctx context.Context, key K, operation func() (any, error)) (any, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		g.mu.Lock()
		pending := g.active[key]
		if pending == nil {
			pending = &accountOperation{done: make(chan struct{})}
			if g.active == nil {
				g.active = make(map[K]*accountOperation)
			}
			g.active[key] = pending
			g.mu.Unlock()
			return g.run(ctx, key, pending, operation)
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
				continue
			}
			return pending.value, pending.err
		}
	}
}

func (g *OperationGroup[K]) run(ctx context.Context, key K, pending *accountOperation, operation func() (any, error)) (value any, err error) {
	completed := false
	defer func() {
		if !completed {
			// Panic/Goexit still releases waiters; the owner retains its normal
			// stack unwinding. Never put a Provider's panic payload in errors.
			pending.err = errors.New("account operation interrupted")
		}
		if completed && pending.err != nil && ctx.Err() != nil {
			// Only unsuccessful canceled owners need a new live executor.
			// A successful bounded commit remains the caller's known result.
			pending.ownerCanceled = true
			value, err = nil, ctx.Err()
		}
		g.mu.Lock()
		delete(g.active, key)
		close(pending.done)
		g.mu.Unlock()
	}()
	if err = ctx.Err(); err != nil {
		pending.err, completed = err, true
		return nil, err
	}
	pending.value, pending.err = operation()
	completed = true
	return pending.value, pending.err
}
