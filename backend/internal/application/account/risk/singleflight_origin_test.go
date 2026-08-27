package risk

import (
	"context"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestWaiterInheritsLeaderOriginDoesNotCascade：Build 降智是 leader、巡检是
// waiter 时，共享结果必须保留 leader 的 origin=Build。waiter 若改写成
// origin=webID，applyConsequences 会把通道隔离的定罪升级成 Web/Console 连坐。
func TestWaiterInheritsLeaderOriginDoesNotCascade(t *testing.T) {
	t.Parallel()
	accounts := newFakeAccounts()
	accounts.token[90] = "sso"
	accounts.linkedWeb[7] = 90
	accounts.linkedBack[90] = []uint64{7}
	accounts.linkedConsole[90] = []uint64{55}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &heldChecker{release: make(chan struct{}), result: deniedResult()}
	cfg := baseTestConfig()
	cfg.Concurrency = 1
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, checker, nil)

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for checker.arrivals.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if checker.arrivals.Load() == 0 {
		t.Fatal("leader never entered the checker")
	}

	waiterDone := make(chan StoredVerdict, 1)
	go func() {
		waiterDone <- service.checkNow(context.Background(), 90, 90, accountdomain.RiskTriggerPatrol)
	}()
	deadline = time.Now().Add(2 * time.Second)
	for service.waiters.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if service.waiters.Load() == 0 {
		t.Fatal("patrol waiter never parked on the in-flight check")
	}

	close(checker.release)
	select {
	case <-leaderDone:
	case <-time.After(2 * time.Second):
		t.Fatal("leader never completed")
	}
	var shared StoredVerdict
	select {
	case shared = <-waiterDone:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never completed")
	}

	if checker.arrivals.Load() != 1 {
		t.Fatalf("checker arrivals = %d, want 1 (singleflight)", checker.arrivals.Load())
	}
	if shared.OriginAccountID != 7 {
		t.Fatalf("waiter origin = %d, want leader build 7 (not overwritten to web 90)", shared.OriginAccountID)
	}
	if shared.Trigger != accountdomain.RiskTriggerDegrade {
		t.Fatalf("waiter trigger = %q, want leader degrade", shared.Trigger)
	}
	stored, err := store.GetRiskVerdict(context.Background(), 90)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OriginAccountID != 7 {
		t.Fatalf("persisted origin = %d, want 7", stored.OriginAccountID)
	}

	service.applyConsequences(context.Background(), 90, 90, shared, 0)
	if !accounts.flagged[7] {
		t.Fatal("leader must flag the degraded build")
	}
	if !accounts.flagged[90] {
		t.Fatal("patrol waiter still flags the web identity it was checking")
	}
	if accounts.flagged[55] {
		t.Fatal("shared Build-origin denied must not cascade to Console")
	}
}

// TestPatrolLeaderStillCascadesWhenBuildWaits：相反方向——巡检是 leader 时
// 身份连坐仍由 leader 落地，Build waiter 继承 origin=webID 不得拆掉连坐。
func TestPatrolLeaderStillCascadesWhenBuildWaits(t *testing.T) {
	t.Parallel()
	accounts := newFakeAccounts()
	accounts.token[90] = "sso"
	accounts.linkedWeb[7] = 90
	accounts.linkedBack[90] = []uint64{7}
	accounts.linkedConsole[90] = []uint64{55}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &heldChecker{release: make(chan struct{}), result: deniedResult()}
	cfg := baseTestConfig()
	cfg.Concurrency = 1
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, checker, nil)

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		service.PatrolTick(context.Background(), []uint64{90})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for checker.arrivals.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if checker.arrivals.Load() == 0 {
		t.Fatal("patrol leader never entered the checker")
	}

	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)
	}()
	deadline = time.Now().Add(2 * time.Second)
	for service.waiters.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	close(checker.release)
	select {
	case <-leaderDone:
	case <-time.After(2 * time.Second):
		t.Fatal("patrol leader never completed")
	}
	select {
	case <-waiterDone:
	case <-time.After(2 * time.Second):
		t.Fatal("build waiter never completed")
	}

	for _, id := range []uint64{90, 7, 55} {
		if !accounts.flagged[id] {
			t.Fatalf("patrol-led SSO denied must still cascade to %d, flagged=%v", id, accounts.flagged)
		}
	}
	stored, err := store.GetRiskVerdict(context.Background(), 90)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OriginAccountID != 90 || stored.Trigger != accountdomain.RiskTriggerPatrol {
		t.Fatalf("persisted origin/trigger = %d/%q, want web 90/patrol", stored.OriginAccountID, stored.Trigger)
	}
}
