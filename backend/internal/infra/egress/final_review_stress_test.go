//go:build egress_final_review

package egress

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

func TestFinalReviewSustainedLoadOverloadAndHotUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("sustained final audit")
	}
	runtime.GC()
	startG := runtime.NumGoroutine()
	startFD, _ := os.ReadDir("/proc/self/fd")
	var proxyTransports []*http.Transport
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "x")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "healthy")
	}))
	defer origin.Close()
	var proxyHits atomic.Int64
	proxy := func() *httptest.Server {
		tr := &http.Transport{MaxIdleConns: 32, MaxIdleConnsPerHost: 32}
		proxyTransports = append(proxyTransports, tr)
		t.Cleanup(tr.CloseIdleConnections)
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxyHits.Add(1)
			req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, r.RequestURI, nil)
			res, err := tr.RoundTrip(req)
			if err != nil {
				return
			}
			defer res.Body.Close()
			for k, v := range res.Header {
				w.Header()[k] = v
			}
			w.WriteHeader(res.StatusCode)
			w.(http.Flusher).Flush()
			_, _ = io.Copy(w, res.Body)
		}))
	}
	proxyA, proxyB := proxy(), proxy()
	defer proxyA.Close()
	defer proxyB.Close()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	node := domain.Node{ID: 1, Name: "stress", Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy(t, cipher, proxyA.URL)}
	r := &e2eRepo{egressRepositoryTestStub: egressRepositoryTestStub{nodes: []domain.Node{node}}, config: domain.DefaultOperationsConfig()}
	m := NewManagerWithLimits(r, cipher, netbudget.Limits{Connections: 16, Dialing: 8, Requests: 16, Clients: 64, Waiters: 64, QueueTimeout: 20 * time.Millisecond})
	defer m.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var hits, capacity, canceled, unexpected atomic.Int64
	var samplesMu sync.Mutex
	var samples []time.Duration
	call := func() {
		callCtx, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		start := time.Now()
		lease, err := m.Acquire(callCtx, domain.ScopeBuild, "stress-account")
		if err == nil {
			defer lease.Release()
			req, _ := http.NewRequestWithContext(callCtx, http.MethodGet, origin.URL, nil)
			var res *http.Response
			res, err = lease.Do(req)
			status := 0
			if res != nil {
				status = res.StatusCode
				_, readErr := io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
				if err == nil {
					err = readErr
				}
			}
			lease.Observe(status, err)
		}
		switch {
		case err == nil:
			hits.Add(1)
			samplesMu.Lock()
			samples = append(samples, time.Since(start))
			samplesMu.Unlock()
		case errors.Is(err, netbudget.ErrCapacity):
			capacity.Add(1)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, errNodeSnapshotInvalidated), errors.Is(err, errClientCacheInvalidated), errors.Is(err, ErrClientRetired):
			canceled.Add(1)
		default:
			if unexpected.Add(1) <= 3 {
				t.Logf("unexpected request error: %v", err)
			}
		}
	}
	phase := func(duration time.Duration, workers int) {
		deadline := time.Now().Add(duration)
		var group sync.WaitGroup
		for range workers {
			group.Add(1)
			go func() {
				defer group.Done()
				for time.Now().Before(deadline) {
					call()
				}
			}()
		}
		group.Wait()
	}
	phase(5*time.Second, 16)
	baselineHits := hits.Load()
	var held []*http.Response
	var leases []*Lease
	for range 16 {
		lease, err := m.Acquire(ctx, domain.ScopeBuild, "stress-account")
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+"/hold", nil)
		res, err := lease.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, res)
		leases = append(leases, lease)
	}
	beforeOverloadHits := proxyHits.Load()
	beforeOverloadCapacity := capacity.Load()
	phase(3*time.Second, 32)
	overloadCapacity := capacity.Load() - beforeOverloadCapacity
	if overloadCapacity == 0 || hits.Load() != baselineHits || proxyHits.Load() != beforeOverloadHits {
		t.Errorf("overload did not enforce request budget: capacity=%d admitted=%d", capacity.Load(), hits.Load()-baselineHits)
	}
	for i, res := range held {
		_ = res.Body.Close()
		leases[i].Release()
	}
	hotDone := make(chan struct{})
	stopHot := make(chan struct{})
	var edits atomic.Int64
	go func() {
		defer close(hotDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopHot:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				url := proxyA.URL
				if edits.Add(1)%2 == 0 {
					url = proxyB.URL
				}
				binding, err := cipher.Encrypt(url)
				if err != nil {
					return
				}
				r.mu.Lock()
				r.nodes[0].EncryptedProxyURL = binding
				r.nodes[0].BindingRevision++
				r.mu.Unlock()
				m.ForgetClearance(node.ID)
			}
		}
	}()
	phase(30*time.Second, 32)
	close(stopHot)
	<-hotDone
	recovered := hits.Load() - baselineHits
	if recovered < 1000 || unexpected.Load() != 0 {
		t.Errorf("recovery failed: requests=%d unexpected=%d", recovered, unexpected.Load())
	}
	if err = m.FlushFeedback(ctx); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	state := r.nodes[0].HealthState()
	r.mu.Unlock()
	if state.FailureCount != 0 || state.CooldownUntil != nil {
		t.Errorf("load polluted health: %+v", state)
	}
	if err = m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for m.RuntimeStats().Network.Connections != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stats := m.RuntimeStats().Network
	if stats.Clients != 0 || stats.Requests != 0 || stats.Connections != 0 || stats.Waiters != 0 || stats.Dialing != 0 {
		t.Errorf("shutdown retained resources: %+v", stats)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	if len(samples) == 0 {
		t.Fatal("no successful samples")
	}
	proxyA.Close()
	proxyB.Close()
	for _, tr := range proxyTransports {
		tr.CloseIdleConnections()
	}
	origin.Close()
	runtime.GC()
	deadline = time.Now().Add(time.Second)
	for runtime.NumGoroutine() > startG+2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	fd, _ := os.ReadDir("/proc/self/fd")
	t.Logf("requests=%d baseline=%d recovered=%d rejected_capacity=%d overload_rejections=%d canceled_or_reselected=%d unexpected=%d edits=%d proxy_hits=%d samples=%d p50=%v p95=%v p99=%v goroutines=%d->%d FD=%d->%d final=%+v", hits.Load(), baselineHits, recovered, capacity.Load(), overloadCapacity, canceled.Load(), unexpected.Load(), edits.Load(), proxyHits.Load(), len(samples), samples[len(samples)/2], samples[len(samples)*95/100], samples[len(samples)*99/100], startG, runtime.NumGoroutine(), len(startFD), len(fd), stats)
}
