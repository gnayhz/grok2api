package egress

import (
	"testing"
	"time"
)

// 池统计:选中按池记录、失败按节点累计到含该节点的所有池、清零只清一个池。
func TestPoolStatsRecordAndReset(t *testing.T) {
	// 包级单例会被其他测试污染(Feedback 系也会记失败),先清零全局。
	poolNodeStats.mu.Lock()
	poolNodeStats.failures = make(map[uint64]poolNodeFailure)
	poolNodeStats.pools = make(map[uint64]map[uint64]*PoolNodeStat)
	poolNodeStats.mu.Unlock()
	ResetPoolStats(101)
	ResetPoolStats(102)
	RecordPoolSelection(101, 1)
	RecordPoolSelection(101, 1)
	RecordPoolSelection(101, 2)
	RecordPoolSelection(102, 1)
	items, _ := PoolStatsSnapshot(101)
	if len(items) != 2 {
		t.Fatalf("pool 101 items = %d, want 2", len(items))
	}
	counts := map[uint64]uint64{}
	for _, item := range items {
		counts[item.NodeID] = item.Selections
		if item.LastSelectedAt.IsZero() {
			t.Fatalf("node %d missing LastSelectedAt", item.NodeID)
		}
	}
	if counts[1] != 2 || counts[2] != 1 {
		t.Fatalf("selections = %v, want node1=2 node2=1", counts)
	}
	// 失败按节点计数,快照合并展示:同属两池的节点失败一次,两池快照都显示 1
	// (节点级计数,不伪装成池归因 — Feedback 路径无池上下文)。
	RecordPoolNodeFailure(1)
	items101, _ := PoolStatsSnapshot(101)
	items102, _ := PoolStatsSnapshot(102)
	var fail101, fail102 uint64
	for _, item := range items101 {
		if item.NodeID == 1 {
			fail101 = item.Failures
		}
	}
	for _, item := range items102 {
		if item.NodeID == 1 {
			fail102 = item.Failures
		}
	}
	if fail101 != 1 || fail102 != 1 {
		t.Fatalf("failures pool101=%d pool102=%d, want 1/1 (node-level merged into snapshots)", fail101, fail102)
	}
	// 清零只影响目标池
	ResetPoolStats(101)
	items101, _ = PoolStatsSnapshot(101)
	items102, _ = PoolStatsSnapshot(102)
	if len(items101) != 0 {
		t.Fatalf("pool 101 after reset = %d items, want 0", len(items101))
	}
	if len(items102) != 1 {
		t.Fatalf("pool 102 after pool-101 reset = %d items, want 1 (isolation)", len(items102))
	}
}

// 容量上限:订阅换血会持续产生新节点 ID, 删除池/换血池的旧统计没有任何
// 配置回调可清理; 超限后最旧条目必须被逐出, 内存有界, 新统计照常记账。
func TestPoolStatsCapacityEviction(t *testing.T) {
	poolNodeStats.mu.Lock()
	poolNodeStats.pools = make(map[uint64]map[uint64]*PoolNodeStat)
	poolNodeStats.poolSince = make(map[uint64]time.Time)
	poolNodeStats.failures = make(map[uint64]poolNodeFailure)
	poolNodeStats.since = time.Now().UTC().Add(-time.Hour)
	poolNodeStats.mu.Unlock()

	old := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < poolStatsMaxEntries+100; i++ {
		RecordPoolSelection(uint64(i%10), uint64(i))
	}
	RecordPoolNodeFailure(1)
	// 手工把一半条目回拨为"最旧", 模拟长时运行下的陈旧分布。
	poolNodeStats.mu.Lock()
	i := 0
	for _, nodes := range poolNodeStats.pools {
		for _, stat := range nodes {
			if i%2 == 0 {
				stat.LastSelectedAt = old
			}
			i++
		}
	}
	poolNodeStats.mu.Unlock()

	// 再写一条, 触发越限逐出。
	RecordPoolSelection(99, 999999)

	poolNodeStats.mu.RLock()
	defer poolNodeStats.mu.RUnlock()
	total := len(poolNodeStats.failures)
	for _, nodes := range poolNodeStats.pools {
		total += len(nodes)
	}
	if total > poolStatsMaxEntries {
		t.Fatalf("stats exceeded capacity: %d > %d", total, poolStatsMaxEntries)
	}
	if _, ok := poolNodeStats.pools[99][999999]; !ok {
		t.Fatal("newest selection must survive eviction")
	}
	if poolNodeStats.failures[1].count == 0 {
		t.Fatal("recent failure must survive eviction")
	}
}

// 重置隔离与快照合并(两池共享一节点):重置池 A 只清 A 的选择计数并刷新
// A 的起点, 不得影响池 B 的计数与起点; 共享节点的失败计数是全局节点级
// (反馈路径无池上下文), 重置 A 后仍要在 B 的快照里可见——两个池各自
// 展示同一节点的真实失败数, 不会被单池重置抹掉。
func TestPoolStatsResetIsolationAndGlobalFailureMerge(t *testing.T) {
	poolNodeStats.mu.Lock()
	poolNodeStats.pools = make(map[uint64]map[uint64]*PoolNodeStat)
	poolNodeStats.poolSince = make(map[uint64]time.Time)
	poolNodeStats.failures = make(map[uint64]poolNodeFailure)
	poolNodeStats.since = time.Now().UTC()
	poolNodeStats.mu.Unlock()

	const shared, onlyB = 10, 20
	RecordPoolSelection(1, shared) // 池 A: 共享节点
	RecordPoolSelection(2, shared) // 池 B: 同一共享节点
	RecordPoolSelection(2, onlyB)  // 池 B 独有节点
	RecordPoolNodeFailure(shared)
	RecordPoolNodeFailure(shared)
	RecordPoolNodeFailure(onlyB)

	beforeB, sinceB := PoolStatsSnapshot(2)
	if len(beforeB) != 2 {
		t.Fatalf("pool B snapshot before reset = %+v", beforeB)
	}
	for _, stat := range beforeB {
		want := uint64(2)
		if stat.NodeID == onlyB {
			want = 1
		}
		if stat.Failures != want {
			t.Fatalf("node %d failures = %d, want %d (global per-node merge)", stat.NodeID, stat.Failures, want)
		}
	}

	ResetPoolStats(1)

	afterA, sinceA := PoolStatsSnapshot(1)
	if len(afterA) != 0 {
		t.Fatalf("reset pool A still has entries: %+v", afterA)
	}
	if !sinceA.After(sinceB) && !sinceA.Equal(sinceB) {
		t.Fatalf("pool A since not refreshed: %v", sinceA)
	}
	afterB, sinceB2 := PoolStatsSnapshot(2)
	if len(afterB) != 2 {
		t.Fatalf("reset of pool A cleared pool B entries: %+v", afterB)
	}
	if !sinceB2.Equal(sinceB) {
		t.Fatalf("reset of pool A moved pool B's window: %v -> %v", sinceB, sinceB2)
	}
	for _, stat := range afterB {
		want := uint64(2)
		if stat.NodeID == onlyB {
			want = 1
		}
		if stat.Selections != 1 {
			t.Fatalf("pool B selection for node %d = %d, want 1 (untouched by A's reset)", stat.NodeID, stat.Selections)
		}
		if stat.Failures != want {
			t.Fatalf("global failure count for node %d lost after A's reset: %d, want %d", stat.NodeID, stat.Failures, want)
		}
	}
}
