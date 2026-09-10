package egress

import (
	"context"
	"errors"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

// Start enables periodic idle reclamation. Construction remains usable without
// Start for compatibility; every worker it creates is owned by Close.
func (m *Manager) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.transport.closed.Load() {
		return ErrRuntimeClosed
	}
	m.startOnce.Do(func() {
		m.tasks.start("idle_loop", func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-m.tasks.ctx.Done():
					return
				case <-ticker.C:
					m.transport.sweepIdle()
				}
			}
		})
	})
	return nil
}

func (m *Manager) BeginDrain() {
	if m == nil {
		return
	}
	m.transport.closed.Store(true)
	m.transport.network.BeginDrain()
	m.tasks.stop()
}

// Drain stops admission, cancels maintenance, then lets response bodies finish
// within the caller's deadline. It flushes accepted observations after requests.
func (m *Manager) Drain(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.BeginDrain()
	if err := m.transport.network.Drain(ctx); err != nil {
		return err
	}
	if err := m.tasks.wait(ctx); err != nil {
		return err
	}
	return m.health.flush(ctx)
}

// Close closes remaining real sockets and joins all owned writers before the
// application closes storage. It is safe to call repeatedly.
func (m *Manager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.BeginDrain()
	m.transport.network.Close()
	m.transport.close()
	taskErr := m.tasks.wait(ctx)
	flushErr := m.health.flush(ctx)
	healthErr := m.health.close(ctx)
	return errors.Join(taskErr, flushErr, healthErr)
}

type RuntimeStats struct {
	Network             netbudget.Stats `json:"network"`
	CachedClients       int             `json:"cachedClients"`
	RetiredClients      int             `json:"retiredClients"`
	Tasks               map[string]int  `json:"tasks"`
	RejectedTasks       uint64          `json:"rejectedTasks"`
	DroppedObservations uint64          `json:"droppedObservations"`
	HealthWriteErrors   uint64          `json:"healthWriteErrors"`
}

func (m *Manager) RuntimeStats() RuntimeStats {
	s := RuntimeStats{Network: m.transport.network.Stats(), Tasks: make(map[string]int), DroppedObservations: m.health.dropped.Load(), HealthWriteErrors: m.health.writeErrors.Load()}
	m.transport.clientMu.RLock()
	s.CachedClients = len(m.transport.clients)
	m.transport.clientMu.RUnlock()
	m.transport.owned.Range(func(_, value any) bool {
		h := value.(*clientHandle)
		h.mu.Lock()
		if h.retired {
			s.RetiredClients++
		}
		h.mu.Unlock()
		return true
	})
	m.tasks.mu.Lock()
	for k, v := range m.tasks.active {
		s.Tasks[k] = v
	}
	s.RejectedTasks = m.tasks.dropped
	m.tasks.mu.Unlock()
	return s
}
