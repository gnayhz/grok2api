package browsertransport

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	stdh2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/tls-client/profiles"
)

func trustedTransport(t *testing.T, server *httptest.Server, cfg Config) *Transport {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	cfg.Profile, cfg.RootCAs = profiles.Chrome_146, roots
	tr := New(cfg)
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

func TestRealTLSProtocolsCompressionTrailersAndReuse(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := "HTTP1"
		if h2 {
			name = "HTTP2"
		}
		t.Run(name, func(t *testing.T) {
			var connections atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if (r.ProtoMajor == 2) != h2 {
					t.Errorf("protocol %s", r.Proto)
				}
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Trailer", "X-Stream-End")
				zw := gzip.NewWriter(w)
				_, _ = zw.Write([]byte("healthy stream"))
				_ = zw.Close()
				w.Header().Set("X-Stream-End", "complete")
			}))
			server.EnableHTTP2 = h2
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			server.StartTLS()
			defer server.Close()
			tr := trustedTransport(t, server, Config{})
			for i := 0; i < 5; i++ {
				req, _ := fhttp.NewRequest(http.MethodGet, server.URL, nil)
				req.Header.Set("Accept-Encoding", "gzip")
				res, err := tr.RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(res.Body)
				_ = res.Body.Close()
				if err != nil || string(body) != "healthy stream" || !res.Uncompressed || res.Trailer.Get("X-Stream-End") != "complete" {
					t.Fatalf("body=%q, err=%v, uncompressed=%v, trailers=%v", body, err, res.Uncompressed, res.Trailer)
				}
			}
			if connections.Load() != 1 {
				t.Fatalf("five requests used %d connections", connections.Load())
			}
		})
	}
}

func TestEstablishmentDeadlineDoesNotTruncateHTTP2Body(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("start"))
		w.(http.Flusher).Flush()
		select {
		case <-time.After(250 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("end"))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	tr := trustedTransport(t, server, Config{HandshakeTimeout: 100 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := fhttp.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	tr.CloseIdleConnections() // a live body owns its HTTP/2 connection
	body, err := io.ReadAll(res.Body)
	if err != nil || string(body) != "startend" {
		t.Fatalf("long body %q: %v", body, err)
	}
}

func TestHTTP2ReconnectWaitersCancelAndReleaseSocket(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	var dials atomic.Int32
	entered, socketClosed := make(chan struct{}), make(chan struct{})
	tr := trustedTransport(t, server, Config{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
		client, peer := net.Pipe()
		go func() {
			defer peer.Close()
			defer close(socketClosed)
			buf := make([]byte, 16384)
			_, _ = peer.Read(buf)
			close(entered)
			_, _ = io.Copy(io.Discard, peer)
		}()
		return client, nil
	}})
	req, _ := fhttp.NewRequest(http.MethodGet, server.URL, nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	leader := make(chan error, 1)
	go func() { _, err := tr.RoundTrip(req.WithContext(ctx)); leader <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reconnect did not enter handshake")
	}
	waitCtx, stopWait := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer stopWait()
	start := time.Now()
	_, err = tr.RoundTrip(req.WithContext(waitCtx))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 300*time.Millisecond {
		t.Fatalf("waiter cancellation: %v after %v", err, time.Since(start))
	}
	cancel()
	select {
	case err := <-leader:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("leader cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("leader did not cancel")
	}
	select {
	case <-socketClosed:
	case <-time.After(time.Second):
		t.Fatal("handshake socket leaked")
	}
	if dials.Load() != 2 {
		t.Fatalf("shared reconnect made %d dials", dials.Load())
	}
}

func TestChromeClientHelloAndWebSocketALPN(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP", true: "WebSocket"}[ws], func(t *testing.T) {
			hellos := make(chan *tls.ClientHelloInfo, 1)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
			server.TLS = &tls.Config{GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) { hellos <- info; return nil, nil }}
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			tr := trustedTransport(t, server, Config{})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if ws {
				conn, err := tr.DialWebSocketTLS(ctx, "tcp", strings.TrimPrefix(server.URL, "https://"))
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.Close()
			} else {
				req, _ := fhttp.NewRequestWithContext(ctx, "GET", server.URL, nil)
				res, err := tr.RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = res.Body.Close()
			}
			info := <-hellos
			spec, err := profiles.Chrome_146.GetClientHelloSpec()
			if err != nil {
				t.Fatal(err)
			}
			stripGrease := func(values []uint16) []uint16 {
				var out []uint16
				for _, v := range values {
					if v&0x0f0f != 0x0a0a {
						out = append(out, v)
					}
				}
				return out
			}
			if !reflect.DeepEqual(stripGrease(info.CipherSuites), stripGrease(spec.CipherSuites)) {
				t.Fatalf("cipher order differs from profile: %x", info.CipherSuites)
			}
			wantALPN := []string{"h2", "http/1.1"}
			if ws {
				wantALPN = []string{"http/1.1"}
			}
			if !reflect.DeepEqual(info.SupportedProtos, wantALPN) {
				t.Fatalf("ALPN %v, want %v", info.SupportedProtos, wantALPN)
			}
		})
	}
}

