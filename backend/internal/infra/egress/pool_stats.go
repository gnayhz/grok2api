package egress

import (
	"sort"
	"sync"
	"time"
)

// poolNodeStatCounters 在进程内存中累计每个池内每个节点的调度结果：
// 选中次数证明策略分布，失败次数证明故障切换行为。这是验证调度策略
// 是否生效的最直接证据；重启或手动清零后归零，不落库。
type poolNodeStatCounters struct {
	mu sync.RWMutex
	// since 全局起点;poolSince 按池记录清零时刻,避免重置一个池时
	// 悄悄改掉所有池的统计起点。
	since     time.Time
	poolSince map[uint64]time.Time
	pools     map[uint64]map[uint64]*PoolNodeStat
	failures  map[uint64]poolNodeFailure
}

// poolNodeFailure 带最近写入时间, 供容量驱逐挑选最旧条目。
type poolNodeFailure struct {
	count uint64
	at    time.Time
}

// poolStatsMaxEntries 是池统计的容量上限。统计是纯观测数据(重启即清零),
// 不感知池配置生命周期:订阅同步会持续换入新节点 ID, 删除池/换血池的旧
// 条目没有任何回调可清理。超限时按 LastSelectedAt/最近写入时间逐出最旧
// 条目——牺牲最陈旧的观测精度, 换取内存有界。
const poolStatsMaxEntries = 16384

// PoolNodeStat 是一个池内一个节点的调度统计快照。
type PoolNodeStat struct {
	PoolID         uint64    `json:"poolId,string"`
	NodeID         uint64    `json:"nodeId,string"`
	Selections     uint64    `json:"selections"`
	Failures       uint64    `json:"failures"`
	LastSelectedAt time.Time `json:"lastSelectedAt"`
}

var poolNodeStats = &poolNodeStatCounters{
	since:     time.Now().UTC(),
	poolSince: make(map[uint64]time.Time),
	pools:     make(map[uint64]map[uint64]*PoolNodeStat),
	failures:  make(map[uint64]poolNodeFailure),
}

// RecordPoolSelection 在池调度选中节点时调用（AcquirePoolRouted）。
func RecordPoolSelection(poolID, nodeID uint64) {
	poolNodeStats.mu.Lock()
	defer poolNodeStats.mu.Unlock()
	nodes := poolNodeStats.pools[poolID]
	if nodes == nil {
		nodes = make(map[uint64]*PoolNodeStat)
		poolNodeStats.pools[poolID] = nodes
	}
	stat := nodes[nodeID]
	if stat == nil {
		stat = &PoolNodeStat{PoolID: poolID, NodeID: nodeID}
		nodes[nodeID] = stat
	}
	now := time.Now().UTC()
	stat.Selections++
	stat.LastSelectedAt = now
	poolNodeStats.evictLocked(now)
}

// evictLocked 在条目总数超限时逐出最旧的统计。仅在越过上限时才付出
// O(n log n) 排序代价, 常态路径为 O(1)。
func (c *poolNodeStatCounters) evictLocked(now time.Time) {
	total := len(c.failures)
	for _, nodes := range c.pools {
		total += len(nodes)
	}
	if total <= poolStatsMaxEntries {
		return
	}
	type aged struct {
		poolID, nodeID uint64
		at             time.Time
	}
	entries := make([]aged, 0, total)
	for poolID, nodes := range c.pools {
		for nodeID, stat := range nodes {
			entries = append(entries, aged{poolID, nodeID, stat.LastSelectedAt})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	excess := total - poolStatsMaxEntries
	evicted := 0
	for i := 0; i < excess && i < len(entries); i++ {
		poolID, nodeID := entries[i].poolID, entries[i].nodeID
		if nodes := c.pools[poolID]; nodes != nil {
			delete(nodes, nodeID)
			if len(nodes) == 0 {
				delete(c.pools, poolID)
				delete(c.poolSince, poolID)
			}
		}
		evicted++
	}
	// 池条目不足抵扣超限时, 继续按最近写入时间逐出最旧的失败计数。
	if evicted < excess && len(c.failures) > 0 {
		failureAges := make([]aged, 0, len(c.failures))
		for nodeID, failure := range c.failures {
			failureAges = append(failureAges, aged{nodeID: nodeID, at: failure.at})
		}
		sort.Slice(failureAges, func(i, j int) bool { return failureAges[i].at.Before(failureAges[j].at) })
		for _, entry := range failureAges {
			if evicted >= excess {
				break
			}
			delete(c.failures, entry.nodeID)
			evicted++
		}
	}
}

// RecordPoolNodeFailure 在节点请求失败被记账时调用（Feedback 隔离/传输
// 失败/防爬拒绝）。失败按节点计数:租约上下文不透传到反馈路径,无法
// 归因到具体池——同一节点在 N 个池里各 +1 会伪装成池归因,这里只记
// 全局节点计数,快照读取时按节点合并展示。
func RecordPoolNodeFailure(nodeID uint64) {
	poolNodeStats.mu.Lock()
	defer poolNodeStats.mu.Unlock()
	failure := poolNodeStats.failures[nodeID]
	failure.count++
	failure.at = time.Now().UTC()
	poolNodeStats.failures[nodeID] = failure
	poolNodeStats.evictLocked(failure.at)
}

// PoolStatsSnapshot 返回一个池的统计快照（只含有记录的节点；前端与
// 成员列表合并展示零值行）。
func PoolStatsSnapshot(poolID uint64) ([]PoolNodeStat, time.Time) {
	poolNodeStats.mu.RLock()
	defer poolNodeStats.mu.RUnlock()
	nodes := poolNodeStats.pools[poolID]
	items := make([]PoolNodeStat, 0, len(nodes))
	for _, stat := range nodes {
		value := *stat
		value.Failures = poolNodeStats.failures[value.NodeID].count
		items = append(items, value)
	}
	if since, ok := poolNodeStats.poolSince[poolID]; ok {
		return items, since
	}
	return items, poolNodeStats.since
}

// ResetPoolStats 清零一个池的统计，便于做干净的策略验证实验。只影响该
// 池的起点;失败计数是全局节点级的(反馈路径没有池上下文),保留不动。
func ResetPoolStats(poolID uint64) {
	poolNodeStats.mu.Lock()
	defer poolNodeStats.mu.Unlock()
	delete(poolNodeStats.pools, poolID)
	poolNodeStats.poolSince[poolID] = time.Now().UTC()
}
