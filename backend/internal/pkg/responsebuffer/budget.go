// Package responsebuffer accounts for retained response bytes across a request
// and a process. Reservations are taken before allocating backing arrays.
package responsebuffer

import (
	"context"
	"errors"
	"sync"
)

const (
	DefaultRequestLimit = 96 << 20
	DefaultProcessLimit = 512 << 20
	JSONLimit           = 32 << 20
)

var (
	ErrExhausted = errors.New("response resource budget exhausted")
	ErrLimit     = errors.New("response buffer size limit exceeded")
	processPool  = NewPool(DefaultProcessLimit)
)

type Snapshot struct {
	Limit    int64  `json:"limit_bytes"`
	Used     int64  `json:"used_bytes"`
	Peak     int64  `json:"peak_bytes"`
	Rejected uint64 `json:"rejected"`
}

type Pool struct {
	mu    sync.Mutex
	state Snapshot
}

func NewPool(limit int64) *Pool    { return &Pool{state: Snapshot{Limit: limit}} }
func (p *Pool) Snapshot() Snapshot { p.mu.Lock(); defer p.mu.Unlock(); return p.state }
func ProcessSnapshot() Snapshot    { return processPool.Snapshot() }

type Budget struct {
	pool  *Pool
	state Snapshot // guarded by pool.mu, including every lease on this budget
}

func (p *Pool) Request(limit int64) *Budget { return &Budget{pool: p, state: Snapshot{Limit: limit}} }
func NewRequest() *Budget                   { return processPool.Request(DefaultRequestLimit) }

// NewLimitedRequest reserves from the same process pool with a smaller
// per-operation ceiling, for bounded background measurements.
func NewLimitedRequest(limit int64) *Budget {
	if limit <= 0 || limit > DefaultRequestLimit {
		limit = DefaultRequestLimit
	}
	return processPool.Request(limit)
}
func (b *Budget) Snapshot() Snapshot { b.pool.mu.Lock(); defer b.pool.mu.Unlock(); return b.state }

type contextKey struct{}

func WithContext(ctx context.Context, b *Budget) context.Context {
	return context.WithValue(ctx, contextKey{}, b)
}

// WithRequestLimit establishes an operation's retained-memory allowance only
// when the caller has not already supplied one. The process pool remains the
// hard aggregate limit; a nested operation cannot relax its caller's budget.
func WithRequestLimit(ctx context.Context, limit int64) context.Context {
	if b, ok := ctx.Value(contextKey{}).(*Budget); ok && b != nil {
		return ctx
	}
	if limit <= 0 {
		limit = DefaultRequestLimit
	}
	return WithContext(ctx, processPool.Request(min(limit, DefaultProcessLimit)))
}
func FromContext(ctx context.Context) *Budget {
	if b, ok := ctx.Value(contextKey{}).(*Budget); ok && b != nil {
		return b
	}
	return NewRequest()
}

type Lease struct {
	budget *Budget
	size   int64
}

func (b *Budget) Reserve(n int) (*Lease, error) {
	if b == nil {
		b = NewRequest()
	}
	b.pool.mu.Lock()
	defer b.pool.mu.Unlock()
	if n < 0 || int64(n) > b.state.Limit-b.state.Used || int64(n) > b.pool.state.Limit-b.pool.state.Used {
		b.state.Rejected++
		b.pool.state.Rejected++
		return nil, ErrExhausted
	}
	b.state.Used += int64(n)
	b.pool.state.Used += int64(n)
	b.state.Peak = max(b.state.Peak, b.state.Used)
	b.pool.state.Peak = max(b.pool.state.Peak, b.pool.state.Used)
	return &Lease{budget: b, size: int64(n)}, nil
}
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.budget.pool.mu.Lock()
	defer l.budget.pool.mu.Unlock()
	l.budget.state.Used -= l.size
	l.budget.pool.state.Used -= l.size
	l.size = 0
}
