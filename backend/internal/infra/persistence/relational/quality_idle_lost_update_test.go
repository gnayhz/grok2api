package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestQualityIdleCooldownDoesNotLoseConcurrentFailureCount 确定性复现丢更新窗口：
// idle 冷却写曾携带调用方快照走 UpdateHealth，与并发 markFailure 的计数递增
// 竞争会把 failure_count 回滚到旧快照。修复后的定向两列写（cooldown+marker）
// 必须让迟到的泛型计数存活。交错顺序固定：快照=2 → 泛型递增到 3 → idle 写。
func TestQualityIdleCooldownDoesNotLoseConcurrentFailureCount(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	rows := seedBatchUpdateAccounts(t, database, 1)
	id := accountModelIDs(rows)[0]

	// 调用方持有的快照：failure_count=2（读发生在并发递增之前）。
	if err := repo.UpdateHealth(ctx, id, account.ProviderBuild, 2, nil, "", false); err != nil {
		t.Fatal(err)
	}
	// 并发泛型失败先落地：计数 2 -> 3，附带 504 冷却。
	genericUntil := time.Now().UTC().Add(5 * time.Minute)
	if err := repo.UpdateHealth(ctx, id, account.ProviderBuild, 3, &genericUntil, "upstream status 504", false); err != nil {
		t.Fatal(err)
	}

	// 迟到的 idle 定向写（旧实现会以快照 2 走 UpdateHealth 回滚计数）。
	idleUntil := time.Now().UTC().Add(2 * time.Minute)
	if err := repo.UpdateQualityIdleCooldown(ctx, id, account.ProviderBuild, idleUntil); err != nil {
		t.Fatal(err)
	}

	var after accountModel
	if err := database.db.Where("id = ?", id).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.FailureCount != 3 {
		t.Fatalf("failure_count = %d, want 3 (concurrent generic increment must survive the idle write)", after.FailureCount)
	}
	if after.LastError != account.LastErrorQualityIdle {
		t.Fatalf("last_error = %q, want %q (idle marker owns the write)", after.LastError, account.LastErrorQualityIdle)
	}
	if after.CooldownUntil == nil || !after.CooldownUntil.Equal(idleUntil) {
		t.Fatalf("cooldown_until = %v, want the idle cooldown %v", after.CooldownUntil, idleUntil)
	}
}
