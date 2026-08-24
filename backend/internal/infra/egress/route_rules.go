package egress

import (
	"context"
	"errors"
	"fmt"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// routingTargetNodeCacheTTL bounds the per-target node lookup so routing hits
// do not turn into one DB round trip per request. One second matches the
// operations config snapshot cadence: a node edit becomes visible at the
// same latency a routing edit already does. The cache lives on the Manager
// instance so test managers with the same node IDs never share entries.
const routingTargetNodeCacheTTL = time.Second

type cachedRoutingTargetNode struct {
	node      domain.Node
	expiresAt time.Time
}

// cachedRoutingTargetNode returns the current snapshot of one fixed routing
// target. A ErrNotFound miss is not an error: the caller falls back to the
// automatic schedule. Any other DB error is a read failure and is returned:
// 一次抖动把固定目标静默降级成自动调度,等于让流量无声绕开管理员配置
// 的出口,必须留痕并按读失败语义上抛。
func (m *Manager) cachedRoutingTargetNode(ctx context.Context, nodeID uint64) (domain.Node, bool, error) {
	now := time.Now().UTC()
	m.routeRuleNodeMu.Lock()
	cached, ok := m.routeRuleNodeCache[nodeID]
	m.routeRuleNodeMu.Unlock()
	if ok && now.Before(cached.expiresAt) {
		return cached.node, true, nil
	}
	node, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return domain.Node{}, false, nil
		}
		m.log().Warn("egress_routing_target_read_failed", "node_id", nodeID, "error", err.Error())
		return domain.Node{}, false, fmt.Errorf("读取固定路由目标节点 %d: %w", nodeID, err)
	}
	m.routeRuleNodeMu.Lock()
	m.routeRuleNodeCache[nodeID] = cachedRoutingTargetNode{node: node, expiresAt: now.Add(routingTargetNodeCacheTTL)}
	m.routeRuleNodeMu.Unlock()
	return node, true, nil
}
