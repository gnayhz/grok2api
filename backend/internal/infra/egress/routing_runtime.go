package egress

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"

	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// routingRuntime owns immutable routing snapshots, selection state and session
// pins. Its lease callback is the sole boundary to transport construction and
// authoritative quality admission; it cannot mutate solver or client caches.
type routingRuntime struct {
	bindingGeneration    atomic.Uint64
	repository           repository.EgressRepository
	cipher               security.Cryptor
	health               *healthRuntime
	tasks                *taskRuntime
	log                  func() *slog.Logger
	leaseForNode         func(context.Context, domain.Scope, string, string, bool, domain.Node) (*Lease, bool, error)
	managedClearanceMode func() bool
	qualitySchedulable   func(context.Context, uint64) bool
	waitForFailureProbe  func(context.Context, uint64) (bool, error)
	sharedLoad           func(context.Context, *sharedLoadGroup, string, func() (any, error)) (any, error)
	nodeMu               sync.RWMutex
	operationsMu         sync.RWMutex
	inflight             sync.Map
	inflightMu           sync.RWMutex
	inflightEntries      int
	nodes                map[string]cachedNodeSnapshot
	healthyNodes         map[uint64]time.Time
	proxyFlagMemo        map[uint64]proxyFlagMemoEntry
	nodeVersions         map[string]uint64
	nodeLoads            sharedLoadGroup
	poolLoads            sharedLoadGroup
	operationsConfig     cachedOperationsConfig
	operationsConfigLoad sharedLoadGroup
	operationsConfigVer  uint64
	routeRuleNodeVersion uint64
	routeRuleNodeMu      sync.RWMutex
	routeRuleNodeCache   map[uint64]cachedRoutingTargetNode
	routingTargetLoads   sharedLoadGroup
	fallbackMu           sync.Mutex
	poolCacheVersion     uint64
	poolFallbacks        map[uint64]cachedPoolFallback
	rotationMu           sync.Mutex
	rotationCursors      map[uint64]uint64
	sessionPinMu         sync.Mutex
	sessionPins          map[string]sessionNodePin
	sessionPinSweep      time.Time
	rotationPersists     map[uint64]*rotationPersistState
}

