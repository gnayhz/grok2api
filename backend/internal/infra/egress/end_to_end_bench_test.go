package egress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

// Real HTTP traffic measures local management overhead in the complete request
// path. Samples exclude warm-up; both paths use the same server, payload, Go
// transport and keep-alive policy. This is not an upstream latency SLA.
func TestRuntimeEndToEndLatencyAndResourceStability(t *testing.T) {
	if testing.Short() {
		t.Skip("sustained local network measurement")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("healthy payload")) }))
	defer server.Close()
	raw, err := newBuildClientConfigured("", time.Second, buildConnectionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.CloseIdleConnections()
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{Connections: 16, Requests: 32, Clients: 16})
	defer m.Close(context.Background())
	ctx := context.Background()
	call := func(managed bool) time.Duration {
		start := time.Now()
		var lease *Lease
		if managed {
			var err error
			lease, err = m.Acquire(ctx, domain.ScopeBuild, "")
			if err != nil {
				t.Fatal(err)
			}
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
		var res *http.Response
		var err error
		if managed {
			res, err = lease.Do(req)
		} else {
			res, err = raw.Do(req)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.Copy(io.Discard, res.Body); err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if managed {
			lease.Observe(res.StatusCode, nil)
			lease.Release()
		}
		return time.Since(start)
	}
	for i := 0; i < 100; i++ {
		call(false)
		call(true)
	}
	fds := func() int { entries, _ := os.ReadDir("/proc/self/fd"); return len(entries) }
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	goroutineBefore, fdBefore := runtime.NumGoroutine(), fds()
	for batch := 0; batch < 5; batch++ {
		rawSamples, managedSamples := make([]time.Duration, 500), make([]time.Duration, 500)
		for i := 0; i < 500; i++ {
			rawSamples[i] = call(false)
			managedSamples[i] = call(true)
		}
		percentile := func(v []time.Duration, p int) time.Duration {
			sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
			return v[(len(v)-1)*p/100]
		}
		s := m.RuntimeStats()
		if s.Network.Connections > 16 || s.Network.Requests != 0 || s.Network.Waiters != 0 || s.Network.Dialing != 0 {
			t.Fatalf("resource accounting after batch %d: %+v", batch, s)
		}
		t.Logf("batch=%d raw p50=%v p95=%v p99=%v managed p50=%v p95=%v p99=%v sockets=%d clients=%d", batch, percentile(rawSamples, 50), percentile(rawSamples, 95), percentile(rawSamples, 99), percentile(managedSamples, 50), percentile(managedSamples, 95), percentile(managedSamples, 99), s.Network.Connections, s.Network.Clients)
	}
	if err = m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw.CloseIdleConnections()
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if runtime.NumGoroutine() > goroutineBefore+4 || fds() > fdBefore+4 || after.HeapAlloc > before.HeapAlloc+8<<20 {
		t.Fatalf("resource growth goroutines %d->%d FD %d->%d heap %d->%d", goroutineBefore, runtime.NumGoroutine(), fdBefore, fds(), before.HeapAlloc, after.HeapAlloc)
	}
	if s := m.RuntimeStats(); s.Network.Connections != 0 || s.Network.Clients != 0 || s.Network.Requests != 0 {
		t.Fatalf("shutdown leaked %+v", s)
	}
	t.Logf("settled goroutines %d->%d FD %d->%d heap %d->%d", goroutineBefore, runtime.NumGoroutine(), fdBefore, fds(), before.HeapAlloc, after.HeapAlloc)
}
