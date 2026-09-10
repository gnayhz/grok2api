package redis

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func TestRedisReplayAtomicUpdateIntegration(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	prefix := fmt.Sprintf("replay-atomic-test:%d:", time.Now().UnixNano())
	a, err := Open(ctx, Config{Address: address, KeyPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, Config{Address: address, KeyPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	defer cleanupRedisTestPrefix(t, context.Background(), a.client, prefix)
	stores := []*ReasoningReplayStore{NewReasoningReplayStore(a), NewReasoningReplayStore(b)}
	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs <- stores[i%2].Update(ctx, "model", "shared", time.Now().Add(time.Minute), func(old [][]byte) [][]byte { return append(old, []byte(fmt.Sprintf("turn-%d", i))) })
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	items, ok, err := stores[0].Get(ctx, "model", "shared", time.Now(), time.Minute)
	if err != nil || !ok || len(items) != workers {
		t.Fatalf("lost updates: count=%d ok=%v err=%v", len(items), ok, err)
	}
	seen := map[string]bool{}
	for _, item := range items {
		seen[string(item)] = true
	}
	if len(seen) != workers {
		t.Fatal("duplicate or missing turns")
	}
	t.Run("corrupt_read_preserves_value", func(t *testing.T) {
		key := stores[0].redisKey("model", "corrupt")
		if err := a.client.Set(ctx, key, "broken-json", time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
		called := false
		err := stores[0].Update(ctx, "model", "corrupt", time.Now().Add(time.Minute), func(old [][]byte) [][]byte { called = true; return [][]byte{[]byte("replacement")} })
		if err == nil || called {
			t.Fatal("decode error was treated as empty history")
		}
		got, err := a.client.Get(ctx, key).Result()
		if err != nil || got != "broken-json" {
			t.Fatal("failed read overwrote existing state")
		}
	})
	t.Run("expired_value_does_not_reappear", func(t *testing.T) {
		key := stores[0].redisKey("model", "expired")
		if err := a.client.Set(ctx, key, `["b2xk"]`, time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
		if err := a.client.ExpireAt(ctx, key, time.Now().Add(-time.Second)).Err(); err != nil {
			t.Fatal(err)
		}
		if err := stores[1].Update(ctx, "model", "expired", time.Now().Add(time.Minute), func(old [][]byte) [][]byte {
			if len(old) != 0 {
				t.Error("expired history supplied to merge")
			}
			return [][]byte{[]byte("new")}
		}); err != nil {
			t.Fatal(err)
		}
		items, ok, err := stores[0].Get(ctx, "model", "expired", time.Now(), time.Minute)
		if err != nil || !ok || len(items) != 1 || string(items[0]) != "new" {
			t.Fatal("expired history resurrected")
		}
	})
}
