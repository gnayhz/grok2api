package egress

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"net/http"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// 回归锁(P1,质量隔离被静默终止):节点处于出口 IP 质量隔离(CooldownUntil=
// +24h, LastError=exit_ip_quality)时,在隔离落地前已建立的在途租约、或跨
// 实例部署里尚未看到隔离状态的路径,仍可能完成一次成功请求并触发成功
// Feedback。Feedback 的 succeeded 分支无条件清空 CooldownUntil/LastError——
// 24h 质量隔离被一次偶然成功的请求静默终止,坏 IP 节点立即回到调度。
// 期望:成功反馈不得触碰质量隔离状态;隔离的解除只属于轮换验证/冷却到期/
// 归因撤销三条显式路径。
func TestSuccessFeedbackDoesNotClearQualityQuarantine(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(24 * time.Hour).UTC()
	repository := &mutableEgressRepository{node: domain.Node{
		ID: 1, Name: "quarantined", Enabled: true, Health: 0.05,
		FailureCount: 3, CooldownUntil: &until, LastError: domain.LastErrorExitIPQuality,
	}}
	manager := NewManagerWithLimits(repository, cipher, netbudget.Limits{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	manager.Feedback(context.Background(), 1, http.StatusOK, nil)

	if err := manager.FlushFeedback(context.Background()); err != nil {
		t.Fatal(err)
	}

	node := repository.node
	if node.LastError != domain.LastErrorExitIPQuality || node.CooldownUntil == nil || !node.CooldownUntil.Equal(until) {
		t.Fatalf("success feedback cleared the quality quarantine: lastError=%q cooldown=%v (want %v)", node.LastError, node.CooldownUntil, until)
	}
}

// 传输失败仍记数，但不能把质量隔离替换成较短的传输冷却。
func TestFailureFeedbackStillEscalatesOnQuarantinedNode(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(24 * time.Hour).UTC()
	repository := &mutableEgressRepository{node: domain.Node{
		ID: 1, Name: "quarantined", Enabled: true, Health: 0.05,
		FailureCount: 3, CooldownUntil: &until, LastError: domain.LastErrorExitIPQuality,
	}}
	manager := NewManagerWithLimits(repository, cipher, netbudget.Limits{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	manager.Feedback(context.Background(), 1, 0, context.DeadlineExceeded)

	if err := manager.FlushFeedback(context.Background()); err != nil {
		t.Fatal(err)
	}

	if repository.node.FailureCount != 4 || repository.node.LastError != domain.LastErrorExitIPQuality || repository.node.CooldownUntil == nil || !repository.node.CooldownUntil.Equal(until) {
		t.Fatalf("failure must be counted while preserving quarantine: %+v", repository.node.HealthState())
	}
}
