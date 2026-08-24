package egress

import (
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// sticky 走仓储序(candidates[0]),rotation 内部重排 — 两者"首"必须一致。
// 池成员的唯一排序权威在仓储层(priority > 0 DESC, priority ASC, id ASC);
// sticky 直接取仓储序首位,rotation 在无游标可钉住时推进到"下一可用成员"。
// 若两策略对同一成员列表选出不同的首,说明调度层私自重排——违反
// "池只负责选择策略、排序由仓储单一实现"的边界。
func TestStickyAndRotationOrderRuleConsistency(t *testing.T) {
	manager, repo := newPoolTestManager(t)
	// 乱序输入模拟"仓储已按规则排序"的输出
	repoOrder := []domain.Node{
		{ID: 7, Enabled: true, Health: 1, PoolPriority: 1},
		{ID: 2, Enabled: true, Health: 1, PoolPriority: 0},
		{ID: 5, Enabled: true, Health: 1, PoolPriority: 0},
		{ID: 9, Enabled: true, Health: 1, PoolPriority: 3},
	}
	stickyPick := manager.selectPoolNode(domain.Pool{ID: 1, Strategy: domain.PoolStrategySticky}, repoOrder, repoOrder, "")
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyRotation}
	rotationPick := manager.selectPoolNode(domain.Pool{ID: 2, Enabled: true, Strategy: domain.PoolStrategyRotation}, repoOrder, repoOrder, "")
	if stickyPick.ID != rotationPick.ID {
		t.Fatalf("sticky=%d rotation=%d: 两策略的首必须一致(同一排序规则)", stickyPick.ID, rotationPick.ID)
	}
}
