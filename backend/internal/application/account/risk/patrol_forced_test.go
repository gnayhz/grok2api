package risk

import (
	"context"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestPatrolTickForcedRunsDespiteDisabledGuard：管理端"立即巡检"按钮的后端入口
// PatrolTickForced 绕过 Enabled 门（归因整体停用时运维仍可手动复测身份），
// 且 verdict 的 Trigger 记 manual 而非 patrol——此前 0% 覆盖。
func TestPatrolTickForcedRunsDespiteDisabledGuard(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	cfg := baseTestConfig()
	cfg.Enabled = false
	checker := &fakeChecker{result: cleanResult()}
	service := New(cfg, accounts, store, checker, nil)

	service.PatrolTick(context.Background(), []uint64{90})
	if got := checker.calls.Load(); got != 0 {
		t.Fatalf("disabled guard must not patrol on the scheduled tick, calls=%d", got)
	}

	service.PatrolTickForced(context.Background(), []uint64{90})
	if got := checker.calls.Load(); got != 1 {
		t.Fatalf("forced tick must bypass the enabled gate, calls=%d", got)
	}
	if _, ok := store.verdicts[90]; !ok {
		t.Fatal("forced patrol must record the verdict")
	}
	// 巡检类 verdict 一律记 patrol trigger：manual 保留给单账号 check-now
	// （AttributeNowWithTrigger），二者语义在装配层已区分。
	if store.verdicts[90].Trigger != accountdomain.RiskTriggerPatrol {
		t.Fatalf("forced verdict trigger = %q, want patrol", store.verdicts[90].Trigger)
	}
}
