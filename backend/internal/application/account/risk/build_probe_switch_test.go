package risk

import (
	"context"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// 开关关闭时,未关联 Build 降智不得触发原生探针(保持纯行为兜底)。
func TestBuildProbeSwitchOffSkipsProbe(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	prober := &fakeBuildProber{result: BuildProbeResult{Verdict: BuildProbeClean, CheckedAt: time.Now().UTC()}}
	cfg := baseBuildTestConfig()
	cfg.BuildProbeEnabled = false
	service := New(cfg, accounts, store, &fakeChecker{}, nil)
	service.SetBuildProber(prober)

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)

	if prober.calls != 0 {
		t.Fatalf("prober calls = %d, want 0 (switch off)", prober.calls)
	}
	if _, err := store.GetRiskVerdict(context.Background(), 7); err == nil {
		t.Fatal("no verdict may be written while the fallback is off")
	}
}

// 开关开启时正常走差分探针。
func TestBuildProbeSwitchOnRunsProbe(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	prober := &fakeBuildProber{result: BuildProbeResult{Verdict: BuildProbeClean, CheckedAt: time.Now().UTC()}}
	cfg := baseBuildTestConfig()
	cfg.BuildProbeEnabled = true
	service := New(cfg, accounts, store, &fakeChecker{}, nil)
	service.SetBuildProber(prober)

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)

	if prober.calls != 1 {
		t.Fatalf("prober calls = %d, want 1 (switch on)", prober.calls)
	}
}
