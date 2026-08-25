package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	maxEntries        = 10000
	maxDeviceSessions = 1000
	shardCount        = 64
)

type rateWindow struct {
	startedAt time.Time
	count     int
}

// RateLimiter 提供单实例固定分钟窗口限流。
type RateLimiter struct {
	shards [shardCount]rateShard
}

type rateShard struct {
	mu      sync.Mutex
	windows map[string]rateWindow
}

func NewRateLimiter() *RateLimiter {
	limiter := &RateLimiter{}
	for index := range limiter.shards {
		limiter.shards[index].windows = make(map[string]rateWindow)
	}
	return limiter
}

func (r *RateLimiter) Allow(_ context.Context, key string, limit int, now time.Time) (bool, time.Duration, error) {
	if limit <= 0 {
		return true, 0, nil
	}
	shard := &r.shards[shardIndex(key)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	window := shard.windows[key]
	if window.startedAt.IsZero() || now.Sub(window.startedAt) >= time.Minute {
		window = rateWindow{startedAt: now, count: 0}
	}
	if window.count >= limit {
		shard.windows[key] = window
		remaining := time.Minute - now.Sub(window.startedAt)
		if remaining < time.Second {
			remaining = time.Second
		}
		return false, remaining, nil
	}
	window.count++
	shard.windows[key] = window
	if len(shard.windows) > maxEntriesPerShard() {
		cleanupRateShard(shard, now)
	}
	return true, 0, nil
}

func cleanupRateShard(shard *rateShard, now time.Time) {
	for key, window := range shard.windows {
		if now.Sub(window.startedAt) >= time.Minute {
			delete(shard.windows, key)
		}
	}
	for len(shard.windows) > maxEntriesPerShard() {
		var oldestKey string
		var oldest time.Time
		for key, window := range shard.windows {
			if oldestKey == "" || window.startedAt.Before(oldest) {
				oldestKey = key
				oldest = window.startedAt
			}
		}
		delete(shard.windows, oldestKey)
	}
}

// ConcurrencyLimiter 提供单实例并发租约。
type ConcurrencyLimiter struct {
	shards       [shardCount]concurrencyShard
	indicesCache sync.Pool
}

type concurrencyShard struct {
	mu     sync.Mutex
	counts map[string]int
}

func NewConcurrencyLimiter() *ConcurrencyLimiter {
	limiter := &ConcurrencyLimiter{}
	for index := range limiter.shards {
		limiter.shards[index].counts = make(map[string]int)
	}
	// 池存 *[]int（指针）而非 []int：Put 一个切片会把 24 字节 slice header
	// 装箱进 any 接口，每次归还都产生一次堆分配；指针可直接放入接口字，
	// 归还零分配（sync.Pool 文档建议的形态）。
	limiter.indicesCache.New = func() any {
		buffer := make([]int, 0, 256)
		return &buffer
	}
	return limiter
}

func (l *ConcurrencyLimiter) Acquire(_ context.Context, key string, limit int) (func(), bool, error) {
	if limit <= 0 {
		return func() {}, true, nil
	}
	shard := &l.shards[shardIndex(key)]
	shard.mu.Lock()
	if shard.counts[key] >= limit {
		shard.mu.Unlock()
		return nil, false, nil
	}
	shard.counts[key]++
	shard.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			shard.mu.Lock()
			defer shard.mu.Unlock()
			shard.counts[key]--
			if shard.counts[key] <= 0 {
				delete(shard.counts, key)
			}
		})
	}, true, nil
}

