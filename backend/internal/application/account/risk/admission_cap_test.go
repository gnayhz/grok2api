package risk

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// countingBlocker holds every Check until release closes and counts arrivals.
type countingBlocker struct {
	release      chan struct{}
	started      chan struct{}
	leaderSeated chan struct{}
	seatedOnce   sync.Once
	arrivals     atomic.Int32
	result       CheckResult
}

func (c *countingBlocker) Check(ctx context.Context, token string) CheckResult {
	c.arrivals.Add(1)
	c.seatedOnce.Do(func() { close(c.leaderSeated) })
	if c.started != nil {
		c.started <- struct{}{}
	}
	<-c.release
	return c.result
}

// TestAdmissionCapDropsAndRecovers proves three invariants deterministically:
// (1) the 65th queued attribution is dropped synchronously (pending capped at
// maxQueuedAttributions, its admission key reclaimed); (2) all 64 admitted
// attributions for the SAME web identity merge into exactly one RSC check and
// one verdict save (singleflight) while every admitted account plus the web
// identity still receives consequences; (3) after drain a fresh event is
// admitted again — the cap recovers instead of wedging.
func TestAdmissionCapDropsAndRecovers(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso"
	const n = maxQueuedAttributions
	builds := make([]uint64, n+1)
	for i := range builds {
		builds[i] = uint64(100 + i)
		accounts.linkedWeb[builds[i]] = 90
	}
	accounts.linkedBack[90] = builds
	accounts.linkedConsole[90] = []uint64{91}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &countingBlocker{release: make(chan struct{}), started: make(chan struct{}, 1), leaderSeated: make(chan struct{}), result: deniedResult()}
	cfg := baseTestConfig()
	cfg.Concurrency = 1
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, checker, nil)

	// staged admission: fire one event, wait until the leader is seated inside
	// Check (still in flight), then fire the rest - they all merge onto the
	// in-flight check deterministically. The old single burst relied on
	// scheduling luck and failed 4/5 under -count=5 when the second wave
	// arrived after the leader had cleared the inflight map.
	service.OnDegraded(context.Background(), accountdomain.Credential{ID: builds[0], Provider: accountdomain.ProviderBuild}, 0)
	<-checker.leaderSeated
	for _, id := range builds[1:] {
		service.OnDegraded(context.Background(), accountdomain.Credential{ID: id, Provider: accountdomain.ProviderBuild}, 0)
	}
	// 等到全部已准入的等待者真正停靠到在途检查上：高负载下个别
	// goroutine 可能延迟到 leader 完成并清理 inflight 之后才执行 LoadOrStore，合法地成为新
	// leader（arrivals=2）——停靠计数把“已合并”变成可等待的确定性条件。
	parkDeadline := time.Now().Add(5 * time.Second)
	for service.waiters.Load() < maxQueuedAttributions-1 && time.Now().Before(parkDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := service.waiters.Load(); got != maxQueuedAttributions-1 {
		t.Fatalf("parked waiters = %d, want %d", got, maxQueuedAttributions-1)
	}

	// (1) cap: the last event was dropped synchronously; the cap holds.
	if got := service.pending.Load(); got != maxQueuedAttributions {
		t.Fatalf("pending = %d, want %d", got, maxQueuedAttributions)
	}
	if _, held := service.admissionDedup.Load(builds[n]); held {
		t.Fatal("dropped admission key must be reclaimed")
	}

	// Wait until the leader is inside the checker, then release.
	select {
	case <-checker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("leader never reached the checker")
	}
	close(checker.release)

	// (2) drain: all admitted attributions complete.
	deadline := time.Now().Add(2 * time.Second)
	for service.pending.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := service.pending.Load(); got != 0 {
		t.Fatalf("pending did not drain: %d", got)
	}
	if got := checker.arrivals.Load(); got != 1 {
		t.Fatalf("checker arrivals = %d, want exactly 1 (singleflight merge)", got)
	}
	if got := store.saves.Load(); got != 1 {
		t.Fatalf("verdict saves = %d, want exactly 1", got)
	}
	if store.verdicts[90].Verdict != VerdictDenied {
		t.Fatalf("web verdict = %#v, want denied", store.verdicts[90])
	}
	accounts.mu.Lock()
	for _, id := range builds[:n] {
		if !accounts.flagged[id] {
			t.Fatalf("admitted build %d must be flagged", id)
		}
	}
	// 通道隔离：每个准入事件只处置自己（对应 Build）；Web 90 与 Console 91
	// 不级联，未被任何降智事件涉及就保持未标。
	if accounts.flagged[90] || accounts.flagged[91] {
		t.Fatal("web identity and console must not be cascaded (channel-scoped)")
	}
	// Note: builds[n] was dropped at admission and receives no consequence of
	// its own; the drop guarantees no EXTRA check (arrivals stays 1 above).
	accounts.mu.Unlock()

	// (3) recovery: a fresh event for another build is admitted again and
	// settles straight from the cached denied verdict (no extra check).
	accounts.linkedWeb[200] = 90
	accounts.linkedBack[90] = append(builds[:n:n], 200)
	service.OnDegraded(context.Background(), accountdomain.Credential{ID: 200, Provider: accountdomain.ProviderBuild}, 0)
	if got := service.pending.Load(); got != 1 {
		t.Fatalf("post-drain pending = %d, want 1 (cap must recover)", got)
	}
	deadline = time.Now().Add(2 * time.Second)
	for service.pending.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := service.pending.Load(); got != 0 {
		t.Fatalf("recovery event did not drain: %d", got)
	}
	if got := checker.arrivals.Load(); got != 1 {
		t.Fatalf("cached denied must skip re-check, arrivals = %d", got)
	}
	accounts.mu.Lock()
	defer accounts.mu.Unlock()
	if !accounts.flagged[200] {
		t.Fatal("recovered event must apply consequences")
	}
}
