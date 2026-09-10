package egress

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
// target. An ErrNotFound miss is returned as found=false; the caller rejects
// an explicitly configured missing target. Other DB errors are returned:
// 一次抖动把固定目标静默降级成自动调度,等于让流量无声绕开管理员配置
// 的出口,必须留痕并按读失败语义上抛。
func (m *routingRuntime) cachedRoutingTargetNode(ctx context.Context, nodeID uint64) (domain.Node, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return domain.Node{}, false, err
		}
		m.routeRuleNodeMu.RLock()
		cached, ok := m.routeRuleNodeCache[nodeID]
		m.routeRuleNodeMu.RUnlock()
		if ok && time.Now().Before(cached.expiresAt) {
			return cached.node, true, nil
		}
		loaded, err := m.sharedLoad(ctx, &m.routingTargetLoads, strconv.FormatUint(nodeID, 10), func() (any, error) {
			m.routeRuleNodeMu.RLock()
			cached, ok := m.routeRuleNodeCache[nodeID]
			version := m.routeRuleNodeVersion
			m.routeRuleNodeMu.RUnlock()
			if ok && time.Now().Before(cached.expiresAt) {
				return cached.node, nil
			}
			loadCtx, cancel := m.tasks.context(ctx, 5*time.Second)
			defer cancel()
			node, err := m.getRuntimeNode(loadCtx, nodeID)
			m.routeRuleNodeMu.Lock()
			defer m.routeRuleNodeMu.Unlock()
			if version != m.routeRuleNodeVersion {
				return nil, errNodeSnapshotInvalidated
			}
			if err != nil {
				return nil, err
			}
			// Remove expired entries on a miss. Target churn must not grow this map forever.
			now := time.Now()
			for id, value := range m.routeRuleNodeCache {
				if !now.Before(value.expiresAt) {
					delete(m.routeRuleNodeCache, id)
				}
			}
			if len(m.routeRuleNodeCache) >= 4096 {
				for id := range m.routeRuleNodeCache {
					delete(m.routeRuleNodeCache, id)
					break
				}
			}
			m.routeRuleNodeCache[nodeID] = cachedRoutingTargetNode{node: node, expiresAt: now.Add(routingTargetNodeCacheTTL)}
			return node, nil
		})
		if errors.Is(err, errNodeSnapshotInvalidated) {
			continue
		}
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return domain.Node{}, false, nil
			}
			if ctx.Err() == nil {
				m.log().Warn("egress_routing_target_read_failed", "node_id", nodeID, "error", err.Error())
			}
			return domain.Node{}, false, fmt.Errorf("读取固定路由目标节点 %d: %w", nodeID, err)
		}
		return loaded.(domain.Node), true, nil
	}
}
