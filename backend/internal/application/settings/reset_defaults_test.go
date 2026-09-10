package settings

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

// TestResetToDefaultsRestoresFileBaseline：重置必须持久记录文件默认标记并使
// 内存配置回到文件基线（revision 前进、快照反映基线值），再次重置继续推进时钟。
func TestResetToDefaultsRestoresFileBaseline(t *testing.T) {
	repo := &runtimeSettingsRepositoryStub{}
	cfg := testConfig(t)
	// 文件基线为构造值；后台保存改为 2048。
	service := newTestService(cfg, time.Time{}, 0, repo, nil, func(next config.Config) {})
	service.SetFileConfig(cfg)
	snapshot := service.Get()
	updated := snapshot.Config
	updated.Server.MaxConcurrentRequests = 2048
	if _, err := service.Update(context.Background(), 0, updated); err != nil {
		t.Fatal(err)
	}
	if service.Get().Config.Server.MaxConcurrentRequests != 2048 {
		t.Fatal("update did not take effect")
	}

	// 重置：回到文件基线。
	reset, err := service.ResetToDefaults(context.Background(), service.Get().Revision)
	if err != nil {
		t.Fatal(err)
	}
	if reset.Config.Server.MaxConcurrentRequests != cfg.Server.MaxConcurrentRequests {
		t.Fatalf("reset left %d, want file default %d", reset.Config.Server.MaxConcurrentRequests, cfg.Server.MaxConcurrentRequests)
	}
	if reset.Revision <= snapshot.Revision {
		t.Fatalf("reset must advance revision: %d <= %d", reset.Revision, snapshot.Revision)
	}

	// 覆盖已移除，版本仍然持久化。
	if _, updatedAt, revision, found, err := repo.Get(context.Background()); err != nil || found || revision != reset.Revision || !updatedAt.Equal(reset.UpdatedAt) {
		t.Fatalf("reset must retain its durable clock: found=%v err=%v", found, err)
	}

	// 幂等：再次重置成功。
	if _, err := service.ResetToDefaults(context.Background(), service.Get().Revision); err != nil {
		t.Fatalf("second reset must be idempotent: %v", err)
	}
}

// TestResetWithoutFileConfigUsesCurrent：未登记文件基线时（旧装配路径），
// 重置退化为「重置为当前值」——覆盖移除并保留版本，优先级语义恢复。
func TestResetWithoutFileConfigUsesCurrent(t *testing.T) {
	repo := &runtimeSettingsRepositoryStub{}
	service := newTestService(testConfig(t), time.Time{}, 0, repo, nil, nil)
	if _, err := service.ResetToDefaults(context.Background(), service.Get().Revision); err != nil {
		t.Fatal(err)
	}
	if _, _, _, found, _ := repo.Get(context.Background()); found {
		t.Fatal("override must be absent")
	}
}
