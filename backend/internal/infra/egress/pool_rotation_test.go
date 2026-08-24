package egress

import (
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func rotationPick(t *testing.T, manager *Manager, pool domain.Pool, candidates, all []domain.Node) uint64 {
	t.Helper()
	return manager.selectPoolNode(pool, candidates, all, "acct").ID
}

// rotation: 游标只进不回。A 坏 → B,A 恢复仍在 B,B 坏 → C,C 坏绕回最先可用的。
func TestPoolStrategyRotationAdvancesWithoutRegressing(t *testing.T) {
	manager, repo := newPoolTestManager(t)
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyRotation, FallbackMode: domain.PoolFallbackNone}
	mk := func(ids ...uint64) []domain.Node {
		out := make([]domain.Node, 0, len(ids))
		for _, id := range ids {
			out = append(out, domain.Node{ID: id, Enabled: true, Health: 1})
		}
		return out
	}
	all := mk(1, 2, 3)
	pick := func(candidates ...uint64) uint64 {
		return rotationPick(t, manager, repo.pool[1], mk(candidates...), all)
	}

	// 1. 全健康:钉在 A(1)
	for i := 0; i < 3; i++ {
		if got := pick(1, 2, 3); got != 1 {
			t.Fatalf("healthy pool picked %d, want 1", got)
		}
	}
	// 2. A 坏:顺位 B(2)
	if got := pick(2, 3); got != 2 {
		t.Fatalf("after A failure picked %d, want 2", got)
	}
	// 3. A 恢复:仍在 B(2) —— 这是与 sticky 的本质区别
	if got := pick(1, 2, 3); got != 2 {
		t.Fatalf("after A recovery picked %d, want 2 (must not regress)", got)
	}
	// 4. B 坏:顺位 C(3),不回头到 A
	if got := pick(1, 3); got != 3 {
		t.Fatalf("after B failure picked %d, want 3 (not 1)", got)
	}
	// 5. C 坏(A 已恢复):绕回 A
	if got := pick(1); got != 1 {
		t.Fatalf("after C failure picked %d, want wrap to 1", got)
	}
}

// 单候选也推进游标;游标在 C 时剩 C 可用 → C;A/B 恢复后仍在 C。
func TestPoolStrategyRotationSingleCandidateAdvancesCursor(t *testing.T) {
	manager, repo := newPoolTestManager(t)
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyRotation, FallbackMode: domain.PoolFallbackNone}
	mk := func(ids ...uint64) []domain.Node {
		out := make([]domain.Node, 0, len(ids))
		for _, id := range ids {
			out = append(out, domain.Node{ID: id, Enabled: true, Health: 1})
		}
		return out
	}
	all := mk(1, 2, 3)
	pick := func(candidates ...uint64) uint64 {
		return rotationPick(t, manager, repo.pool[1], mk(candidates...), all)
	}
	if got := pick(1, 2, 3); got != 1 {
		t.Fatalf("initial pick %d, want 1", got)
	}
	if got := pick(2, 3); got != 2 {
		t.Fatalf("after A failure pick %d, want 2", got)
	}
	if got := pick(3); got != 3 {
		t.Fatalf("single-candidate pick %d, want 3", got)
	}
	if got := pick(1, 2, 3); got != 3 {
		t.Fatalf("after recovery pick %d, want stay on 3", got)
	}
}

// 首选顺序 priority 生效: priority 小者为首,轮换按它前进。
func TestPoolStrategyRotationRespectsPriority(t *testing.T) {
	manager, repo := newPoolTestManager(t)
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyRotation, FallbackMode: domain.PoolFallbackNone}
	// ID 大的节点 priority 小 → 应排第一
	all := []domain.Node{
		{ID: 1, Enabled: true, Health: 1, PoolPriority: 20},
		{ID: 2, Enabled: true, Health: 1, PoolPriority: 10},
		{ID: 3, Enabled: true, Health: 1, PoolPriority: 30},
	}
	pick := func(candidates []domain.Node) uint64 { return rotationPick(t, manager, repo.pool[1], candidates, all) }
	if got := pick(all); got != 2 {
		t.Fatalf("priority order pick %d, want 2 (priority 10 first)", got)
	}
	// 2 坏 → 下一个按 priority 是 1(20) 而不是 3(30)
	if got := pick([]domain.Node{all[0], all[2]}); got != 1 {
		t.Fatalf("after 2 failure pick %d, want 1 (priority 20 before 30)", got)
	}
}

// rotation 游标按池隔离。
func TestPoolStrategyRotationCursorPerPool(t *testing.T) {
	manager, repo := newPoolTestManager(t)
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyRotation}
	repo.pool[2] = domain.Pool{ID: 2, Enabled: true, Strategy: domain.PoolStrategyRotation}
	mk := func(ids ...uint64) []domain.Node {
		out := make([]domain.Node, 0, len(ids))
		for _, id := range ids {
			out = append(out, domain.Node{ID: id, Enabled: true, Health: 1})
		}
		return out
	}
	all := mk(1, 2)
	if got := rotationPick(t, manager, repo.pool[1], all, all); got != 1 {
		t.Fatalf("pool1 first pick %d, want 1", got)
	}
	all2 := mk(2)
	if got := rotationPick(t, manager, repo.pool[1], all2, all2); got != 2 {
		t.Fatalf("pool1 advanced pick %d, want 2", got)
	}
	if got := rotationPick(t, manager, repo.pool[2], all, all); got != 1 {
		t.Fatalf("pool2 pick %d, want 1 (cursor per pool)", got)
	}
}
