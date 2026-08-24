package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// 管理端编辑(Get→改→Update)不得回滚后台窄列写:rotation worker 的
// last_rotated_at/rotation_attempts 与质量隔离的 cooldown_until 是独立账本,
// 全行覆盖会让已耗尽的节点重新获得换 IP 预算、已隔离节点提前回池。
func TestUpdateEgressNodePreservesConcurrentRuntimeWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := OpenSQLite(ctx, t.TempDir()+"/egress-update-preserve.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	proxy, _ := cipher.Encrypt("http://preserve.example:8080")
	probedAt := time.Now().UTC()
	created, err := repo.CreateEgressNode(ctx, egress.Node{Name: "preserve", Enabled: true, EncryptedProxyURL: proxy, Health: 1, ProbeStatus: egress.ProbeStatusHealthy, LastProbedAt: &probedAt})
	if err != nil {
		t.Fatal(err)
	}

	// 模拟后台:rotation 记账(成功轮换 + 2 次尝试)与质量隔离冷却。
	rotatedAt := time.Now().UTC().Add(time.Minute)
	if err := repo.UpdateEgressNodeRotationState(ctx, created.ID, &rotatedAt, 2, "canary degraded"); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(2 * time.Hour).UTC()
	degradedAt := time.Now().UTC()
	if err := repo.UpdateEgressNodeQualityState(ctx, created.ID, 0.25, 4, &until, egress.LastErrorExitIPQuality, 2, &degradedAt); err != nil {
		t.Fatal(err)
	}

	// 管理端用拿到手的老快照(隔离前的健康状态)改名保存。
	created.Name = "preserve-renamed"
	updated, err := repo.UpdateEgressNode(ctx, created)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "preserve-renamed" {
		t.Fatalf("rename lost: %q", updated.Name)
	}
	// 运行态列必须保留后台写入, 不得被旧快照回滚。
	if updated.LastRotatedAt == nil || !updated.LastRotatedAt.Equal(rotatedAt) {
		t.Fatalf("last_rotated_at rolled back: %v", updated.LastRotatedAt)
	}
	if updated.RotationAttempts != 2 || updated.LastRotationError != "canary degraded" {
		t.Fatalf("rotation attempts/error rolled back: %d %q", updated.RotationAttempts, updated.LastRotationError)
	}
	if updated.CooldownUntil == nil || !updated.CooldownUntil.After(time.Now()) {
		t.Fatalf("quarantine cooldown rolled back: %v", updated.CooldownUntil)
	}
	if updated.LastError != egress.LastErrorExitIPQuality || updated.Health != 0.25 || updated.FailureCount != 4 {
		t.Fatalf("quality state rolled back: %q %v %d", updated.LastError, updated.Health, updated.FailureCount)
	}
	_ = repository.ErrNotFound
}
