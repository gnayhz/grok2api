package risk

import (
	"context"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// fakeBuildProber 按脚本返回差分探针结论。
type fakeBuildProber struct {
	result BuildProbeResult
	calls  int
}

func (f *fakeBuildProber) ProbeBuildThinking(_ context.Context, _, _ uint64) BuildProbeResult {
	f.calls++
	return f.result
}

func baseBuildTestConfig() Config {
	return Config{Enabled: true, Concurrency: 2, Timeout: time.Second, OnDenied: "flag", PatrolInterval: 30 * 24 * time.Hour, ErrorRetry: time.Hour, BuildProbeEnabled: true}
}

// 未关联 Build 降智 → Build 原生探针接管;denied 需要近期 clean 见证人,
// 无见证人时压制为 error(差分双降仍可能是两条脏 IP)。
func TestBuildNativeProbeDeniedNeedsWitness(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	prober := &fakeBuildProber{result: BuildProbeResult{Verdict: BuildProbeDenied, Details: "attempt1=degraded attempt2=degraded reroll pool node", CheckedAt: time.Now().UTC()}}
	service := New(baseBuildTestConfig(), accounts, store, &fakeChecker{}, nil)
	service.SetBuildProber(prober)

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)

	if prober.calls != 1 {
		t.Fatalf("prober calls = %d, want 1", prober.calls)
	}
	stored, err := store.GetRiskVerdict(context.Background(), 7)
	if err != nil {
		t.Fatal("verdict must be saved even when suppressed")
	}
	if stored.Verdict != VerdictError {
		t.Fatalf("verdict = %s, want error (denied suppressed without witness)", stored.Verdict)
	}
	if accounts.flagged[7] {
		t.Fatal("suppressed denied must not flag the account")
	}
}

// 有近期 clean 见证人时差分 denied 正常生效,且只标记该 Build(通道隔离)。
func TestBuildNativeProbeDeniedWithWitnessFlags(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	// 见证人:另一个未关联 Build 的近期 clean 结论。
	if err := store.SaveRiskVerdict(context.Background(), 8, StoredVerdict{Verdict: VerdictClean, Source: buildProbeSourceTag, CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	prober := &fakeBuildProber{result: BuildProbeResult{Verdict: BuildProbeDenied, Details: "both paths degraded", CheckedAt: time.Now().UTC()}}
	service := New(baseBuildTestConfig(), accounts, store, &fakeChecker{}, nil)
	service.SetBuildProber(prober)

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)

	if !accounts.flagged[7] {
		t.Fatal("witnessed denied must flag the degraded build")
	}
	if accounts.flagged[8] {
		t.Fatal("witness account must not be flagged")
	}
}

// 见证人过期(>7d)时 denied 重新压制。
func TestBuildNativeProbeWitnessStaleSuppresses(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	if err := store.SaveRiskVerdict(context.Background(), 8, StoredVerdict{Verdict: VerdictClean, Source: buildProbeSourceTag, CheckedAt: time.Now().Add(-8 * 24 * time.Hour).UTC()}); err != nil {
		t.Fatal(err)
	}
	prober := &fakeBuildProber{result: BuildProbeResult{Verdict: BuildProbeDenied, CheckedAt: time.Now().UTC()}}
	service := New(baseBuildTestConfig(), accounts, store, &fakeChecker{}, nil)
	service.SetBuildProber(prober)

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)

	if accounts.flagged[7] {
		t.Fatal("stale witness must suppress the denied")
	}
}

// clean 结论解除降智冷却并转出口嫌疑(与 SSO 路径同语义)。
func TestBuildNativeProbeCleanClearsCooldown(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	prober := &fakeBuildProber{result: BuildProbeResult{Verdict: BuildProbeClean, Details: "attempt2=thinking reroll", CheckedAt: time.Now().UTC()}}
	service := New(baseBuildTestConfig(), accounts, store, &fakeChecker{}, nil)
	service.SetBuildProber(prober)
	accounts.cooldown[7] = true

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 42)

	if len(accounts.cleared) != 1 || accounts.cleared[0] != 7 {
		t.Fatalf("cleared = %v, want [7]", accounts.cleared)
	}
	if accounts.flagged[7] {
		t.Fatal("clean must not flag")
	}
}

