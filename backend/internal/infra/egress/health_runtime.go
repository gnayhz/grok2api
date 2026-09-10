package egress

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const healthShardCount = 4
const healthEntriesPerShard = 512
const healthWriteTimeout = 2 * time.Second

type healthBinding struct {
	nodeID   uint64
	proxy    string
	revision uint64
}

type healthReport struct {
	binding     healthBinding
	baseline    domain.HealthState
	scope       domain.Scope
	observation domain.HealthObservation
	known       bool
	proxyPool   bool
}

type healthEntry struct {
	state    domain.HealthState
	known    bool
	pending  *healthReport
	working  bool
	sequence uint64
	expires  time.Time
	lru      *list.Element
}

type healthShard struct {
	mu      sync.Mutex
	entries map[healthBinding]*healthEntry
	queue   []healthBinding
	lru     list.List
	wake    chan struct{}
	changed chan struct{}
	done    chan struct{}
	started bool
}

// healthRuntime owns all request observation queues and local health overlays.
// Four persistent workers serialize each node's writes. Queue coalescing is
// bounded by binding count, independently of request rate or database latency.
type healthRuntime struct {
	hasOverlays atomic.Bool
	ctx         context.Context
	cancel      context.CancelFunc
	process     func(context.Context, healthReport) (domain.HealthState, error)
	invalidate  func()
	shards      [healthShardCount]healthShard
	dropped     atomic.Uint64
	writeErrors atomic.Uint64
}

func newHealthRuntime(process func(context.Context, healthReport) (domain.HealthState, error), invalidate func()) *healthRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	h := &healthRuntime{ctx: ctx, cancel: cancel, process: process, invalidate: invalidate}
	for i := range h.shards {
		s := &h.shards[i]
		s.entries = make(map[healthBinding]*healthEntry)
		s.wake = make(chan struct{}, 1)
		s.changed = make(chan struct{})
		s.done = make(chan struct{})
	}
	return h
}

func (h *healthRuntime) shard(id uint64) *healthShard { return &h.shards[id%healthShardCount] }

func (h *healthRuntime) overlay(node domain.Node) domain.Node {
	if h == nil || node.ID == 0 || !h.hasOverlays.Load() {
		return node
	}
	s := h.shard(node.ID)
	s.mu.Lock()
	e := s.entries[healthBinding{node.ID, node.EncryptedProxyURL, node.BindingRevision}]
	if e != nil && e.known && e.state.Revision >= node.HealthRevision && (e.pending != nil || e.working || time.Now().Before(e.expires)) {
		state := e.state
		// Local and persisted failures can independently advance to the same
		// revision. A pending failure overlay must not erase a cooldown read
		// from storage just because its predicted revision is as high.
		if state.FailureCount > 0 {
			if node.LastError == domain.LastErrorExitIPQuality || (node.CooldownUntil != nil && (state.CooldownUntil == nil || node.CooldownUntil.After(*state.CooldownUntil))) {
				state.CooldownUntil, state.LastError = node.CooldownUntil, node.LastError
			}
		}
		node = state.ApplyTo(node)
	}
	s.mu.Unlock()
	return node
}

