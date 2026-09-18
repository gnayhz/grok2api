package admission

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrDeadline is the sentinel for an expired wait-before-delivery budget.
// Transport mapping to HTTP 504 quality_admission_timeout stays in gateway.
var ErrDeadline = errors.New("response admission deadline exceeded")

// Wait owns only the wait before delivery. Commit and expiry serialize on
// one lock, so a timer cannot cancel a stream after delivery has been committed.
type Wait struct {
	ctx       context.Context
	cancel    context.CancelCauseFunc
	mu        sync.Mutex
	started   time.Time
	deadline  time.Time
	timer     *time.Timer
	committed bool
}

func NewWait(parent context.Context, started time.Time, budget time.Duration) *Wait {
	ctx, cancel := context.WithCancelCause(parent)
	a := &Wait{ctx: ctx, cancel: cancel, started: started}
	a.SetBudget(budget)
	return a
}

func (a *Wait) Context() context.Context { return a.ctx }

// Remaining reports the pre-delivery budget without extending it or canceling
// an accepted stream. Parent deadlines may be shorter than the local budget.
func (a *Wait) Remaining() (time.Duration, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	deadline := a.deadline
	if a.committed {
		deadline = time.Time{}
	}
	if parent, ok := a.ctx.Deadline(); ok && (deadline.IsZero() || parent.Before(deadline)) {
		deadline = parent
	}
	if deadline.IsZero() {
		return 0, false
	}
	return max(0, time.Until(deadline)), true
}

func (a *Wait) SetBudget(budget time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.committed || a.ctx.Err() != nil {
		return
	}
	if a.timer != nil {
		a.timer.Stop()
	}
	if budget <= 0 {
		a.deadline = time.Time{}
		return
	}
	a.deadline = a.started.Add(budget)
	a.timer = time.AfterFunc(time.Until(a.deadline), func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if !a.committed && !a.deadline.IsZero() && !time.Now().Before(a.deadline) {
			a.cancel(ErrDeadline)
		}
	})
}

func (a *Wait) Disable() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deadline = time.Time{}
	if a.timer != nil {
		a.timer.Stop()
	}
}

func (a *Wait) Commit() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.failureLocked(); err != nil {
		return err
	}
	a.committed = true
	if a.timer != nil {
		a.timer.Stop()
	}
	return nil
}

func (a *Wait) Failure() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failureLocked()
}

func (a *Wait) failureLocked() error {
	if !a.committed && !a.deadline.IsZero() && !time.Now().Before(a.deadline) {
		a.cancel(ErrDeadline)
	}
	if errors.Is(context.Cause(a.ctx), ErrDeadline) {
		return ErrDeadline
	}
	if a.ctx.Err() != nil {
		return a.ctx.Err()
	}
	return nil
}

func (a *Wait) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timer != nil {
		a.timer.Stop()
	}
	a.cancel(context.Canceled)
}
