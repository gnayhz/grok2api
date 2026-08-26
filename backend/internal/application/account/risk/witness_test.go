package risk

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// countingChecker 按调用序号返回不同结果,用于见证人复验场景。
type countingChecker struct {
	results []CheckResult
	calls   atomic.Int32
}

func (c *countingChecker) Check(_ context.Context, _ string) CheckResult {
	i := int(c.calls.Add(1)) - 1
	if i >= len(c.results) {
		i = len(c.results) - 1
	}
	return c.results[i]
}

// TestSuppressedDeniedHealsViaWitness：熔断压下的 denied 通过见证人复验
// 自愈——用最近 clean 的身份再探一次,看到 thinking 即重跑真实判定。
// 这是修复"整池真风控把熔断器永久锁死(清标后再也标不上)"的关键机制。
func TestSuppressedDeniedHealsViaWitness(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"   // 被测身份(降智)
	accounts.token[95] = "witness-sso" // 见证人(最近 clean)
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	// 调用序: 90 denied(被熔断压成 error+Suppressed) -> 见证人 95 clean(治愈熔断) -> 重跑 90 denied(生效)
	checker := &countingChecker{results: []CheckResult{
		{Verdict: VerdictError, Source: "sso_probe", Suppressed: true, Error: "denied suppressed"},
		{Verdict: VerdictClean, Source: "sso_probe"},
		{Verdict: VerdictDenied, Source: "sso_probe"},
	}}
	service := New(baseTestConfig(), accounts, store, checker, nil)
	service.UpdateChecker(checker, "sso_probe")
	if err := store.SaveRiskVerdict(context.Background(), 95, StoredVerdict{Verdict: VerdictClean, Source: "sso_probe", CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	verdict := service.checkNow(context.Background(), 90)
	if verdict.Verdict != VerdictDenied {
		t.Fatalf("verdict = %s, want denied (witness must heal the breaker)", verdict.Verdict)
	}
	if got := checker.calls.Load(); got != 3 {
		t.Fatalf("checker calls = %d, want 3 (suppressed + witness + retry)", got)
	}
}

// 见证人缺失(库里没有任何同方法 clean)时,压制结论保持 error。
func TestSuppressedDeniedStaysErrorWithoutWitness(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &countingChecker{results: []CheckResult{
		{Verdict: VerdictError, Source: "sso_probe", Suppressed: true, Error: "denied suppressed"},
	}}
	service := New(baseTestConfig(), accounts, store, checker, nil)
	service.UpdateChecker(checker, "sso_probe")
	verdict := service.checkNow(context.Background(), 90)
	if verdict.Verdict != VerdictError {
		t.Fatalf("verdict = %s, want error (no witness available)", verdict.Verdict)
	}
	if got := checker.calls.Load(); got != 1 {
		t.Fatalf("checker calls = %d, want 1 (no witness probe)", got)
	}
}

// 复验限频: 10 分钟窗口内第二个 Suppressed 不再消耗见证人额度。
func TestWitnessRateLimited(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.token[95] = "witness-sso"
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &countingChecker{results: []CheckResult{
		{Verdict: VerdictError, Source: "sso_probe", Suppressed: true, Error: "denied suppressed"},
	}}
	service := New(baseTestConfig(), accounts, store, checker, nil)
	service.UpdateChecker(checker, "sso_probe")
	if err := store.SaveRiskVerdict(context.Background(), 95, StoredVerdict{Verdict: VerdictClean, Source: "sso_probe", CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if verdict := service.checkNow(context.Background(), 90); verdict.Verdict != VerdictError {
		t.Fatalf("first verdict = %s, want error (witness checker returns suppressed shape)", verdict.Verdict)
	}
	if got := checker.calls.Load(); got != 2 {
		t.Fatalf("calls after first = %d, want 2 (check + witness)", got)
	}
	// 清掉 inflight 让第二次 checkNow 真实执行;限频内不应再探见证人。
	if verdict := service.checkNow(context.Background(), 90); verdict.Verdict != VerdictError {
		t.Fatalf("second verdict = %s, want error", verdict.Verdict)
	}
	if got := checker.calls.Load(); got != 3 {
		t.Fatalf("calls after second = %d, want 3 (rate-limited: only the real check)", got)
	}
}
