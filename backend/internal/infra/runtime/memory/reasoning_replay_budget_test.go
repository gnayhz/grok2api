package memory

import (
	"context"
	"testing"
	"time"
)

// TestReasoningReplayStoreByteBudgetEvictsOldest 锁定字节维度预算:
// 条数上限只约束条数,单条回放可达数 MiB——只按条数限界时最坏内存无界。
// 预算内新写入必须按 storedAt 淘汰最旧条目回到预算内。
func TestReasoningReplayStoreByteBudgetEvictsOldest(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	item := make([]byte, 96<<10)
	// 预算 256KiB:单条 96KiB+开销≈96.1KiB,两条≈192.3KiB 在预算内,
	// 第三条(≈288.5KiB)必然超预算并淘汰最旧条目。
	budget := int64(256 << 10)
	store := NewReasoningReplayStoreWithBudget(10240, budget)

	store.Set(ctx, "grok-4.5", "session-a", [][]byte{item}, now.Add(time.Hour))
	time.Sleep(2 * time.Millisecond)
	store.Set(ctx, "grok-4.5", "session-b", [][]byte{item}, now.Add(time.Hour))
	time.Sleep(2 * time.Millisecond)
	store.Set(ctx, "grok-4.5", "session-c", [][]byte{item}, now.Add(time.Hour))

	store.mu.Lock()
	total := store.totalBytes
	count := len(store.values)
	store.mu.Unlock()
	if total > budget {
		t.Fatalf("totalBytes %d exceeds budget %d", total, budget)
	}
	if count != 2 {
		t.Fatalf("entries = %d, want 2 (oldest evicted by byte budget)", count)
	}
	if _, ok, _ := store.Get(ctx, "grok-4.5", "session-a", now.Add(time.Minute), time.Hour); ok {
		t.Fatal("oldest entry must be evicted first")
	}
	for _, key := range []string{"session-b", "session-c"} {
		if items, ok, _ := store.Get(ctx, "grok-4.5", key, now.Add(time.Minute), time.Hour); !ok || len(items) != 1 {
			t.Fatalf("entry %s missing after budget eviction", key)
		}
	}
}

// TestReasoningReplayStoreReplaceAccountsBytes 锁定同键替换的字节记账:
// 替换更小的条目后,预算占用必须回落(记账泄漏会让预算永久紧缩)。
func TestReasoningReplayStoreReplaceAccountsBytes(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store := NewReasoningReplayStoreWithBudget(10240, 512<<10)
	big := make([]byte, 200<<10)
	small := make([]byte, 4<<10)
	store.Set(ctx, "grok-4.5", "session-a", [][]byte{big}, now.Add(time.Hour))
	store.Set(ctx, "grok-4.5", "session-a", [][]byte{small}, now.Add(time.Hour))
	store.mu.Lock()
	total := store.totalBytes
	store.mu.Unlock()
	want := reasoningReplayEntryOverheadBytes + int64(len(small)) + 16
	if total != want {
		t.Fatalf("totalBytes = %d after replace, want %d", total, want)
	}
}

// TestReasoningReplayStoreGetCloneIsolation 锁定返回值隔离:调用方修改
// 返回切片不得影响存储内的条目(锁外拷贝路径的可见性契约)。
func TestReasoningReplayStoreGetCloneIsolation(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store := NewReasoningReplayStore(16)
	store.Set(ctx, "grok-4.5", "session-a", [][]byte{[]byte("replay-item")}, now.Add(time.Hour))
	first, ok, _ := store.Get(ctx, "grok-4.5", "session-a", now.Add(time.Minute), time.Hour)
	if !ok || len(first) != 1 || string(first[0]) != "replay-item" {
		t.Fatalf("first get = %#v ok=%v", first, ok)
	}
	first[0][0] = 'X'
	second, _, _ := store.Get(ctx, "grok-4.5", "session-a", now.Add(2*time.Minute), time.Hour)
	if string(second[0]) != "replay-item" {
		t.Fatalf("stored entry mutated through returned slice: %q", second[0])
	}
}

// TestReasoningReplayStoreConcurrentSetGetSmoke 并发烟测:锁外拷贝改造后
// 并发 Set/Get 不得出现数据竞争或计数漂移(go test -race 下验证)。
func TestReasoningReplayStoreConcurrentSetGetSmoke(t *testing.T) {
	ctx := context.Background()
	store := NewReasoningReplayStoreWithBudget(64, 1<<20)
	done := make(chan struct{})
	for worker := 0; worker < 4; worker++ {
		go func(worker int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 200; i++ {
				key := string(rune('a' + worker))
				store.Set(ctx, "grok-4.5", key, [][]byte{make([]byte, 8<<10)}, time.Now().Add(time.Hour))
				store.Get(ctx, "grok-4.5", key, time.Now(), time.Hour)
			}
		}(worker)
	}
	for worker := 0; worker < 4; worker++ {
		<-done
	}
	store.mu.Lock()
	total := store.totalBytes
	store.mu.Unlock()
	if total < 0 || total > 1<<20 {
		t.Fatalf("totalBytes drifted out of bounds: %d", total)
	}
}
