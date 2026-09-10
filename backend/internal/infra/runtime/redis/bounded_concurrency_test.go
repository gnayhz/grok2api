package redis

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestBoundedLeaseSharesCapacityWithoutShorteningProductionLease(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS requires an isolated Redis instance")
	}
	ctx := context.Background()
	store, err := Open(ctx, Config{Address: address, KeyPrefix: "maturity:leases:" + time.Now().Format("150405.000000") + ":", ConcurrencyLease: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	limiter := NewConcurrencyLimiter(store)
	production, ok, err := limiter.Acquire(ctx, "account:1", 2)
	if err != nil || !ok {
		t.Fatalf("production: %v %v", ok, err)
	}
	defer production()
	oldProbe, ok, err := limiter.AcquireBounded(ctx, "account:1", 2, 150*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("probe: %v %v", ok, err)
	}
	defer oldProbe()
	if ttl, err := store.client.PTTL(ctx, store.key("concurrency", "account:1")).Result(); err != nil || ttl < 2*time.Hour {
		t.Fatalf("short probe truncated shared key: %v %v", ttl, err)
	}
	if release, ok, err := limiter.Acquire(ctx, "account:1", 2); err != nil || ok {
		if release != nil {
			release()
		}
		t.Fatalf("shared capacity oversold: %v %v", ok, err)
	}
	time.Sleep(200 * time.Millisecond)
	newProbe, ok, err := limiter.AcquireBounded(ctx, "account:1", 2, time.Second)
	if err != nil || !ok {
		t.Fatalf("lost probe capacity not recovered: %v %v", ok, err)
	}
	defer newProbe()
	oldProbe()
	if count, err := limiter.Current(ctx, "account:1"); err != nil || count != 2 {
		t.Fatalf("old owner released new work or production: %d %v", count, err)
	}
	newProbe()
	if count, err := limiter.Current(ctx, "account:1"); err != nil || count != 1 {
		t.Fatalf("production lost: %d %v", count, err)
	}
}
