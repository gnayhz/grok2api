// Package browsertransport owns connection scheduling for browser HTTP traffic.
// The wire implementation and fingerprints stay with fhttp/uTLS; no network
// operation runs under a transport or origin state lock.
package browsertransport

import (
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptrace"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/tls-client/profiles"
	tls "github.com/bogdanfinn/utls"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

var ErrOriginCapacity = fmt.Errorf("browser transport origin capacity exhausted: %w", netbudget.ErrCapacity)

type DialContextFunc func(context.Context, string, string) (net.Conn, error)

type Config struct {
	Profile          profiles.ClientProfile
	DialContext      DialContextFunc
	RootCAs          *x509.CertPool
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	WriteTimeout     time.Duration
	MaxOrigins       int
}

type Transport struct {
	cfg      Config
	sessions tls.ClientSessionCache
	mu       sync.Mutex
	origins  map[string]*origin
}

type origin struct {
	owner   *Transport
	address string
	secure  bool
	// active/lastUsed are owned by Transport.mu, the remaining fields by mu.
	active   int
	lastUsed time.Time
	mu       sync.Mutex
	pending  chan struct{}
	h1       *fhttp.Transport
	h2       *h2Pool
	first    net.Conn
}

func New(cfg Config) *Transport {
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 90 * time.Second
	}
	if cfg.MaxOrigins <= 0 {
		cfg.MaxOrigins = 128
	}
	if cfg.DialContext == nil {
		d := &net.Dialer{Timeout: cfg.HandshakeTimeout, KeepAlive: 30 * time.Second}
		cfg.DialContext = d.DialContext
	}
	return &Transport{cfg: cfg, sessions: tls.NewLRUClientSessionCache(128), origins: make(map[string]*origin)}
}

func (t *Transport) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, t.cfg.HandshakeTimeout)
	defer cancel()
	return t.cfg.DialContext(ctx, network, address)
}

// DialWebSocketTLS forces HTTP/1.1 ALPN, while preserving the TLS profile.
func (t *Transport) DialWebSocketTLS(ctx context.Context, network, address string) (net.Conn, error) {
	conn, finish, err := t.handshake(ctx, network, address, true)
	if err != nil {
		return nil, err
	}
	if err = finish(); err != nil {
		return nil, err
	}
	return &h1WireConn{UConn: conn}, nil
}

// handshake leaves its establishment deadline armed until finish, so HTTP/2
// preface writes are covered too. finish removes it before any response stream.
func (t *Transport) handshake(parent context.Context, network, address string, forceH1 bool) (*tls.UConn, func() error, error) {
	ctx, cancel := context.WithTimeout(parent, t.cfg.HandshakeTimeout)
	raw, err := t.cfg.DialContext(ctx, network, address)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	deadline, _ := ctx.Deadline()
	if err = raw.SetDeadline(deadline); err != nil {
		cancel()
		_ = raw.Close()
		return nil, nil, err
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = raw.Close(); close(stopped) })
	finish := func() error {
		if !stop() {
			<-stopped
		}
		err := ctx.Err()
		cancel()
		if err == nil {
			err = raw.SetDeadline(time.Time{})
		}
		if err != nil {
			_ = raw.Close()
		}
		return err
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		_ = raw.Close()
		_ = finish()
		return nil, nil, err
	}
	conn := tls.UClient(raw, &tls.Config{ServerName: host, RootCAs: t.cfg.RootCAs, ClientSessionCache: t.sessions, OmitEmptyPsk: true}, t.cfg.Profile.GetClientHelloId(), false, forceH1, true)
	trace := httptrace.ContextClientTrace(ctx)
	if trace != nil && trace.TLSHandshakeStart != nil {
		trace.TLSHandshakeStart()
	}
	err = conn.HandshakeContext(ctx)
	if trace != nil && trace.TLSHandshakeDone != nil {
		state := conn.ConnectionState()
		trace.TLSHandshakeDone(stdtls.ConnectionState{Version: state.Version, HandshakeComplete: state.HandshakeComplete, DidResume: state.DidResume, CipherSuite: state.CipherSuite, NegotiatedProtocol: state.NegotiatedProtocol, ServerName: state.ServerName, PeerCertificates: state.PeerCertificates, VerifiedChains: state.VerifiedChains}, err)
	}
	if err != nil {
		_ = raw.Close()
		if contextErr := finish(); contextErr != nil {
			err = contextErr
		}
		return nil, nil, err
	}
	return conn, finish, nil
}

func (t *Transport) acquire(u *url.URL) (*origin, error) {
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("unsupported browser request scheme")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	address := net.JoinHostPort(u.Hostname(), port)
	key := u.Scheme + "://" + address
	t.mu.Lock()
	o := t.origins[key]
	var evicted *origin
	if o == nil {
		if len(t.origins) >= t.cfg.MaxOrigins {
			var oldestKey string
			for k, candidate := range t.origins {
				if candidate.active == 0 && (evicted == nil || candidate.lastUsed.Before(evicted.lastUsed)) {
					oldestKey, evicted = k, candidate
				}
			}
			if evicted == nil {
				t.mu.Unlock()
				return nil, ErrOriginCapacity
			}
			delete(t.origins, oldestKey)
		}
		o = &origin{owner: t, address: address, secure: u.Scheme == "https"}
		t.origins[key] = o
	}
	o.active++
	o.lastUsed = time.Now()
	t.mu.Unlock()
	if evicted != nil {
		evicted.closeIdle()
	}
	return o, nil
}

