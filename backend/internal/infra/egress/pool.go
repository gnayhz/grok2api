package egress

import (
	"context"
	"errors"
	"hash/fnv"
	"math/rand/v2"
	"sort"
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

// SetDegradeEvidenceCooldowns configures soft-cooldown escalation bounds.
func (m *Manager) SetDegradeEvidenceCooldowns(base, max time.Duration) {
	if base <= 0 {
		base = 5 * time.Minute
	}
	if max < base {
		max = base
	}
	m.softMu.Lock()
	m.softBase, m.softMax = base, max
	m.softMu.Unlock()
}

// MarkDegradeEvidence applies the pending soft cooldown after one degrade
// verdict (withhold / idle / header-budget). The node is avoided by every
// account until attribution confirms (CLEAN upgrades to quarantine) or the
// escalated deadline expires. Repeat evidence inside the window doubles the
// cooldown, bounded by max — with attribution unavailable this converges the
// "hit a degraded exit every request" pattern to at most once per cooldown.
func (m *Manager) MarkDegradeEvidence(nodeID uint64) {
	if m == nil || nodeID == 0 {
		return
	}
	now := time.Now().UTC()
	m.softMu.Lock()
	defer m.softMu.Unlock()
	state := m.softCooldowns[nodeID]
	state.count++
	until := m.softBase
	for i := 1; i < state.count; i++ {
		until *= 2
		if until >= m.softMax {
			until = m.softMax
			break
		}
	}
	if until > m.softMax {
		until = m.softMax
	}
	state.until = now.Add(until)
	m.softCooldowns[nodeID] = state
	m.log().Warn("egress_node_degrade_evidence", "node_id", nodeID, "cooldown", until.String(), "evidence_count", state.count)
}

// ClearDegradeEvidence lifts the pending soft cooldown. Called when
// attribution exonerates the node (RSC RISK — the account is the problem).
func (m *Manager) ClearDegradeEvidence(nodeID uint64) {
	if m == nil || nodeID == 0 {
		return
	}
	m.softMu.Lock()
	delete(m.softCooldowns, nodeID)
	m.softMu.Unlock()
}

func (m *Manager) nodeSoftCooled(nodeID uint64, now time.Time) bool {
	// 读多写少:未冷却的节点(绝大多数)走 RLock 快路径, 只有需要淘汰
	// 过期条目时才升级为写锁——池候选过滤会对每个成员调用一次,
	// 独占锁在百成员池上是可测的串行化热点。
	m.softMu.RLock()
	state, ok := m.softCooldowns[nodeID]
	if !ok {
		m.softMu.RUnlock()
		return false
	}
	cooled := now.Before(state.until)
	if cooled || state.count > 1 {
		// 冷却中, 或处于阶梯窗口内的过期条目(保留 count 供递增):
		// 只读, 无需升级。
		m.softMu.RUnlock()
		return cooled
	}
	m.softMu.RUnlock()
	m.softMu.Lock()
	// 双检:解锁间隙内可能被并发写入新证据。
	state, ok = m.softCooldowns[nodeID]
	if ok && !now.Before(state.until) && !(state.count > 1 && now.Before(state.until.Add(m.softMax))) {
		delete(m.softCooldowns, nodeID)
	}
	m.softMu.Unlock()
	return false
}

// poolCandidates 把池成员(全成员序,含首选顺序)过滤成当前可调度候选。
// 节点无作用域:池服务路由送来的任何流量。请求内排除在此生效;固定节点
// 额外受 L2 软冷却与硬冷却约束。代理池模式节点豁免两类冷却(旋转端点的
// 单次降智/失败不代表端点坏——与文档"守卫不动代理池节点"一致), 仅保留
// 请求内排除(L1)兜底。
func (m *Manager) poolCandidates(ctx context.Context, nodes []domain.Node, now time.Time) []domain.Node {
	candidates := make([]domain.Node, 0, len(nodes))
	for _, node := range nodes {
		if !node.Enabled {
			continue
		}
		if nodeExcluded(ctx, node.ID) {
			continue
		}
		if m.snapshotProxyPoolFlag(node) {
			candidates = append(candidates, node)
			continue
		}
		if m.nodeSoftCooled(node.ID, now) {
			continue
		}
		if node.CooldownUntil == nil || !now.Before(*node.CooldownUntil) {
			candidates = append(candidates, node)
		}
	}
	return candidates
}

// cachedPoolMembers returns the cached pool row and its stable-ordered
// members, refreshing both together under one TTL. DB 读失败原样上抛:
// 绝不能与"池耗尽"混淆——耗尽可回退 direct,读失败回退 direct 等于
// 一次抖动就让流量绕过全部代理。
func (m *Manager) cachedPoolMembers(ctx context.Context, poolID uint64, now time.Time) (domain.Pool, []domain.Node, error) {
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
	pool, err := store.GetEgressPool(ctx, poolID)
	if err != nil {
		return domain.Pool{}, nil, err
	}
	nodes, err := store.ListEgressNodesByPool(ctx, poolID)
	if err != nil {
		return domain.Pool{}, nil, err
	}
	m.fallbackMu.Lock()
	m.poolFallbacks[poolID] = cachedPoolFallback{pool: pool, members: nodes, expiresAt: now.Add(poolCacheTTL)}
	m.fallbackMu.Unlock()
	return pool, nodes, nil
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
// direct results are fallbacks. PoolRouteNone means no lease was produced and
// the caller continues with the automatic schedule.
func (m *Manager) AcquirePoolRouted(ctx context.Context, scope domain.Scope, affinity string, poolID uint64, allowDirect bool, encryptedCredentialCookies string) (*Lease, PoolRouteOutcome, error) {
	now := time.Now().UTC()
	// 浏览器作用域走池时同样进入 Clearance 托管生命周期:池只是分组,
	// Web/Console 流量对 FlareSolverr 刷新的需求不因经过池而消失。
	managedClearance := usesBrowserClearance(scope) && m.managedClearanceMode()
	visited := map[uint64]struct{}{poolID: {}}
	current := poolID
	for {
		pool, members, err := m.cachedPoolMembers(ctx, current, now)
		if err != nil {
			if !errors.Is(err, repository.ErrNotFound) {
				// 读失败向调用方报错,acquire 会退回自动调度(绝不能回退 direct)。
				return nil, PoolRouteNone, err
			}
			return nil, PoolRouteNone, nil
		}
		if !pool.Enabled {
			return nil, PoolRouteNone, nil
		}
		candidates := m.poolCandidates(ctx, members, now)
		if len(candidates) == 0 {
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

// InvalidatePoolCache drops cached pool resolutions (config change hook) and
// the in-memory rotation cursors: a deleted pool must not keep steering the
// cursor map, and a membership rewrite should re-seed from the persisted row.
// rotationPersists 一并清空:写协程下一轮发现 state == nil 即退出,不会把按
// 旧成员序算出的游标写回新池;DB 行的 CAS 条件也兜住旧值覆盖。
func (m *Manager) InvalidatePoolCache() {
	m.fallbackMu.Lock()
	m.poolFallbacks = make(map[uint64]cachedPoolFallback)
	m.fallbackMu.Unlock()
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
//     traffic.
func (m *Manager) selectPoolNode(pool domain.Pool, nodes, allMembers []domain.Node, affinity string) domain.Node {
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

// selectRotationNode implements the rotation strategy: a persistent per-pool
// cursor (stored on the pool row, surviving restarts) pins traffic to one
// member until that member leaves the candidate set; the cursor then advances
// to the next available member in full-member order (priority, then id),
// wrapping around. Recovery of an earlier member never moves the cursor back.
func (m *Manager) selectRotationNode(pool domain.Pool, candidates, allMembers []domain.Node) domain.Node {
	if len(candidates) == 0 {
		return domain.Node{}
	}
	// 全成员序内聚在此处排序,规则必须与仓储 ORDER BY 完全一致:
	// (priority > 0) DESC, priority ASC, id ASC —— 已设 priority 的排前,
	// 未设(0)排后。否则同一池里 sticky(仓储序)与 rotation(内部序)
	// 的"首"会指向不同节点。
	ordered := append([]domain.Node(nil), allMembers...)
	sort.SliceStable(ordered, func(i, j int) bool {
		pi, pj := ordered[i].PoolPriority > 0, ordered[j].PoolPriority > 0
		if pi != pj {
			return pi
		}
		if ordered[i].PoolPriority != ordered[j].PoolPriority {
			return ordered[i].PoolPriority < ordered[j].PoolPriority
		}
		return ordered[i].ID < ordered[j].ID
	})
	allMembers = ordered
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
func (m *Manager) persistRotationCursor(poolID, fromNodeID, nodeID uint64) {
	m.rotationMu.Lock()
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
	go m.writeRotationCursor(poolID, fromNodeID, nodeID)
}

// writeRotationCursor 异步落盘一个游标值并在成功后处理登记的 pending
// 推进;写失败把 last 清零,让下一次推进重试。写脱离调用方生命周期
// (Background),选路绝不等待 DB。
func (m *Manager) writeRotationCursor(poolID, fromNodeID, nodeID uint64) {
	for {
		err := error(nil)
		if store, ok := m.repository.(interface {
			UpdateEgressPoolRotationCursor(context.Context, uint64, uint64, uint64) error
		}); ok {
			err = store.UpdateEgressPoolRotationCursor(context.Background(), poolID, fromNodeID, nodeID)
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
		if next == 0 || next == state.last {
			state.writing = false
			m.rotationMu.Unlock()
			return
		}
		fromNodeID, nodeID = state.last, next
		m.rotationMu.Unlock()
	}
}

// affinityNodeScore implements rendezvous (highest-random-weight) hashing:
// score = fnv1a64(affinity || nodeID). Stable per (identity, node) pair and
// independent of pool size, so membership changes cause minimal reshuffle.
func affinityNodeScore(affinity string, nodeID uint64) uint64 {
	hash := fnv.New64a()
	hash.Write([]byte(affinity))
	var idBytes [8]byte
	for i := 0; i < 8; i++ {
		idBytes[i] = byte(nodeID >> (56 - 8*i))
	}
	hash.Write(idBytes[:])
	return hash.Sum64()
}
