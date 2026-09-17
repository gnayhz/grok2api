package egress

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// TestPoolCandidatesFilterQualityIneligible 锚定 B2 查询点 3(池内过滤,
// 批8 追加整改):质量轴不合格(羁押/ban)的池成员在选择前剔除——
// 策略(random/affinity/least-used)只从可用成员中挑,被押成员不再让
// 整个池路由失败;全部被押时候选为空(真·池耗尽,按 fallback 契约走)。
func TestPoolCandidatesFilterQualityIneligible(t *testing.T) {
	manager := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	manager.SetExitEligibility(stubExitEligibility{ineligible: map[uint64]bool{116: true, 117: true}})
	nodes := []domain.Node{
		{ID: 52, Name: "tunnel-a", Enabled: true, Health: 1},
		{ID: 107, Name: "tunnel-b", Enabled: true, Health: 1},
		{ID: 116, Name: "remanded", Enabled: true, Health: 1},
		{ID: 117, Name: "banned", Enabled: true, Health: 1},
	}
	candidates := manager.routing.appendPoolCandidates(nil, context.Background(), nodes, time.Now().UTC())
	if len(candidates) != 2 {
		t.Fatalf("被押成员必须在候选阶段剔除, got %d: %+v", len(candidates), candidates)
	}
	for _, node := range candidates {
		if node.ID == 116 || node.ID == 117 {
			t.Fatalf("不合格成员泄漏进候选: %d", node.ID)
		}
	}
	// 取证通道豁免:探针钉扎路径不过滤(B2 双通道)。
	probeCandidates := manager.routing.appendPoolCandidates(nil, WithExitEligibilityBypass(context.Background()), nodes, time.Now().UTC())
	if len(probeCandidates) != 4 {
		t.Fatalf("取证通道不得过滤候选, got %d", len(probeCandidates))
	}
	// 全部被押 → 候选为空(池耗尽,交给 fallback 契约)。
	allHeld := manager.routing.appendPoolCandidates(nil, context.Background(), nodes[:1], time.Now().UTC())
	manager.SetExitEligibility(stubExitEligibility{ineligible: map[uint64]bool{52: true}})
	allHeld = manager.routing.appendPoolCandidates(nil, context.Background(), nodes[:1], time.Now().UTC())
	if len(allHeld) != 0 {
		t.Fatalf("全部被押时候选必须为空, got %d", len(allHeld))
	}
}

// TestQualitySchedulableSeamSemantics 候选过滤与租约收口同款语义:
// nil 缝隙恒真(D2)、直连(节点 0)不适用、取证 ctx 放行。
func TestQualitySchedulableSeamSemantics(t *testing.T) {
	manager := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	if !manager.qualitySchedulable(context.Background(), 42) {
		t.Fatal("nil 缝隙必须恒真")
	}
	manager.SetExitEligibility(stubExitEligibility{ineligible: map[uint64]bool{42: true}})
	if manager.qualitySchedulable(context.Background(), 42) {
		t.Fatal("被押节点必须不合格")
	}
	if !manager.qualitySchedulable(context.Background(), 0) {
		t.Fatal("直连(节点 0)不受质量资格约束")
	}
	if !manager.qualitySchedulable(WithExitEligibilityBypass(context.Background()), 42) {
		t.Fatal("取证通道必须放行")
	}
}
