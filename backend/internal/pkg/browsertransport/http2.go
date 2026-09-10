package browsertransport

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/http2"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

var ErrDrainingCapacity = fmt.Errorf("browser HTTP/2 draining connection capacity exhausted: %w", netbudget.ErrCapacity)

// h2Pool replaces fhttp's default pool, whose shared dial wait and DialTLS
// callback cannot honor individual request cancellation. The transport still
// implements HTTP/2 framing and its limited, protocol-safe retry policy.
type h2Pool struct {
	owner       *Transport
	address     string
	transport   *http2.Transport
	mu          sync.Mutex
	connections map[*http2.ClientConn]*h2Connection
	pending     chan struct{}
}

type h2Connection struct {
	client  *http2.ClientConn
	raw     net.Conn
	wire    *h2WireConn
	refs    int
	retired bool
}

type h2AttemptKey struct{}
type h2Attempt struct {
	pool   *h2Pool
	conn   atomic.Pointer[h2Connection]
	done   chan struct{}
	cancel context.CancelFunc
	stop   func() bool
	once   sync.Once
}

func newH2Pool(owner *Transport, address string) *h2Pool {
	p := &h2Pool{owner: owner, address: address, connections: make(map[*http2.ClientConn]*h2Connection)}
	profile := owner.cfg.Profile
	p.transport = &http2.Transport{
		ConnPool: p, StrictMaxConcurrentStreams: true,
		Settings: maps.Clone(profile.GetSettings()), SettingsOrder: slices.Clone(profile.GetSettingsOrder()),
		ConnectionFlow: profile.GetConnectionFlow(), HeaderPriority: profile.GetHeaderPriority(),
		PseudoHeaderOrder: slices.Clone(profile.GetPseudoHeaderOrder()), Priorities: slices.Clone(profile.GetPriorities()),
		InitialStreamID: profile.GetStreamID(), AllowHTTP: profile.GetAllowHTTP(),
		IdleConnTimeout: owner.cfg.IdleTimeout, ReadIdleTimeout: 20 * time.Second, PingTimeout: 10 * time.Second,
		PushHandler: &http2.DefaultPushHandler{},
	}
	return p
}

func (p *h2Pool) add(cc *http2.ClientConn, wire *h2WireConn) *h2Connection {
	c := &h2Connection{client: cc, raw: wire.raw, wire: wire}
	p.mu.Lock()
	p.connections[cc] = c
	p.mu.Unlock()
	return c
}

func (p *h2Pool) RoundTrip(req *fhttp.Request) (*fhttp.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	attempt := &h2Attempt{pool: p, done: make(chan struct{}), cancel: cancel}
	attempt.stop = context.AfterFunc(ctx, func() { attempt.interruptCanceledWrite() })
	req = req.WithContext(context.WithValue(ctx, h2AttemptKey{}, attempt))
	res, err := p.transport.RoundTrip(req)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		attempt.finish()
		return nil, err
	}
	res.Body = newReleaseBody(ctx, res.Body, attempt.finish, cancel)
	return res, nil
}

func (p *h2Pool) GetClientConn(req *fhttp.Request, address string) (*http2.ClientConn, error) {
	if address != p.address {
		return nil, errors.New("HTTP/2 origin mismatch")
	}
	attempt, ok := req.Context().Value(h2AttemptKey{}).(*h2Attempt)
	if !ok {
		return nil, errors.New("HTTP/2 request has no connection owner")
	}
	// A second pool lookup is a protocol-approved retry; retire the previous
	// connection so a GOAWAY/unusable connection cannot be selected again.
	attempt.release(true)
	for {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		var discarded []net.Conn
		for cc, c := range p.connections {
			if !c.retired && (!req.Close || c.refs == 0) {
				c.refs++
				if req.Close {
					c.retired = true
				}
				attempt.conn.Store(c)
				p.mu.Unlock()
				for _, raw := range discarded {
					_ = raw.Close()
				}
				return cc, nil
			}
			if c.refs == 0 {
				delete(p.connections, cc)
				discarded = append(discarded, c.raw)
			}
		}
		if pending := p.pending; pending != nil {
			p.mu.Unlock()
			for _, raw := range discarded {
				_ = raw.Close()
			}
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-pending:
				continue
			}
		}
		// Old connections may be draining after GOAWAY. Keep that category
		// bounded even when many response bodies remain open.
		if len(p.connections) >= 8 {
			p.mu.Unlock()
			for _, raw := range discarded {
				_ = raw.Close()
			}
			return nil, ErrDrainingCapacity
		}
		pending := make(chan struct{})
		p.pending = pending
		p.mu.Unlock()
		for _, raw := range discarded {
			_ = raw.Close()
		}
		conn, finish, err := p.owner.handshake(req.Context(), "tcp", p.address, false)
		var cc *http2.ClientConn
		var wire *h2WireConn
		if err == nil {
			if conn.ConnectionState().NegotiatedProtocol != "h2" {
				err = errors.New("HTTP/2 origin changed negotiated protocol")
			} else {
				wire = newH2WireConn(conn, p.owner.cfg.WriteTimeout)
				cc, err = p.transport.NewClientConn(wire)
			}
			if finishErr := finish(); err == nil {
				err = finishErr
			}
			if err != nil {
				_ = conn.Close()
			}
		}
		p.mu.Lock()
		if err == nil {
			wire.established.Store(true)
			c := &h2Connection{client: cc, raw: conn.NetConn(), wire: wire, refs: 1, retired: req.Close}
			p.connections[cc] = c
			attempt.conn.Store(c)
		}
		p.pending = nil
		close(pending)
		p.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return cc, nil
	}
}

func (a *h2Attempt) finish() {
	a.once.Do(func() { a.stop(); close(a.done); a.cancel(); a.release(false) })
}

func (a *h2Attempt) release(retire bool) {
	c := a.conn.Swap(nil)
	if c == nil {
		return
	}
	p := a.pool
	p.mu.Lock()
	c.refs--
	c.retired = c.retired || retire
	closeConn := c.refs == 0 && c.retired
	if closeConn {
		delete(p.connections, c.client)
	}
	p.mu.Unlock()
	if closeConn {
		_ = c.raw.Close()
	}
}

func (p *h2Pool) MarkDead(cc *http2.ClientConn) {
	p.mu.Lock()
	c := p.connections[cc]
	closeConn := c != nil && c.refs == 0
	if c != nil {
		c.retired = true
		if closeConn {
			delete(p.connections, cc)
		}
	}
	p.mu.Unlock()
	if closeConn {
		_ = c.raw.Close()
	}
}

func (p *h2Pool) closeIdle() {
	p.mu.Lock()
	var conns []net.Conn
	for cc, c := range p.connections {
		if c.refs == 0 {
			delete(p.connections, cc)
			conns = append(conns, c.raw)
		}
	}
	p.mu.Unlock()
	// Closing the socket wakes the HTTP/2 reader without entering the
	// library's connection/write locks, which may be busy with another stream.
	for _, conn := range conns {
		_ = conn.Close()
	}
}
