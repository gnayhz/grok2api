package inference

import (
	"context"
	"errors"
	"sync"
)

var ErrAttemptBudget = errors.New("physical attempt limit exhausted")

// AttemptBudget is owned by one logical execution. Reservations let a caller
// establish capacity before a necessary state transition. Only unconsumed
// reservations may be returned; an actual transport submission spends its slot.
type AttemptBudget struct {
	mu               sync.Mutex
	limit, allocated int
	closed           bool
}

func NewAttemptBudget(limit int) *AttemptBudget {
	if limit < 1 {
		limit = 1
	}
	return &AttemptBudget{limit: limit}
}

type AttemptPermit struct {
	budget             *AttemptBudget
	consumed, released bool
}

func (b *AttemptBudget) Reserve(ctx context.Context) (*AttemptPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b == nil {
		return nil, ErrAttemptBudget
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.allocated >= b.limit {
		return nil, ErrAttemptBudget
	}
	b.allocated++
	return &AttemptPermit{budget: b}, nil
}

// Consume returns false for a permit already used by an earlier transport call;
// a connection retry then needs a fresh slot from the same logical budget.
func (p *AttemptPermit) Consume(expected *AttemptBudget) (bool, error) {
	if p != nil && p.budget != expected {
		return false, ErrAttemptBudget
	}
	if p == nil || p.budget == nil {
		return false, nil
	}
	b := p.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.released || b.closed {
		return false, ErrAttemptBudget
	}
	if p.consumed {
		return false, nil
	}
	p.consumed = true
	return true, nil
}
func (p *AttemptPermit) Release() {
	if p == nil || p.budget == nil {
		return
	}
	b := p.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if !p.consumed && !p.released {
		p.released = true
		b.allocated--
	}
}
func (b *AttemptBudget) Close() {
	if b != nil {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
	}
}
func (b *AttemptBudget) Remaining() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0
	}
	return b.limit - b.allocated
}