func (l *ConcurrencyLimiter) Current(_ context.Context, key string) (int, error) {
	shard := &l.shards[shardIndex(key)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	return shard.counts[key], nil
}

func (l *ConcurrencyLimiter) CurrentMany(_ context.Context, keys []string) (map[string]int, error) {
	// Most accounts have no active lease. Keep the sparse result contract used
	// by the planner instead of allocating one map entry for every candidate.
	values := make(map[string]int)
	if len(keys) == 0 {
		return values, nil
	}

	// 选号会一次读取整个候选池。先按分片聚合下标，避免同一分片在一次快照中
	// 被反复加锁数千次，并缩短高并发请求之间的锁竞争窗口。
	var counts [shardCount]int
	for _, key := range keys {
		counts[shardIndex(key)]++
	}
	var offsets [shardCount + 1]int
	for index := range shardCount {
		offsets[index+1] = offsets[index] + counts[index]
	}
	cursors := offsets
	groupedPtr, _ := l.indicesCache.Get().(*[]int)
	if groupedPtr == nil || cap(*groupedPtr) < len(keys) {
		replacement := make([]int, len(keys))
		groupedPtr = &replacement
	} else {
		*groupedPtr = (*groupedPtr)[:len(keys)]
	}
	grouped := *groupedPtr
	defer func() {
		// 不保留异常大的候选池缓冲，避免一次峰值长期占用进程内存。
		if cap(*groupedPtr) <= maxEntries {
			*groupedPtr = (*groupedPtr)[:0]
			l.indicesCache.Put(groupedPtr)
		}
	}()
	for keyIndex, key := range keys {
		shard := int(shardIndex(key))
		grouped[cursors[shard]] = keyIndex
		cursors[shard]++
	}
	for shardIndex := range shardCount {
		if offsets[shardIndex] == offsets[shardIndex+1] {
			continue
		}
		shard := &l.shards[shardIndex]
		shard.mu.Lock()
		for _, keyIndex := range grouped[offsets[shardIndex]:offsets[shardIndex+1]] {
			key := keys[keyIndex]
			if count := shard.counts[key]; count > 0 {
				values[key] = count
			}
		}
		shard.mu.Unlock()
	}
	return values, nil
}

type stickyBinding struct {
	accountID uint64
	expiresAt time.Time
}

// StickyStore 提供有界的单实例会话粘滞状态。
type StickyStore struct {
	shards [shardCount]stickyShard
}

type stickyShard struct {
	mu       sync.Mutex
	bindings map[string]stickyBinding
}

func NewStickyStore() *StickyStore {
	store := &StickyStore{}
	for index := range store.shards {
		store.shards[index].bindings = make(map[string]stickyBinding)
	}
	return store
}

func (s *StickyStore) Get(_ context.Context, affinityKey string, now time.Time) (uint64, bool, error) {
	shard := &s.shards[shardIndex(affinityKey)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	binding, ok := shard.bindings[affinityKey]
	if !ok {
		return 0, false, nil
	}
	if !now.Before(binding.expiresAt) {
		delete(shard.bindings, affinityKey)
		return 0, false, nil
	}
	return binding.accountID, true, nil
}

func (s *StickyStore) Bind(_ context.Context, affinityKey string, proposedAccountID uint64, now, expiresAt time.Time) (uint64, error) {
	if affinityKey == "" || proposedAccountID == 0 || !now.Before(expiresAt) {
		return proposedAccountID, nil
	}
	shard := &s.shards[shardIndex(affinityKey)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if binding, ok := shard.bindings[affinityKey]; ok && now.Before(binding.expiresAt) {
		binding.expiresAt = expiresAt
		shard.bindings[affinityKey] = binding
		return binding.accountID, nil
	}
	shard.bindings[affinityKey] = stickyBinding{accountID: proposedAccountID, expiresAt: expiresAt}
	pruneStickyBindingsLocked(shard, now)
	return proposedAccountID, nil
}

func (s *StickyStore) Set(_ context.Context, affinityKey string, accountID uint64, expiresAt time.Time) error {
	if affinityKey == "" {
		return nil
	}
	shard := &s.shards[shardIndex(affinityKey)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	shard.bindings[affinityKey] = stickyBinding{accountID: accountID, expiresAt: expiresAt}
	pruneStickyBindingsLocked(shard, time.Now())
	return nil
}

func pruneStickyBindingsLocked(shard *stickyShard, now time.Time) {
	if len(shard.bindings) > maxEntriesPerShard() {
		for key, binding := range shard.bindings {
			if !now.Before(binding.expiresAt) {
				delete(shard.bindings, key)
			}
		}
		for len(shard.bindings) > maxEntriesPerShard() {
			var oldestKey string
			var oldest time.Time
			for key, binding := range shard.bindings {
				if oldestKey == "" || binding.expiresAt.Before(oldest) {
					oldestKey = key
					oldest = binding.expiresAt
				}
			}
			delete(shard.bindings, oldestKey)
		}
	}
}

func (s *StickyStore) DeleteByAccount(_ context.Context, accountID uint64) error {
	for index := range s.shards {
		shard := &s.shards[index]
		shard.mu.Lock()
		for key, binding := range shard.bindings {
			if binding.accountID == accountID {
				delete(shard.bindings, key)
			}
		}
		shard.mu.Unlock()
	}
	return nil
}

func (s *StickyStore) DeleteByAccounts(_ context.Context, accountIDs []uint64) error {
	ids := make(map[uint64]struct{}, len(accountIDs))
	for _, accountID := range accountIDs {
		if accountID != 0 {
			ids[accountID] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	for index := range s.shards {
		shard := &s.shards[index]
		shard.mu.Lock()
		for key, binding := range shard.bindings {
			if _, remove := ids[binding.accountID]; remove {
				delete(shard.bindings, key)
			}
		}
		shard.mu.Unlock()
	}
	return nil
}

func shardIndex(key string) uint32 {
	const offset32 = uint32(2166136261)
	const prime32 = uint32(16777619)
	hash := offset32
	for index := 0; index < len(key); index++ {
		hash ^= uint32(key[index])
		hash *= prime32
	}
	return hash % shardCount
}

func maxEntriesPerShard() int { return (maxEntries + shardCount - 1) / shardCount }

// DeviceSessionStore 保存不会跨重启恢复的短期 OAuth 会话。
type DeviceSessionStore struct {
	mu       sync.Mutex
	sessions map[string]account.DeviceSession
}

func NewDeviceSessionStore() *DeviceSessionStore {
	return &DeviceSessionStore{sessions: make(map[string]account.DeviceSession)}
}

func (s *DeviceSessionStore) Create(_ context.Context, value account.DeviceSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for id, session := range s.sessions {
		if !now.Before(session.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
	if _, exists := s.sessions[value.ID]; !exists && len(s.sessions) >= maxDeviceSessions {
		var earliestID string
		var earliestExpiry time.Time
		for id, session := range s.sessions {
			if earliestID == "" || session.ExpiresAt.Before(earliestExpiry) {
				earliestID = id
				earliestExpiry = session.ExpiresAt
			}
		}
		delete(s.sessions, earliestID)
	}
	s.sessions[value.ID] = value
	return nil
}

func (s *DeviceSessionStore) Get(_ context.Context, id string, now time.Time) (account.DeviceSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[id]
	if !ok || !now.Before(value.ExpiresAt) {
		delete(s.sessions, id)
		return account.DeviceSession{}, repository.ErrNotFound
	}
	return value, nil
}

func (s *DeviceSessionStore) Update(_ context.Context, value account.DeviceSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[value.ID]; !ok {
		return repository.ErrNotFound
	}
	s.sessions[value.ID] = value
	return nil
}

func (s *DeviceSessionStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}

// LockStore 提供单实例非阻塞短期锁。
type LockStore struct {
	mu    sync.Mutex
	locks map[string]lockEntry
}

// lockEntry 记录锁的持有令牌与到期时间。TTL 与 redis 实现对齐:持锁方
// panic/丢弃 release 闭包时, 锁在 TTL 后自愈(此前永不过期, 维护任务会
// 永久卡死直到重启进程)。
type lockEntry struct {
	token     string
	expiresAt time.Time // 零值=不过期(兼容显式传 0 的调用方)
}

func NewLockStore() *LockStore { return &LockStore{locks: make(map[string]lockEntry)} }

func (s *LockStore) Acquire(_ context.Context, key string, ttl time.Duration) (func(), bool, error) {
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, false, err
	}
	token := hex.EncodeToString(tokenBytes)
	now := time.Now().UTC()
	s.mu.Lock()
	if entry, exists := s.locks[key]; exists {
		if entry.expiresAt.IsZero() || now.Before(entry.expiresAt) {
			s.mu.Unlock()
			return nil, false, nil
		}
		// 过期锁:惰性回收后允许重新获取。
		delete(s.locks, key)
	}
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = now.Add(ttl)
	}
	s.locks[key] = lockEntry{token: token, expiresAt: expiresAt}
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if entry, exists := s.locks[key]; exists && entry.token == token {
				delete(s.locks, key)
			}
		})
	}, true, nil
}
