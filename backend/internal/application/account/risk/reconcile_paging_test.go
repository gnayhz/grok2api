package risk

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

// pagedStore serves risky identities in exact pages and records the cursor of
// every ListRiskyVerdictAccountIDsAfter call, so the reconcile loop's paging
// behavior is observable (test-quality audit P1: page-boundary/cursor test).
type pagedStore struct {
	mu           sync.Mutex
	verdicts     map[uint64]StoredVerdict
	pageLimit    int
	cursors      []uint64
	listErrAfter int // 1-based call number that fails; 0 = never
	calls        int
}

func (p *pagedStore) DeleteCleanVerdictsExceptSources(_ context.Context, _ ...string) (int64, error) {
	return 0, nil
}

func (p *pagedStore) MostRecentCleanVerdict(_ context.Context, _ string, _ time.Duration) (uint64, bool, error) {
	return 0, false, nil
}

func (p *pagedStore) GetRiskVerdict(_ context.Context, id uint64) (relationalVerdict, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.verdicts[id]; ok {
		return v, nil
	}
	return StoredVerdict{}, ErrNotFound
}

func (p *pagedStore) SaveRiskVerdict(_ context.Context, id uint64, v StoredVerdict) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verdicts[id] = v
	return nil
}

func (p *pagedStore) DeleteRiskVerdict(_ context.Context, id uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.verdicts, id)
	return nil
}

func (p *pagedStore) ListRiskyVerdictAccountIDs(_ context.Context) ([]uint64, error) {
	return nil, nil
}

func (p *pagedStore) ListRiskyVerdictAccountIDsAfter(_ context.Context, afterID uint64) ([]uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.cursors = append(p.cursors, afterID)
	if p.listErrAfter != 0 && p.calls >= p.listErrAfter {
		return nil, errors.New("paged store failure")
	}
	var ids []uint64
	for id, v := range p.verdicts {
		if id > afterID && (v.Verdict == VerdictDenied || v.Verdict == VerdictFlagged) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > p.pageLimit {
		ids = ids[:p.pageLimit]
	}
	return ids, nil
}

// countingAccounts records consequence applications per identity.
type countingAccounts struct {
	mu      sync.Mutex
	flagged map[uint64]int
	tokens  map[uint64]string
	linkedW map[uint64]uint64
}

func (c *countingAccounts) DecryptedAccessToken(_ context.Context, id uint64) (string, error) {
	return c.tokens[id], nil
}
func (c *countingAccounts) LinkedWebAccountID(_ context.Context, buildID uint64) (uint64, bool, error) {
	web, ok := c.linkedW[buildID]
	return web, ok, nil
}
func (c *countingAccounts) LinkedBuildAccountIDs(_ context.Context, webID uint64) ([]uint64, error) {
	return nil, nil
}
func (c *countingAccounts) LinkedConsoleAccountIDs(_ context.Context, webID uint64) ([]uint64, error) {
	return nil, nil
}
func (c *countingAccounts) SetAccountEnabled(_ context.Context, id uint64, enabled bool, reason string) error {
	return nil
}
func (c *countingAccounts) SetAccountRiskAttribution(ctx context.Context, id uint64, flagged bool, trigger string, origin uint64, detail string, checkedAt time.Time) error {
	return c.SetAccountRiskStatus(ctx, id, flagged)
}
func (c *countingAccounts) SetAccountRiskStatus(_ context.Context, id uint64, flagged bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if flagged {
		c.flagged[id]++
	} else {
		delete(c.flagged, id)
	}
	return nil
}
func (c *countingAccounts) ClearMissingThinkingCooldown(_ context.Context, id uint64) error {
	return nil
}

// TestReconcilePagesAcrossBoundary pins the cursor contract: a full first page
// (exactly riskyVerdictPageLimit identities) must trigger a second fetch whose
// cursor is the last ID of page one, every identity is processed exactly once,
// and a short second page terminates the loop.
func TestReconcilePagesAcrossBoundary(t *testing.T) {
	const extra = 2
	total := riskyVerdictPageLimit + extra
	store := &pagedStore{verdicts: map[uint64]StoredVerdict{}, pageLimit: riskyVerdictPageLimit}
	for id := uint64(1); id <= uint64(total); id++ {
		store.verdicts[id] = StoredVerdict{Verdict: VerdictDenied, DeniedStreak: 1, CheckedAt: time.Now().UTC()}
	}
	accounts := &countingAccounts{flagged: map[uint64]int{}, tokens: map[uint64]string{}, linkedW: map[uint64]uint64{}}
	cfg := baseTestConfig()
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, &fakeChecker{}, nil)

	service.ReconcileRiskyVerdicts(context.Background())

	store.mu.Lock()
	calls, cursors := store.calls, append([]uint64(nil), store.cursors...)
	store.mu.Unlock()
	if calls != 2 {
		t.Fatalf("list calls = %d, want 2 (full page then short page)", calls)
	}
	if len(cursors) != 2 || cursors[0] != 0 || cursors[1] != uint64(riskyVerdictPageLimit) {
		t.Fatalf("cursors = %v, want [0 %d]", cursors, riskyVerdictPageLimit)
	}

	accounts.mu.Lock()
	defer accounts.mu.Unlock()
	if len(accounts.flagged) != total {
		t.Fatalf("flagged identities = %d, want %d", len(accounts.flagged), total)
	}
	for id, count := range accounts.flagged {
		if count != 1 {
			t.Fatalf("identity %d flagged %d times, want exactly 1 (no cursor replay)", id, count)
		}
	}
}

// TestReconcileStopsOnListErrorWithoutCursorAdvance pins the error contract:
// a failing page fetch aborts reconciliation; the cursor already consumed
// stays valid and no half-page consequences are applied twice on the next
// startup pass (idempotent replay remains safe).
func TestReconcileStopsOnListErrorWithoutCursorAdvance(t *testing.T) {
	store := &pagedStore{verdicts: map[uint64]StoredVerdict{}, pageLimit: 10, listErrAfter: 1}
	for id := uint64(1); id <= 3; id++ {
		store.verdicts[id] = StoredVerdict{Verdict: VerdictDenied, DeniedStreak: 1, CheckedAt: time.Now().UTC()}
	}
	accounts := &countingAccounts{flagged: map[uint64]int{}, tokens: map[uint64]string{}, linkedW: map[uint64]uint64{}}
	cfg := baseTestConfig()
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, &fakeChecker{}, nil)

	service.ReconcileRiskyVerdicts(context.Background())

	store.mu.Lock()
	calls := store.calls
	store.mu.Unlock()
	if calls != 1 {
		t.Fatalf("list calls = %d, want exactly 1 (abort on first failure)", calls)
	}
	accounts.mu.Lock()
	defer accounts.mu.Unlock()
	if len(accounts.flagged) != 0 {
		t.Fatalf("no consequences may run when the very first page fails, got %d", len(accounts.flagged))
	}
}
