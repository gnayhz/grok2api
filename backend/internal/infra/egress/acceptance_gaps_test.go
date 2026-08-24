package egress

import (
	"context"
	"errors"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// 验收②:canary 通过回池(Release/暂定放行)必须清掉 L2 软冷却,
// 否则指数升级的证据会继续压着一个已验证健康的节点。
func TestReleaseClearsSoftCooldown(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, testCipher(t))
	manager.SetDegradeEvidenceCooldowns(5*time.Minute, time.Hour)
	manager.MarkDegradeEvidence(9)
	manager.MarkDegradeEvidence(9) // 升级到 10m
	if !manager.nodeSoftCooled(9, time.Now().UTC()) {
		t.Fatalf("precondition: soft cooldown expected")
	}
	manager.ClearDegradeEvidence(9)
	if manager.nodeSoftCooled(9, time.Now().UTC()) {
		t.Fatalf("soft cooldown not cleared")
	}
}

// 验收③:新隔离周期必须重置换 IP 尝试计数——
// 否则上一周期耗尽(attempts==max)的节点再次被隔离时永远不轮换。
func TestFreshQuarantineResetsRotationAttempts(t *testing.T) {
	repo := &qualityRotationRepo{node: newNodeWith(9)}
	repo.node.RotationAttempts = 3 // 上一周期已耗尽
	manager := NewManager(repo, testCipher(t))
	if _, err := manager.QuarantineNodeForQuality(context.Background(), 9, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if repo.node.RotationAttempts != 0 {
		t.Fatalf("rotation attempts not reset on fresh quarantine: %d", repo.node.RotationAttempts)
	}
}

// 验收④:固定路由目标(含金丝雀钉住节点)在 L2 软冷却期间不可用,
// 调用方退回自动调度而非继续命中受检出口。
func TestPinnedNodeRejectsSoftCooledTarget(t *testing.T) {
	repo := &qualityRotationRepo{node: newNodeWith(11)}
	manager := NewManager(repo, testCipher(t))
	repo.node.EncryptedProxyURL = encryptedProxy(t, manager.cipher, "http://127.0.0.1:9100")
	manager.SetDegradeEvidenceCooldowns(5*time.Minute, time.Hour)
	ctx := WithPinnedNode(context.Background(), 11)
	if _, _, err := manager.AcquireIfConfigured(ctx, domain.ScopeBuild, "a"); err != nil {
		t.Fatalf("healthy pinned target rejected: %v", err)
	}
	manager.MarkDegradeEvidence(11)
	_, _, err := manager.AcquireIfConfigured(WithPinnedNode(context.Background(), 11), domain.ScopeBuild, "a")
	if !errors.Is(err, ErrRoutingTargetUnavailable) {
		t.Fatalf("soft-cooled pinned target err = %v, want ErrRoutingTargetUnavailable", err)
	}
}

// ---- helpers ----

func testCipher(t *testing.T) *security.Cipher {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

func newNodeWith(id uint64) domain.Node {
	return domain.Node{ID: id, Name: "n", Enabled: true, Health: 1, EncryptedProxyURL: "enc"}
}

type qualityRotationRepo struct {
	egressRepositoryTestStub
	node domain.Node
}

func (r *qualityRotationRepo) GetEgressNode(context.Context, uint64) (domain.Node, error) {
	return r.node, nil
}

func (r *qualityRotationRepo) UpdateEgressNodeQualityState(_ context.Context, _ uint64, health float64, failures int, cooldown *time.Time, lastErr string, degrade int, degradedAt *time.Time) error {
	r.node.Health, r.node.FailureCount, r.node.CooldownUntil, r.node.LastError = health, failures, cooldown, lastErr
	r.node.DegradeCount, r.node.LastDegradedAt = degrade, degradedAt
	return nil
}

func (r *qualityRotationRepo) UpdateEgressNodeRotationState(_ context.Context, _ uint64, rotatedAt *time.Time, attempts int, lastErr string) error {
	r.node.RotationAttempts, r.node.LastRotationError = attempts, lastErr
	if rotatedAt != nil {
		r.node.LastRotatedAt = rotatedAt
	}
	return nil
}

// 质量验证钉住(canary)与降智重试钉住语义对照:验证钉住必须能取到处于
// 质量隔离冷却/L2 软冷却中的受检节点(否则 canary 永远无法执行, 回池链路
// 失效); 重试钉住仍然拒绝(降智重试不得撞回坏出口)。
func TestQualityVerificationPinBypassesCooldowns(t *testing.T) {
	repo := &qualityRotationRepo{node: newNodeWith(11)}
	manager := NewManager(repo, testCipher(t))
	repo.node.EncryptedProxyURL = encryptedProxy(t, manager.cipher, "http://127.0.0.1:9100")
	manager.SetDegradeEvidenceCooldowns(5*time.Minute, time.Hour)
	until := time.Now().Add(time.Hour)
	repo.node.CooldownUntil = &until
	repo.node.LastError = domain.LastErrorExitIPQuality
	manager.MarkDegradeEvidence(11)

	// 验证钉住:冷却中的节点可获取。
	lease, _, err := manager.AcquireIfConfigured(WithQualityVerificationNode(context.Background(), 11), domain.ScopeBuild, "canary")
	if err != nil || lease == nil {
		t.Fatalf("quality verification pin must bypass quarantine cooldown: lease=%v err=%v", lease, err)
	}
	lease.Release()

	// 对照:重试钉住仍然拒绝同一节点。
	if _, _, err := manager.AcquireIfConfigured(WithPinnedNode(context.Background(), 11), domain.ScopeBuild, "retry"); !errors.Is(err, ErrRoutingTargetUnavailable) {
		t.Fatalf("degraded-retry pin err = %v, want ErrRoutingTargetUnavailable", err)
	}
}
