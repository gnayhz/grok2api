package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestResetQuotaStateLiftsPenaltyCooldown 验证重置额度同时把惩罚冷却恢复到健康基线：
// 空流/缺推理误判产生的 24h 冷却不再只能干等过期。
func TestResetQuotaStateLiftsPenaltyCooldown(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	rows := seedBatchUpdateAccounts(t, database, 2)
	ids := accountModelIDs(rows)
	until := time.Now().UTC().Add(24 * time.Hour)
	if err := database.db.Model(&accountModel{}).Where("id = ?", ids[0]).Updates(map[string]any{
		"cooldown_until": until, "failure_count": 3, "last_error": "upstream status 504",
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := repo.ResetQuotaState(ctx, account.ProviderBuild, ids); err != nil {
		t.Fatal(err)
	}

	var after accountModel
	if err := database.db.Where("id = ?", ids[0]).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.CooldownUntil != nil {
		t.Fatalf("cooldown_until = %v, want nil", after.CooldownUntil)
	}
	if after.FailureCount != 0 {
		t.Fatalf("failure_count = %d, want 0", after.FailureCount)
	}
	if after.LastError != "" {
		t.Fatalf("last_error = %q, want empty", after.LastError)
	}
	// 健康账号不应产生无效写入副作用（列值保持原样）。
	var healthy accountModel
	if err := database.db.Where("id = ?", ids[1]).Take(&healthy).Error; err != nil {
		t.Fatal(err)
	}
	if healthy.FailureCount != 0 || healthy.LastError != "" || healthy.CooldownUntil != nil {
		t.Fatalf("healthy account mutated: %+v", healthy)
	}
}

// TestResetProviderQuotaStateLiftsPenaltyCooldown 全量重置同样解除启用账号的惩罚冷却，
// 且不触碰未启用账号（activeOnly 语义保持）。
func TestResetProviderQuotaStateLiftsPenaltyCooldown(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	rows := seedBatchUpdateAccounts(t, database, 2)
	ids := accountModelIDs(rows)
	until := time.Now().UTC().Add(24 * time.Hour)
	if err := database.db.Model(&accountModel{}).Where("id IN ?", ids).Updates(map[string]any{
		"cooldown_until": until, "failure_count": 1, "last_error": "upstream status 504",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.Model(&accountModel{}).Where("id = ?", ids[1]).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}

	count, err := repo.ResetProviderQuotaState(ctx, account.ProviderBuild, true)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("reset count = %d, want 1", count)
	}

	var active, disabled accountModel
	if err := database.db.Where("id = ?", ids[0]).Take(&active).Error; err != nil {
		t.Fatal(err)
	}
	if active.CooldownUntil != nil || active.FailureCount != 0 || active.LastError != "" {
		t.Fatalf("active penalty not lifted: %+v", active)
	}
	if err := database.db.Where("id = ?", ids[1]).Take(&disabled).Error; err != nil {
		t.Fatal(err)
	}
	if disabled.CooldownUntil == nil || disabled.FailureCount != 1 {
		t.Fatalf("disabled account penalty should be untouched: %+v", disabled)
	}
}
