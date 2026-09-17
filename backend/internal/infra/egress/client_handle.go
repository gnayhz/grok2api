package egress

import (
	"context"
	"errors"
	"io"
	"net/http/httptrace"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"github.com/chenyme/grok2api/backend/internal/port/physical"
)

var ErrClientRetired = physical.ErrClientRetired

// clientHandle keeps a retired client's budget until its last request ends.
// Closing a cache entry only closes idle connections; active streams retain
// their owner and trigger a final idle close when they complete.
type clientHandle struct {
	leases        int
	registry      *clientRegistry
	client        requestClient
	releaseBudget func()
	mu            sync.Mutex
	refs          int
	retired       bool
	finalized     sync.Once
}

func (h *clientHandle) begin(ctx context.Context) (context.Context, func(), error) {
	callCtx, release, err := h.registry.network.BeginRequest(ctx)
	if err != nil {
		return nil, nil, err
	}
	h.mu.Lock()
	if h.retired && h.leases == 0 {
		h.mu.Unlock()
		release()
		return nil, nil, ErrClientRetired
	}
	h.refs++
	h.mu.Unlock()
	var connectionMu sync.Mutex
	connectionDone := func() {}
	connectionFinished := false
	callCtx = httptrace.WithClientTrace(callCtx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		connectionMu.Lock()
		if connectionFinished {
			connectionMu.Unlock()
			return
		}
		connectionDone()
		connectionDone = netbudget.MarkActive(info.Conn)
		connectionMu.Unlock()
	}})
	var once sync.Once
	finish := func() {
		once.Do(func() {
			connectionMu.Lock()
			connectionFinished = true
			connectionDone()
			connectionMu.Unlock()
			h.mu.Lock()
			h.refs--
			final := h.retired && h.refs == 0 && h.leases == 0
			h.mu.Unlock()
			if final {
				h.finalize()
			}
			release()
		})
	}
	stop := context.AfterFunc(callCtx, finish)
	return callCtx, func() { stop(); finish() }, nil
}
func (h *clientHandle) retire() {
	h.mu.Lock()
	h.retired = true
	final := h.refs == 0 && h.leases == 0
	h.mu.Unlock()
	if final {
		h.finalize()
	} else {
		h.client.CloseIdleConnections()
	}
}
func (h *clientHandle) finalize() {
	h.finalized.Do(func() { h.client.CloseIdleConnections(); h.registry.owned.Delete(h.client); h.releaseBudget() })
}

type completionBody struct {
	io.ReadCloser
	once   sync.Once
	finish func()
}

func (b *completionBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.finish)
	}
	return n, err
}
func (b *completionBody) Close() error { err := b.ReadCloser.Close(); b.once.Do(b.finish); return err }

func runtimeCapacityError(err error) bool {
	return IsPhysicalCallAdmissionError(err) || errors.Is(err, ErrPhysicalCallLimit) || errors.Is(err, netbudget.ErrCapacity) || errors.Is(err, netbudget.ErrClosed) || errors.Is(err, ErrClientRetired)
}

func (h *clientHandle) retainLease() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.retired {
		return false
	}
	h.leases++
	return true
}
func (h *clientHandle) releaseLease() {
	h.mu.Lock()
	h.leases--
	final := h.retired && h.leases == 0 && h.refs == 0
	h.mu.Unlock()
	if final {
		h.finalize()
	}
}
func (h *clientHandle) idle() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.leases == 0 && h.refs == 0
}
