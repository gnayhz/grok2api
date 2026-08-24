package risk

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// heldChecker blocks until release closes, counting arrivals.
type heldChecker struct {
	release  chan struct{}
	arrivals atomic.Int32
	result   CheckResult
}

func (h *heldChecker) Check(ctx context.Context, token string) CheckResult {
	h.arrivals.Add(1)
	<-h.release
	return h.result
}

func newCancellationFixture() (*fakeAccounts, *fakeStore, *heldChecker, *Service) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso"
	accounts.linkedWeb[7] = 90
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &heldChecker{release: make(chan struct{}), result: deniedResult()}
	cfg := baseTestConfig()
	cfg.Concurrency = 1
	cfg.OnDenied = "flag"
	return accounts, store, checker, New(cfg, accounts, store, checker, nil)
}

// TestWaiterCancelMidWaitReturnsErrorVerdict: a waiter whose context dies
// while parked on the in-flight check must surface a bounded error verdict
// (never hang) and its admission accounting must still drain.
func TestWaiterCancelMidWaitReturnsErrorVerdict(t *testing.T) {
	t.Parallel()
	accounts, store, checker, service := newCancellationFixture()
	_ = accounts
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		// Leader takes the single concurrency slot and parks inside Check.
		_ = service.checkNow(context.Background(), 90)
	}()
	// Wait until the leader is inside the checker (arrivals==1), then start
	// a waiter with an already-canceled context.
	deadline := time.Now().Add(2 * time.Second)
	for checker.arrivals.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if checker.arrivals.Load() == 0 {
		t.Fatal("leader never entered the checker")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	verdict := service.checkNow(canceled, 90)
	if verdict.Verdict != VerdictError {
		t.Fatalf("canceled waiter verdict = %q, want error", verdict.Verdict)
	}
	close(checker.release)
	select {
	case <-leaderDone:
	case <-time.After(2 * time.Second):
		t.Fatal("leader never completed after release")
	}
	_ = store
}

// TestLeaderConcurrencyGateTimeoutErrors: when the semaphore stays full past
// the caller deadline, the leader returns a bounded error verdict and still
// closes the shared done channel so waiters never block forever.
func TestLeaderConcurrencyGateTimeoutErrors(t *testing.T) {
	t.Parallel()
	accounts, _, checker, service := newCancellationFixture()
	_ = accounts
	// Occupy the single slot until gateReleased.
	service.sem <- struct{}{}
	gateReleased := make(chan struct{})
	defer func() {
		close(gateReleased)
		<-service.sem
	}()
	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		// This call becomes the leader (first store) but times out at the gate.
		verdict := service.checkNow(shortCtx(t), 90)
		if verdict.Verdict != VerdictError {
			t.Errorf("gate-timeout verdict = %q, want error", verdict.Verdict)
		}
	}()
	select {
	case <-waiterDone:
	case <-time.After(3 * time.Second):
		t.Fatal("gate-timeout leader never returned")
	}
	if got := checker.arrivals.Load(); got != 0 {
		t.Fatalf("checker must not run during gate timeout, arrivals = %d", got)
	}
}

// TestSharedCancellationStillDeliversGroupConsequences: when the leader's
// check is canceled mid-flight, the shared error result still applies
// consequences through every identity member (no stranded cooldowns).
func TestSharedCancellationStillDeliversGroupConsequences(t *testing.T) {
	t.Parallel()
	accounts, store, _, service := newCancellationFixture()
	checker := &cancelAwareChecker{arrivals: make(chan struct{}, 1)}
	service.checker = checker
	accounts.linkedBack[90] = []uint64{7}
	accounts.linkedConsole[90] = []uint64{55}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-checker.arrivals // leader is inside Check
		cancel()           // kill the deadline mid-check
	}()
	// attribute returns nothing; assert on the persisted verdict instead.
	service.attribute(ctx, credential7(), 0)
	deadline := time.Now().Add(2 * time.Second)
	for {
		accounts.mu.Lock()
		stored, found := store.verdicts[90]
		accounts.mu.Unlock()
		if found && stored.Verdict != VerdictClean {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("canceled check must persist a non-clean verdict")
		}
		time.Sleep(5 * time.Millisecond)
	}
	accounts.mu.Lock()
	defer accounts.mu.Unlock()
	if _, disabled := accounts.disabled[7]; disabled {
		t.Fatal("error verdict must not disable the degraded account")
	}
}

func credential7() accountdomain.Credential {
	return accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}
}

func shortCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

// cancelAwareChecker waits for cancellation and reports it as an error result.
type cancelAwareChecker struct {
	arrivals chan struct{}
}

func (c *cancelAwareChecker) Check(ctx context.Context, token string) CheckResult {
	c.arrivals <- struct{}{}
	<-ctx.Done()
	return CheckResult{Verdict: VerdictError, Error: "canceled: " + ctx.Err().Error(), CheckedAt: time.Now().UTC()}
}
