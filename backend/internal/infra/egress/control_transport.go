package egress

import (
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ManageHTTPTransport attaches a control-plane transport to the same runtime
// budgets without routing it as provider traffic. The caller configures its
// destination/proxy/redirect policy and closes the returned owner after use.
// The transport must be new and unused; its custom dial policy is preserved.
func (m *Manager) ManageHTTPTransport(ctx context.Context, tr *http.Transport) (http.RoundTripper, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	release, err := m.transport.network.TryClient()
	if err != nil {
		return nil, nil, err
	}
	dial := tr.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return m.transport.network.Dial(ctx, dial, network, address)
	}
	client := &http.Client{Transport: tr}
	h := &clientHandle{registry: m.transport, client: client, releaseBudget: release, leases: 1}
	m.transport.owned.Store(client, h)
	owner := &controlTransport{handle: h, inner: tr}
	// Admission may have raced shutdown after TryClient. An owner published
	// after registry closure is retired immediately and cannot leak its permit.
	if m.transport.closed.Load() {
		owner.close()
		return nil, nil, ErrRuntimeClosed
	}
	return owner, owner.close, nil
}

type controlTransport struct {
	handle *clientHandle
	inner  *http.Transport
	closed atomic.Bool
	once   sync.Once
}

func (t *controlTransport) close() {
	t.once.Do(func() {
		t.closed.Store(true)
		t.handle.retire()
		t.handle.releaseLease()
	})
}

func (t *controlTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.closed.Load() {
		return nil, ErrClientRetired
	}
	ctx, finish, err := t.handle.begin(req.Context())
	if err != nil {
		return nil, err
	}
	response, err := t.inner.RoundTrip(req.WithContext(ctx))
	if err != nil || response == nil || response.Body == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		finish()
		return response, err
	}
	response.Body = &completionBody{ReadCloser: response.Body, finish: finish}
	return response, nil
}
