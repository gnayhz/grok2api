package gateway

import (
	"context"
	"io"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
)

// attemptResources owns one physical call, including bodies returned with an
// error. Replacing a body transfers ownership through an idempotent wrapper;
// closing a converter, a replay body and the lease can never close raw I/O twice.
type attemptResources struct {
	mu     sync.Mutex
	body   io.ReadCloser
	cancel context.CancelFunc
	stop   func() bool
	closed bool
}

func newAttemptResources(parent context.Context) (context.Context, *attemptResources) {
	ctx, cancel := context.WithCancel(parent)
	r := &attemptResources{cancel: cancel}
	stop := context.AfterFunc(ctx, r.close)
	r.mu.Lock()
	if r.closed {
		stop()
	} else {
		r.stop = stop
	}
	r.mu.Unlock()
	return ctx, r
}

func (r *attemptResources) own(body io.ReadCloser) io.ReadCloser {
	if body == nil {
		return nil
	}
	b := &onceCloseBody{ReadCloser: body}
	r.mu.Lock()
	closed := r.closed
	if !closed {
		r.body = b
	}
	r.mu.Unlock()
	if closed {
		_ = b.Close()
	}
	return b
}

func (r *attemptResources) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	body, cancel, stop := r.body, r.cancel, r.stop
	r.body = nil
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
	if cancel != nil {
		cancel()
	}
	if body != nil {
		_ = body.Close()
	}
}

type onceCloseBody struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (b *onceCloseBody) Close() error {
	b.once.Do(func() { b.err = b.ReadCloser.Close() })
	return b.err
}

func (b *onceCloseBody) BorrowBytes() ([]byte, func(), bool) {
	return responsebuffer.Borrow(b.ReadCloser)
}

func (b *onceCloseBody) ResponseBudget() *responsebuffer.Budget {
	return responsebuffer.BudgetOf(b.ReadCloser)
}

func (b *onceCloseBody) CanonicalStream() *responseflow.Stream {
	return responseflow.FromReader(b.ReadCloser)
}

// For embedded gateway consumers, the first Read is the delivery boundary.
// HTTP callers commit explicitly before publishing status or response headers.
type admissionBody struct {
	io.ReadCloser
	commit func() error
}

func (b *admissionBody) Read(p []byte) (int, error) {
	if err := b.commit(); err != nil {
		return 0, err
	}
	return b.ReadCloser.Read(p)
}
