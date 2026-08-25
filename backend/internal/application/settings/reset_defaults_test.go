package settings

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

// TestResetToDefaultsRestoresFileBaseline：重置必须删除持久化行并使
// 内存配置回到文件基线（revision 前进、快照反映基线值），再次重置幂等。
func TestResetToDefaultsRestoresFileBaseline(t *testing.T) {
	repo := &runtimeSettingsRepositoryStub{}
	cfg := testConfig(t)
	// 文件基线为构造值；后台保存改为 2048。
	service := NewService(cfg, time.Time{}, 0, repo, nil, func(next config.Config) {})
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
	reset, err := service.ResetToDefaults(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reset.Config.Server.MaxConcurrentRequests != cfg.Server.MaxConcurrentRequests {
		t.Fatalf("reset left %d, want file default %d", reset.Config.Server.MaxConcurrentRequests, cfg.Server.MaxConcurrentRequests)
	}
	if reset.Revision <= snapshot.Revision {
		t.Fatalf("reset must advance revision: %d <= %d", reset.Revision, snapshot.Revision)
	}

	// 行已删除。
	if _, _, _, found, err := repo.Get(context.Background()); err != nil || found {
		t.Fatalf("row must be deleted after reset: found=%v err=%v", found, err)
	}

	// 幂等：再次重置成功。
	if _, err := service.ResetToDefaults(context.Background()); err != nil {
		t.Fatalf("second reset must be idempotent: %v", err)
	}
}

// TestResetWithoutFileConfigUsesCurrent：未登记文件基线时（旧装配路径），
// 重置退化为「重置为当前值」——行仍被删除，优先级语义恢复。
func TestResetWithoutFileConfigUsesCurrent(t *testing.T) {
	repo := &runtimeSettingsRepositoryStub{}
	service := NewService(testConfig(t), time.Time{}, 0, repo, nil, nil)
	if _, err := service.ResetToDefaults(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, found, _ := repo.Get(context.Background()); found {
		t.Fatal("row must be deleted")
	}
}
