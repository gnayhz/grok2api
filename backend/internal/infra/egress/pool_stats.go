package egress

import (
	"container/list"
	"sync"
	"time"
)

// poolNodeStatCounters 在进程内存中累计每个池内每个节点的调度结果：
// 选中次数证明策略分布，失败次数证明故障切换行为。这是验证调度策略
// 是否生效的最直接证据；重启或手动清零后归零，不落库。
type poolNodeStatCounters struct {
	mu      sync.RWMutex
	lru     list.List
	entries map[poolStatKey]*list.Element
	// since 全局起点;poolSince 按池记录清零时刻,避免重置一个池时
	// 悄悄改掉所有池的统计起点。
	since     time.Time
	poolSince map[uint64]time.Time
	pools     map[uint64]map[uint64]*PoolNodeStat
	failures  map[uint64]poolNodeFailure
}

// poolNodeFailure 带最近写入时间, 供容量驱逐挑选最旧条目。
type poolStatKey struct {
	poolID, nodeID uint64
	failure        bool
}

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
	poolNodeStats.touchLocked(poolStatKey{poolID: poolID, nodeID: nodeID})
}

// touchLocked maintains one bounded LRU for selection and failure records.
// Recording an existing member does constant work regardless of pool count;
// admitting a new member evicts at most one old record.
func (c *poolNodeStatCounters) touchLocked(key poolStatKey) {
	if c.entries == nil {
		c.entries = make(map[poolStatKey]*list.Element)
	}
	if item := c.entries[key]; item != nil {
		c.lru.MoveToBack(item)
		return
	}
	c.entries[key] = c.lru.PushBack(key)
	if c.lru.Len() <= poolStatsMaxEntries {
		return
	}
	oldest := c.lru.Front()
	removed := oldest.Value.(poolStatKey)
	c.lru.Remove(oldest)
	delete(c.entries, removed)
	if removed.failure {
		delete(c.failures, removed.nodeID)
		return
	}
	nodes := c.pools[removed.poolID]
	delete(nodes, removed.nodeID)
	if len(nodes) == 0 {
		delete(c.pools, removed.poolID)
		delete(c.poolSince, removed.poolID)
	}
}

// RecordPoolNodeFailure 在节点请求失败被记账时调用（Feedback 隔离/传输
// 失败/防爬拒绝）。失败按节点计数:租约上下文不透传到反馈路径,无法
// 归因到具体池——同一节点在 N 个池里各 +1 会伪装成池归因,这里只记
// 全局节点计数,快照读取时按节点合并展示。
func RecordPoolNodeFailure(nodeID uint64) { recordPoolNodeFailures(nodeID, 1) }

func recordPoolNodeFailures(nodeID uint64, count int) {
	poolNodeStats.mu.Lock()
	defer poolNodeStats.mu.Unlock()
	failure := poolNodeStats.failures[nodeID]
	failure.count += uint64(max(1, count))
	failure.at = time.Now().UTC()
	poolNodeStats.failures[nodeID] = failure
	poolNodeStats.touchLocked(poolStatKey{nodeID: nodeID, failure: true})
}

// poolSelectionCounts 返回一个池内各成员的累计选中次数(least-used
// 策略消费;无记录的成员缺席=0,新成员优先承接)。
func poolSelectionCounts(poolID uint64) map[uint64]uint64 {
	poolNodeStats.mu.RLock()
	defer poolNodeStats.mu.RUnlock()
	counts := make(map[uint64]uint64, len(poolNodeStats.pools[poolID]))
	for nodeID, stat := range poolNodeStats.pools[poolID] {
		if stat != nil {
			counts[nodeID] = stat.Selections
		}
	}
	return counts
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
	for nodeID := range poolNodeStats.pools[poolID] {
		key := poolStatKey{poolID: poolID, nodeID: nodeID}
		if element := poolNodeStats.entries[key]; element != nil {
			poolNodeStats.lru.Remove(element)
			delete(poolNodeStats.entries, key)
		}
	}
	delete(poolNodeStats.pools, poolID)
	// Unknown/deleted pools must not create an unbounded timestamp tombstone map.
	if len(poolNodeStats.poolSince) >= poolStatsMaxEntries {
		clear(poolNodeStats.poolSince)
	}
	poolNodeStats.poolSince[poolID] = time.Now().UTC()
}
