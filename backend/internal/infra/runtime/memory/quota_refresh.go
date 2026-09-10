package memory

import (
	"container/heap"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	maxQuotaRefreshDirty                 = 100000
	quotaRefreshExpiryCompactionMinStale = 1024
)

type quotaRefreshDirtyState struct {
	accountID  uint64
	mode       string
	generation uint64
	expiresAt  time.Time
	dirty      bool
}

type QuotaRefreshCoordinator struct {
	mu      sync.Mutex
	values  map[string]quotaRefreshDirtyState
	dirty   map[string]struct{}
	expires quotaRefreshExpiryHeap
}

func NewQuotaRefreshCoordinator() *QuotaRefreshCoordinator {
	return &QuotaRefreshCoordinator{values: make(map[string]quotaRefreshDirtyState), dirty: make(map[string]struct{})}
}

type quotaRefreshExpiry struct {
	key        string
	generation uint64
	expiresAt  time.Time
}

type quotaRefreshExpiryHeap []quotaRefreshExpiry

func (h quotaRefreshExpiryHeap) Len() int           { return len(h) }
func (h quotaRefreshExpiryHeap) Less(i, j int) bool { return h[i].expiresAt.Before(h[j].expiresAt) }
func (h quotaRefreshExpiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *quotaRefreshExpiryHeap) Push(value any)    { *h = append(*h, value.(quotaRefreshExpiry)) }
func (h *quotaRefreshExpiryHeap) Pop() any {
	values := *h
	last := values[len(values)-1]
	*h = values[:len(values)-1]
	return last
}

func (c *QuotaRefreshCoordinator) MarkQuotaRefreshDirty(_ context.Context, accountID uint64, mode string, ttl time.Duration) (repository.QuotaRefreshVersion, error) {
	mode = strings.TrimSpace(mode)
	if accountID == 0 || mode == "" || ttl <= 0 {
		return repository.QuotaRefreshVersion{}, fmt.Errorf("quota refresh identity is invalid")
	}
	now := time.Now().UTC()
	key := quotaRefreshKey(accountID, mode)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	state := c.values[key]
	if _, dirty := c.dirty[key]; !dirty && len(c.dirty) >= maxQuotaRefreshDirty {
		return repository.QuotaRefreshVersion{}, fmt.Errorf("quota refresh dirty set is full")
	}
	state.accountID = accountID
	state.mode = mode
	state.generation++
	state.expiresAt = now.Add(ttl).Truncate(time.Millisecond)
	state.dirty = true
	c.values[key] = state
	c.dirty[key] = struct{}{}
	heap.Push(&c.expires, quotaRefreshExpiry{key: key, generation: state.generation, expiresAt: state.expiresAt})
	c.compactExpiryHeapLocked()
	return repository.QuotaRefreshVersion{Generation: state.generation, ExpiresAt: state.expiresAt}, nil
}

func (c *QuotaRefreshCoordinator) GetQuotaRefreshState(_ context.Context, accountID uint64, mode string) (repository.QuotaRefreshVersion, bool, error) {
	now := time.Now().UTC()
	key := quotaRefreshKey(accountID, strings.TrimSpace(mode))
	c.mu.Lock()
	defer c.mu.Unlock()
	state, ok := c.values[key]
	if !ok || !now.Before(state.expiresAt) {
		delete(c.values, key)
		delete(c.dirty, key)
		c.compactExpiryHeapLocked()
		return repository.QuotaRefreshVersion{}, false, nil
	}
	return repository.QuotaRefreshVersion{Generation: state.generation, ExpiresAt: state.expiresAt}, state.dirty, nil
}

func (c *QuotaRefreshCoordinator) ClearQuotaRefreshDirty(_ context.Context, accountID uint64, mode string, version repository.QuotaRefreshVersion) (bool, error) {
	key := quotaRefreshKey(accountID, strings.TrimSpace(mode))
	now := time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	state, ok := c.values[key]
	if !ok || !now.Before(state.expiresAt) {
		delete(c.values, key)
		delete(c.dirty, key)
		c.compactExpiryHeapLocked()
		return false, nil
	}
	if state.generation != version.Generation || !state.expiresAt.Equal(version.ExpiresAt) || !state.dirty {
		return false, nil
	}
	state.dirty = false
	c.values[key] = state
	delete(c.dirty, key)
	return true, nil
}

func (c *QuotaRefreshCoordinator) ScanQuotaRefreshDirty(_ context.Context, now time.Time, cursor uint64, limit int) ([]repository.QuotaRefreshDirty, uint64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	values := make([]repository.QuotaRefreshDirty, 0, min(limit, len(c.dirty)))
	for key := range c.dirty {
		state, ok := c.values[key]
		if !ok || !state.dirty || !now.Before(state.expiresAt) {
			delete(c.dirty, key)
			if ok && !now.Before(state.expiresAt) {
				delete(c.values, key)
			}
			continue
		}
		values = append(values, repository.QuotaRefreshDirty{AccountID: state.accountID, Mode: state.mode, Version: repository.QuotaRefreshVersion{Generation: state.generation, ExpiresAt: state.expiresAt}})
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].AccountID != values[j].AccountID {
			return values[i].AccountID < values[j].AccountID
		}
		return values[i].Mode < values[j].Mode
	})
	c.compactExpiryHeapLocked()
	if cursor >= uint64(len(values)) {
		return nil, 0, nil
	}
	end := min(cursor+uint64(limit), uint64(len(values)))
	page := values[cursor:end]
	if end == uint64(len(values)) {
		end = 0
	}
	return page, end, nil
}

func (c *QuotaRefreshCoordinator) pruneLocked(now time.Time) {
	for c.expires.Len() > 0 && !now.Before(c.expires[0].expiresAt) {
		expired := heap.Pop(&c.expires).(quotaRefreshExpiry)
		state, ok := c.values[expired.key]
		if !ok || state.generation != expired.generation || !state.expiresAt.Equal(expired.expiresAt) {
			continue
		}
		delete(c.values, expired.key)
		delete(c.dirty, expired.key)
	}
}

// compactExpiryHeapLocked bounds stale generation entries without adding a
// second index. Rebuilding is amortized and retains exactly one expiry per key.
func (c *QuotaRefreshCoordinator) compactExpiryHeapLocked() {
	if len(c.expires) <= len(c.values)*2+quotaRefreshExpiryCompactionMinStale {
		return
	}
	values := make(quotaRefreshExpiryHeap, 0, len(c.values))
	for key, state := range c.values {
		values = append(values, quotaRefreshExpiry{key: key, generation: state.generation, expiresAt: state.expiresAt})
	}
	heap.Init(&values)
	c.expires = values
}

func quotaRefreshKey(accountID uint64, mode string) string {
	return fmt.Sprintf("%d:%s", accountID, mode)
}