func newRoutingRuntime(m *Manager) *routingRuntime {
	return &routingRuntime{repository: m.repository, cipher: m.cipher, health: m.health, tasks: m.tasks, log: m.log, leaseForNode: m.leaseForNode, managedClearanceMode: m.managedClearanceMode, qualitySchedulable: m.qualitySchedulable, waitForFailureProbe: m.waitForFailureProbe, sharedLoad: m.sharedLoad,
		nodes: make(map[string]cachedNodeSnapshot), healthyNodes: make(map[uint64]time.Time), proxyFlagMemo: make(map[uint64]proxyFlagMemoEntry), nodeVersions: make(map[string]uint64), routeRuleNodeCache: make(map[uint64]cachedRoutingTargetNode), poolFallbacks: make(map[uint64]cachedPoolFallback), rotationCursors: make(map[uint64]uint64), rotationPersists: make(map[uint64]*rotationPersistState), sessionPins: make(map[string]sessionNodePin),
	}
}
func (m *routingRuntime) acquire(ctx context.Context, scope domain.Scope, affinity string, allowDirect bool, encryptedCredentialCookies string) (*Lease, bool, error) {
	ctx = m.selectionContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	managedClearance := isGrokWebScope(scope) && m.managedClearanceMode()
	// 质量验证(canary)钉住受检节点:绕过路由层, 且由 acquireFixedTarget 的
	// verification 分支绕过冷却/排除守卫(见其注释)。验证属质量取证通道,
	// 出口资格缝隙同样放行(B2 生产/探针双通道)。
	if verificationNode := qualityVerificationNodeFromContext(ctx); verificationNode != 0 {
		ctx = WithExitEligibilityBypass(ctx)
		lease, err := m.acquireFixedTarget(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, verificationNode, true)
		if err != nil {
			return nil, true, err
		}
		return lease, true, nil
	}
	if pinned := pinnedNodeFromContext(ctx); pinned != 0 {
		lease, err := m.acquireFixedTarget(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, pinned, false)
		if err != nil {
			return nil, true, err
		}
		return lease, true, nil
	}
	config, supported, configErr := m.loadOperationsConfig(ctx, now)
	if configErr != nil {
		return nil, false, fmt.Errorf("读取出口路由配置: %w", configErr)
	}
	target := domain.RoutingTarget{Mode: domain.RoutingTargetAuto}
	if supported {
		target = config.TargetFor(scope, TrafficClassFromContext(ctx))
	}
	// 统计按"实际作出决策的层级"归因:类别规则命中记 class:*,否则作用域
	// 规则命中记 scope:*,再否则记 default。未配置任何规则时走自动调度,
	// 不产生统计(徽标本身已说明)。
	level := "default"
	ruleConfigured := false
	if supported {
		level, ruleConfigured = decidingRoutingLevel(config, scope, TrafficClassFromContext(ctx))
	}
	// 路由层级解析：语义(流量类别) → 作用域 → 总出口 → 自动调度。「回退」
	// 只发生在配置阶梯的降级(更具体层级未配置时落到下一层级);一旦某层级
	// 配置了明确目标,该目标就是强绑定:固定节点不可用或代理池整体失效时
	// 快速失败,绝不静默改道到边界外的节点——账号出口 IP 的无声突变本身就是
	// 风险。需要容错应配置代理池:池是 any-of 契约,成员轮换/链式池/池内
	// 直连回退都在配置边界之内。
	switch target.Mode.Normalized() {
	case domain.RoutingTargetDirect:
		// 直连是显式路由决策，无需 allowDirect —— 与旧版直连路由规则一致。
		lease, _, err := m.leaseForNode(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, domain.Node{ID: 0, Name: "route-direct", Enabled: true, Health: 1})
		if err != nil {
			return nil, true, fmt.Errorf("获取直连出口: %w", err)
		}
		if ruleConfigured {
			RecordRoutingOutcome(level, target, RoutingOutcomeHit)
		}
		return lease, true, nil
	case domain.RoutingTargetNode:
		lease, err := m.acquireFixedTarget(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, target.NodeID, false)
		if err == nil {
			if ruleConfigured {
				RecordRoutingOutcome(level, target, RoutingOutcomeHit)
			}
			return lease, true, nil
		}
		if !errors.Is(err, ErrRoutingTargetUnavailable) {
			return nil, true, err
		}
		// 固定节点目标=强绑定:节点被质量守卫隔离/冷却/停用而不可用时快速
		// 失败,让操作者立即看到配置失效,而不是流量悄悄改道其它出口。回退
		// 计数如实记录「配置的出口没有接住流量」。需要容错请配置代理池。
		if ruleConfigured {
			RecordRoutingOutcome(level, target, RoutingOutcomeFallback)
		}
		m.log().Warn("egress_strict_target_unavailable", "level", level, "node_id", target.NodeID, "error", err.Error())
		return nil, true, fmt.Errorf("路由固定出口不可用(严格绑定,不自动改道): %w", err)
	case domain.RoutingTargetPool:
		// 池的 direct 回退是降级而非主路由决策(与显式 direct 路由不同),
		// 必须遵守调用方的 allowDirect 契约:AcquireIfConfigured 不接受
		// manager 直连租约,否则会绕过调用方 fallback transport 的
		// HTTP_PROXY 语义。
		lease, outcome, err := m.AcquirePoolRouted(ctx, scope, affinity, target.PoolID, allowDirect, encryptedCredentialCookies)
		if err != nil && lease != nil {
			// 防御:AcquirePoolRouted 的 direct 回退分支可能同时透传租约与错误,
			// 先释放租约再失败,避免 inflight 计数泄漏。
			lease.Release()
		}
		if err != nil {
			if ctx.Err() != nil {
				// 请求已取消:不得为死请求租约节点、抬高 inflight 计数
				// 并触发无意义的健康反馈。
				return nil, true, ctx.Err()
			}
			// 池路由读失败(DB 抖动)同样严格失败:配置了池目标就是圈定了
			// 出口边界,静默退回自动调度等于流量无声逃出边界(且 DB 故障时
			// 自动调度的节点列表读取也会失败,回退并不能换来可用性)。
			if !errors.Is(err, context.Canceled) {
				m.log().Warn("egress_pool_route_failed", "pool_id", target.PoolID, "error", err.Error())
			}
			if ruleConfigured {
				RecordRoutingOutcome(level, target, RoutingOutcomeFallback)
			}
			return nil, true, fmt.Errorf("%w 路由代理池不可用(严格绑定,不自动改道): %w", ErrRoutingTargetUnavailable, err)
		}
		if lease != nil && outcome != PoolRouteNone {
			// 只有目标池自身选出成员才算命中;链式回退池/回退直连是配置边界
			// 之内的降级,记 Fallback 让行内统计如实反映"目标池没有亲自接住
			// 流量"。
			if ruleConfigured {
				outcomeKind := RoutingOutcomeFallback
				if outcome == PoolRouteMember {
					outcomeKind = RoutingOutcomeHit
				}
				RecordRoutingOutcome(level, target, outcomeKind)
			}
			return lease, true, nil
		}
		// 池整体未产出租约(池被删除/停用、成员全部冷却且无可用的链式/直连
		// 回退):严格失败。自动调度里的节点不在管理员圈定的边界内,静默改道
		// 与固定节点不可用改道是同一种意外。
		if ruleConfigured {
			RecordRoutingOutcome(level, target, RoutingOutcomeFallback)
		}
		return nil, true, fmt.Errorf("%w 路由代理池 %d 未产出出口(严格绑定,不自动改道): 池不存在/停用或全部成员不可用", ErrRoutingTargetUnavailable, target.PoolID)
	}
	// 自动调度：所有启用节点按健康度与调用方亲和选择,包含已入池节点。
	nodes, err := m.listNodes(ctx, now)
	if err != nil {
		return nil, false, err
	}
	buffer := borrowNodeCandidates(len(nodes))
	defer releaseNodeCandidates(buffer)
	available := (*buffer)[:0]
	hasNodes := false
	for _, node := range nodes {
		node = m.health.overlay(node)
		if !node.Enabled {
			continue
		}
		if nodeExcluded(ctx, node.ID) {
			continue
		}
		proxyPool := m.snapshotProxyPoolFlag(node)
		// 自动调度是全量兜底池:节点是纯资源,入池只是分组,
		// 不把节点从自动调度里"消费"掉——否则建池会让兜底容量缩水。
		hasNodes = true
		// 质量轴不合格(羁押/ban)的节点在选择前剔除(与池内过滤同款,
		// B2 查询点 3):自动调度不因部分节点被押而随机命中失败;全部
		// 被押时仍如实报"没有可用出口",不静默改道直连。
		if !m.qualitySchedulable(ctx, node.ID) {
			continue
		}
		// 冷却口径统一由 domain.CooldownBlocksScheduling 给出:池模式节点
		// 豁免普通冷却,出口 IP 质量隔离对它们同样生效(不再在此另拼条件)。
		if domain.CooldownBlocksScheduling(node.CooldownUntil, node.LastError, proxyPool, now) {
			continue
		}
		if proxyPool {
			node = domain.RotatingEndpointHealth(node.HealthState()).ApplyTo(node)
		}
		available = append(available, node)
	}
	if len(available) == 0 {
		if hasNodes {
			return nil, false, fmt.Errorf("当前没有可用的出口节点")
		}
		if !allowDirect {
			recordSelection(ctx, Selection{NodeName: "direct", Scope: scope})
			return nil, false, nil
		}
		available = []domain.Node{{ID: 0, Name: "direct", Enabled: true, Health: 1}}
	}
	selected := m.selectNode(available, affinity)
	// 会话钉扎优先于账号哈希:可用集的任何增减都会改变账号哈希落点,
	// 对进行中的会话等于无声换出口。钉住后可用集波动只影响新会话。
	if session := buildSessionForScope(ctx, scope); session != "" {
		selected = m.pinSessionNode(ctx, session, available, selected)
	}
	return m.leaseForNode(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, selected)
}

