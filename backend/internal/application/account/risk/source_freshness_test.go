package risk

import (
	"context"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// homepage 时代的 clean 结论（source="rsc"）在 sso_probe 下不得短路新探针
// ——grok.com 停止下发 botFlag 后旧方法把所有账号都读成 clean，正是这次
// 线上"降智不标风控"的根因。
func TestFreshVerdictRejectsForeignMethodClean(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: CheckResult{Verdict: VerdictDenied, Source: "sso_probe"}}
	service := New(baseTestConfig(), accounts, store, checker, nil)
	service.UpdateChecker(checker, "sso_probe")
	if err := store.SaveRiskVerdict(context.Background(), 90, StoredVerdict{Verdict: VerdictClean, Source: "rsc", CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	accounts.linkedWeb[91] = 90
	service.attribute(context.Background(), accountdomain.Credential{ID: 91, Provider: accountdomain.ProviderBuild}, 0)
	stored, err := store.GetRiskVerdict(context.Background(), 90)
	if err != nil {
		t.Fatal("probe must have run and overwritten the stale clean")
	}
	if stored.Verdict != VerdictDenied || stored.Source != "sso_probe" {
		t.Fatalf("stored = %#v, want denied/sso_probe", stored)
	}
	if checker.calls.Load() != 1 {
		t.Fatalf("checker calls = %d, want 1 (foreign-method clean must not short-circuit)", checker.calls.Load())
	}
}

// 同方法的 clean 结论在巡检有效期内仍然新鲜（保留缓存价值）。
func TestFreshVerdictAcceptsSameMethodClean(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: CheckResult{Verdict: VerdictClean, Source: "sso_probe"}}
	service := New(baseTestConfig(), accounts, store, checker, nil)
	service.UpdateChecker(checker, "sso_probe")
	if err := store.SaveRiskVerdict(context.Background(), 90, StoredVerdict{Verdict: VerdictClean, Source: "sso_probe", CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	accounts.linkedWeb[91] = 90
	service.attribute(context.Background(), accountdomain.Credential{ID: 91, Provider: accountdomain.ProviderBuild}, 0)
	if checker.calls.Load() != 0 {
		t.Fatalf("checker calls = %d, want 0 (same-method clean stays fresh)", checker.calls.Load())
	}
}

// denied 是载荷还在下发时的真实检测，跨方法仍永久权威（对账回放依赖）。
func TestFreshVerdictKeepsDeniedAcrossMethods(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: CheckResult{Verdict: VerdictClean, Source: "sso_probe"}}
	service := New(Config{Enabled: true, Concurrency: 2, Timeout: time.Second, OnDenied: "flag", PatrolInterval: 30 * 24 * time.Hour, ErrorRetry: time.Hour}, accounts, store, checker, nil)
	service.UpdateChecker(checker, "sso_probe")
	// 已确认 denied(DeniedStreak>=2)在 DeniedTTL 内保持权威:方法切换
	// 不得使其失效。(-90d 永久权威语义已由 DeniedTTL 取代,过期可重探。)
	if err := store.SaveRiskVerdict(context.Background(), 90, StoredVerdict{Verdict: VerdictDenied, DeniedStreak: 2, Source: "rsc", CheckedAt: time.Now().Add(-time.Hour).UTC()}); err != nil {
		t.Fatal(err)
	}
	accounts.linkedWeb[91] = 90
	service.attribute(context.Background(), accountdomain.Credential{ID: 91, Provider: accountdomain.ProviderBuild}, 0)
	if checker.calls.Load() != 0 {
		t.Fatalf("checker calls = %d, want 0 (denied stays permanent)", checker.calls.Load())
	}
	accounts.mu.Lock()
	flagged := accounts.flagged[91]
	accounts.mu.Unlock()
	if !flagged {
		t.Fatal("legacy denied verdict must still flag the degraded build (channel-scoped)")
	}
}

// InvalidateStaleCleanVerdicts 只清理跨方法的 clean，保留 denied/error/同方法。
func TestInvalidateStaleCleanVerdictsSelectivity(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{}
	service := New(baseTestConfig(), accounts, store, checker, nil)
	service.UpdateChecker(checker, "sso_probe")
	now := time.Now().UTC()
	for id, verdict := range map[uint64]StoredVerdict{
		1: {Verdict: VerdictClean, Source: "rsc", CheckedAt: now},
		2: {Verdict: VerdictClean, Source: "sso_probe", CheckedAt: now},
		3: {Verdict: VerdictDenied, Source: "rsc", CheckedAt: now},
		4: {Verdict: VerdictError, Source: "rsc", CheckedAt: now},
	} {
		if err := store.SaveRiskVerdict(context.Background(), id, verdict); err != nil {
			t.Fatal(err)
		}
	}
	service.InvalidateStaleCleanVerdicts(context.Background())
	for id, wantGone := range map[uint64]bool{1: true, 2: false, 3: false, 4: false} {
		_, err := store.GetRiskVerdict(context.Background(), id)
		gone := err != nil
		if gone != wantGone {
			t.Fatalf("verdict %d gone=%v, want %v", id, gone, wantGone)
		}
	}
}
