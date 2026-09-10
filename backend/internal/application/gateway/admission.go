package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

var errAdmissionDeadline = errors.New("response admission deadline exceeded")

// admission owns only the wait before delivery. Commit and expiry serialize on
// one lock, so a timer cannot cancel a stream after delivery has been committed.
// The parent request still owns the lifetime of an admitted stream.
type admission struct {
	ctx       context.Context
	cancel    context.CancelCauseFunc
	mu        sync.Mutex
	started   time.Time
	deadline  time.Time
	timer     *time.Timer
	committed bool
}

func newAdmission(parent context.Context, started time.Time, budget time.Duration) *admission {
	ctx, cancel := context.WithCancelCause(parent)
	a := &admission{ctx: ctx, cancel: cancel, started: started}
	a.setBudget(budget)
	return a
}

// Normalizers publish the actual tool profile before network I/O. A revision
// changes the deadline measured from request start, never starts a fresh budget.
func (a *admission) setBudget(budget time.Duration) {
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
			a.cancel(errAdmissionDeadline)
		}
	})
}

func (a *admission) disable() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deadline = time.Time{}
	if a.timer != nil {
		a.timer.Stop()
	}
}

func (a *admission) commit() error {
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

func (a *admission) failure() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failureLocked()
}

func (a *admission) failureLocked() error {
	if !a.committed && !a.deadline.IsZero() && !time.Now().Before(a.deadline) {
		a.cancel(errAdmissionDeadline)
	}
	if errors.Is(context.Cause(a.ctx), errAdmissionDeadline) {
		return &UpstreamFailure{HTTPStatus: http.StatusGatewayTimeout, Code: "quality_admission_timeout",
			PublicMessage: "等待上游响应超过准入时限，请稍后重试", Cause: errAdmissionDeadline}
	}
	if a.ctx.Err() != nil {
		return a.ctx.Err()
	}
	return nil
}

func (a *admission) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timer != nil {
		a.timer.Stop()
	}
	a.cancel(context.Canceled)
}