func (m *routingRuntime) acquireFixedTarget(ctx context.Context, scope domain.Scope, affinity, encryptedCredentialCookies string, managedClearance bool, nodeID uint64, verification bool) (*Lease, error) {
	waitedForProbe := false
	for {
		selected, ok, err := m.cachedRoutingTargetNode(ctx, nodeID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: node %d not found", ErrRoutingTargetUnavailable, nodeID)
		}
		selected = m.health.overlay(selected)
		if !domain.CanNodeServeFixedTarget(selected) {
			return nil, fmt.Errorf("%w: node %d not schedulable", ErrRoutingTargetUnavailable, selected.ID)
		}
		// 质量验证模式(canary 钉住):被验证节点必然处于质量隔离冷却中,
		// L2 软冷却也可能仍在生效(它们在验证通过/暂定放行时才被清除);
		// 跳过排除与冷却检查直接取租约, 否则"验证通过→解除隔离"的回池
		// 链路整体失效。非验证路径(路由固定目标/降智同号重试)不受影响。
		if verification {
			lease, _, err := m.leaseForNode(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, selected)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrRoutingTargetUnavailable, err)
			}
			return lease, nil
		}
		// 请求内排除(降智守卫)对固定目标同样生效:重试必须离开坏出口,
		// 否则守卫对固定路由配置完全失效。以不可用告终,调用方快速失败。
		if nodeExcluded(ctx, selected.ID) {
			return nil, fmt.Errorf("%w: node %d excluded by degrade guard", ErrRoutingTargetUnavailable, nodeID)
		}
		// 冷却口径与自动调度/池成员过滤共用 domain 唯一判定:池模式节点豁免
		// 普通冷却,但出口 IP 质量隔离同样阻断固定目标——固定的是隧道,不是
		// 被隔离的降智出口。
		if domain.CooldownBlocksScheduling(selected.CooldownUntil, selected.LastError, m.isProxyPoolNode(selected), time.Now().UTC()) {
			if !waitedForProbe && selected.LastError == domain.LastErrorTransport {
				completed, waitErr := m.waitForFailureProbe(ctx, nodeID)
				if waitErr != nil {
					return nil, waitErr
				}
				if completed {
					waitedForProbe = true
					continue
				}
			}
			return nil, fmt.Errorf("%w: node %d cooling down", ErrRoutingTargetUnavailable, nodeID)
		}
		lease, _, err := m.leaseForNode(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, selected)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrRoutingTargetUnavailable, err)
		}
		return lease, nil
	}
}

