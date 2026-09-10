package clientkey

import (
	"context"
	"sync"
	"time"
)

const (
	keyTouchInterval       = time.Minute
	keyTouchTimeout        = 3 * time.Second
	touchTrackerMaxEntries = 10000
)

// touchTracker owns both throttling and the lifetime of last-used display writes.
// Admission and closing share one lock so Close cannot miss an accepted write.
type touchTracker struct {
	mu          sync.Mutex
	lastTouched map[uint64]time.Time
	active      map[*touchWork]struct{}
	closed      bool
	done        chan struct{}
}

type touchWork struct{ cancel context.CancelFunc }

func newTouchTracker() *touchTracker {
	return &touchTracker{lastTouched: make(map[uint64]time.Time), active: make(map[*touchWork]struct{}), done: make(chan struct{})}
}

func (c *touchTracker) deleteID(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.lastTouched, id)
}

func (c *touchTracker) deleteIDs(ids []uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		delete(c.lastTouched, id)
	}
}

func (c *touchTracker) start(ctx context.Context, id uint64, now time.Time) (context.Context, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, nil
	}
	if last := c.lastTouched[id]; !last.IsZero() && now.Sub(last) < keyTouchInterval {
		return nil, nil
	}
	c.lastTouched[id] = now
	if len(c.lastTouched) > touchTrackerMaxEntries {
		var oldestID uint64
		var oldest time.Time
		for candidateID, touchedAt := range c.lastTouched {
			if oldestID == 0 || touchedAt.Before(oldest) {
				oldestID, oldest = candidateID, touchedAt
			}
		}
		delete(c.lastTouched, oldestID)
	}
	// Caller disconnects do not cancel display updates; service shutdown does.
	touchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keyTouchTimeout)
	work := &touchWork{cancel: cancel}
	c.active[work] = struct{}{}
	return touchCtx, func() {
		cancel()
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.active, work)
		if c.closed && len(c.active) == 0 {
			close(c.done)
		}
	}
}

func (c *touchTracker) close(ctx context.Context) error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		for work := range c.active {
			work.cancel()
		}
		if len(c.active) == 0 {
			close(c.done)
		}
	}
	c.mu.Unlock()
	select {
	case <-c.done:
		return nil
	default:
	}
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops accepting and cancels noncritical last-used writes, then waits for
// their repository calls to return. A deadline preserves ownership: retry Close
// before releasing storage. Billing settlement and other synchronous operations
// remain available to producers still completing their own shutdown.
func (s *Service) Close(ctx context.Context) error { return s.touches.close(ctx) }
