package gateway

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/pkg/resultcache"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Both versions use identical stores/keys. A long TTL isolates a hit, and a
// nanosecond TTL isolates a miss; production retains its 25ms policy.
func BenchmarkSelectorCapacitySnapshots(b *testing.B) {
	for _, backend := range []string{"memory", "redis"} {
		b.Run(backend, func(b *testing.B) {
			var limiter repository.ConcurrencyLimiter = memory.NewConcurrencyLimiter()
			if backend == "redis" {
				address := os.Getenv("TEST_REDIS_ADDRESS")
				if address == "" {
					b.Skip("isolated TEST_REDIS_ADDRESS required")
				}
				store, err := redisruntime.Open(context.Background(), redisruntime.Config{Address: address, KeyPrefix: fmt.Sprintf("g18-bench-%d:", time.Now().UnixNano()), ConcurrencyLease: time.Minute})
				if err != nil {
					b.Fatal(err)
				}
				defer store.Close()
				limiter = redisruntime.NewConcurrencyLimiter(store)
			}
			for _, count := range []int{300, 3000} {
				for _, hit := range []bool{false, true} {
					b.Run(fmt.Sprintf("%d/hit=%t", count, hit), func(b *testing.B) {
						keys := make([]string, count)
						for i := range keys {
							keys[i] = repository.AccountConcurrencyKey(uint64(i + 1))
						}
						selector := NewSelector(nil, limiter, nil, nil, time.Hour, time.Second, time.Minute)
						ttl := time.Nanosecond
						if hit {
							ttl = time.Hour
						}
						selector.concurrencySnapshots = resultcache.New[[32]byte, map[string]int](maxConcurrencySnapshots, ttl)
						ctx := context.Background()
						if _, err := selector.loadConcurrencySnapshot(ctx, keys); err != nil {
							b.Fatal(err)
						}
						b.ReportAllocs()
						b.ResetTimer()
						for b.Loop() {
							if _, err := selector.loadConcurrencySnapshot(ctx, keys); err != nil {
								b.Fatal(err)
							}
						}
					})
				}
			}
		})
	}
}