func (m *routingRuntime) loadOperationsConfig(ctx context.Context, now time.Time) (domain.OperationsConfig, bool, error) {
	configRepository, ok := m.repository.(operationsConfigRepository)
	if !ok {
		return domain.OperationsConfig{}, false, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return domain.OperationsConfig{}, true, err
		}
		m.operationsMu.RLock()
		cached := m.operationsConfig
		m.operationsMu.RUnlock()
		if now.Before(cached.expiresAt) {
			return cached.value, true, nil
		}
		loaded, err := m.sharedLoad(ctx, &m.operationsConfigLoad, "operations", func() (any, error) {
			m.operationsMu.RLock()
			cached, version := m.operationsConfig, m.operationsConfigVer
			m.operationsMu.RUnlock()
			if time.Now().Before(cached.expiresAt) {
				return cached.value, nil
			}
			loadCtx, cancel := m.tasks.context(ctx, 5*time.Second)
			defer cancel()
			value, err := configRepository.GetEgressOperationsConfig(loadCtx)
			if err != nil {
				return nil, err
			}
			m.operationsMu.Lock()
			defer m.operationsMu.Unlock()
			if version != m.operationsConfigVer {
				return nil, errNodeSnapshotInvalidated
			}
			m.operationsConfig = cachedOperationsConfig{value: value, expiresAt: time.Now().Add(operationsConfigSnapshotTTL)}
			return value, nil
		})
		if errors.Is(err, errNodeSnapshotInvalidated) {
			now = time.Now()
			continue
		}
		if err != nil {
			return domain.OperationsConfig{}, true, err
		}
		return loaded.(domain.OperationsConfig), true, nil
	}
}

func (m *routingRuntime) selectNode(nodes []domain.Node, affinity string) domain.Node {
	if affinity != "" {
		digest := sha256.Sum256([]byte(affinity))
		selected := nodes[int(binary.BigEndian.Uint64(digest[:8])%uint64(len(nodes)))]
		if selected.Health >= 0.8 || len(nodes) == 1 {
			return selected
		}
		for _, node := range nodes {
			if node.Health > selected.Health {
				selected = node
			}
		}
		return selected
	}
	best := nodes[0]
	bestCurrent := m.inflightCount(best.ID)
	for _, node := range nodes[1:] {
		current := m.inflightCount(node.ID)
		if current < bestCurrent || (current == bestCurrent && node.Health > best.Health) {
			best = node
			bestCurrent = current
		}
	}
	return best
}

