package egress

import (
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// TestPoolStrategyLeastUsedAnchorsG9 锚定 G9:最少使用策略把选择压向
// 统计窗口内选中次数最少的成员;无记录成员视为 0 优先;并列取稳定序。
func TestPoolStrategyLeastUsedAnchorsG9(t *testing.T) {
	manager, _ := newPoolTestManager(t)
	pool := domain.Pool{ID: 41, Enabled: true, Strategy: domain.PoolStrategyLeastUsed}
	nodes := []domain.Node{{ID: 1}, {ID: 2}, {ID: 3}}

	// 全零(新池):并列取稳定序首位。
	if got := manager.routing.selectPoolNode(pool, nodes, nodes, ""); got.ID != 1 {
		t.Fatalf("并列应取稳定序首位 = %d", got.ID)
	}
	// 节点 2 使用最少 → 持续选中直至追平。
	ResetPoolStats(41)
	for i := 0; i < 3; i++ {
		RecordPoolSelection(41, 1)
	}
	RecordPoolSelection(41, 3)
	if got := manager.routing.selectPoolNode(pool, nodes, nodes, ""); got.ID != 2 {
		t.Fatalf("最少使用成员应胜出 = %d", got.ID)
	}
	// 多轮后分布收敛到均匀(每成员差 ≤1)。
	ResetPoolStats(41)
	counts := map[uint64]int{}
	for i := 0; i < 90; i++ {
		chosen := manager.routing.selectPoolNode(pool, nodes, nodes, "")
		counts[chosen.ID]++
		RecordPoolSelection(41, chosen.ID)
	}
	for id, count := range counts {
		if count < 29 || count > 31 {
			t.Fatalf("成员 %d 分布 = %d,应收敛均匀", id, count)
		}
	}
	// 单成员早返回不受策略影响。
	if got := manager.routing.selectPoolNode(pool, nodes[:1], nodes[:1], ""); got.ID != 1 {
		t.Fatal("单成员应直选")
	}
	// 策略值经域校验合法(Normalized 直通)。
	if !domain.PoolStrategyLeastUsed.IsValid() || domain.PoolStrategyLeastUsed.Normalized() != domain.PoolStrategyLeastUsed {
		t.Fatal("least-used 必须是合法池策略")
	}
}
