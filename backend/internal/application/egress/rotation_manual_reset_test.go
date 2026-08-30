package egress

import (
	"context"
	"strings"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// TestRotateNodeResetsExhaustedCycle 手动轮换必须能重开已耗尽的隔离周期:
// attempts>=max 时,RotateNode 清账本并入队,而不是"已排队"后在 worker 里
// 被 attempts 检查静默丢弃(线上:两节点 rotation exhausted,
// 管理端点击更换出口无任何效果)。
func TestRotateNodeResetsExhaustedCycle(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.10"}
	service, repo, _, _, _ := newRotationTestService(t, domain.Node{
		ID: 77, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.9",
		RotationAttempts: 3, LastRotationError: "rotation attempts exhausted",
	}, true, probe, EgressQualityProbeResult{Outcome: EgressQualityProbeClean})

	if err := service.RotateNode(context.Background(), 77); err != nil {
		t.Fatalf("manual rotate on exhausted node must reset and enqueue: %v", err)
	}
	node, err := repo.GetEgressNode(context.Background(), 77)
	if err != nil {
		t.Fatal(err)
	}
	if node.RotationAttempts != 0 || node.LastRotationError != "" {
		t.Fatalf("rotation ledger must reset on manual trigger: attempts=%d err=%q", node.RotationAttempts, node.LastRotationError)
	}
}

// TestRotateNodeReportsSkipReasons 校验入队前的真实错误返回:
// 停用节点与无 webhook 的节点,操作者应立即看到原因而非假排队成功。
func TestRotateNodeReportsSkipReasons(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy}
	service, repo, _, _, _ := newRotationTestService(t, domain.Node{
		ID: 78, Name: "warp-disabled", Enabled: false, Health: 1, // 停用节点
	}, true, probe, EgressQualityProbeResult{Outcome: EgressQualityProbeClean})

	if err := service.RotateNode(context.Background(), 78); err == nil || !strings.Contains(err.Error(), "停用") {
		t.Fatalf("disabled node must surface real reason, got: %v", err)
	}

	// 清空 webhook 密文 → 返回配置错误(fixture 强制 RotationEnabled=true
	// 并自动注入 webhook,先取节点再清空以模拟未配置场景)。
	node, err := repo.GetEgressNode(context.Background(), 78)
	if err != nil {
		t.Fatal(err)
	}
	node.Enabled = true
	node.EncryptedRotationURL = ""
	repo.node = node
	if err := service.RotateNode(context.Background(), 78); err == nil || !strings.Contains(err.Error(), "webhook") {
		t.Fatalf("webhook-missing node must surface real reason, got: %v", err)
	}
}
