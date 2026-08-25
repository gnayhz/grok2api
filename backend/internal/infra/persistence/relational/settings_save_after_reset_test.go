package relational

import (
	"context"
	"testing"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// TestRuntimeSettingsSaveAfterReset 锁定 ResetToDefaults 后首次保存的死锁
// 回归:重置删除持久化行但服务层 revision 前进,Save(expectedRevision>0)
// 的 UPDATE 命中 0 行。行不存在时必须回退 Create(主键并发冲突仍按
// ErrConflict),否则恢复默认后的第一次保存永远 409,直到进程重启
// (2026-08-25 演示实例实测命中)。
func TestRuntimeSettingsSaveAfterReset(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := NewRuntimeSettingsRepository(database, cipher)

	// 首次保存(revision 0 → insert)与读取。
	if _, revision, err := repo.Save(ctx, settingsdomain.Config{}, 0); err != nil || revision != 1 {
		t.Fatalf("initial save: revision=%d err=%v", revision, err)
	}
	// 模拟 ResetToDefaults:行删除,服务层账本 revision 前进到 2。
	if err := repo.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	// 此后首次保存带 expectedRevision=2:不得返回 ErrConflict。
	_, revision, saveErr := repo.Save(ctx, settingsdomain.Config{}, 2)
	if saveErr != nil {
		t.Fatalf("save after reset must insert, got err=%v", saveErr)
	}
	if revision != 3 {
		t.Fatalf("revision continuity: got %d, want 3", revision)
	}
	// 行存在时的 revision 竞争语义保持:过期 expectedRevision 仍冲突。
	if _, _, err := repo.Save(ctx, settingsdomain.Config{}, 2); err == nil {
		t.Fatal("stale revision on existing row must conflict")
	}
}
