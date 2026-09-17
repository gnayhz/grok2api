package egress

import (
	"context"
	"errors"
	"hash/fnv"
	"math/rand/v2"
	"sort"
	"strconv"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// cachedPoolFallback memoizes one pool resolution (row + ordered members) for
// request bursts. Members ride the same 1s TTL as every other egress snapshot:
// membership edits become visible within a second without a per-request DB hit.
type cachedPoolFallback struct {
	pool      domain.Pool
	members   []domain.Node
	expiresAt time.Time
}

const poolCacheTTL = time.Second

func (m *routingRuntime) appendPoolCandidates(candidates []domain.Node, ctx context.Context, nodes []domain.Node, now time.Time) []domain.Node {
	for _, node := range nodes {
		node = m.health.overlay(node)
		if !node.Enabled {
			continue
		}
		if nodeExcluded(ctx, node.ID) {
			continue
		}
		// 质量轴不合格(羁押/ban)的成员在选择前剔除(B2 查询点 3 池内
		// 过滤):策略从可用成员中挑,而不是挑到被押成员后整个池失败。
		if !m.qualitySchedulable(ctx, node.ID) {
			continue
		}
		// 冷却口径与自动调度/固定目标共用 domain 唯一判定:池模式成员豁免
		// 普通冷却,出口 IP 质量隔离对它们同样剔除。
		if domain.CooldownBlocksScheduling(node.CooldownUntil, node.LastError, m.snapshotProxyPoolFlag(node), now) {
			continue
		}
		candidates = append(candidates, node)
	}
	return candidates
}

// cachedPoolMembers returns the cached pool row and its stable-ordered
// members, refreshing both together under one TTL. DB 读失败原样上抛:
// 绝不能与"池耗尽"混淆——耗尽可回退 direct,读失败回退 direct 等于
// 一次抖动就让流量绕过全部代理。
func (m *routingRuntime) cachedPoolMembers(ctx context.Context, poolID uint64, now time.Time) (domain.Pool, []domain.Node, error) {
	for {
		if err := ctx.Err(); err != nil {
			return domain.Pool{}, nil, err
		}
		m.fallbackMu.Lock()
		cached, ok := m.poolFallbacks[poolID]
		m.fallbackMu.Unlock()
		if ok && now.Before(cached.expiresAt) {
			return cached.pool, cached.members, nil
		}
		store, exists := m.repository.(egressPoolStore)
		if !exists {
			return domain.Pool{}, nil, errors.New("repository does not support pools")
		}
		loaded, err := m.sharedLoad(ctx, &m.poolLoads, strconv.FormatUint(poolID, 10), func() (any, error) {
			m.fallbackMu.Lock()
			cached, ok := m.poolFallbacks[poolID]
			version := m.poolCacheVersion
			m.fallbackMu.Unlock()
			if ok && time.Now().Before(cached.expiresAt) {
				return cached, nil
			}
			loadCtx, cancel := m.tasks.context(ctx, 5*time.Second)
			defer cancel()
			pool, err := store.GetEgressPool(loadCtx, poolID)
			if err != nil {
				return nil, err
			}
			var nodes []domain.Node
			if runtimeStore, ok := m.repository.(runtimeNodeRepository); ok {
				nodes, err = runtimeStore.ListRuntimeEgressPoolNodes(loadCtx, poolID)
			} else {
				nodes, err = store.ListEgressNodesByPool(loadCtx, poolID)
			}
			if err != nil {
				return nil, err
			}
			m.fallbackMu.Lock()
			defer m.fallbackMu.Unlock()
			if version != m.poolCacheVersion {
				return nil, errNodeSnapshotInvalidated
			}
			now := time.Now()
			for id, value := range m.poolFallbacks {
				if !now.Before(value.expiresAt) {
					delete(m.poolFallbacks, id)
				}
			}
			refreshed := cachedPoolFallback{pool: pool, members: nodes, expiresAt: now.Add(poolCacheTTL)}
			if len(m.poolFallbacks) >= 4096 {
				for id := range m.poolFallbacks {
					delete(m.poolFallbacks, id)
					break
				}
			}
			m.poolFallbacks[poolID] = refreshed
			return refreshed, nil
		})
		if errors.Is(err, errNodeSnapshotInvalidated) {
			now = time.Now()
			continue
		}
		if err != nil {
			return domain.Pool{}, nil, err
		}
		refreshed := loaded.(cachedPoolFallback)
		return refreshed.pool, refreshed.members, nil
	}
}

// PoolRouteOutcome 分类池路由的实际出口,统计口径由此决定:只有"目标池
// 自身选出了成员"才算命中;链式回退到别的池、或回退直连都是降级。
type PoolRouteOutcome int

const (
	PoolRouteNone PoolRouteOutcome = iota
	PoolRouteMember
	PoolRouteChainedPool
	PoolRouteDirect
)

// AcquirePoolRouted resolves a dedicated pool (with its fallback chain) for
// routing decisions. The outcome reports what actually served the request:
// PoolRouteMember means the target pool itself picked a node; chained-pool and
// direct results are fallbacks. PoolRouteNone means no lease was produced;
// an explicitly configured pool target is then rejected by the caller.
func (m *routingRuntime) AcquirePoolRouted(ctx context.Context, scope domain.Scope, affinity string, poolID uint64, allowDirect bool, encryptedCredentialCookies string) (*Lease, PoolRouteOutcome, error) {
	ctx = m.selectionContext(ctx)
	now := time.Now().UTC()
	// 浏览器作用域走池时同样进入 Clearance 托管生命周期:池只是分组,
	// Web/Console 流量对 FlareSolverr 刷新的需求不因经过池而消失。
	managedClearance := isGrokWebScope(scope) && m.managedClearanceMode()
	visited := map[uint64]struct{}{poolID: {}}
	current := poolID
	for {
		pool, members, err := m.cachedPoolMembers(ctx, current, now)
		if err != nil {
			if !errors.Is(err, repository.ErrNotFound) {
				// 读失败向调用方报错:配置了池目标就是圈定出口边界,严格失败,
				// 绝不退回自动调度、更不回退 direct。
				return nil, PoolRouteNone, err
			}
			return nil, PoolRouteNone, nil
		}
		if !pool.Enabled {
			return nil, PoolRouteNone, nil
		}
		buffer := borrowNodeCandidates(len(members))
		candidates := m.appendPoolCandidates((*buffer)[:0], ctx, members, now)
		if len(candidates) == 0 {
			releaseNodeCandidates(buffer)
			switch pool.FallbackMode.Normalized() {
			case domain.PoolFallbackDirect:
				if !allowDirect {
					return nil, PoolRouteNone, nil
				}
				recordSelection(ctx, Selection{NodeName: "pool-direct", Scope: scope})
				direct := domain.Node{ID: 0, Name: "pool-direct", Enabled: true, Health: 1}
				// 与 acquire 的直连分支一致:回退 direct 同样进入 Clearance 托管
				// 生命周期, 否则浏览器作用域的回退租约既无 cf_clearance 也无
				// FlareSolverr 刷新的 UA/cookie, 直连流量大概率被 403 拒绝。
				lease, _, err := m.leaseForNode(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, direct)
				if lease == nil {
					return nil, PoolRouteNone, err
				}
				return lease, PoolRouteDirect, err
			case domain.PoolFallbackPool:
				next := pool.FallbackPoolID
				if next == 0 {
					return nil, PoolRouteNone, nil
				}
				if _, seen := visited[next]; seen {
					m.log().Warn("egress_pool_fallback_cycle", "pool_id", current, "fallback", next)
					return nil, PoolRouteNone, nil
				}
				visited[next] = struct{}{}
				current = next
				continue
			default:
				return nil, PoolRouteNone, nil
			}
		}
		selected := m.selectPoolNode(pool, candidates, members, affinity)
		// Only affinity permits session pinning. Explicit random, least-used,
		// sticky and rotation strategies must retain their configured semantics.
		if session := buildSessionForScope(ctx, scope); session != "" && pool.Strategy.Normalized() == domain.PoolStrategyAffinity {
			selected = m.pinSessionNode(ctx, session, candidates, selected)
		}
		releaseNodeCandidates(buffer)
		RecordPoolSelection(pool.ID, selected.ID)
		lease, _, err := m.leaseForNode(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, selected)
		if err != nil {
			return nil, PoolRouteNone, err
		}
		if current == poolID {
			return lease, PoolRouteMember, nil
		}
		return lease, PoolRouteChainedPool, nil
	}
}

// invalidatePoolSnapshots refreshes membership and health without resetting
// rotation progress. Transport feedback must not send a rotation pool backwards.
func (m *routingRuntime) invalidatePoolSnapshots() {
	m.fallbackMu.Lock()
	m.poolCacheVersion++
	clear(m.poolFallbacks)
	m.fallbackMu.Unlock()
}

// InvalidatePoolCache drops cached pool resolutions and local rotation state
// after pool configuration changes. Subsequent selection re-seeds from the
// persisted row; already-running persistence still relies on repository CAS.
func (m *routingRuntime) InvalidatePoolCache() {
	m.invalidatePoolSnapshots()
	m.rotationMu.Lock()
	m.rotationCursors = make(map[uint64]uint64)
	m.rotationPersists = make(map[uint64]*rotationPersistState)
	m.rotationMu.Unlock()
}

// selectPoolNode applies the pool's scheduling strategy:
//   - affinity (default): rendezvous hashing on the caller identity keeps every
//     account on a stable exit IP; a node leaving/rejoining only reshuffles the
//     callers that hashed onto it;
//   - random: every request picks a random member;
//   - sticky: always the first schedulable member in stable id order — it only
//     moves on when that member breaks;
//   - rotation: stay on the current member until it breaks, then advance to
//     the next member in stable id order (wrapping) and never regress on
//     recovery — unlike sticky, a recovered earlier member does not reclaim
//     traffic;
//   - least-used (G9): pick the member with the fewest selections in the
//     current stats window — evens usage; ties resolve to stable order.
func (m *routingRuntime) selectPoolNode(pool domain.Pool, nodes, allMembers []domain.Node, affinity string) domain.Node {
	// rotation 必须先于单节点早返回:只剩一个候选时也要推进游标,
	// 否则其他成员恢复后会"偷回"流量,违背只进不回的语义。
	if pool.Strategy.Normalized() == domain.PoolStrategyRotation {
		return m.selectRotationNode(pool, nodes, allMembers)
	}
	if len(nodes) == 1 {
		return nodes[0]
	}
	switch pool.Strategy.Normalized() {
	case domain.PoolStrategyRandom:
		return nodes[rand.IntN(len(nodes))]
	case domain.PoolStrategySticky:
		return nodes[0]
	case domain.PoolStrategyLeastUsed:
		return selectLeastUsedNode(pool.ID, nodes)
	default:
		if affinity == "" {
			return m.selectNode(nodes, "")
		}
		best := nodes[0]
		bestScore := affinityNodeScore(affinity, nodes[0].ID)
		for _, node := range nodes[1:] {
			if score := affinityNodeScore(affinity, node.ID); score > bestScore {
				best, bestScore = node, score
			}
		}
		if best.Health >= 0.8 {
			return best
		}
		healthiest := best
		for _, node := range nodes {
			if node.Health > healthiest.Health {
				healthiest = node
			}
		}
		return healthiest
	}
}

// selectLeastUsedNode implements the least-used strategy (G9): the member
// with the fewest recorded selections wins; ties resolve to the stable
// candidate order (priority, then id). Counts come from the in-process
// pool stats window (reset on restart) — members without a record count
// as zero and pick up traffic first.
func selectLeastUsedNode(poolID uint64, nodes []domain.Node) domain.Node {
	counts := poolSelectionCounts(poolID)
	best := nodes[0]
	bestUsage := counts[best.ID]
	for _, node := range nodes[1:] {
		if usage := counts[node.ID]; usage < bestUsage {
			best, bestUsage = node, usage
		}
	}
	return best
}

// selectRotationNode implements the rotation strategy: a persistent per-pool
// cursor (stored on the pool row, surviving restarts) pins traffic to one
// member until that member leaves the candidate set; the cursor then advances
// to the next available member in full-member order (priority, then id),
// wrapping around. Recovery of an earlier member never moves the cursor back.
func (m *routingRuntime) selectRotationNode(pool domain.Pool, candidates, allMembers []domain.Node) domain.Node {
	if len(candidates) == 0 {
		return domain.Node{}
	}
	// 全成员序内聚,规则必须与仓储 ORDER BY 完全一致:
	// (priority > 0) DESC, priority ASC, id ASC —— 已设 priority 的排前,
	// 未设(0)排后。否则同一池里 sticky(仓储序)与 rotation(内部序)
	// 的"首"会指向不同节点。
	// 仓储按同序返回是常态:先用 O(n) 探测,已有序直接使用(旋转池热路径
	// 每请求一次,百成员池的拷贝+排序是纯浪费);仅当顺序不符(仓储实现
	// 变更/测试夹具)才回退到拷贝+排序,正确性不依赖仓储约定。
	rotationLess := func(i, j int) bool {
		pi, pj := allMembers[i].PoolPriority > 0, allMembers[j].PoolPriority > 0
		if pi != pj {
			return pi
		}
		if allMembers[i].PoolPriority != allMembers[j].PoolPriority {
			return allMembers[i].PoolPriority < allMembers[j].PoolPriority
		}
		return allMembers[i].ID < allMembers[j].ID
	}
	if !sort.SliceIsSorted(allMembers, rotationLess) {
		ordered := append([]domain.Node(nil), allMembers...)
		sort.SliceStable(ordered, rotationLess)
		allMembers = ordered
	}
	available := func(id uint64) bool {
		for _, node := range candidates {
			if node.ID == id {
				return true
			}
		}
		return false
	}
	// 游标读取:内存热值(本进程最近推进)优先,持久值兑底(重启恢复)。
	m.rotationMu.Lock()
	cursor, hot := m.rotationCursors[pool.ID]
	m.rotationMu.Unlock()
	if !hot {
		cursor = pool.RotationCursorNodeID
	}
	if cursor != 0 && available(cursor) {
		for _, node := range candidates {
			if node.ID == cursor {
				return node
			}
		}
	}
	// 游标不可用: 在全成员序中找游标位置(在池但坏了),从它之后找第一个
	// 可用成员。游标节点已被移出池时其位置无从定位——从头开始(管理员
	// 主动移除成员,重置起点是可接受的语义);都没有则绕回列表头。
	start := 0
	for index, node := range allMembers {
		if node.ID == cursor {
			start = index + 1
			break
		}
	}
	for offset := 1; offset <= len(allMembers); offset++ {
		node := allMembers[(start+offset-1)%len(allMembers)]
		if available(node.ID) {
			m.persistRotationCursor(pool.ID, cursor, node.ID)
			return node
		}
	}
	// 不应达到(候选非空且在成员序里)——防御性兑底。
	m.persistRotationCursor(pool.ID, cursor, candidates[0].ID)
	return candidates[0]
}
func (m *routingRuntime) persistRotationCursor(poolID, fromNodeID, nodeID uint64) {
	m.rotationMu.Lock()
	if len(m.rotationCursors) >= 4096 {
		for id, state := range m.rotationPersists {
			if !state.writing {
				delete(m.rotationPersists, id)
				delete(m.rotationCursors, id)
				break
			}
		}
		if len(m.rotationCursors) >= 4096 {
			m.rotationMu.Unlock()
			return
		}
	}
	m.rotationCursors[poolID] = nodeID
	state := m.rotationPersists[poolID]
	if state == nil {
		state = &rotationPersistState{}
		m.rotationPersists[poolID] = state
	}
	if state.writing {
		// 已有写在进行:只登记最新目标,由写完成后的循环补写,
		// 并发推进合并为一次 DB 写。
		state.pending = nodeID
		m.rotationMu.Unlock()
		return
	}
	if state.last == nodeID {
		// DB 已是该值(进程内或并发推进已写过),重复写纯属写放大。
		m.rotationMu.Unlock()
		return
	}
	state.writing = true
	m.rotationMu.Unlock()
	if !m.tasks.start("rotation", func() { m.writeRotationCursor(poolID, fromNodeID, nodeID) }) {
		m.rotationMu.Lock()
		state.writing = false
		m.rotationMu.Unlock()
	}
}

// writeRotationCursor 异步落盘一个游标值并在成功后处理登记的 pending
// 推进;写失败把 last 清零,让下一次推进重试。写脱离调用方生命周期
// (Background),选路绝不等待 DB。
func (m *routingRuntime) writeRotationCursor(poolID, fromNodeID, nodeID uint64) {
	for {
		err := error(nil)
		if store, ok := m.repository.(interface {
			UpdateEgressPoolRotationCursor(context.Context, uint64, uint64, uint64) error
		}); ok {
			writeCtx, cancel := context.WithTimeout(m.tasks.ctx, 2*time.Second)
			err = store.UpdateEgressPoolRotationCursor(writeCtx, poolID, fromNodeID, nodeID)
			cancel()
			if err != nil {
				m.log().Warn("egress_rotation_cursor_save_failed", "pool_id", poolID, "error", err.Error())
			}
		}
		// 仓库不支持游标持久化时 err 保持 nil:按"内存态游标"处理,
		// last 记为目标值仅用于进程内去重, 重启回退到首个成员是既定语义。
		m.rotationMu.Lock()
		state := m.rotationPersists[poolID]
		if state == nil {
			m.rotationMu.Unlock()
			return
		}
		if err == nil {
			state.last = nodeID
		} else {
			state.last = 0
		}
		next := state.pending
		state.pending = 0
		if next == 0 || next == state.last || m.tasks.ctx.Err() != nil {
			state.writing = false
			m.rotationMu.Unlock()
			return
		}
		fromNodeID, nodeID = state.last, next
		m.rotationMu.Unlock()
	}
}

// affinityNodeScore implements rendezvous (highest-random-weight) hashing:
// score = fmix64(fnv1a64(affinity || nodeID)). Stable per (identity, node) pair and
// independent of pool size, so membership changes cause minimal reshuffle.
func affinityNodeScore(affinity string, nodeID uint64) uint64 {
	hash := fnv.New64a()
	hash.Write([]byte(affinity))
	var idBytes [8]byte
	for i := 0; i < 8; i++ {
		idBytes[i] = byte(nodeID >> (56 - 8*i))
	}
	hash.Write(idBytes[:])
	// Avalanche the suffix bits before comparing scores. Raw FNV-1a scores
	// for consecutive node IDs are correlated: a 16-member pool placed 51.2%
	// of 10,000 accounts on its last node. This stable fmix64 finalizer keeps
	// rendezvous's minimal-remapping property and avoids per-node allocations.
	score := hash.Sum64()
	score ^= score >> 33
	score *= 0xff51afd7ed558ccd
	score ^= score >> 33
	score *= 0xc4ceb9fe1a85ec53
	return score ^ (score >> 33)
}
