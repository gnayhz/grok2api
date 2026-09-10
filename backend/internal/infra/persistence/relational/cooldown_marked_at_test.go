package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestCooldownMarkedAtLifecycle 锁定冷却写入时刻列:UpdateHealth 写冷却时落
// markedAt、清零时同步清空,idle 定向写同样落,Get 投影必须暴露。该列是 RSC
// clean 自愈解除"最短保持期"的判据——历史事故中 clean 在惩罚写入 6ms 后
// 解除冷却,降智波峰内池子冻结,长冷却形同虚设。
func TestCooldownMarkedAtLifecycle(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	rows := seedBatchUpdateAccounts(t, database, 1)
	id := accountModelIDs(rows)[0]

	until := time.Now().UTC().Add(time.Hour)
	if err := seedHealthFixture(repo, ctx, id, account.ProviderBuild, 1, &until, account.LastErrorMissingThinking, false); err != nil {
		t.Fatal(err)
	}
	var row accountModel
	if err := database.db.Where("id = ?", id).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.CooldownMarkedAt == nil {
		t.Fatal("cooldown_marked_at must be written when a cooldown is set")
	}
	credential, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if credential.CooldownMarkedAt == nil {
		t.Fatal("Credential projection must expose CooldownMarkedAt")
	}

	// 成功标记清零冷却:标记同步清空。
	if _, err := repo.ApplyHealth(ctx, id, account.ProviderBuild, account.HealthEvent{Kind: account.HealthSuccess, ObservedRevision: credential.HealthRevision}); err != nil {
		t.Fatal(err)
	}
	var cleared accountModel
	if err := database.db.Where("id = ?", id).Take(&cleared).Error; err != nil {
		t.Fatal(err)
	}
	if cleared.CooldownMarkedAt != nil {
		t.Fatal("cooldown_marked_at must be cleared when the cooldown clears")
	}

	// idle 定向写同样落标记。
	idleCooldown := 2 * time.Minute
	if _, err := repo.ApplyHealth(ctx, id, account.ProviderBuild, account.HealthEvent{Kind: account.HealthQualityIdle, RetryAfter: idleCooldown}); err != nil {
		t.Fatal(err)
	}
	var idled accountModel
	if err := database.db.Where("id = ?", id).Take(&idled).Error; err != nil {
		t.Fatal(err)
	}
	if idled.CooldownMarkedAt == nil {
		t.Fatal("cooldown_marked_at must be written by the idle cooldown path")
	}
}
