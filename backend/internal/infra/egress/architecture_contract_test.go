package egress

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// These contract checks isolate cross-origin and retirement dependencies.
// The local TCP listener reads ClientHello and never sends ServerHello.
func blockedBrowserHandshake(t *testing.T) (*browserClient, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buffer := make([]byte, 8192)
		if _, err := conn.Read(buffer); err != nil {
			return
		}
		close(entered)
		_, _ = io.Copy(io.Discard, conn)
	}()
	client, err := newBrowserClientWithBudget("", DefaultUserAgent, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+listener.Addr().String(), nil)
		res, err := client.Do(req)
		if res != nil {
			_ = res.Body.Close()
		}
		done <- err
	}()
	t.Cleanup(func() { cancel(); _ = listener.Close(); client.CloseIdleConnections(); <-serverDone })
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("did not enter TLS handshake")
	}
	return client, cancel, done
}

func TestRuntimeHealthyOriginIsolatedFromOtherOriginTLS(t *testing.T) {
	client, cancelBlocked, blockedDone := blockedBrowserHandshake(t)
	var served atomic.Int32
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		_, _ = w.Write([]byte("healthy"))
	}))
	defer healthy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, healthy.URL, nil)
		res, err := client.Do(req)
		if res != nil {
			_ = res.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("healthy independent origin failed: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Errorf("healthy origin not reached (calls=%d); Do remains blocked 220ms beyond its deadline by another origin's TLS handshake", served.Load())
		cancelBlocked()
		<-done
	}
	cancelBlocked()
	<-blockedDone
}

func TestRuntimeBuildAcquireDoesNotWaitForBrowserRetirement(t *testing.T) {
	client, cancelBlocked, blockedDone := blockedBrowserHandshake(t)
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	now := time.Now()
	// Saturate the client cache while its oldest browser client is still in a
	// slow handshake. Capacity eviction must not delay an unrelated Build path.
	for id := uint64(1); id <= maxCachedClients; id++ {
		m.transport.clients[clientCacheKey{nodeID: id, scope: domain.ScopeWeb}] = cachedClient{client: noopRequestClient{}, lastUsed: now}
	}
	m.transport.clients[clientCacheKey{nodeID: 1, scope: domain.ScopeWeb}] = cachedClient{client: client, browser: client, lastUsed: now.Add(-time.Minute)}
	m.transport.lastClientCleanup = now
	done := make(chan error, 1)
	go func() {
		_, err := m.transport.clientForContext(context.Background(), 100000, domain.ScopeBuild, "", "", "", false, "", clientOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Error("unrelated Build client acquisition waits for retired browser client's network handshake")
		cancelBlocked()
		<-done
	}
	cancelBlocked()
	<-blockedDone
}

func BenchmarkPoolAccountingScale(b *testing.B) {
	for _, item := range []struct {
		name  string
		pools int
	}{{"1", 1}, {"100", 100}, {"1000", 1000}, {"10000", 10000}} {
		b.Run(item.name, func(b *testing.B) {
			previous := poolNodeStats
			poolNodeStats = &poolNodeStatCounters{pools: make(map[uint64]map[uint64]*PoolNodeStat), failures: make(map[uint64]poolNodeFailure), poolSince: make(map[uint64]time.Time)}
			b.Cleanup(func() { poolNodeStats = previous })
			for i := 1; i <= item.pools; i++ {
				id := uint64(i)
				RecordPoolSelection(id, id)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				RecordPoolSelection(1, 1)
			}
		})
	}
}
