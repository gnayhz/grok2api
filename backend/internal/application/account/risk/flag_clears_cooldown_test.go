package risk

import (
	"context"
	"testing"
	"time"
)

// flagIdentity 必须同时清掉身份组内各账号的 missing-thinking 冷却:
// rsc_denied 已永久排除调度,残留冷却只是 UI 噪音(线上"有标又冷却"的
// 困惑态)。标志由 SetAccountRiskStatus 写入,冷却清理走既有 cleared 面。
func TestFlagClearsMissingThinkingCooldownScoped(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: CheckResult{Verdict: VerdictDenied, Source: "sso_probe"}}
	service := New(Config{Enabled: true, Concurrency: 2, Timeout: time.Second, OnDenied: "flag", PatrolInterval: 30 * 24 * time.Hour, ErrorRetry: time.Hour, DeniedConfirmations: 1}, accounts, store, checker, nil)
	// web 90 + build 91 + console 92 同一身份组;三者都带着 missing-thinking 冷却。
	accounts.linkedBack[90] = []uint64{91}
	accounts.linkedConsole[90] = []uint64{92}
	for _, id := range []uint64{90, 91, 92} {
		accounts.cooldown[id] = true
	}
	verdict := StoredVerdict{Verdict: VerdictDenied, DeniedStreak: 1, Source: "sso_probe", CheckedAt: time.Now().UTC()}
	service.applyConsequences(context.Background(), 91, 90, verdict, 0)
	if !accounts.flagged[91] || accounts.cooldown[91] {
		t.Fatalf("degraded build 91: flagged=%v cooldown=%v, want flagged and cleared", accounts.flagged[91], accounts.cooldown[91])
	}
	for _, id := range []uint64{90, 92} {
		if accounts.flagged[id] || !accounts.cooldown[id] {
			t.Fatalf("account %d: flagged=%v cooldown=%v, must stay untouched", id, accounts.flagged[id], accounts.cooldown[id])
		}
	}
	if len(accounts.cleared) != 1 || accounts.cleared[0] != 91 {
		t.Fatalf("clear calls = %v, want [91] (channel-scoped)", accounts.cleared)
	}
}
