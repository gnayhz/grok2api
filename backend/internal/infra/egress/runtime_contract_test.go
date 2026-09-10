package egress

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestRuntimeRetiredActiveClientKeepsBudgetAndStream(t *testing.T) {
	finishServer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("start"))
		w.(http.Flusher).Flush()
		select {
		case <-finishServer:
			w.Write([]byte("end"))
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{Clients: 1, Connections: 2})
	defer m.Close(context.Background())
	lease, _, err := m.leaseForNode(context.Background(), domain.ScopeBuild, "", "", false, domain.Node{})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", server.URL, nil)
	res, err := lease.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	m.transport.invalidate(map[uint64]struct{}{0: {}}, "")
	if s := m.RuntimeStats(); s.RetiredClients != 1 || s.Network.Clients != 1 || s.Network.Requests != 1 {
		t.Fatalf("retired accounting %+v", s)
	}
	_, _, err = m.leaseForNode(context.Background(), domain.ScopeBuild, "", "", false, domain.Node{})
	if !errors.Is(err, netbudget.ErrCapacity) {
		t.Fatalf("active client capacity bypassed %v", err)
	}
	close(finishServer)
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	lease.Release()
	if err != nil || string(body) != "startend" {
		t.Fatalf("retirement cut stream %q %v", body, err)
	}
	next, _, err := m.leaseForNode(context.Background(), domain.ScopeBuild, "", "", false, domain.Node{})
	if err != nil {
		t.Fatal(err)
	}
	next.Release()
}

func TestRuntimeCloseCancelsSharedSolveAndJoinsIt(t *testing.T) {
	solver := &notifiedSolver{entered: make(chan struct{}), gate: make(chan struct{})}
	m := NewManager(egressRepositoryTestStub{}, nil)
	m.clearance.solver = solver
	m.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", Timeout: time.Minute})
	done := make(chan error, 1)
	go func() {
		_, _, _, err := m.clearance.ensureClearance(context.Background(), domain.Node{}, "", "", "", "direct", false)
		done <- err
	}()
	<-solver.entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("solve survived close")
		}
	case <-ctx.Done():
		t.Fatal("solve waiter leaked")
	}
	for k, n := range m.RuntimeStats().Tasks {
		if n != 0 {
			t.Fatalf("task %s remains: %d", k, n)
		}
	}
}

func TestClearanceConfigChangeRejectsOldSolve(t *testing.T) {
	solver := &notifiedSolver{entered: make(chan struct{}), gate: make(chan struct{})}
	m := NewManager(egressRepositoryTestStub{}, nil)
	defer m.Close(context.Background())
	m.clearance.solver = solver
	m.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", TargetURL: "https://first.test", Timeout: time.Second})
	done := make(chan error, 1)
	go func() {
		_, _, _, err := m.clearance.ensureClearance(context.Background(), domain.Node{}, "", "", "", "direct", false)
		done <- err
	}()
	<-solver.entered
	m.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", TargetURL: "https://second.test", Timeout: time.Second})
	close(solver.gate)
	if err := <-done; !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("old solver accepted: %v", err)
	}
	m.clearance.clearanceMu.Lock()
	state := m.clearance.clearances["direct"]
	m.clearance.clearanceMu.Unlock()
	if state.cookies != "" {
		t.Fatalf("old cookie cached %q", state.cookies)
	}
}

func TestLeaseObservationIgnoresBusinessAndDownstreamErrors(t *testing.T) {
	m := NewManager(egressRepositoryTestStub{}, nil)
	defer m.Close(context.Background())
	lease := &Lease{NodeID: 1, Scope: domain.ScopeBuild, clearanceManager: m, healthBaseline: domain.HealthState{Health: 1}}
	for _, err := range []error{errors.New("invalid JSON"), errors.New("downstream write failed"), neterrorpkg.ErrUpstreamOutputLoop} {
		lease.Observe(200, err)
	}
	if node := m.health.overlay(domain.Node{ID: 1, Health: 1}); node.FailureCount != 0 {
		t.Fatalf("business error cooled node %+v", node.HealthState())
	}
	lease.Observe(0, neterrorpkg.MarkTransport(errors.New("connection reset"), neterrorpkg.PhaseRequest))
	if node := m.health.overlay(domain.Node{ID: 1, Health: 1}); node.FailureCount != 1 {
		t.Fatalf("physical failure not recorded %+v", node.HealthState())
	}
}

