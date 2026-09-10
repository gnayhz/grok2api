package history

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// Force all legacy readers to observe the same snapshot before any writer runs.
// The atomic repository update must bypass this split Get/Set path entirely.
type synchronizedReplayReads struct {
	*memory.ReasoningReplayStore
	readers atomic.Int32
	ready   chan struct{}
	count   int32
}

func (s *synchronizedReplayReads) Get(ctx context.Context, model, key string, now time.Time, ttl time.Duration) ([][]byte, bool, error) {
	items, ok, err := s.ReasoningReplayStore.Get(ctx, model, key, now, ttl)
	if s.readers.Add(1) == s.count {
		close(s.ready)
	}
	select {
	case <-s.ready:
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
	return items, ok, err
}
func TestConcurrentCompletionsPreserveEveryReplayTurn(t *testing.T) {
	const n = 16
	base := memory.NewReasoningReplayStore(4)
	store := &synchronizedReplayReads{ReasoningReplayStore: base, ready: make(chan struct{}), count: n}
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := []byte(fmt.Sprintf(`{"output":[{"type":"reasoning","encrypted_content":"%s"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer %d"}]}]}`, validEncrypted(byte(i+1)), i))
			replay.StoreFromCompleted(ctx, "grok-4.6", "concurrent", payload)
		}(i)
	}
	wg.Wait()
	items, _, err := base.Get(ctx, "grok-4.6", "concurrent", time.Now(), time.Hour)
	if err != nil || len(items) != n*2 {
		t.Fatalf("concurrent completion loss: got %d items, want %d; err=%v", len(items), n*2, err)
	}
}

func TestReplayStoreEvictionRemovesServerRestorationButNotClientHistory(t *testing.T) {
	ctx := context.Background()
	store := memory.NewReasoningReplayStore(1)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, nil)
	cipher := validEncrypted(50)
	payload := []byte(fmt.Sprintf(`{"output":[{"type":"reasoning","encrypted_content":"%s"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`, cipher))
	replay.StoreFromCompleted(ctx, "m", "main", payload)
	body := []byte(`{"input":[{"type":"message","role":"user","content":"question"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"type":"message","role":"user","content":"next"}]}`)
	restored := replay.Apply(ctx, "m", "main", body)
	if string(restored) == string(body) {
		t.Fatal("setup did not restore reasoning")
	}
	replay.StoreFromCompleted(ctx, "m", "other", payload)
	if got := replay.Apply(ctx, "m", "main", body); string(got) != string(body) {
		t.Fatal("evicted main unexpectedly restored")
	}
	if got := replay.Apply(ctx, "m", "main", restored); string(got) != string(restored) {
		t.Fatal("client-provided reasoning was changed after eviction")
	}
}