// stalledSocket uses a real pipe with an unread peer to reproduce a socket
// write that cannot progress. Reads and the warm-up handshake use real TLS/TCP.
type stalledSocket struct {
	net.Conn
	blocked   net.Conn
	stall     atomic.Bool
	entered   chan struct{}
	once      sync.Once
	closeOnce sync.Once
}

func (s *stalledSocket) Write(p []byte) (int, error) {
	if s.stall.Load() {
		s.once.Do(func() { close(s.entered) })
		return s.blocked.Write(p)
	}
	return s.Conn.Write(p)
}
func (s *stalledSocket) SetWriteDeadline(d time.Time) error {
	_ = s.blocked.SetWriteDeadline(d)
	return s.Conn.SetWriteDeadline(d)
}
func (s *stalledSocket) Close() error {
	s.closeOnce.Do(func() { _ = s.blocked.Close(); _ = s.Conn.Close() })
	return nil
}

func TestHTTP2BlockedWriteCancellationAndIdleClose(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	var socket *stalledSocket
	tr := trustedTransport(t, server, Config{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		blocked, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		socket = &stalledSocket{Conn: raw, blocked: blocked, entered: make(chan struct{})}
		return socket, nil
	}})
	req, _ := fhttp.NewRequest("GET", server.URL, nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	socket.stall.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := tr.RoundTrip(req.WithContext(ctx)); finished <- err }()
	select {
	case <-socket.entered:
	case <-time.After(time.Second):
		t.Fatal("write did not block")
	}
	idleDone := make(chan struct{})
	go func() { tr.CloseIdleConnections(); close(idleDone) }()
	select {
	case <-idleDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("idle close waited for active socket write")
	}
	// Another waiter on this origin must also cancel while the first request
	// owns fhttp's stream mutex. It must not wait for the ten-second write limit.
	waiterCtx, stopWaiter := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer stopWaiter()
	waiter := make(chan error, 1)
	go func() { _, err := tr.RoundTrip(req.WithContext(waiterCtx)); waiter <- err }()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiter: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("waiter remained behind blocked connection mutex")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("canceled write leaked")
	}
}