func (h *healthRuntime) submit(r healthReport) {
	if h.ctx.Err() != nil {
		return
	}
	s := h.shard(r.binding.nodeID)
	s.mu.Lock()
	if h.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	e := s.entries[r.binding]
	if e == nil {
		if r.known && r.observation.Kind == domain.HealthSuccess && healthStateHealthy(r.baseline) {
			s.mu.Unlock()
			return
		}
		if len(s.entries) >= healthEntriesPerShard {
			var removed *list.Element
			for item := s.lru.Front(); item != nil; item = item.Next() {
				candidate := s.entries[item.Value.(healthBinding)]
				if !candidate.working && candidate.pending == nil {
					removed = item
					break
				}
			}
			if removed == nil {
				h.dropped.Add(1)
				s.mu.Unlock()
				return
			}
			delete(s.entries, removed.Value.(healthBinding))
			s.lru.Remove(removed)
		}
		e = &healthEntry{state: r.baseline, known: r.known}
		e.lru = s.lru.PushBack(r.binding)
		s.entries[r.binding] = e
	} else {
		s.lru.MoveToBack(e.lru)
		if r.known && (!e.known || r.baseline.Revision > e.state.Revision) {
			e.state, e.known = r.baseline, true
		}
	}
	if r.known && !r.proxyPool && r.binding.nodeID != 0 {
		if r.observation.Kind == domain.HealthSuccess && healthStateHealthy(e.state) {
			s.mu.Unlock()
			return
		}
		// Do not coalesce a recovery ahead of a failure still waiting to be
		// written. A later request can recover after the failure is persisted.
		if r.observation.Kind == domain.HealthSuccess && e.pending != nil && e.pending.observation.Kind != domain.HealthSuccess {
			s.mu.Unlock()
			return
		}
		next, accepted := e.state.Apply(r.observation)
		if !accepted {
			s.mu.Unlock()
			return
		}
		e.state = next
		h.hasOverlays.Store(true)
	}
	e.sequence++
	e.expires = time.Now().Add(nodeSnapshotTTL)
	if previous := e.pending; previous != nil {
		if previous.observation.Kind != domain.HealthSuccess && r.observation.Kind != domain.HealthSuccess {
			r.observation = r.observation.MergeFailures(previous.observation)
		}
	} else {
		s.queue = append(s.queue, r.binding)
	}
	e.pending = &r
	if !s.started {
		s.started = true
		go h.run(s)
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.mu.Unlock()
}

func healthStateHealthy(state domain.HealthState) bool {
	return state.Health >= 1 && state.FailureCount == 0 && state.CooldownUntil == nil && state.LastError == ""
}

func (h *healthRuntime) run(s *healthShard) {
	defer close(s.done)
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-s.wake:
		}
		for {
			s.mu.Lock()
			if len(s.queue) == 0 {
				s.mu.Unlock()
				break
			}
			key := s.queue[0]
			s.queue[0] = healthBinding{}
			s.queue = s.queue[1:]
			e := s.entries[key]
			r, sequence := *e.pending, e.sequence
			e.pending = nil
			e.working = true
			s.mu.Unlock()
			ctx, cancel := context.WithTimeout(h.ctx, healthWriteTimeout)
			state, err := h.process(ctx, r)
			cancel()
			if err != nil {
				h.writeErrors.Add(1)
			}
			// Invalidate before signaling Flush: readers after the barrier
			// must not receive a snapshot from before the completed write.
			h.invalidate()
			s.mu.Lock()
			e.working = false
			if e.sequence == sequence {
				if err == nil {
					e.state, e.known = state, true
				} else if errors.Is(err, repository.ErrConflict) {
					e.known = false
				}
				e.expires = time.Now().Add(nodeSnapshotTTL)
				if err != nil && e.known && e.state.CooldownUntil != nil && e.state.CooldownUntil.After(e.expires) {
					e.expires = *e.state.CooldownUntil
				}
			}
			close(s.changed)
			s.changed = make(chan struct{})
			s.mu.Unlock()
			if h.ctx.Err() != nil {
				return
			}
		}
	}
}

func (h *healthRuntime) flush(ctx context.Context) error {
	for i := range h.shards {
		s := &h.shards[i]
		for {
			s.mu.Lock()
			busy := len(s.queue) > 0
			if !busy {
				for _, e := range s.entries {
					if e.working {
						busy = true
						break
					}
				}
			}
			changed := s.changed
			s.mu.Unlock()
			if !busy {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-h.ctx.Done():
				return errors.New("egress health runtime closed")
			case <-changed:
			}
		}
	}
	return nil
}

func (h *healthRuntime) close(ctx context.Context) error {
	h.cancel()
	for i := range h.shards {
		s := &h.shards[i]
		s.mu.Lock()
		started := s.started
		s.mu.Unlock()
		if started {
			select {
			case <-s.done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		s.mu.Lock()
		s.queue = nil
		for _, e := range s.entries {
			if e.pending != nil {
				h.dropped.Add(1)
			}
			e.pending = nil
			e.working = false
		}
		close(s.changed)
		s.changed = make(chan struct{})
		s.mu.Unlock()
	}
	return nil
}

// FlushFeedback is a persistence barrier for maintenance and deterministic
// verification. Request delivery never calls it.
func (m *Manager) FlushFeedback(ctx context.Context) error { return m.health.flush(ctx) }