// unconfigured 不落库不重试(功能未启用时保持纯行为兜底)。
func TestBuildNativeProbeUnconfiguredNoVerdict(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	prober := &fakeBuildProber{result: BuildProbeResult{Verdict: BuildProbeUnconfigured, Details: "no reasoning build model", CheckedAt: time.Now().UTC()}}
	service := New(baseBuildTestConfig(), accounts, store, &fakeChecker{}, nil)
	service.SetBuildProber(prober)

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)

	if _, err := store.GetRiskVerdict(context.Background(), 7); err == nil {
		t.Fatal("unconfigured must not save a verdict")
	}
}

// 缓存复用:同方法 clean 在有效期内不重探;denied 永久新鲜。
func TestBuildNativeVerdictCacheReuse(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	prober := &fakeBuildProber{result: BuildProbeResult{Verdict: BuildProbeClean, CheckedAt: time.Now().UTC()}}
	service := New(baseBuildTestConfig(), accounts, store, &fakeChecker{}, nil)
	service.SetBuildProber(prober)
	// 直接落一条同方法 clean,第二次降智应复用而不探测。
	if err := store.SaveRiskVerdict(context.Background(), 7, StoredVerdict{Verdict: VerdictClean, Source: buildProbeSourceTag, CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)
	if prober.calls != 0 {
		t.Fatalf("prober calls = %d, want 0 (same-method clean stays fresh)", prober.calls)
	}
}

// SSO 探针的 clean 缓存不得短路 Build 原生探测(方法归属隔离),反之亦然。
func TestBuildNativeRejectsForeignMethodClean(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	// 植入一条 SSO 方法(错误归属)的 clean。
	if err := store.SaveRiskVerdict(context.Background(), 7, StoredVerdict{Verdict: VerdictClean, Source: "sso_probe", CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	prober := &fakeBuildProber{result: BuildProbeResult{Verdict: BuildProbeClean, CheckedAt: time.Now().UTC()}}
	service := New(baseBuildTestConfig(), accounts, store, &fakeChecker{}, nil)
	service.SetBuildProber(prober)
	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)
	if prober.calls != 1 {
		t.Fatalf("prober calls = %d, want 1 (foreign-method clean must not short-circuit)", prober.calls)
	}
}

// 人工清除未关联 Build 的标志时,其原生 verdict 一并删除(否则 denied 永久
// 新鲜会立刻把标打回来)。
func TestClearIdentityVerdictsRemovesUnlinkedBuildVerdict(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	service := New(baseBuildTestConfig(), accounts, store, &fakeChecker{}, nil)
	if err := store.SaveRiskVerdict(context.Background(), 7, StoredVerdict{Verdict: VerdictDenied, Source: buildProbeSourceTag, CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := service.ClearIdentityVerdicts(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetRiskVerdict(context.Background(), 7); err == nil {
		t.Fatal("unlinked build verdict must be removed on manual clear")
	}
}

// 方法切换清理保留 build_probe 的 clean 缓存(SSO 方法变更不影响另一体系)。
func TestInvalidateStaleCleansKeepsBuildProbeCache(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{}
	service := New(baseBuildTestConfig(), accounts, store, checker, nil)
	service.UpdateChecker(checker, "sso_probe")
	now := time.Now().UTC()
	for id, verdict := range map[uint64]StoredVerdict{
		1: {Verdict: VerdictClean, Source: "rsc", CheckedAt: now},
		2: {Verdict: VerdictClean, Source: buildProbeSourceTag, CheckedAt: now},
	} {
		if err := store.SaveRiskVerdict(context.Background(), id, verdict); err != nil {
			t.Fatal(err)
		}
	}
	service.InvalidateStaleCleanVerdicts(context.Background())
	if _, err := store.GetRiskVerdict(context.Background(), 1); err == nil {
		t.Fatal("homepage-era clean must be purged")
	}
	if _, err := store.GetRiskVerdict(context.Background(), 2); err != nil {
		t.Fatal("build_probe clean must survive an SSO method switch")
	}
}
