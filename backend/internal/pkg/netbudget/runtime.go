// Package netbudget owns process-local network limits and socket shutdown.
package netbudget

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var ErrCapacity = errors.New("egress runtime capacity exhausted")
var ErrClosed = errors.New("egress runtime closed")

// Limits are startup configuration. Zero values select safe defaults.
type Limits struct {
	Connections  int           `yaml:"maxConnections"`
	Dialing      int           `yaml:"maxDialing"`
	Requests     int           `yaml:"maxRequests"`
	Waiters      int           `yaml:"maxWaiters"`
	Clients      int           `yaml:"maxClients"`
	QueueTimeout time.Duration `yaml:"-"`
}

func (l Limits) Defaults() Limits {
	if l.Connections <= 0 {
		l.Connections = 512
	}
	if l.Dialing <= 0 {
		l.Dialing = 32
	}
	if l.Requests <= 0 {
		l.Requests = 2048
	}
	if l.Waiters <= 0 {
		l.Waiters = 512
	}
	if l.Clients <= 0 {
		l.Clients = 2048
	}
	if l.QueueTimeout <= 0 {
		l.QueueTimeout = 2 * time.Second
	}
	return l
}

type gate struct{ tokens chan struct{} }

func newGate(n int) gate { return gate{tokens: make(chan struct{}, n)} }
func (g *gate) try() bool {
	select {
	case g.tokens <- struct{}{}:
		return true
	default:
		return false
	}
}
func (g *gate) release() { <-g.tokens }

type Runtime struct {
	admissionDone                           chan struct{}
	admissionOnce                           sync.Once
	limits                                  Limits
	ctx                                     context.Context
	cancel                                  context.CancelFunc
	draining                                atomic.Bool
	connections, dialing, requests, clients gate
	waiting                                 atomic.Int64
	rejected                                atomic.Uint64
	mu                                      sync.Mutex
	sockets                                 map[*Conn]struct{}
	changed                                 chan struct{}
	pressure                                func()
}

type Stats struct {
	ActiveConnections       int    `json:"activeConnections"`
	IdleConnections         int    `json:"idleConnections"`
	EstablishingConnections int    `json:"establishingConnections"`
	Connections             int    `json:"connections"`
	Dialing                 int    `json:"dialing"`
	Requests                int    `json:"requests"`
	Clients                 int    `json:"clients"`
	Waiters                 int64  `json:"waiters"`
	Rejected                uint64 `json:"rejected"`
	Draining                bool   `json:"draining"`
	Limits                  Limits `json:"limits"`
}

func New(limits Limits) *Runtime {
	limits = limits.Defaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &Runtime{admissionDone: make(chan struct{}), limits: limits, ctx: ctx, cancel: cancel, connections: newGate(limits.Connections), dialing: newGate(limits.Dialing), requests: newGate(limits.Requests), clients: newGate(limits.Clients), sockets: make(map[*Conn]struct{}), changed: make(chan struct{})}
}
func (r *Runtime) SetPressureHandler(fn func()) { r.mu.Lock(); r.pressure = fn; r.mu.Unlock() }
func (r *Runtime) Stats() Stats {
	s := Stats{Connections: len(r.connections.tokens), Dialing: len(r.dialing.tokens), Requests: len(r.requests.tokens), Clients: len(r.clients.tokens), Waiters: r.waiting.Load(), Rejected: r.rejected.Load(), Draining: r.draining.Load(), Limits: r.limits}
	r.mu.Lock()
	for c := range r.sockets {
		if c.active.Load() > 0 {
			s.ActiveConnections++
		} else if c.ready.Load() {
			s.IdleConnections++
		}
	}
	r.mu.Unlock()
	s.EstablishingConnections = max(0, s.Connections-s.ActiveConnections-s.IdleConnections)
	return s
}

func (r *Runtime) signal() {
	r.mu.Lock()
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
}
func (r *Runtime) acquire(ctx context.Context, g *gate, pressure bool) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrCapacity, err)
	}
	if r.draining.Load() {
		return ErrClosed
	}
	if g.try() {
		if r.draining.Load() {
			g.release()
			return ErrClosed
		}
		return nil
	}
	if pressure {
		r.mu.Lock()
		fn := r.pressure
		r.mu.Unlock()
		if fn != nil {
			fn()
		}
	}
	if r.waiting.Add(1) > int64(r.limits.Waiters) {
		r.waiting.Add(-1)
		r.rejected.Add(1)
		return ErrCapacity
	}
	defer r.waiting.Add(-1)
	timer := time.NewTimer(r.limits.QueueTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		// The deadline expired waiting for local admission, before any proxy
		// observation. Preserve both the cause and the capacity classification.
		return errors.Join(ErrCapacity, ctx.Err())
	case <-r.admissionDone:
		return ErrClosed
	case <-timer.C:
		r.rejected.Add(1)
		return ErrCapacity
	case g.tokens <- struct{}{}:
		if r.draining.Load() {
			g.release()
			return ErrClosed
		}
		return nil
	}
}

