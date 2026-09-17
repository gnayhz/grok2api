package egress

import (
	"context"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

type notifiedSolver struct {
	entered, gate chan struct{}
	calls         atomic.Int32
}

func (s *notifiedSolver) Solve(ctx context.Context, _ ClearanceConfig, _ string) (clearanceSolution, error) {
	if s.calls.Add(1) == 1 {
		close(s.entered)
	}
	select {
	case <-s.gate:
		return clearanceSolution{Cookies: "cf_clearance=fresh", UserAgent: "fresh-UA"}, nil
	case <-ctx.Done():
		return clearanceSolution{}, ctx.Err()
	}
}

func TestClearanceCancellationDoesNotBlockOrCancelSharedSolve(t *testing.T) {
	solver := &notifiedSolver{entered: make(chan struct{}), gate: make(chan struct{})}
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	m.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", Timeout: time.Second})
	m.clearance.solver = solver
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	read := func(ctx context.Context) error {
		_, _, _, err := m.clearance.ensureClearance(ctx, domain.Node{}, "", "", "", "direct", false)
		return err
	}
	go func() { done <- read(ctx) }()
	<-solver.entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled solve waiter: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("canceled caller stayed blocked on solver")
	}
	follower := make(chan error, 1)
	go func() { follower <- read(context.Background()) }()
	close(solver.gate)
	if err := <-follower; err != nil {
		t.Fatal(err)
	}
	if solver.calls.Load() != 1 {
		t.Fatalf("duplicate solve: %d", solver.calls.Load())
	}
}

func TestBrowserPoolDoesNotReplaySubmittedPOST(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("X-Resin-Error", "UPSTREAM_CONNECT_FAILED")
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	client, err := newBrowserClientWithBudget("", DefaultUserAgent, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	lease := &Lease{client: client, browser: client, proxyPool: true}
	req, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("already-submitted"))
	res, err := lease.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if calls.Load() != 1 {
		t.Fatalf("submitted POST replayed %d times", calls.Load())
	}
}

func TestClientCreationRejectsGenerationAlias(t *testing.T) {
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	m.transport.clientVersions[7] = 1
	entered, gate := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	m.transport.newBuildClient = func(string, time.Duration) (requestClient, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-gate
		}
		return noopRequestClient{}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := m.transport.clientForContext(context.Background(), 7, domain.ScopeBuild, "", "", "", false, "", clientOptions{})
		done <- err
	}()
	<-entered
	m.transport.clientMu.Lock()
	m.transport.invalidateAllClientVersionsLocked()
	m.transport.clientMu.Unlock()
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("old client generation was published: builds=%d", calls.Load())
	}
}

func TestSessionInsertDoesNotEvictFullSharedBudget(t *testing.T) {
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	now := time.Now()
	for i := range maxCachedClients {
		m.transport.clients[clientCacheKey{nodeID: uint64(i + 1), scope: domain.ScopeBuild}] = cachedClient{client: noopRequestClient{}, lastUsed: now}
	}
	_, err := m.transport.clientForContext(context.Background(), 1, domain.ScopeBuild, "", "", "", false, "", clientOptions{sessionKey: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.transport.clients) != maxCachedClients+1 {
		t.Fatalf("session evicted a shared client: count=%d", len(m.transport.clients))
	}
}

func TestSessionPinsRespectCapacityWithinSweepWindow(t *testing.T) {
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	m.routing.sessionPinSweep = time.Now()
	for i := range maxSessionPinnedNodes + 10 {
		m.routing.sessionPins[fmt.Sprint(i)] = sessionNodePin{lastUsed: time.Now()}
	}
	m.routing.sweepSessionPinsLocked(time.Now())
	if len(m.routing.sessionPins) > maxSessionPinnedNodes {
		t.Fatalf("pin budget bypassed: %d", len(m.routing.sessionPins))
	}
}

func TestLeaseConcurrentReleaseIsIdempotent(t *testing.T) {
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	lease, _, err := m.leaseForNode(context.Background(), domain.ScopeBuild, "", "", false, domain.Node{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(lease.Release)
	}
	wg.Wait()
	if count := m.routing.inflightCount(0); count != 0 {
		t.Fatalf("inflight after concurrent release: %d", count)
	}
}

func TestCanceledLeaseReleasesRetiredClientWithoutBodyConsumer(t *testing.T) {
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	lease, err := m.AcquireBuildEnvironmentDirect(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	m.transport.invalidate(map[uint64]struct{}{0: {}}, "")
	if s := m.RuntimeStats(); s.RetiredClients != 1 {
		t.Fatalf("lease did not pin retired client: %+v", s)
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for m.RuntimeStats().Network.Clients != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	lease.Release()
	lease.Release()
	if s := m.RuntimeStats(); s.Network.Clients != 0 || s.RetiredClients != 0 || m.routing.inflightCount(0) != 0 {
		t.Fatalf("canceled lease retained resource ownership: %+v", s)
	}
}

func TestFixedNodeCounterChurnRetainsActiveLeasesWithinCapacity(t *testing.T) {
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	m.incrementInflight(1)
	for id := uint64(2); id < 20000; id++ {
		m.incrementInflight(id)
		m.decrementInflight(id)
	}
	if m.routing.inflightEntries > maxRetainedInflightCounters || m.routing.inflightCount(1) != 1 {
		t.Fatalf("counter churn: entries=%d active=%d", m.routing.inflightEntries, m.routing.inflightCount(1))
	}
	m.decrementInflight(1)
}
