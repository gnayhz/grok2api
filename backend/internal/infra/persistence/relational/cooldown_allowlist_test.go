package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestClearMissingThinkingCooldownAllowList 在持久层锁定 clean 归因的清除白名单：
// 只有 missing-thinking 家族与 quality-idle 三种标记的冷却会被解除；泛型 5xx
// 冷却与计数必须原样保留（否则任何 clean 判定都能顺手洗掉真实故障惩罚，
// 形成永不冷却循环）。clean 判定只证明本次降智与账号无关，不证明账号健康。
func TestClearMissingThinkingCooldownAllowList(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	rows := seedBatchUpdateAccounts(t, database, 5)
	ids := accountModelIDs(rows)
	until := time.Now().UTC().Add(24 * time.Hour)

	cases := []struct {
		index     int
		lastError string
		mustClear bool
	}{
		{0, account.LastErrorMissingThinking, true},
		{1, account.LastErrorMissingThinkingDisabled, true},
		{2, account.LastErrorQualityIdle, true},
		{3, "upstream status 500", false},
		{4, "", false}, // healthy: idempotent no-op
	}
	for _, tc := range cases {
		if tc.lastError == "" {
			continue // healthy baseline: no seeded state
		}
		if err := database.db.Model(&accountModel{}).Where("id = ?", ids[tc.index]).Updates(map[string]any{
			"failure_count": 3, "last_error": tc.lastError, "cooldown_until": until,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range cases {
		if err := repo.ClearMissingThinkingCooldown(ctx, ids[tc.index]); err != nil {
			t.Fatalf("clear(%d): %v", tc.index, err)
		}
		var after accountModel
		if err := database.db.Where("id = ?", ids[tc.index]).Take(&after).Error; err != nil {
			t.Fatal(err)
		}
		if tc.mustClear {
			if after.CooldownUntil != nil || after.FailureCount != 0 || after.LastError != "" {
				t.Fatalf("case %d (%s): quality marker must fully clear, got until=%v failures=%d last=%q",
					tc.index, tc.lastError, after.CooldownUntil, after.FailureCount, after.LastError)
			}
			continue
		}
		if tc.lastError == "" {
			if after.FailureCount != 0 || after.LastError != "" || after.CooldownUntil != nil {
				t.Fatalf("healthy account mutated by idempotent clear: %+v", after)
			}
			continue
		}
		if after.CooldownUntil == nil || after.FailureCount != 3 || after.LastError != tc.lastError {
			t.Fatalf("generic 5xx state must survive clean attribution, got until=%v failures=%d last=%q",
				after.CooldownUntil, after.FailureCount, after.LastError)
		}
	}
}
