package egress

import (
	"testing"
	"time"
)

// nodeSoftCooled 的过期条目淘汰分支:冷却已过且不在阶梯窗口(count<=1)→
// 条目删除;冷却已过但在阶梯窗口(count>1 且距 until 不超过 softMax)→
// 条目保留(供下一次证据递增)。双检锁的第二检在测试中以直接状态构造驱动。
func TestNodeSoftCooledExpiredEntryEviction(t *testing.T) {
	manager, _ := newPoolTestManager(t)
	now := time.Now().UTC()

	// (1) count<=1 且已过期且超出阶梯窗口:条目被删除。
	manager.softMu.Lock()
	manager.softCooldowns[7] = softCooldown{count: 1, until: now.Add(-time.Hour)}
	manager.softMu.Unlock()
	if manager.nodeSoftCooled(7, now) {
		t.Fatal("expired entry reported cooled")
	}
	manager.softMu.RLock()
	_, exists := manager.softCooldowns[7]
	manager.softMu.RUnlock()
	if exists {
		t.Fatal("expired single-count entry not evicted — map grows unboundedly for one-shot evidence")
	}

	// (2) count>1 且距 until 在 softMax 内:条目保留(阶梯语义,供递增)。
	manager.softMu.Lock()
	manager.softCooldowns[8] = softCooldown{count: 3, until: now.Add(-time.Minute)}
	manager.softMu.Unlock()
	if manager.nodeSoftCooled(8, now) {
		t.Fatal("past-window multi-count entry reported cooled")
	}
	manager.softMu.RLock()
	_, kept := manager.softCooldowns[8]
	manager.softMu.RUnlock()
	if !kept {
		t.Fatal("ladder-window entry evicted — escalate-on-repeat semantics broken")
	}

	// (3) 冷却中:如实返回 true,条目不动。
	manager.softMu.Lock()
	manager.softCooldowns[9] = softCooldown{count: 2, until: now.Add(time.Hour)}
	manager.softMu.Unlock()
	if !manager.nodeSoftCooled(9, now) {
		t.Fatal("actively cooled entry reported schedulable")
	}
}
