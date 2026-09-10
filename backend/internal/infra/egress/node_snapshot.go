package egress

import (
	"context"
	"errors"
	"sort"
	"sync/atomic"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// nodeSnapshotKey is the single global node-snapshot cache key: nodes are
// scope-free resources, so one snapshot serves every request family.
const nodeSnapshotKey = "nodes"

func (m *routingRuntime) listNodes(ctx context.Context, now time.Time) ([]domain.Node, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m.nodeMu.RLock()
		if snapshot, ok := m.nodes[nodeSnapshotKey]; ok && now.Before(snapshot.expiresAt) {
			// Node snapshots are replaced with a copied slice and treated as immutable
			// by callers. Returning the shared read-only slice avoids a per-request copy.
			values := snapshot.values
			m.nodeMu.RUnlock()
			return values, nil
		}
		m.nodeMu.RUnlock()
		loaded, err := m.sharedLoad(ctx, &m.nodeLoads, nodeSnapshotKey, func() (any, error) {
			checkTime := time.Now().UTC()
			m.nodeMu.RLock()
			if snapshot, ok := m.nodes[nodeSnapshotKey]; ok && checkTime.Before(snapshot.expiresAt) {
				values := snapshot.values
				m.nodeMu.RUnlock()
				return values, nil
			}
			version := m.nodeVersions[nodeSnapshotKey]
			m.nodeMu.RUnlock()
			// 脱离领头调用方的请求生命周期:singleflight 合并的所有等待者共享这次
			// 回源, 领头者断开不应把 DB 读取消连坐给它们(等待者自身的取消仍由
			// 各自的 select 处理)。5s 足够覆盖一次快照查询。
			loadCtx, cancel := m.tasks.context(ctx, 5*time.Second)
			defer cancel()
			values, err := m.listRuntimeNodes(loadCtx)
			if err != nil {
				return nil, err
			}
			m.nodeMu.Lock()
			if m.nodeVersions[nodeSnapshotKey] != version {
				m.nodeMu.Unlock()
				return nil, errNodeSnapshotInvalidated
			}
			m.replaceNodeSnapshotLocked(values, checkTime.Add(nodeSnapshotTTL))
			values = m.nodes[nodeSnapshotKey].values
			m.nodeMu.Unlock()
			return values, nil
		})
		if err != nil {
			if errors.Is(err, errNodeSnapshotInvalidated) && ctx.Err() == nil {
				now = time.Now().UTC()
				continue
			}
			return nil, err
		}
		return loaded.([]domain.Node), nil
	}
}

func (m *routingRuntime) InvalidateNodeSnapshots() { m.invalidateNodes() }

func (m *routingRuntime) invalidateNodes() {
	m.nodeMu.Lock()
	m.nodeVersions[nodeSnapshotKey]++
	snapshot, ok := m.nodes[nodeSnapshotKey]
	if ok {
		delete(m.nodes, nodeSnapshotKey)
	}
	for _, node := range snapshot.values {
		delete(m.healthyNodes, node.ID)
	}
	m.nodeMu.Unlock()
	// Routing-target node caching lives outside the node snapshot; drop it
	// whenever node state changes so a disabled or deleted target stops
	// serving immediately.
	m.routeRuleNodeMu.Lock()
	m.routeRuleNodeVersion++
	clear(m.routeRuleNodeCache)
	m.routeRuleNodeMu.Unlock()
	m.invalidatePoolSnapshots()
}

func (m *routingRuntime) replaceNodeSnapshotLocked(values []domain.Node, expiresAt time.Time) {
	values = append([]domain.Node(nil), values...)
	sort.SliceStable(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	for _, node := range m.nodes[nodeSnapshotKey].values {
		delete(m.healthyNodes, node.ID)
	}
	poolFlags := make(map[uint64]bool, len(values))
	for _, node := range values {
		if nodeIsHealthy(node) {
			m.healthyNodes[node.ID] = expiresAt
		} else {
			delete(m.healthyNodes, node.ID)
		}
		poolFlags[node.ID] = m.isProxyPoolNodeDirect(node)
	}
	m.nodes[nodeSnapshotKey] = cachedNodeSnapshot{values: values, poolFlags: poolFlags, expiresAt: expiresAt}
	m.sweepDeletedInflightLocked(values)
}

// sweepDeletedInflightLocked 清理"已不存在且计数为零"的节点 inflight 计数
// 条目。计数器永不删除是为避免 ABA(并发 release 对替换计数器递减), 但订阅
// 换血会持续产生新节点 ID, 零散条目无界累积——与 poolNodeStats 容量逐出
// 同类。只删 [快照中不存在 && count==0] 的条目:
//   - count>0 说明仍有删除前创建的租约在途, 保留避免丢计数;
//   - 对已删条目的迟来递减已被 decrementInflight 的 Load 守卫吞掉;
//   - 节点 ID 不复用(autoincrement), 不存在 ABA 复活。
func (m *routingRuntime) sweepDeletedInflightLocked(values []domain.Node) {
	m.inflightMu.Lock()
	defer m.inflightMu.Unlock()
	live := make(map[uint64]struct{}, len(values))
	for _, node := range values {
		live[node.ID] = struct{}{}
	}
	m.inflight.Range(func(key, value any) bool {
		nodeID, ok := key.(uint64)
		if !ok {
			return true
		}
		if _, exists := live[nodeID]; exists {
			return true
		}
		if value.(*atomic.Int64).Load() != 0 {
			return true
		}
		m.inflight.Delete(nodeID)
		m.inflightEntries--
		return true
	})
}

func (m *routingRuntime) InvalidateOperationsConfig() {
	m.operationsMu.Lock()
	m.operationsConfig = cachedOperationsConfig{}
	m.operationsConfigVer++
	m.operationsMu.Unlock()
}
