package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

func TestFinalReviewPinnedDownloadSharesClientBudget(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{Clients: 1, QueueTimeout: 20 * time.Millisecond})
	defer m.Close(context.Background())
	cached, err := m.transport.clientFor(7, domain.ScopeBuild, proxy.URL, "", "", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !cached.handle.retainLease() {
		t.Fatal("cannot retain cached client")
	}
	defer cached.handle.releaseLease()
	lease := &Lease{NodeID: 7, Scope: domain.ScopeWebAsset, ProxyURL: proxy.URL, client: cached.client, clientHandle: cached.handle, clearanceManager: m}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://1.1.1.1:443/image.png", nil)
	request.Host = "example.com"
	_, err = lease.DoPinnedHTTPS(request, "example.com")
	if !errors.Is(err, netbudget.ErrCapacity) || calls.Load() != 0 {
		t.Fatalf("pinned client bypassed maxClients=1: proxyCalls=%d clients=%d error=%v", calls.Load(), m.RuntimeStats().Network.Clients, err)
	}
}

func TestFinalReviewPinnedDownloadOwnsResponseLifecycle(t *testing.T) {
	for _, mode := range []string{"eof", "close", "cancel", "shutdown", "redirect", "tls_error", "setup_error"} {
		t.Run(mode, func(t *testing.T) {
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "example.com" || r.TLS.ServerName != "example.com" {
					t.Errorf("download changed Host/SNI: %s/%s", r.Host, r.TLS.ServerName)
				}
				if mode == "redirect" {
					w.Header().Set("Location", "https://invalid.example/redirected")
					w.WriteHeader(http.StatusFound)
					return
				}
				if mode == "eof" {
					_, _ = io.WriteString(w, "image")
					return
				}
				w.Header().Set("Content-Length", "100")
				_, _ = io.WriteString(w, "x")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer origin.Close()
			proxy, calls := newFinalReviewCONNECTProxy(t, origin.Listener.Addr().String())
			m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{Clients: 2, Requests: 1, Connections: 1})
			defer m.Close(context.Background())
			cached, err := m.transport.clientFor(7, domain.ScopeBuild, proxy.URL, "", "", false, "")
			if err != nil || !cached.handle.retainLease() {
				t.Fatalf("retain original client: %v", err)
			}
			defer cached.handle.releaseLease()
			lease := &Lease{NodeID: 7, Scope: domain.ScopeWebAsset, ProxyURL: proxy.URL, client: cached.client, clientHandle: cached.handle, clearanceManager: m}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://1.1.1.1:443/image.png", nil)
			request.Host = "example.com"
			roots := x509.NewCertPool()
			roots.AddCert(origin.Certificate())
			var response *http.Response
			if mode == "tls_error" || mode == "setup_error" {
				if mode == "setup_error" {
					lease.ProxyURL = "unsupported://proxy"
				}
				response, err = lease.DoPinnedHTTPS(request, "example.com")
				if err == nil {
					t.Fatal("expected TLS/configuration failure")
				}
			} else {
				response, err = lease.doPinnedHTTPS(request, "example.com", &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				stats := m.RuntimeStats().Network
				if stats.Clients != 2 || stats.Requests != 1 {
					t.Fatalf("missing download owner during response: %+v", stats)
				}
				if mode != "redirect" && (stats.ActiveConnections != 1 || stats.Dialing != 0) {
					t.Fatalf("incorrect socket handoff: %+v", stats)
				}
				switch mode {
				case "eof":
					body, err := io.ReadAll(response.Body)
					if err != nil || string(body) != "image" {
						t.Fatalf("body=%q err=%v", body, err)
					}
				case "close":
					_ = response.Body.Close()
				case "cancel":
					cancel() // No body read/close before checking ownership.
				case "shutdown":
					if err := m.Close(context.Background()); err != nil {
						t.Fatal(err)
					}
				case "redirect":
					if response.StatusCode != http.StatusFound {
						t.Fatalf("redirect policy changed: %d", response.StatusCode)
					}
					_ = response.Body.Close()
				}
			}
			wantClients := 1
			if mode == "shutdown" {
				wantClients = 0
			}
			deadline := time.Now().Add(time.Second)
			for {
				s := m.RuntimeStats().Network
				if s.Clients == wantClients && s.Requests == 0 && s.Connections == 0 && s.Dialing == 0 && s.Waiters == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("download retained resources: %+v", s)
				}
				time.Sleep(time.Millisecond)
			}
			wantCalls := int32(1)
			if mode == "setup_error" {
				wantCalls = 0
			}
			if calls.Load() != wantCalls {
				t.Fatalf("proxy calls=%d want=%d", calls.Load(), wantCalls)
			}
		})
	}
}

// The proxy verifies the public pinned destination, then connects only to the
// local test TLS origin. Both tunnel directions close when either side ends.
func newFinalReviewCONNECTProxy(t *testing.T, originAddress string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	var sockets sync.Map
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodConnect || r.Host != "1.1.1.1:443" {
			t.Errorf("unexpected CONNECT target: %s %s", r.Method, r.Host)
			http.Error(w, "bad target", http.StatusBadRequest)
			return
		}
		upstream, err := net.DialTimeout("tcp", originAddress, time.Second)
		if err != nil {
			http.Error(w, "local origin unavailable", http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		sockets.Store(client, upstream)
		defer sockets.Delete(client)
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(upstream, buffered)
			_ = upstream.Close()
			close(done)
		}()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		<-done
	}))
	t.Cleanup(func() {
		proxy.Close()
		sockets.Range(func(k, v any) bool { _ = k.(net.Conn).Close(); _ = v.(net.Conn).Close(); return true })
	})
	return proxy, &calls
}
