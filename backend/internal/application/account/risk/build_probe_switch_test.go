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

// 已关联 Web SSO 的 Build 降智，在开关打开时仍走同通道差分探针，
// 不得改走 grok.com fast SSO（大池误判路径：Web≠Build、探针出口脏）。
func TestLinkedBuildUsesNativeProbeWhenEnabled(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedWeb[7] = 90
	accounts.linkedBack[90] = []uint64{7}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: deniedResult()}
	prober := &fakeBuildProber{result: BuildProbeResult{Verdict: BuildProbeClean, CheckedAt: time.Now().UTC()}}
	cfg := baseBuildTestConfig()
	cfg.BuildProbeEnabled = true
	service := New(cfg, accounts, store, checker, nil)
	service.SetBuildProber(prober)

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)

	if checker.calls.Load() != 0 {
		t.Fatalf("sso checker calls = %d, want 0 (Build channel owns linked degrade)", checker.calls.Load())
	}
	if prober.calls != 1 {
		t.Fatalf("prober calls = %d, want 1", prober.calls)
	}
	if accounts.flagged[7] || accounts.flagged[90] {
		t.Fatal("clean Build probe must not flag the identity group")
	}
}

// 开关关闭时，已关联 Build 仍走 SSO（向后兼容）。
func TestLinkedBuildKeepsSSOWhenProbeOff(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedWeb[7] = 90
	accounts.linkedBack[90] = []uint64{7}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: deniedResult()}
	prober := &fakeBuildProber{result: BuildProbeResult{Verdict: BuildProbeClean, CheckedAt: time.Now().UTC()}}
	cfg := baseBuildTestConfig()
	cfg.BuildProbeEnabled = false
	service := New(cfg, accounts, store, checker, nil)
	service.SetBuildProber(prober)

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)

	if prober.calls != 0 {
		t.Fatalf("prober calls = %d, want 0 (switch off)", prober.calls)
	}
	if checker.calls.Load() != 1 {
		t.Fatalf("sso checker calls = %d, want 1", checker.calls.Load())
	}
}
