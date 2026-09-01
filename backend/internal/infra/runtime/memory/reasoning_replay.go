package memory

import (
	"context"
	"sort"
	"sync"
	"time"
)

type reasoningReplayEntry struct {
	items     [][]byte
	expiresAt time.Time
	storedAt  time.Time
}

// ReasoningReplayStore 提供单实例有界推理回放缓存。
type ReasoningReplayStore struct {
	mu         sync.Mutex
	maxSize    int
	maxBytes   int64
	totalBytes int64
	evictBatch int
	values     map[string]reasoningReplayEntry
	ttlSlide   bool
}

// defaultReasoningReplayMaxBytes 是字节维度的默认预算:条目上限只约束
// 条数,单条回放捕获可达数 MiB(见 reasoningreplay.maxReplayCaptureBytes),
// 只按条数限界时最坏内存无界。256MiB 覆盖典型会话规模,超预算按
// storedAt 淘汰最旧条目。
const defaultReasoningReplayMaxBytes int64 = 256 << 20

// reasoningReplayEntryOverheadBytes 估算单条目 map/结构体固定开销。
const reasoningReplayEntryOverheadBytes int64 = 128

// NewReasoningReplayStore 创建内存推理回放仓储；maxSize 为全局条目上限,
// 字节预算取默认值。
func NewReasoningReplayStore(maxSize int) *ReasoningReplayStore {
	return NewReasoningReplayStoreWithBudget(maxSize, 0)
}

// NewReasoningReplayStoreWithBudget 创建带显式字节预算的内存回放仓储;
// maxBytes <= 0 时使用默认预算。
func NewReasoningReplayStoreWithBudget(maxSize int, maxBytes int64) *ReasoningReplayStore {
	if maxSize < 1 {
		maxSize = 10240
	}
	if maxBytes <= 0 {
		maxBytes = defaultReasoningReplayMaxBytes
	}
	evictBatch := maxSize / 80
	if evictBatch < 1 {
		evictBatch = 1
	}
	if evictBatch > 128 {
		evictBatch = 128
	}
	return &ReasoningReplayStore{maxSize: maxSize, maxBytes: maxBytes, evictBatch: evictBatch, values: make(map[string]reasoningReplayEntry, maxSize), ttlSlide: true}
}

func reasoningReplayMapKey(model, sessionKey string) string {
	return model + "\x00" + sessionKey
}

func cloneReplayItems(items [][]byte) [][]byte {
	if len(items) == 0 {
		return nil
	}
	cloned := make([][]byte, 0, len(items))
	for _, item := range items {
		cloned = append(cloned, append([]byte(nil), item...))
	}
	return cloned
}

func (s *ReasoningReplayStore) Get(_ context.Context, model, sessionKey string, now time.Time, ttl time.Duration) ([][]byte, bool, error) {
	if model == "" || sessionKey == "" {
		return nil, false, nil
	}
	key := reasoningReplayMapKey(model, sessionKey)
	s.mu.Lock()
	entry, ok := s.values[key]
	if !ok {
		s.mu.Unlock()
		return nil, false, nil
	}
	if !now.Before(entry.expiresAt) {
		s.totalBytes -= reasoningReplayEntrySize(entry)
		delete(s.values, key)
		s.mu.Unlock()
		return nil, false, nil
	}
	if s.ttlSlide {
		if ttl <= 0 {
			ttl = entry.expiresAt.Sub(entry.storedAt)
			if ttl <= 0 {
				ttl = time.Hour
			}
		}
		entry.expiresAt = now.Add(ttl)
		entry.storedAt = now
		s.values[key] = entry
	}
	// 条目存入后不可变(Set 总是整体替换、从不在位修改),锁内取引用、
	// 锁外深拷贝安全——单条回放可达数 MiB,锁内拷贝会阻塞其他会话的读写。
	items := entry.items
	s.mu.Unlock()
	return cloneReplayItems(items), true, nil
}

func (s *ReasoningReplayStore) Set(_ context.Context, model, sessionKey string, items [][]byte, expiresAt time.Time) error {
	if model == "" || sessionKey == "" || len(items) == 0 || expiresAt.IsZero() {
		return nil
	}
	key := reasoningReplayMapKey(model, sessionKey)
	// 深拷贝在锁外完成:单条回放捕获可达数 MiB,锁内拷贝期间其他会话的
	// 回放读写全部被单一互斥量阻塞。
	entry := reasoningReplayEntry{items: cloneReplayItems(items), expiresAt: expiresAt, storedAt: time.Now()}
	now := entry.storedAt
	s.mu.Lock()
	if previous, exists := s.values[key]; exists {
		s.totalBytes -= reasoningReplayEntrySize(previous)
	} else {
		s.evictLocked(now)
	}
	s.values[key] = entry
	s.totalBytes += reasoningReplayEntrySize(entry)
	// 同键替换也可能放大字节占用:超预算时立即按最旧淘汰。
	if s.totalBytes > s.maxBytes {
		s.evictLocked(now)
	}
	s.mu.Unlock()
	return nil
}

func (s *ReasoningReplayStore) Delete(_ context.Context, model, sessionKey string) error {
	if model == "" || sessionKey == "" {
		return nil
	}
	key := reasoningReplayMapKey(model, sessionKey)
	s.mu.Lock()
	if entry, exists := s.values[key]; exists {
		s.totalBytes -= reasoningReplayEntrySize(entry)
		delete(s.values, key)
	}
	s.mu.Unlock()
	return nil
}

func (s *ReasoningReplayStore) evictLocked(now time.Time) {
	for key, entry := range s.values {
		if !now.Before(entry.expiresAt) {
			s.totalBytes -= reasoningReplayEntrySize(entry)
			delete(s.values, key)
		}
	}
	if len(s.values) < s.maxSize && s.totalBytes <= s.maxBytes {
		return
	}
	type candidate struct {
		key      string
		size     int64
		storedAt time.Time
	}
	candidates := make([]candidate, 0, len(s.values))
	for key, entry := range s.values {
		candidates = append(candidates, candidate{key: key, size: reasoningReplayEntrySize(entry), storedAt: entry.storedAt})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].storedAt.Before(candidates[j].storedAt) })
	// 条数压力沿用批次摊销(容量处一次性淘汰一批,避免每次 Set 都全量排序);
	// 字节压力精确淘汰到回到预算内——批次是条数维度的摊销下限,不得成为
	// 字节维度的强制淘汰下限;字节压力也不受批次上限约束。
	toEvict := 0
	if len(s.values) >= s.maxSize {
		toEvict = s.evictBatch
		if toEvict > len(candidates) {
			toEvict = len(candidates)
		}
	}
	for index := 0; index < len(candidates); index++ {
		if index >= toEvict && s.totalBytes <= s.maxBytes {
			break
		}
		s.totalBytes -= candidates[index].size
		delete(s.values, candidates[index].key)
	}
}

// reasoningReplayEntrySize 估算条目的常驻字节数(items 内容 + 每片 slice
// 头 + 条目固定开销)。仅用于预算记账,不要求精确到分配器级别。
func reasoningReplayEntrySize(entry reasoningReplayEntry) int64 {
	total := reasoningReplayEntryOverheadBytes
	for _, item := range entry.items {
		total += int64(len(item)) + 16
	}
	return total
}