func (m *routingRuntime) pinSessionNode(ctx context.Context, session string, available []domain.Node, fallback domain.Node) domain.Node {
	now := time.Now().UTC()
	m.sessionPinMu.Lock()
	defer m.sessionPinMu.Unlock()
	if m.sessionPins == nil {
		m.sessionPins = make(map[string]sessionNodePin)
	}
	pinned, had := m.sessionPins[session]
	if had {
		for _, node := range available {
			if node.ID == pinned.nodeID {
				m.sessionPins[session] = sessionNodePin{nodeID: pinned.nodeID, lastUsed: now}
				m.sweepSessionPinsLocked(now)
				return node
			}
		}
	}
	m.sessionPins[session] = sessionNodePin{nodeID: fallback.ID, lastUsed: now}
	m.sweepSessionPinsLocked(now)
	if had && pinned.nodeID != fallback.ID {
		m.log().Info("egress_session_node_repinned", "session", session, "from_node", pinned.nodeID, "to_node", fallback.ID)
	} else if !had {
		m.log().Info("egress_session_node_pinned", "session", session, "node", fallback.ID)
	}
	return fallback
}

func (m *routingRuntime) sweepSessionPinsLocked(now time.Time) {
	if len(m.sessionPins) <= maxSessionPinnedNodes && !m.sessionPinSweep.IsZero() && now.Sub(m.sessionPinSweep) < sessionPinSweepInterval {
		return
	}
	m.sessionPinSweep = now
	for key, pin := range m.sessionPins {
		if now.Sub(pin.lastUsed) >= sessionPinIdleTTL {
			delete(m.sessionPins, key)
		}
	}
	if len(m.sessionPins) <= maxSessionPinnedNodes {
		return
	}
	type pinEntry struct {
		key      string
		lastUsed time.Time
	}
	entries := make([]pinEntry, 0, len(m.sessionPins))
	for key, pin := range m.sessionPins {
		entries = append(entries, pinEntry{key: key, lastUsed: pin.lastUsed})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].lastUsed.Before(entries[j].lastUsed) })
	// Reclaim a batch at capacity so a burst of new sessions does not sort the
	// whole table under its mutex on every subsequent request.
	remove := max(len(entries)-maxSessionPinnedNodes, maxSessionPinnedNodes/16)
	for _, entry := range entries[:remove] {
		delete(m.sessionPins, entry.key)
	}
}

const maxRetainedInflightCounters = 4096

func (m *routingRuntime) incrementInflight(nodeID uint64) {
	m.inflightMu.RLock()
	if value, ok := m.inflight.Load(nodeID); ok {
		value.(*atomic.Int64).Add(1)
		m.inflightMu.RUnlock()
		return
	}
	m.inflightMu.RUnlock()
	m.inflightMu.Lock()
	defer m.inflightMu.Unlock()
	if value, ok := m.inflight.Load(nodeID); ok {
		value.(*atomic.Int64).Add(1)
		return
	}
	// Fixed/pool-only traffic may never refresh the full node snapshot. Reclaim
	// inactive counters on capacity as well, keeping active counters stable.
	if m.inflightEntries >= maxRetainedInflightCounters {
		m.inflight.Range(func(key, value any) bool {
			if value.(*atomic.Int64).Load() == 0 {
				m.inflight.Delete(key)
				m.inflightEntries--
			}
			return true
		})
	}
	value := &atomic.Int64{}
	value.Store(1)
	m.inflight.Store(nodeID, value)
	m.inflightEntries++
}

func (m *routingRuntime) decrementInflight(nodeID uint64) {
	m.inflightMu.RLock()
	reclaim := false
	if value, ok := m.inflight.Load(nodeID); ok {
		reclaim = value.(*atomic.Int64).Add(-1) == 0 && m.inflightEntries > maxRetainedInflightCounters
	}
	m.inflightMu.RUnlock()
	if reclaim {
		m.inflightMu.Lock()
		if value, ok := m.inflight.Load(nodeID); ok && value.(*atomic.Int64).Load() == 0 {
			m.inflight.Delete(nodeID)
			m.inflightEntries--
		}
		m.inflightMu.Unlock()
	}
}