func TestNodeEditDuringClientConstructionCannotPublishOldLease(t *testing.T) {
	repo := &e2eRepo{pools: map[uint64]domain.Pool{}}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("http://old-proxy:8080")
	if err != nil {
		t.Fatal(err)
	}
	repo.nodes = []domain.Node{{ID: 7, Name: "old", Enabled: true, Health: 1, EncryptedProxyURL: encrypted}}
	config := domain.DefaultOperationsConfig()
	config.ScopeTargets = map[domain.Scope]domain.RoutingTarget{domain.ScopeBuild: {Mode: domain.RoutingTargetNode, NodeID: 7}}
	repo.config = config
	m := NewManager(repo, cipher)
	defer m.Close(context.Background())
	entered, gate := make(chan struct{}), make(chan struct{})
	var builds atomic.Int32
	m.transport.newBuildClient = func(string, time.Duration) (requestClient, error) {
		if builds.Add(1) == 1 {
			close(entered)
			<-gate
		}
		return &scriptedRequestClient{}, nil
	}
	done := make(chan *Lease, 1)
	failed := make(chan error, 1)
	go func() {
		lease, err := m.Acquire(context.Background(), domain.ScopeBuild, "")
		if err != nil {
			failed <- err
		} else {
			done <- lease
		}
	}()
	select {
	case <-entered:
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("construction did not begin")
	}
	repo.mu.Lock()
	repo.nodes[0].Name = "edited"
	repo.nodes[0].BindingRevision++
	repo.mu.Unlock()
	m.ForgetClearance(7)
	close(gate)
	select {
	case lease := <-done:
		defer lease.Release()
		if lease.NodeName != "edited" {
			t.Fatalf("old selection published %q", lease.NodeName)
		}
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("selection did not reload")
	}
}

func TestCanceledSharedLoadWaitersDoNotAccumulate(t *testing.T) {
	var group sharedLoadGroup
	gate, entered := make(chan struct{}), make(chan struct{})
	load := func() (any, error) { close(entered); <-gate; return 7, nil }
	first, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := waitSharedLoad(first, &group, "key", load); done <- err }()
	<-entered
	cancel()
	<-done
	for i := 0; i < 5000; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := waitSharedLoad(ctx, &group, "key", load)
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	group.mu.Lock()
	count := len(group.calls)
	group.mu.Unlock()
	if count != 1 {
		t.Fatalf("loads retained %d", count)
	}
	close(gate)
	value, err, _ := group.Do("key", func() (any, error) { return 7, nil })
	if err != nil || value != 7 {
		t.Fatalf("shared completion %v %v", value, err)
	}
}

func TestOldLeaseRejectionDoesNotInvalidateRefreshedClearance(t *testing.T) {
	m := NewManager(egressRepositoryTestStub{}, nil)
	defer m.Close(context.Background())
	c := m.clearance
	if !c.cacheClearance("node:7", clearanceSolution{Cookies: "old", UserAgent: "UA"}, time.Now(), 0, "fingerprint", "binding", time.Minute) {
		t.Fatal("cache old")
	}
	lease := &Lease{clearanceManager: m, clearanceKey: "node:7", clearanceGeneration: c.generationFor("node:7")}
	if !c.cacheClearance("node:7", clearanceSolution{Cookies: "fresh", UserAgent: "UA"}, time.Now(), 0, "fingerprint", "binding", time.Minute) {
		t.Fatal("cache fresh")
	}
	lease.InvalidateClearance()
	c.clearanceMu.Lock()
	state := c.clearances["node:7"]
	c.clearanceMu.Unlock()
	if state.invalid || state.cookies != "fresh" {
		t.Fatalf("old rejection invalidated new solve %+v", state)
	}
}
