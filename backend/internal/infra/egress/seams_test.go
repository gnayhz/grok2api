package egress

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// stubExitEligibility 可编程出口资格谓词。
type stubExitEligibility struct {
	ineligible map[uint64]bool
}

func (s stubExitEligibility) ExitSchedulable(nodeID uint64) bool {
	return !s.ineligible[nodeID]
}

// TestExitEligibilitySeamBlocksLease 锚定 D3-1/B2:出口资格谓词在
// 租约收口点拦截质量轴不合格节点;直连(节点 0)放行。
func TestExitEligibilitySeamBlocksLease(t *testing.T) {
	ctx := context.Background()
	manager := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	manager.SetExitEligibility(stubExitEligibility{ineligible: map[uint64]bool{7: true}})
	node := egress.Node{ID: 7, Name: "seam-node", Enabled: true, Health: 1}
	_, _, err := manager.leaseForNodeWithOptions(ctx, egress.ScopeBuild, "", "", false, node, clientOptions{})
	if !errors.Is(err, ErrRoutingTargetUnavailable) {
		t.Fatalf("不合格节点租约必须被拒: %v", err)
	}
	// 直连(节点 0)不经资格判定。
	direct := egress.Node{ID: 0, Name: "route-direct", Enabled: true, Health: 1}
	if _, _, err := manager.leaseForNodeWithOptions(ctx, egress.ScopeBuild, "", "", false, direct, clientOptions{}); errors.Is(err, ErrRoutingTargetUnavailable) {
		t.Fatalf("直连不受出口资格约束: %v", err)
	}
}

// TestExitEligibilityProbeBypass 锚定 B2 生产/探针双通道:取证上下文
// 放行出口资格缝隙。
func TestExitEligibilityProbeBypass(t *testing.T) {
	manager := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	manager.SetExitEligibility(stubExitEligibility{ineligible: map[uint64]bool{9: true}})
	node := egress.Node{ID: 9, Name: "probe-node", Enabled: true, Health: 1}
	probeCtx := WithExitEligibilityBypass(context.Background())
	if _, _, err := manager.leaseForNodeWithOptions(probeCtx, egress.ScopeBuild, "", "", false, node, clientOptions{}); errors.Is(err, ErrRoutingTargetUnavailable) {
		t.Fatalf("取证通道必须放行: %v", err)
	}
}

// TestExitEligibilityNilSeamUnchanged 缝隙未注入时恒放行(D2 剥离态)。
func TestExitEligibilityNilSeamUnchanged(t *testing.T) {
	manager := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	node := egress.Node{ID: 11, Name: "free-node", Enabled: true, Health: 1}
	if _, _, err := manager.leaseForNodeWithOptions(context.Background(), egress.ScopeBuild, "", "", false, node, clientOptions{}); errors.Is(err, ErrRoutingTargetUnavailable) {
		t.Fatalf("未注入缝隙必须恒放行: %v", err)
	}
}

// TestDialerSeamSatisfiedByManager 编译期断言的行为面(D3-2):
// Manager 满足拨号器缝隙接口——底座内建实现,批4 质量层可替换。
func TestDialerSeamSatisfiedByManager(t *testing.T) {
	var dialer Dialer = NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	if dialer == nil {
		t.Fatal("Manager 必须满足 Dialer 缝隙接口")
	}
}