func (m *routingRuntime) inflightCount(nodeID uint64) int64 {
	if value, ok := m.inflight.Load(nodeID); ok {
		return value.(*atomic.Int64).Load()
	}
	return 0
}

func (m *routingRuntime) isStickyProxyNode(value domain.Node) bool {
	if m == nil || m.cipher == nil || strings.TrimSpace(value.EncryptedProxyURL) == "" {
		return false
	}
	return m.stickyFlagMemoized(value.ID, value.EncryptedProxyURL)
}

func (m *routingRuntime) stickyFlagDirect(value domain.Node) bool {
	if m == nil || m.cipher == nil || strings.TrimSpace(value.EncryptedProxyURL) == "" {
		return false
	}
	proxyURL, err := m.cipher.Decrypt(value.EncryptedProxyURL)
	return err == nil && domain.IsAccountTemplateProxy(proxyURL)
}

func (m *routingRuntime) stickyFlagMemoized(nodeID uint64, ciphertext string) bool {
	m.nodeMu.RLock()
	entry, ok := m.proxyFlagMemo[nodeID]
	m.nodeMu.RUnlock()
	if ok && entry.ciphertext == ciphertext {
		return entry.sticky
	}
	proxyURL, err := m.cipher.Decrypt(ciphertext)
	sticky := err == nil && domain.IsAccountTemplateProxy(proxyURL)
	m.nodeMu.Lock()
	if len(m.proxyFlagMemo) >= proxyFlagMemoMax {
		for id := range m.proxyFlagMemo {
			delete(m.proxyFlagMemo, id)
			break
		}
	}
	m.proxyFlagMemo[nodeID] = proxyFlagMemoEntry{ciphertext: ciphertext, sticky: sticky}
	m.nodeMu.Unlock()
	return sticky
}

// isProxyPoolNode 判定节点的池模式状态:策略由 domain.IsPoolMode 唯一给出,
// 这里只提供记忆化解密得到的"账号模板"输入(不重复解密)。
func (m *routingRuntime) isProxyPoolNode(value domain.Node) bool {
	return domain.IsPoolMode(value.ProxyPool, m.isStickyProxyNode(value))
}

func (m *routingRuntime) isProxyPoolNodeDirect(value domain.Node) bool {
	return domain.IsPoolMode(value.ProxyPool, m.stickyFlagDirect(value))
}

func (m *routingRuntime) snapshotProxyPoolFlag(value domain.Node) bool {
	if value.ProxyPool {
		return true
	}
	// 单次加锁依次查快照判定表与记忆表:池路由路径每个成员都会走到这里,
	// 双重加锁在百成员池上是可测的热点。
	m.nodeMu.RLock()
	snapshot, ok := m.nodes[nodeSnapshotKey]
	if ok {
		if flag, found := snapshot.poolFlags[value.ID]; found {
			m.nodeMu.RUnlock()
			return flag
		}
	}
	entry, memoized := m.proxyFlagMemo[value.ID]
	m.nodeMu.RUnlock()
	if memoized && entry.ciphertext == value.EncryptedProxyURL {
		return domain.IsPoolMode(value.ProxyPool, entry.sticky)
	}
	return domain.IsPoolMode(value.ProxyPool, m.stickyFlagMemoized(value.ID, value.EncryptedProxyURL))
}

func (m *routingRuntime) cachedNodeIsHealthy(nodeID uint64) bool {
	// 读路径走 RLock:成功反馈在每次响应头后都会打这里,1s 窗口内的重复
	// 命中是常态。过期条目不必在读路径删除——TTL 判定(now.Before)已兜底,
	// replaceNodeSnapshotLocked 每次快照重建时全量重写 healthyNodes,不会泄漏。
	m.nodeMu.RLock()
	validUntil, ok := m.healthyNodes[nodeID]
	m.nodeMu.RUnlock()
	return ok && time.Now().UTC().Before(validUntil)
}
