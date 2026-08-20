package egress

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type routeRuleRepository struct {
	egressRepositoryTestStub
	node   domain.Node
	config domain.OperationsConfig
}

func (r *routeRuleRepository) GetEgressNode(context.Context, uint64) (domain.Node, error) {
	if r.node.ID == 0 {
		return domain.Node{}, repository.ErrNotFound
	}
	return r.node, nil
}

func (r *routeRuleRepository) GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error) {
	return r.config, nil
}

func newRouteRuleTestManager(t *testing.T, node domain.Node, config domain.OperationsConfig) *Manager {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	if node.EncryptedProxyURL == "" {
		node.EncryptedProxyURL = mustEncryptRouteRuleProxy(t, cipher, "http://proxy.example:8080")
	}
	manager := NewManager(&routeRuleRepository{node: node, config: config}, cipher)
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		return &scriptedRequestClient{do: func(int, *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		}}, nil
	}
	return manager
}

func mustEncryptRouteRuleProxy(t *testing.T, cipher *security.Cipher, value string) string {
	t.Helper()
	encrypted, err := cipher.Encrypt(value)
	if err != nil {
		t.Fatal(err)
	}
	return encrypted
}

func TestAcquireRoutedFixedLease(t *testing.T) {
	config := domain.OperationsConfig{
		RouteRules: []domain.RouteRule{
			{Scope: domain.ScopeBuild, Class: domain.TrafficClassBilling, TargetMode: domain.RouteRuleTargetFixed, TargetNodeID: 11, Enabled: true},
		},
	}
	node := domain.Node{ID: 11, Name: "cheap", Scope: domain.ScopeBuild, Enabled: true}
	manager := newRouteRuleTestManager(t, node, config)

	lease, err := manager.AcquireRouted(context.Background(), domain.ScopeBuild, "acct", 11)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 11 || lease.ProxyURL == "" {
		t.Fatalf("lease = node %d proxy %q", lease.NodeID, lease.ProxyURL)
	}
}

func TestAcquireRoutedFixedUnavailableVariants(t *testing.T) {
	cooldown := time.Now().UTC().Add(5 * time.Minute)
	variants := map[string]domain.Node{
		"missing node": {ID: 0},
		"disabled":     {ID: 11, Scope: domain.ScopeBuild, Enabled: false},
		"no proxy":     {ID: 11, Scope: domain.ScopeBuild, Enabled: true, EncryptedProxyURL: "-"},
		"wrong scope":  {ID: 11, Scope: domain.ScopeWeb, Enabled: true},
		"cooling down": {ID: 11, Scope: domain.ScopeBuild, Enabled: true, CooldownUntil: &cooldown},
	}
	for name, node := range variants {
		manager := newRouteRuleTestManager(t, node, domain.OperationsConfig{})
		_, err := manager.AcquireRouted(context.Background(), domain.ScopeBuild, "acct", 11)
		if !errors.Is(err, ErrRouteRuleNodeUnavailable) {
			t.Errorf("%s: err = %v, want ErrRouteRuleNodeUnavailable", name, err)
		}
	}
}

// A failing operations-config read must fail open: the decision returns
// not-applied and traffic keeps the ordinary pool path instead of erroring.
func TestRouteRuleForFailsOpenOnConfigError(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewManager(&failingRouteRuleConfigRepository{}, cipher)
	decision := fresh.RouteRuleFor(context.Background(), domain.ScopeBuild, domain.TrafficClassBilling)
	if decision.Applied {
		t.Fatalf("config read failure must fail open, got %+v", decision)
	}
}

type failingRouteRuleConfigRepository struct {
	egressRepositoryTestStub
}

func (r *failingRouteRuleConfigRepository) GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error) {
	return domain.OperationsConfig{}, errors.New("db down")
}

// Pool nodes are valid fixed targets; the lease is a proxy-pool lease whose
// exit IP is decided by the gateway, not by the gateway's pool flag.
func TestAcquireRoutedAcceptsProxyPoolNode(t *testing.T) {
	node := domain.Node{ID: 12, Name: "rotating", Scope: domain.ScopeBuild, Enabled: true, ProxyPool: true}
	config := domain.OperationsConfig{}
	manager := newRouteRuleTestManager(t, node, config)

	lease, err := manager.AcquireRouted(context.Background(), domain.ScopeBuild, "acct", 12)
	if err != nil {
		t.Fatalf("pool node must be routable: %v", err)
	}
	defer lease.Release()
	if lease.NodeID != 12 || !lease.proxyPool {
		t.Fatalf("lease = node %d proxyPool %v", lease.NodeID, lease.proxyPool)
	}
}

// The 1s target cache must serve repeated rule hits without extra DB reads
// and must stay per-manager so different managers never share entries.
func TestAcquireRoutedTargetNodeCache(t *testing.T) {
	node := domain.Node{ID: 21, Name: "cached", Scope: domain.ScopeBuild, Enabled: true}
	manager := newRouteRuleTestManager(t, node, domain.OperationsConfig{})

	lease, err := manager.AcquireRouted(context.Background(), domain.ScopeBuild, "acct", 21)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()

	manager.routeRuleNodeMu.Lock()
	cached, ok := manager.routeRuleNodeCache[21]
	manager.routeRuleNodeMu.Unlock()
	if !ok || cached.node.ID != 21 {
		t.Fatalf("target node was not cached: %+v ok=%v", cached, ok)
	}

	// A second manager with the same node ID must not see the first cache.
	other := newRouteRuleTestManager(t, node, domain.OperationsConfig{})
	other.routeRuleNodeMu.Lock()
	_, shared := other.routeRuleNodeCache[21]
	other.routeRuleNodeMu.Unlock()
	if shared {
		t.Fatal("route-rule target cache must be per-manager")
	}
}

func TestAcquireRoutedDirectLease(t *testing.T) {
	manager := newRouteRuleTestManager(t, domain.Node{}, domain.OperationsConfig{})
	lease, err := manager.AcquireRoutedDirect(context.Background(), domain.ScopeBuild, "acct")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 0 || lease.ProxyURL != "" {
		t.Fatalf("direct lease = node %d proxy %q, want node 0 and empty proxy", lease.NodeID, lease.ProxyURL)
	}
}
