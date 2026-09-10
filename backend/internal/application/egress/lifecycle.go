package egress

import (
	"context"
	"sync"
	"time"
)

// backgroundWork bounds delayed confirmation tasks and joins them before the
// application's database is closed. Rotation's main worker is joined by Run.
type backgroundWork struct {
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	active  int
	closing bool
	changed chan struct{}
}

func (s *Service) background() *backgroundWork {
	s.backgroundOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.backgroundWork = &backgroundWork{ctx: ctx, cancel: cancel, changed: make(chan struct{})}
	})
	return s.backgroundWork
}
func (b *backgroundWork) after(delay time.Duration, work func(context.Context)) bool {
	b.mu.Lock()
	if b.closing || b.active >= 32 {
		b.mu.Unlock()
		return false
	}
	b.active++
	b.mu.Unlock()
	go func() {
		defer func() { b.mu.Lock(); b.active--; close(b.changed); b.changed = make(chan struct{}); b.mu.Unlock() }()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-b.ctx.Done():
			return
		case <-timer.C:
		}
		ctx, cancel := context.WithTimeout(b.ctx, time.Minute)
		defer cancel()
		work(ctx)
	}()
	return true
}
func (s *Service) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	b := s.background()
	b.mu.Lock()
	b.closing = true
	b.cancel()
	b.mu.Unlock()
	s.mu.RLock()
	rotation := s.rotation
	s.mu.RUnlock()
	if rotation != nil {
		rotation.clear()
	}
	for {
		b.mu.Lock()
		active, changed := b.active, b.changed
		b.mu.Unlock()
		if active == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
