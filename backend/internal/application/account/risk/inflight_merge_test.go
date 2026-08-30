package risk

import (
	"context"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestOnDegradedWebCredentialRunsCheck：Web 账号 credential 的 ID 与 webID
// 相同，修复前 admission 与 check 共用 inflight 会自碰撞导致检查永不执行
// （六份外部复核交叉确认）。
func TestOnDegradedWebCredentialRunsCheck(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso"
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: cleanResult()}
	service := New(baseTestConfig(), accounts, store, checker, nil)

	service.OnDegraded(context.Background(), accountdomain.Credential{ID: 90, Provider: accountdomain.ProviderWeb}, 0)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && checker.calls.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if checker.calls.Load() == 0 {
		t.Fatal("web-credential attribution must actually run the RSC check (was self-blocked by shared inflight)")
	}
}

// TestErrorVerdictDoesNotClobberDenied：error 结论不得覆盖已有 denied
// （"denied 永久缓存"不变量——外部复核发现无条件保存会把否决洗成 error）。
func TestErrorVerdictDoesNotClobberDenied(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso"
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{
		90: {Verdict: VerdictDenied, CheckedAt: time.Now().UTC()},
	}}
	// decrypt 失败产生 error verdict 的路径。
	accounts.token[91] = ""
	service := New(baseTestConfig(), accounts, store, &fakeChecker{}, nil)
	service.saveVerdictGuarded(context.Background(), 90, StoredVerdict{Verdict: VerdictError, Error: "x"})
	if store.verdicts[90].Verdict != VerdictDenied {
		t.Fatalf("denied verdict was clobbered: %#v", store.verdicts[90])
	}
}
