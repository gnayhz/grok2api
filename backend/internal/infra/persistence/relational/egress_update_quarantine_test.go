package relational

import (
	"context"
	"testing"
	"time"

	egress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// 管理端配置编辑的运行态重置不得清除在途的质量隔离——读-改-写窗口内
// QuarantineNodeForQuality 落库的隔离(cooldown + exit_ip_quality)如果被
// 重置整组覆盖,降智出口立即回池承流。健康/失败计数按文档意图重置,
// 但隔离冷却与降智计数属于质量守卫的独占状态。
func TestUpdateEgressNodeConfigResetPreservesInFlightQuarantine(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	encrypted, err := cipher.Encrypt("socks5://127.0.0.1:52883")
	if err != nil {
		t.Fatal(err)
	}
	created, err := repo.CreateEgressNode(ctx, egress.Node{Name: "warp", Enabled: true, EncryptedProxyURL: encrypted, Health: 1})
	if err != nil {
		t.Fatal(err)
	}

	// 管理端读取快照(此时运行态全零)。
	stale, err := repo.GetEgressNode(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 窗口内质量隔离落库(与 QuarantineNodeForQuality 同参数形状)。
	cooldown := time.Now().UTC().Add(30 * time.Minute)
	degradedAt := time.Now().UTC()
	if err := repo.UpdateEgressNodeQualityState(ctx, created.ID, 0.05, 3, &cooldown, egress.LastErrorExitIPQuality, 2, &degradedAt); err != nil {
		t.Fatal(err)
	}

	// 管理端改名+换代理(配置变更,重置签名触发)落库。
	stale.Name = "warp-renamed"
	newProxy, err := cipher.Encrypt("socks5://127.0.0.1:52884")
	if err != nil {
		t.Fatal(err)
	}
	stale.EncryptedProxyURL = newProxy
	if _, err := repo.UpdateEgressNode(ctx, stale); err != nil {
		t.Fatal(err)
	}

	final, err := repo.GetEgressNode(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 配置面:改名与新代理生效。
	if final.Name != "warp-renamed" || final.EncryptedProxyURL != newProxy {
		t.Fatalf("config edit lost: %+v", final)
	}
	// 质量隔离必须存活:冷却未到 + 降智计数保留。
	if final.CooldownUntil == nil || final.CooldownUntil.Before(time.Now().UTC().Add(25*time.Minute)) {
		t.Fatalf("in-flight quarantine cooldown wiped by config reset: %+v", final.CooldownUntil)
	}
	if final.LastError != egress.LastErrorExitIPQuality {
		t.Fatalf("quarantine last_error wiped: %q", final.LastError)
	}
	if final.DegradeCount != 2 {
		t.Fatalf("degrade_count wiped: %d", final.DegradeCount)
	}
	// 隔离节点的健康/失败计数是隔离状态的一部分(与 cooldown 共同决定回池),
	// 一并保留;未被隔离的运行态才按文档意图重置。
	if final.Health != 0.05 || final.FailureCount != 3 {
		t.Fatalf("quarantined runtime wiped: health=%v fc=%d", final.Health, final.FailureCount)
	}
	_ = repository.SortQuery{}
}
