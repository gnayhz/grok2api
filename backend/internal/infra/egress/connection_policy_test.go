package egress

import (
	"context"
	"crypto/x509"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	physical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// Use the actual Manager, registry, CONNECT proxy and HTTP transports. The
// server's remote address identifies a TCP connection, independently of cache
// keys, request.Close and httptrace's description of reuse.
func TestBuildSessionConnectionPolicyOnWire(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", h2), func(t *testing.T) {
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"prompt_cache_key":"history-stays","input":"unchanged"}` || r.Header.Get("X-Grok-Conv-Id") != "history-stays" {
					t.Error("network policy changed conversation input")
				}
				w.Header().Set("X-Test-Connection", r.RemoteAddr)
				w.Header().Set("X-Test-Account", r.Header.Get("Authorization"))
				_, _ = io.WriteString(w, "ok")
			}))
			upstream.EnableHTTP2 = h2
			upstream.StartTLS()
			defer upstream.Close()
			proxy := connectionPolicyProxy(t, upstream.Listener.Addr().String())
			for _, session := range []bool{false, true} {
				for _, isolated := range []bool{false, true} {
					for _, fresh := range []bool{false, true} {
						t.Run(fmt.Sprintf("session=%t/isolated=%t/fresh=%t", session, isolated, fresh), func(t *testing.T) {
							cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
							if err != nil {
								t.Fatal(err)
							}
							proxyURL, err := cipher.Encrypt(proxy.URL)
							if err != nil {
								t.Fatal(err)
							}
							m := NewManagerWithLimits(egressRepositoryTestStub{nodes: []domain.Node{{ID: 7, Enabled: true, Health: 1, ProxyPool: fresh, EncryptedProxyURL: proxyURL}}}, cipher, netbudget.Limits{})
							t.Cleanup(func() { _ = m.Close(context.Background()) })
							m.UpdateAccountIsolatedConnections(isolated)
							ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							defer cancel()
							if session {
								ctx = WithBuildSession(ctx, "history-stays")
							}
							ctx, trace := physical.WithTrace(ctx)
							configured := map[requestClient]bool{}
							var connections []string
							for _, account := range []string{"A", "A", "B", "A"} {
								lease, err := m.Acquire(WithAccountIdentity(ctx, account), domain.ScopeBuild, account)
								if err != nil {
									t.Fatal(err)
								}
								wantDecision := SessionReuseNotRequested
								if session {
									wantDecision = SessionReuseAccepted
									if fresh {
										wantDecision = SessionReuseFresh
									}
								}
								wantPolicy := ConnectionPolicy{Fresh: fresh, AccountIsolated: isolated, SessionReuse: wantDecision}
								selection, ok := trace.Selection(domain.ScopeBuild)
								if lease.ConnectionPolicy() != wantPolicy || !ok || selection.Connection != wantPolicy {
									t.Fatalf("lease/trace policy = %+v / %+v, want %+v", lease.ConnectionPolicy(), selection, wantPolicy)
								}
								if !configured[lease.client] {
									trustPolicyTestServer(t, lease, upstream)
									configured[lease.client] = true
								}
								req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, strings.NewReader(`{"prompt_cache_key":"history-stays","input":"unchanged"}`))
								req.Header.Set("Authorization", account)
								req.Header.Set("X-Grok-Conv-Id", "history-stays")
								res, err := lease.Do(req)
								if err != nil {
									lease.Release()
									t.Fatal(err)
								}
								_, err = io.Copy(io.Discard, res.Body)
								_ = res.Body.Close()
								lease.Release()
								if err != nil {
									t.Fatal(err)
								}
								if (res.ProtoMajor == 2) != h2 {
									t.Fatalf("protocol = %s", res.Proto)
								}
								if req.Close || req.Header.Get("Authorization") != account || res.Header.Get("X-Test-Account") != account {
									t.Fatal("caller request or request-local credentials changed")
								}
								connections = append(connections, res.Header.Get("X-Test-Connection"))
							}
							if fresh {
								seen := map[string]bool{}
								for _, conn := range connections {
									if seen[conn] {
										t.Fatalf("fresh connection reused: %v", connections)
									}
									seen[conn] = true
								}
							} else {
								if connections[0] != connections[1] || connections[0] != connections[3] {
									t.Fatalf("same account did not reuse its connection: %v", connections)
								}
								if (connections[0] != connections[2]) != isolated {
									t.Fatalf("account isolation=%t connections=%v", isolated, connections)
								}
							}
						})
					}
				}
			}
		})
	}
}

func trustPolicyTestServer(t *testing.T, lease *Lease, server *httptest.Server) {
	t.Helper()
	transport := lease.client.(*http.Client).Transport.(*http.Transport)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport.TLSClientConfig.RootCAs = roots
}

func connectionPolicyProxy(t *testing.T, target string) *httptest.Server {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != target {
			http.Error(w, "unexpected proxy target", http.StatusBadRequest)
			return
		}
		upstream, err := net.DialTimeout("tcp", target, time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(10 * time.Second))
		_ = upstream.SetDeadline(time.Now().Add(10 * time.Second))
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, buffered); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		<-done
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

func TestSessionIsolationToggleRetiresOldClients(t *testing.T) {
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	ctx := WithBuildSession(context.Background(), "session")
	acquire := func(account string) *Lease {
		t.Helper()
		lease, err := m.Acquire(WithAccountIdentity(ctx, account), domain.ScopeBuild, account)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(lease.Release)
		return lease
	}
	shared := acquire("A")
	m.UpdateAccountIsolatedConnections(true)
	first, second := acquire("A"), acquire("B")
	if shared.client == first.client || first.client == second.client {
		t.Fatal("isolation toggle preserved shared session client")
	}
	if shared.clientHandle.retainLease() {
		shared.clientHandle.releaseLease()
		t.Fatal("old shared session client accepted a new lease")
	}
	m.UpdateAccountIsolatedConnections(false)
	merged := acquire("B")
	if merged.client == second.client || merged.client == shared.client {
		t.Fatal("disabled isolation revived an old client")
	}
	if acquire("A").client != merged.client {
		t.Fatal("disabled isolation did not allow session reuse")
	}
}

func TestFreshSessionConcurrentCallsUseSeparateConnections(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", h2), func(t *testing.T) {
			const calls = 4
			arrivals := make(chan string, calls)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				arrivals <- r.RemoteAddr
				select {
				case <-release:
				case <-r.Context().Done():
				}
				_, _ = io.WriteString(w, "ok")
			}))
			upstream.EnableHTTP2 = h2
			upstream.StartTLS()
			defer upstream.Close()
			defer unblock()
			m := NewManagerWithLimits(egressRepositoryTestStub{nodes: []domain.Node{{ID: 7, Enabled: true, Health: 1, ProxyPool: true}}}, nil, netbudget.Limits{})
			t.Cleanup(func() { _ = m.Close(context.Background()) })
			ctx, cancel := context.WithTimeout(WithBuildSession(context.Background(), "same-session"), 5*time.Second)
			defer cancel()
			lease, err := m.Acquire(ctx, domain.ScopeBuild, "same-account")
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			trustPolicyTestServer(t, lease, upstream)
			done := make(chan error, calls)
			for range calls {
				go func() {
					req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, strings.NewReader("payload"))
					res, err := lease.Do(req)
					if err == nil {
						_, err = io.Copy(io.Discard, res.Body)
						_ = res.Body.Close()
						if (res.ProtoMajor == 2) != h2 {
							err = fmt.Errorf("unexpected protocol %s", res.Proto)
						}
					}
					done <- err
				}()
			}
			seen := map[string]bool{}
			for range calls {
				select {
				case conn := <-arrivals:
					if seen[conn] {
						t.Errorf("concurrent fresh calls multiplexed on %s", conn)
					}
					seen[conn] = true
				case <-ctx.Done():
					t.Fatal("concurrent calls did not reach the server")
				}
			}
			unblock()
			for range calls {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSessionPolicyIsPerAcquisitionEvenOnSharedFreshClient(t *testing.T) {
	m := NewManagerWithLimits(egressRepositoryTestStub{nodes: []domain.Node{{ID: 7, Enabled: true, Health: 1, ProxyPool: true}}}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	first, err := m.Acquire(context.Background(), domain.ScopeBuild, "A")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := m.Acquire(WithBuildSession(context.Background(), "session"), domain.ScopeBuild, "A")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if first.client != second.client {
		t.Fatal("fresh client unnecessarily partitioned by rejected session hint")
	}
	if first.ConnectionPolicy().SessionReuse != SessionReuseNotRequested || second.ConnectionPolicy().SessionReuse != SessionReuseFresh {
		t.Fatalf("cached result leaked another acquisition's hint decision: %+v %+v", first.ConnectionPolicy(), second.ConnectionPolicy())
	}
}

func TestSessionIsolationUpdateAllowsActiveStreamToDrain(t *testing.T) {
	ready, finish := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	unblock := func() { finishOnce.Do(func() { close(finish) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "before\n")
		w.(http.Flusher).Flush()
		close(ready)
		select {
		case <-finish:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "after\n")
	}))
	defer upstream.Close()
	defer unblock()
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(WithBuildSession(context.Background(), "session"), 5*time.Second)
	defer cancel()
	old, err := m.Acquire(ctx, domain.ScopeBuild, "A")
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	res, err := old.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	<-ready
	m.UpdateAccountIsolatedConnections(true)
	newLease, err := m.Acquire(ctx, domain.ScopeBuild, "A")
	if err != nil {
		t.Fatal(err)
	}
	defer newLease.Release()
	if old.client == newLease.client || old.ConnectionPolicy().AccountIsolated || !newLease.ConnectionPolicy().AccountIsolated {
		t.Fatal("policy not frozen for old lease or not applied to new lease")
	}
	unblock()
	body, err := io.ReadAll(res.Body)
	if err != nil || string(body) != "before\nafter\n" {
		t.Fatalf("active stream interrupted: %q %v", body, err)
	}
	old.Release()
	if _, owned := m.transport.owned.Load(old.client); owned {
		t.Fatal("retired client budget remained after stream and lease completion")
	}
}