func TestHTTP2NormalCancellationPreservesOtherStream(t *testing.T) {
	var connections atomic.Int32
	started := make(chan struct{}, 2)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("start"))
		w.(http.Flusher).Flush()
		started <- struct{}{}
		if r.URL.Path == "/cancel" {
			<-r.Context().Done()
			return
		}
		select {
		case <-time.After(100 * time.Millisecond):
			_, _ = w.Write([]byte("end"))
		case <-r.Context().Done():
		}
	}))
	server.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connections.Add(1)
		}
	}
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	tr := trustedTransport(t, server, Config{})
	req, _ := fhttp.NewRequest("GET", server.URL+"/cancel", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := tr.RoundTrip(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	secondReq, _ := fhttp.NewRequest("GET", server.URL+"/healthy", nil)
	second, err := tr.RoundTrip(secondReq)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = first.Body.Close()
	data, err := io.ReadAll(second.Body)
	_ = second.Body.Close()
	if err != nil || string(data) != "startend" || connections.Load() != 1 {
		t.Fatalf("healthy body=%q err=%v connections=%d", data, err, connections.Load())
	}
}

func TestCanceledUnreadBodyReleasesOriginAndConnection(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("start"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	tr := trustedTransport(t, server, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := fhttp.NewRequestWithContext(ctx, "GET", server.URL, nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel() // Deliberately abandon the body: cancellation must retire ownership.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		active := 0
		tr.mu.Lock()
		for _, o := range tr.origins {
			active += o.active
		}
		tr.mu.Unlock()
		if active == 0 {
			_ = res.Body.Close()
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("canceled unread body retained its origin reference")
}

func TestChromeHTTP2SettingsAndPseudoHeaderOrderOnWire(t *testing.T) {
	observed := make(chan error, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.TLS = &tls.Config{NextProtos: []string{"h2"}}
	server.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){"h2": func(_ *http.Server, conn *tls.Conn, _ http.Handler) {
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		preface := make([]byte, len(stdh2.ClientPreface))
		if _, err := io.ReadFull(conn, preface); err != nil {
			observed <- err
			return
		}
		if string(preface) != stdh2.ClientPreface {
			observed <- fmt.Errorf("bad preface %q", preface)
			return
		}
		frames := stdh2.NewFramer(conn, conn)
		_ = frames.WriteSettings()
		var order []uint16
		settings := map[uint16]uint32{}
		for {
			frame, err := frames.ReadFrame()
			if err != nil {
				observed <- err
				return
			}
			switch f := frame.(type) {
			case *stdh2.SettingsFrame:
				if !f.IsAck() {
					_ = f.ForeachSetting(func(s stdh2.Setting) error {
						order = append(order, uint16(s.ID))
						settings[uint16(s.ID)] = s.Val
						return nil
					})
					_ = frames.WriteSettingsAck()
				}
			case *stdh2.HeadersFrame:
				headers, err := hpack.NewDecoder(4096, nil).DecodeFull(f.HeaderBlockFragment())
				if err != nil {
					observed <- err
					return
				}
				var pseudo, custom []string
				for _, h := range headers {
					if strings.HasPrefix(h.Name, ":") {
						pseudo = append(pseudo, h.Name)
					}
					if strings.HasPrefix(h.Name, "x-order-") {
						custom = append(custom, h.Name)
					}
				}
				profile := profiles.Chrome_146
				wantOrder := profile.GetSettingsOrder()
				if len(order) != len(wantOrder) {
					observed <- fmt.Errorf("settings order %v want %v", order, wantOrder)
					return
				}
				for i, id := range wantOrder {
					if order[i] != uint16(id) || settings[uint16(id)] != profile.GetSettings()[id] {
						observed <- fmt.Errorf("setting mismatch order=%v settings=%v", order, settings)
						return
					}
				}
				if !reflect.DeepEqual(pseudo, profile.GetPseudoHeaderOrder()) {
					observed <- fmt.Errorf("pseudo order %v want %v", pseudo, profile.GetPseudoHeaderOrder())
					return
				}
				if !reflect.DeepEqual(custom, []string{"x-order-beta", "x-order-alpha"}) {
					observed <- fmt.Errorf("request header order %v", custom)
					return
				}
				var block bytes.Buffer
				encoder := hpack.NewEncoder(&block)
				_ = encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
				err = frames.WriteHeaders(stdh2.HeadersFrameParam{StreamID: f.StreamID, BlockFragment: block.Bytes(), EndStream: true, EndHeaders: true})
				observed <- err
				return
			}
		}
	}}
	server.StartTLS()
	defer server.Close()
	tr := trustedTransport(t, server, Config{})
	req, _ := fhttp.NewRequest("GET", server.URL, nil)
	req.Header = fhttp.Header{"X-Order-Alpha": {"a"}, "X-Order-Beta": {"b"}, fhttp.HeaderOrderKey: {"x-order-beta", "x-order-alpha"}}
	res, err := tr.RoundTrip(req)
	if err != nil {
		select {
		case observedErr := <-observed:
			t.Fatalf("wire: %v; request: %v", observedErr, err)
		default:
			t.Fatal(err)
		}
	}
	_ = res.Body.Close()
	if err := <-observed; err != nil {
		t.Fatal(err)
	}
}