// Client construction never waits for an unrelated active client to retire.
func (r *Runtime) TryClient() (func(), error) {
	if r.draining.Load() {
		return nil, ErrClosed
	}
	if !r.clients.try() {
		r.rejected.Add(1)
		return nil, ErrCapacity
	}
	var once sync.Once
	return func() { once.Do(func() { r.clients.release(); r.signal() }) }, nil
}

// BeginRequest owns a request through body completion, close or cancellation.
// It links shutdown cancellation without creating a goroutine per request.
func (r *Runtime) BeginRequest(parent context.Context) (context.Context, func(), error) {
	if err := r.acquire(parent, &r.requests, false); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	stopRuntime := context.AfterFunc(r.ctx, cancel)
	var once sync.Once
	finish := func() { once.Do(func() { stopRuntime(); cancel(); r.requests.release(); r.signal() }) }
	return ctx, finish, nil
}

type DialFunc func(context.Context, string, string) (net.Conn, error)

func (r *Runtime) Dial(ctx context.Context, dial DialFunc, network, address string) (net.Conn, error) {
	if err := r.acquire(ctx, &r.connections, true); err != nil {
		return nil, err
	}
	if err := r.acquire(ctx, &r.dialing, false); err != nil {
		r.connections.release()
		return nil, err
	}
	dialCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	raw, err := dial(dialCtx, network, address)
	stop()
	cancel()
	if err != nil {
		r.dialing.release()
		r.connections.release()
		r.signal()
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	if err := raw.SetDeadline(deadline); err != nil {
		_ = raw.Close()
		r.connections.release()
		r.dialing.release()
		r.signal()
		return nil, err
	}
	c := &Conn{Conn: raw, runtime: r}
	r.mu.Lock()
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		_ = raw.Close()
		r.dialing.release()
		r.connections.release()
		return nil, ErrClosed
	}
	r.sockets[c] = struct{}{}
	r.mu.Unlock()
	return c, nil
}

// Conn holds a socket permit until the real socket is closed, including idle,
// draining and WebSocket connections. The count is independent of cache size.
type Conn struct {
	established sync.Once
	ready       atomic.Bool
	active      atomic.Int64
	net.Conn
	runtime *Runtime
	once    sync.Once
	err     error
}

func (c *Conn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.established.Do(func() { c.runtime.dialing.release() })
		r := c.runtime
		r.mu.Lock()
		delete(r.sockets, c)
		close(r.changed)
		r.changed = make(chan struct{})
		r.mu.Unlock()
		r.connections.release()
	})
	return c.err
}

func (r *Runtime) BeginDrain() {
	r.draining.Store(true)
	r.admissionOnce.Do(func() { close(r.admissionDone) })
	r.signal()
}
func (r *Runtime) Drain(ctx context.Context) error {
	r.BeginDrain()
	for {
		r.mu.Lock()
		changed := r.changed
		r.mu.Unlock()
		if len(r.requests.tokens) == 0 && len(r.dialing.tokens) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
func (r *Runtime) Close() {
	r.BeginDrain()
	r.cancel()
	r.mu.Lock()
	sockets := make([]*Conn, 0, len(r.sockets))
	for c := range r.sockets {
		sockets = append(sockets, c)
	}
	r.mu.Unlock()
	for _, c := range sockets {
		_ = c.Close()
	}
}

// BeginWait shares the global waiting budget with connection/request queues.
// Cache hits do not use it; only shared loads and construction waits do.
func (r *Runtime) BeginWait() (func(), error) {
	if r.draining.Load() {
		return nil, ErrClosed
	}
	if r.waiting.Add(1) > int64(r.limits.Waiters) {
		r.waiting.Add(-1)
		r.rejected.Add(1)
		return nil, ErrCapacity
	}
	var once sync.Once
	return func() { once.Do(func() { r.waiting.Add(-1) }) }, nil
}

// MarkActive follows TLS wrappers to the actual owned socket. HTTP/2 requests
// increment independently; a multiplexed connection is idle only at zero.
func MarkActive(conn net.Conn) func() {
	for depth := 0; conn != nil && depth < 16; depth++ {
		if c, ok := conn.(*Conn); ok {
			c.established.Do(func() { _ = c.Conn.SetDeadline(time.Time{}); c.runtime.dialing.release(); c.runtime.signal() })
			c.ready.Store(true)
			c.active.Add(1)
			var once sync.Once
			return func() { once.Do(func() { c.active.Add(-1) }) }
		}
		if wrapped, ok := conn.(interface{ NetConn() net.Conn }); ok {
			conn = wrapped.NetConn()
		} else {
			break
		}
	}
	return func() {}
}
