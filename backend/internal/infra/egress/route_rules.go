package egress

import (
	"context"
	"errors"
	"fmt"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// ErrRouteRuleNodeUnavailable reports that a configured fixed route-rule
// target cannot currently serve the request. Callers fall back to the
// ordinary scope-pool selection instead of failing the request.
var ErrRouteRuleNodeUnavailable = errors.New("egress route rule target unavailable")

// routeRuleNodeCacheTTL bounds the per-target node lookup so rule hits do not
// turn into one DB round trip per request. One second matches the operations
// config snapshot cadence: a node edit becomes visible at the same latency a
// rule edit already does. The cache lives on the Manager instance so test
// managers with the same node IDs never share entries.
const routeRuleNodeCacheTTL = time.Second

type cachedRouteRuleNode struct {
	node      domain.Node
	expiresAt time.Time
}

func (m *Manager) cachedRouteRuleTargetNode(ctx context.Context, nodeID uint64) (domain.Node, bool) {
	now := time.Now().UTC()
	m.routeRuleNodeMu.Lock()
	cached, ok := m.routeRuleNodeCache[nodeID]
	m.routeRuleNodeMu.Unlock()
	if ok && now.Before(cached.expiresAt) {
		return cached.node, true
	}
	node, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return domain.Node{}, false
	}
	m.routeRuleNodeMu.Lock()
	m.routeRuleNodeCache[nodeID] = cachedRouteRuleNode{node: node, expiresAt: now.Add(routeRuleNodeCacheTTL)}
	m.routeRuleNodeMu.Unlock()
	return node, true
}

// RouteRuleDecision reports whether an egress route rule takes over exit
// selection for one upstream call.
type RouteRuleDecision struct {
	Rule domain.RouteRule
	// Applied is true when the rule must be honored for this call.
	Applied bool
}

// RouteRuleFor evaluates the configured rules for one scope and traffic
// class. A rule applies unless the class respects account bindings and the
// caller already carries an explicit node binding (bound nodes must never
// be rerouted).
func (m *Manager) RouteRuleFor(ctx context.Context, scope domain.Scope, class domain.TrafficClass) RouteRuleDecision {
	config, supported, err := m.loadOperationsConfig(ctx, time.Now().UTC())
	if err != nil || !supported {
		return RouteRuleDecision{}
	}
	rule, ok := config.RouteRuleFor(scope, class)
	if !ok {
		return RouteRuleDecision{}
	}
	if egressNodeFromContext(ctx) != 0 && class.RespectsAccountBinding() {
		RecordRouteRuleOutcome(scope, class, RouteRuleOutcomeSkippedBinding)
		return RouteRuleDecision{}
	}
	return RouteRuleDecision{Rule: rule, Applied: true}
}

// AcquireRouted leases the fixed node referenced by a route rule. It returns
// ErrRouteRuleNodeUnavailable when the target cannot serve traffic right now
// so the caller can fall back to the scope pool.
func (m *Manager) AcquireRouted(ctx context.Context, scope domain.Scope, affinity string, nodeID uint64) (*Lease, error) {
	selected, ok := m.cachedRouteRuleTargetNode(ctx, nodeID)
	if !ok {
		return nil, fmt.Errorf("%w: node %d not found", ErrRouteRuleNodeUnavailable, nodeID)
	}
	if !domain.CanNodeServeFixedRouteTarget(selected, scope) {
		return nil, fmt.Errorf("%w: node %d not schedulable", ErrRouteRuleNodeUnavailable, nodeID)
	}
	if !m.isProxyPoolNode(selected) && selected.CooldownUntil != nil && time.Now().UTC().Before(*selected.CooldownUntil) {
		return nil, fmt.Errorf("%w: node %d cooling down", ErrRouteRuleNodeUnavailable, nodeID)
	}
	lease, _, err := m.leaseForNode(ctx, scope, affinity, "", false, selected)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRouteRuleNodeUnavailable, err)
	}
	return lease, nil
}

// AcquireRoutedDirect leases a no-proxy direct connection for a route rule.
func (m *Manager) AcquireRoutedDirect(ctx context.Context, scope domain.Scope, affinity string) (*Lease, error) {
	selected := domain.Node{ID: 0, Name: "route-direct", Scope: scope, Enabled: true, Health: 1}
	lease, _, err := m.leaseForNodeWithOptions(ctx, scope, affinity, "", false, selected, clientOptions{})
	if err != nil {
		return nil, err
	}
	return lease, nil
}
