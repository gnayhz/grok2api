package redis

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRollingRateIsAtomicAcrossClientsAndExpires(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("requires isolated TEST_REDIS_ADDRESS")
	}
	ctx := context.Background()
	cfg := Config{Address: address, KeyPrefix: "maturity:rolling:" + time.Now().Format("150405.000000000") + ":"}
	a, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := a
			if i%2 == 0 {
				s = b
			}
			ok, _, err := s.AllowRolling(ctx, "global", 2, 500*time.Millisecond)
			if err != nil {
				t.Error(err)
			}
			if ok {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 2 {
		t.Fatalf("admitted %d, want 2", accepted.Load())
	}
	if ok, wait, err := b.AllowRolling(ctx, "global", 2, 500*time.Millisecond); err != nil || ok || wait <= 0 {
		t.Fatalf("exhausted window: %v %v %v", ok, wait, err)
	}
	time.Sleep(550 * time.Millisecond)
	if ok, _, err := b.AllowRolling(ctx, "global", 2, 500*time.Millisecond); err != nil || !ok {
		t.Fatalf("expired window: %v %v", ok, err)
	}
}