func (t *Transport) release(o *origin) {
	t.mu.Lock()
	o.active--
	o.lastUsed = time.Now()
	t.mu.Unlock()
}

func (t *Transport) RoundTrip(req *fhttp.Request) (*fhttp.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	o, err := t.acquire(req.URL)
	if err != nil {
		return nil, err
	}
	rt, err := o.transport(req.Context())
	if err != nil {
		t.release(o)
		return nil, err
	}
	res, err := rt.RoundTrip(req)
	if err != nil {
		t.release(o)
		return nil, err
	}
	res.Body = newReleaseBody(req.Context(), res.Body, func() { t.release(o) }, nil)
	return res, nil
}

func (o *origin) transport(ctx context.Context) (fhttp.RoundTripper, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		o.mu.Lock()
		if o.h1 != nil {
			rt := o.h1
			o.mu.Unlock()
			return rt, nil
		}
		if o.h2 != nil {
			rt := o.h2
			o.mu.Unlock()
			return rt, nil
		}
		if pending := o.pending; pending != nil {
			o.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-pending:
				continue
			}
		}
		pending := make(chan struct{})
		o.pending = pending
		o.mu.Unlock()
		h1, h2, first, err := o.initialize(ctx)
		o.mu.Lock()
		o.h1, o.h2, o.first = h1, h2, first
		o.pending = nil
		close(pending)
		o.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
}

func (o *origin) initialize(ctx context.Context) (*fhttp.Transport, *h2Pool, net.Conn, error) {
	if !o.secure {
		return o.newH1(), nil, nil, nil
	}
	conn, finish, err := o.owner.handshake(ctx, "tcp", o.address, false)
	if err != nil {
		return nil, nil, nil, err
	}
	if conn.ConnectionState().NegotiatedProtocol == "h2" {
		pool := newH2Pool(o.owner, o.address)
		wire := newH2WireConn(conn, o.owner.cfg.WriteTimeout)
		cc, err := pool.transport.NewClientConn(wire)
		finishErr := finish()
		if err == nil {
			err = finishErr
		}
		if err != nil {
			_ = conn.Close()
			return nil, nil, nil, err
		}
		wire.established.Store(true)
		pool.add(cc, wire)
		return nil, pool, nil, nil
	}
	if err := finish(); err != nil {
		return nil, nil, nil, err
	}
	return o.newH1(), nil, &h1WireConn{UConn: conn}, nil
}

func (o *origin) newH1() *fhttp.Transport {
	return &fhttp.Transport{
		DialContext: o.owner.DialContext,
		DialTLSContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			o.mu.Lock()
			conn := o.first
			o.first = nil
			o.mu.Unlock()
			if conn != nil {
				return conn, nil
			}
			return o.owner.DialWebSocketTLS(ctx, network, address)
		},
		TLSNextProto:    make(map[string]func(string, *tls.Conn) fhttp.RoundTripper),
		IdleConnTimeout: o.owner.cfg.IdleTimeout,
		MaxIdleConns:    64, MaxIdleConnsPerHost: 64, MaxConnsPerHost: 256,
	}
}

func (t *Transport) CloseIdleConnections() {
	t.mu.Lock()
	origins := make([]*origin, 0, len(t.origins))
	for _, o := range t.origins {
		origins = append(origins, o)
	}
	t.mu.Unlock()
	for _, o := range origins {
		o.closeIdle()
	}
}

func (o *origin) closeIdle() {
	o.mu.Lock()
	h1, h2, first := o.h1, o.h2, o.first
	o.first = nil
	o.mu.Unlock()
	if first != nil {
		_ = first.Close()
	}
	if h1 != nil {
		h1.CloseIdleConnections()
	}
	if h2 != nil {
		h2.closeIdle()
	}
}

type releaseBody struct {
	closeOnce sync.Once
	closeErr  error
	stop      atomic.Pointer[func() bool]
	io.ReadCloser
	once        sync.Once
	release     func()
	beforeClose func()
}

func (b *releaseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.finish()
	}
	return n, err
}

func (b *releaseBody) Close() error {
	b.closeOnce.Do(func() {
		if b.beforeClose != nil {
			b.beforeClose()
		}
		b.closeErr = b.ReadCloser.Close()
		b.finish()
	})
	return b.closeErr
}
func (b *releaseBody) finish() {
	b.once.Do(func() {
		if stop := b.stop.Load(); stop != nil {
			(*stop)()
		}
		b.release()
	})
}
func newReleaseBody(ctx context.Context, body io.ReadCloser, release, beforeClose func()) *releaseBody {
	b := &releaseBody{ReadCloser: body, release: release, beforeClose: beforeClose}
	stop := context.AfterFunc(ctx, func() { _ = b.Close() })
	b.stop.Store(&stop)
	return b
}

// Raw close remains interruptible even while TLS is writing a record.
type h1WireConn struct{ *tls.UConn }

func (c *h1WireConn) Close() error { return c.NetConn().Close() }
