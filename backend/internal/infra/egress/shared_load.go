package egress

import (
	"context"
	"fmt"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

// sharedLoadGroup broadcasts completion through one channel per key. Canceled
// waiters leave no retained channel/list entry, including cancellation storms.
// The group bounds distinct in-flight keys before creating their goroutines.
type sharedLoadGroup struct {
	mu    sync.Mutex
	calls map[string]*sharedCall
}
type sharedCall struct {
	done  chan struct{}
	value any
	err   error
}

const maxSharedLoads = 64

func (g *sharedLoadGroup) start(key string, load func() (any, error)) (*sharedCall, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c := g.calls[key]; c != nil {
		return c, true, nil
	}
	if len(g.calls) >= maxSharedLoads {
		return nil, false, netbudget.ErrCapacity
	}
	if g.calls == nil {
		g.calls = make(map[string]*sharedCall)
	}
	c := &sharedCall{done: make(chan struct{})}
	g.calls[key] = c
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				c.err = fmt.Errorf("egress shared load panicked: %v", recovered)
			}
			g.mu.Lock()
			delete(g.calls, key)
			close(c.done)
			g.mu.Unlock()
		}()
		c.value, c.err = load()
	}()
	return c, false, nil
}
func (g *sharedLoadGroup) Do(key string, load func() (any, error)) (any, error, bool) {
	c, shared, err := g.start(key, load)
	if err != nil {
		return nil, err, false
	}
	<-c.done
	return c.value, c.err, shared
}
func waitSharedLoad(ctx context.Context, group *sharedLoadGroup, key string, load func() (any, error)) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, _, err := group.start(key, load)
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return c.value, c.err
	}
}
