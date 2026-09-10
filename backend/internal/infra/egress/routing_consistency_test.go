package egress

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Capture a repository snapshot before blocking, like a query that races an
// administrator commit. Channels establish ordering without timing the query.
type routingGateRepo struct {
	egressRepositoryTestStub
	mu      sync.Mutex
	node    domain.Node
	config  domain.OperationsConfig
	gate    chan struct{}
	entered chan struct{}
	first   bool
}

func (r *routingGateRepo) snapshot(ctx context.Context) (domain.Node, domain.OperationsConfig, error) {
	r.mu.Lock()
	node, config := r.node, r.config
	block := !r.first
	r.first = true
	r.mu.Unlock()
	if block {
		close(r.entered)
		select {
		case <-r.gate:
		case <-ctx.Done():
			return domain.Node{}, domain.OperationsConfig{}, ctx.Err()
		}
	}
	return node, config, nil
}

func (r *routingGateRepo) GetEgressNode(ctx context.Context, _ uint64) (domain.Node, error) {
	n, _, err := r.snapshot(ctx)
	return n, err
}

func (r *routingGateRepo) ListEgressNodes(ctx context.Context, _ repository.SortQuery) ([]domain.Node, error) {
	n, _, err := r.snapshot(ctx)
	return []domain.Node{n}, err
}

func (r *routingGateRepo) GetEgressOperationsConfig(ctx context.Context) (domain.OperationsConfig, error) {
	_, cfg, err := r.snapshot(ctx)
	return cfg, err
}

func (r *routingGateRepo) GetEgressPool(context.Context, uint64) (domain.Pool, error) {
	return domain.Pool{ID: 1, Enabled: true}, nil
}

func (r *routingGateRepo) ListEgressNodesByPool(ctx context.Context, _ uint64) ([]domain.Node, error) {
	n, _, err := r.snapshot(ctx)
	return []domain.Node{n}, err
}

func newRoutingGateRepo() *routingGateRepo {
	return &routingGateRepo{node: domain.Node{ID: 1, Name: "old", Enabled: true},
		config: domain.OperationsConfig{ProbeIntervalSeconds: 60},
		gate:   make(chan struct{}), entered: make(chan struct{})}
}

var routingReaders = map[string]func(context.Context, *Manager) (string, error){
	"nodes": func(ctx context.Context, m *Manager) (string, error) {
		n, err := m.listNodes(ctx, time.Now())
		if err != nil {
			return "", err
		}
		return n[0].Name, nil
	},
	"target": func(ctx context.Context, m *Manager) (string, error) {
		n, _, err := m.cachedRoutingTargetNode(ctx, 1)
		return n.Name, err
	},
	"pool": func(ctx context.Context, m *Manager) (string, error) {
		_, n, err := m.routing.cachedPoolMembers(ctx, 1, time.Now())
		if err != nil {
			return "", err
		}
		return n[0].Name, nil
	},
	"config": func(ctx context.Context, m *Manager) (string, error) {
		cfg, _, err := m.loadOperationsConfig(ctx, time.Now())
		return fmt.Sprint(cfg.ProbeIntervalSeconds), err
	},
}

func TestRoutingReadersCancelWhileSharedLoadContinues(t *testing.T) {
	for name, read := range routingReaders {
		t.Run(name, func(t *testing.T) {
			r := newRoutingGateRepo()
			defer close(r.gate)
			m := NewManager(r, nil)
			t.Cleanup(func() { _ = m.Close(context.Background()) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := read(ctx, m); done <- err }()
			<-r.entered
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel: %v", err)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("canceled caller remained blocked on shared load")
			}
		})
	}
}

func TestRoutingInvalidationRejectsInflightSnapshots(t *testing.T) {
	for name, read := range routingReaders {
		t.Run(name, func(t *testing.T) {
			r := newRoutingGateRepo()
			m := NewManager(r, nil)
			t.Cleanup(func() { _ = m.Close(context.Background()) })
			done := make(chan string, 1)
			go func() { value, err := read(context.Background(), m); done <- fmt.Sprintf("%s/%v", value, err) }()
			<-r.entered
			r.mu.Lock()
			r.node.Name = "new"
			r.config.ProbeIntervalSeconds = 120
			r.mu.Unlock()
			m.ForgetClearance(1)
			m.InvalidateOperationsConfig()
			m.InvalidatePoolCache()
			close(r.gate)
			want := "new/<nil>"
			if name == "config" {
				want = "120/<nil>"
			}
			if got := <-done; got != want {
				t.Fatalf("stale result escaped invalidation: %s, want %s", got, want)
			}
			if got, _ := read(context.Background(), m); got+"/<nil>" != want {
				t.Fatalf("stale cache: %s", got)
			}
		})
	}
}

func TestNodeMutationInvalidatesAllRoutingViews(t *testing.T) {
	for _, invalidate := range []struct {
		name string
		run  func(*Manager)
	}{
		{"edit", func(m *Manager) { m.ForgetClearance(1) }},
		{"health", func(m *Manager) { m.invalidateNodes() }},
	} {
		t.Run(invalidate.name, func(t *testing.T) {
			r := newRoutingGateRepo()
			close(r.gate)
			m := NewManager(r, nil)
			t.Cleanup(func() { _ = m.Close(context.Background()) })
			for _, name := range []string{"nodes", "target", "pool"} {
				_, _ = routingReaders[name](context.Background(), m)
			}
			r.mu.Lock()
			r.node.Name = "new"
			r.mu.Unlock()
			invalidate.run(m)
			for _, name := range []string{"nodes", "target", "pool"} {
				if got, err := routingReaders[name](context.Background(), m); err != nil || got != "new" {
					t.Errorf("%s stale after mutation: %s, %v", name, got, err)
				}
			}
		})
	}
}

func TestAffinityDistributesSequentialNodeIDs(t *testing.T) {
	m := NewManager(egressRepositoryTestStub{}, nil)
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	nodes := make([]domain.Node, 16)
	for i := range nodes {
		nodes[i] = domain.Node{ID: uint64(i + 1), Enabled: true, Health: 1}
	}
	counts := make(map[uint64]int)
	const accounts = 10000
	for i := range accounts {
		counts[m.routing.selectPoolNode(domain.Pool{}, nodes, nodes, fmt.Sprintf("account_%d", i)).ID]++
	}
	for _, n := range nodes {
		if counts[n.ID] < 400 || counts[n.ID] > 850 {
			t.Errorf("node %d gets %d/%d accounts, expected roughly 1/16", n.ID, counts[n.ID], accounts)
		}
	}
}
