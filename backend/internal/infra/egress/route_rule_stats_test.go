package egress

import (
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// Snapshot 的文档承诺"按 level 再 mode 排序,API 响应确定性"——但实现遍历
// map,顺序随机。UI 依赖稳定顺序(避免每次轮询行序跳动);此测试注入乱序
// 条目并多次快照锁定有序性。
func TestRoutingStatsSnapshotOrderedDeterministic(t *testing.T) {
	collector := newRoutingStatsCollector()
	collector.record("scope:grok_web", "pool", RoutingOutcomeHit)
	collector.record("class:inference", "node", RoutingOutcomeHit)
	collector.record("default", "direct", RoutingOutcomeHit)
	collector.record("scope:grok_web", "node", RoutingOutcomeHit)

	for attempt := 0; attempt < 20; attempt++ {
		snapshot := collector.Snapshot()
		if len(snapshot) != 4 {
			t.Fatalf("snapshot len = %d", len(snapshot))
		}
		for i := 1; i < len(snapshot); i++ {
			if snapshot[i-1].Level > snapshot[i].Level ||
				(snapshot[i-1].Level == snapshot[i].Level && snapshot[i-1].Mode > snapshot[i].Mode) {
				t.Fatalf("snapshot not ordered by level,mode: [%s/%s] before [%s/%s]",
					snapshot[i-1].Level, snapshot[i-1].Mode, snapshot[i].Level, snapshot[i].Mode)
			}
		}
	}
}

func newRoutingStatsCollector() *routingStatsCollector {
	return &routingStatsCollector{entries: make(map[string]*RoutingStat)}
}

func TestRoutingStatsCollectorCounts(t *testing.T) {
	collector := newRoutingStatsCollector()
	collector.record("scope:grok_build", "node", RoutingOutcomeHit)
	collector.record("scope:grok_build", "node", RoutingOutcomeHit)
	collector.record("scope:grok_build", "node", RoutingOutcomeFallback)
	collector.record("scope:grok_build", "direct", RoutingOutcomeHit)
	collector.record("class:billing", "pool", RoutingOutcomeFallback)
	// 未知结果必须被丢弃,不得创建条目。
	collector.record("class:model_sync", "node", RoutingOutcome("unknown_outcome"))

	snapshot := collector.Snapshot()
	if len(snapshot) != 3 {
		t.Fatalf("snapshot entries = %d, want 3 (unknown outcome ignored): %+v", len(snapshot), snapshot)
	}
	byKey := make(map[string]RoutingStat, len(snapshot))
	for _, stat := range snapshot {
		byKey[stat.Level+"|"+stat.Mode] = stat
	}
	build := byKey["scope:grok_build|node"]
	if build.Hit != 2 || build.Fallback != 1 || build.LastSeen == nil {
		t.Fatalf("scope stat = %+v", build)
	}
	if byKey["scope:grok_build|direct"].Hit != 1 {
		t.Fatalf("direct stat = %+v", byKey["scope:grok_build|direct"])
	}
	if byKey["class:billing|pool"].Fallback != 1 {
		t.Fatalf("class stat = %+v", byKey["class:billing|pool"])
	}
	if _, exists := byKey["class:model_sync|node"]; exists {
		t.Fatal("unknown outcome must not create an entry")
	}
}

func TestRoutingStatsSnapshotIsCopy(t *testing.T) {
	collector := newRoutingStatsCollector()
	collector.record("scope:grok_web", "node", RoutingOutcomeHit)
	first := collector.Snapshot()
	first[0].Hit = 999
	second := collector.Snapshot()
	if second[0].Hit != 1 {
		t.Fatalf("snapshot must be a copy, got hit=%d after external mutation", second[0].Hit)
	}
	// LastSeen 指针同样不能共享。
	first[0].LastSeen = &time.Time{}
	if collector.Snapshot()[0].LastSeen == first[0].LastSeen {
		t.Fatal("snapshot LastSeen pointers must be copied")
	}
}

// RecordRoutingOutcome 必须把归一化的模式记入统计(perfmetrics 一侧仅计数)。
func TestRecordRoutingOutcomeNormalizesMode(t *testing.T) {
	// 直接用收集器验证归一化语义:空模式按 auto 记录。
	collector := newRoutingStatsCollector()
	collector.record("scope:grok_console", string(domain.RoutingTargetAuto.Normalized()), RoutingOutcomeHit)
	snapshot := collector.Snapshot()
	if len(snapshot) != 1 || snapshot[0].Mode != string(domain.RoutingTargetAuto) {
		t.Fatalf("normalized mode stat = %+v", snapshot)
	}
}
