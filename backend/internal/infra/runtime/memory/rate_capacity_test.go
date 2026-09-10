package memory

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRateCapacityPreservesUnexpiredWindows(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	limiter := NewRateLimiter()
	victim, continuing := "limited-client", ""
	shard := shardIndex(victim)
	keys := []string{victim}
	for candidate := 0; len(keys) <= maxEntriesPerShard(); candidate++ {
		key := fmt.Sprintf("client-pressure-%d", candidate)
		if shardIndex(key) == shard {
			keys = append(keys, key)
		}
	}
	continuing = keys[1]
	for i, key := range keys[:maxEntriesPerShard()] {
		if allowed, _, err := limiter.Allow(ctx, key, 2, now.Add(time.Duration(i)*time.Microsecond)); err != nil || !allowed {
			t.Fatalf("initial %d: %t %v", i, allowed, err)
		}
	}
	if allowed, _, err := limiter.Allow(ctx, victim, 2, now.Add(time.Second)); err != nil || !allowed {
		t.Fatalf("existing capacity: %t %v", allowed, err)
	}
	overflow := keys[maxEntriesPerShard()]
	if allowed, _, err := limiter.Allow(ctx, overflow, 2, now.Add(2*time.Second)); err == nil || allowed {
		t.Errorf("new identity must report capacity exhaustion: %t %v", allowed, err)
	}
	if allowed, retry, err := limiter.Allow(ctx, victim, 2, now.Add(3*time.Second)); err != nil || allowed || retry != 57*time.Second {
		t.Errorf("live limit was lost: allowed=%t retry=%v err=%v", allowed, retry, err)
	}
	if allowed, _, err := limiter.Allow(ctx, continuing, 2, now.Add(3*time.Second)); err != nil || !allowed {
		t.Errorf("existing unsaturated window must still work: %t %v", allowed, err)
	}
	if allowed, _, err := limiter.Allow(ctx, overflow, 2, now.Add(time.Minute+time.Second)); err != nil || !allowed {
		t.Fatalf("expired capacity did not recover: %t %v", allowed, err)
	}
	if allowed, _, err := limiter.Allow(ctx, victim, 2, now.Add(time.Minute+time.Second)); err != nil || !allowed {
		t.Fatalf("expired old identity did not recover: %t %v", allowed, err)
	}
	if len(limiter.shards[shard].windows) > maxEntriesPerShard() {
		t.Fatal("rate storage exceeded capacity")
	}
}

func BenchmarkRateExistingIdentity(b *testing.B) {
	for _, limited := range []bool{false, true} {
		b.Run(fmt.Sprintf("limited=%t", limited), func(b *testing.B) {
			limiter := NewRateLimiter()
			ctx, now := context.Background(), time.Now()
			limit := int(^uint(0) >> 1)
			if limited {
				limit = 1
			}
			_, _, _ = limiter.Allow(ctx, "client", limit, now)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				allowed, _, err := limiter.Allow(ctx, "client", limit, now)
				if err != nil || allowed == limited {
					b.Fatalf("allow=%t err=%v", allowed, err)
				}
			}
		})
	}
}
