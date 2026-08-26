package risk

import (
	"context"
	"testing"
	"time"
)

// UpdateConfig must hot-swap every runtime field: enabled, onDenied, patrol
// gate, and the concurrency ceiling — the settings surface relies on this.
func TestUpdateConfigHotSwapsRuntimeFields(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: cleanResult()}
	service := New(Config{Enabled: false, Concurrency: 1, Timeout: time.Second, OnDenied: "flag"}, accounts, store, checker, nil)
	if service.Enabled() {
		t.Fatal("service must start disabled")
	}
	if service.PatrolEnabled() {
		t.Fatal("patrol must start disabled")
	}
	service.UpdateConfig(Config{Enabled: true, Concurrency: 4, OnDenied: "markOnly", PatrolEnabled: true, PatrolInterval: 48 * time.Hour})
	if !service.Enabled() || !service.PatrolEnabled() {
		t.Fatal("UpdateConfig must flip enabled/patrolEnabled at runtime")
	}
	cfg := service.config()
	if cfg.Concurrency != 4 || cfg.OnDenied != "markOnly" || cfg.PatrolInterval != 48*time.Hour {
		t.Fatalf("runtime cfg = %#v", cfg)
	}
}

// UpdateChecker atomically replaces the probe implementation.
func TestUpdateCheckerSwapsProbe(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	service := New(baseTestConfig(), accounts, store, &fakeChecker{result: cleanResult()}, nil)
	verdict := service.checkNow(context.Background(), 90, 90)
	if verdict.Verdict != VerdictClean {
		t.Fatalf("initial probe = %#v, want clean", verdict)
	}
	service.UpdateChecker(&fakeChecker{result: deniedResult()}, "")
	if err := store.DeleteRiskVerdict(context.Background(), 90); err != nil {
		t.Fatal(err)
	}
	verdict = service.checkNow(context.Background(), 90, 90)
	if verdict.Verdict != VerdictDenied {
		t.Fatalf("probe after swap = %#v, want denied", verdict)
	}
}
