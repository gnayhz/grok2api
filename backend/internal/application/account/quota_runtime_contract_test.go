package account

import (
	"context"
	"fmt"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"os"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/repository"
	redisclient "github.com/redis/go-redis/v9"
)

type quotaRuntimePair struct {
	first, second repository.QuotaRefreshCoordinator
	lock          repository.DistributedLock
}

func quotaTestStores(t *testing.T) map[string]quotaRuntimePair {
	t.Helper()
	mem := memory.NewQuotaRefreshCoordinator()
	stores := map[string]quotaRuntimePair{"memory": {mem, mem, memory.NewLockStore()}}
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		return stores
	}
	prefix := fmt.Sprintf("g27-contract-%d:", time.Now().UnixNano())
	cfg := redisruntime.Config{Address: address, Database: 15, KeyPrefix: prefix}
	first, err := redisruntime.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := redisruntime.Open(context.Background(), cfg)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	stores["redis"] = quotaRuntimePair{first, second, redisruntime.NewLockStore(first)}
	t.Cleanup(func() {
		defer first.Close()
		defer second.Close()
		client := redisclient.NewClient(&redisclient.Options{Addr: address, DB: 15})
		defer client.Close()
		var cursor uint64
		for {
			keys, next, err := client.Scan(context.Background(), cursor, prefix+"*", 100).Result()
			if err != nil {
				t.Error(err)
				return
			}
			if len(keys) > 0 {
				if err := client.Del(context.Background(), keys...).Err(); err != nil {
					t.Error(err)
				}
			}
			cursor = next
			if cursor == 0 {
				return
			}
		}
	})
	return stores
}

func TestQuotaRuntimeRetainedVersionAndScan(t *testing.T) {
	for name, pair := range quotaTestStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			first, err := pair.first.MarkQuotaRefreshDirty(ctx, 1, "fast", 20*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(30 * time.Millisecond)
			second, err := pair.second.MarkQuotaRefreshDirty(ctx, 1, "fast", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if first.Generation != second.Generation || first.Equal(second) {
				t.Fatalf("expiry identity: first=%+v second=%+v", first, second)
			}
			if cleared, err := pair.first.ClearQuotaRefreshDirty(ctx, 1, "fast", first); err != nil || cleared {
				t.Fatalf("expired confirmation cleared new demand: %v %v", cleared, err)
			}
			if version, dirty, err := pair.first.GetQuotaRefreshState(ctx, 1, "fast"); err != nil || !dirty || !version.Equal(second) {
				t.Fatalf("cross-instance current=%+v dirty=%v err=%v", version, dirty, err)
			}
			if cleared, err := pair.second.ClearQuotaRefreshDirty(ctx, 1, "fast", second); err != nil || !cleared {
				t.Fatalf("current confirmation: %v %v", cleared, err)
			}
			for id := uint64(2); id <= 121; id++ {
				if _, err := pair.first.MarkQuotaRefreshDirty(ctx, id, "fast", time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			seen := map[uint64]bool{}
			var cursor uint64
			for page := 0; page < 10; page++ {
				values, next, err := pair.second.ScanQuotaRefreshDirty(ctx, time.Now(), cursor, 17)
				if err != nil {
					t.Fatal(err)
				}
				if len(values) > 17 {
					t.Fatalf("unbounded page: %d", len(values))
				}
				for _, v := range values {
					if v.AccountID == 1 || v.Version.Generation == 0 || v.Version.ExpiresAt.IsZero() {
						t.Fatalf("invalid dirty member: %+v", v)
					}
					seen[v.AccountID] = true
				}
				cursor = next
				if cursor == 0 {
					break
				}
			}
			if cursor != 0 || len(seen) != 120 {
				t.Fatalf("scan progress: cursor=%d seen=%d", cursor, len(seen))
			}
		})
	}
}

func TestQuotaRecoverySkipsParkedPagesAndRetainsQueueOverflow(t *testing.T) {
	for name, pair := range quotaTestStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			for id := uint64(1); id <= 120; id++ {
				if _, err := pair.second.MarkQuotaRefreshDirty(ctx, id, "fast", time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			service := NewService(nil, nil, nil, nil, nil, nil, security.RandomTokenSource{}, nil, nil, nil)
			service.SetQuotaRefreshCoordinator(pair.first)
			service.quotaRefresh.queue = make(chan quotaRefreshRequest, 1)
			service.recoverSharedQuotaRefreshes(ctx, now)
			if len(service.quotaRefresh.obs) != 100 || len(service.quotaRefresh.queue) != 1 {
				t.Fatalf("first full page not retained: states=%d queue=%d", len(service.quotaRefresh.obs), len(service.quotaRefresh.queue))
			}
			<-service.quotaRefresh.queue
			for _, state := range service.quotaRefresh.obs {
				state.queued = false
				state.failures = quotaRefreshFailureBudget
			}
			service.requeueQuotaRefreshes()
			service.recoverSharedQuotaRefreshes(ctx, now)
			if len(service.quotaRefresh.obs) != 120 || len(service.quotaRefresh.queue) != 1 {
				t.Fatalf("parked page blocked later work: states=%d queue=%d", len(service.quotaRefresh.obs), len(service.quotaRefresh.queue))
			}
			first := <-service.quotaRefresh.queue
			state := service.quotaRefresh.obs[first.key]
			state.queued = false
			state.failures = quotaRefreshFailureBudget
			service.requeueQuotaRefreshes()
			if len(service.quotaRefresh.queue) != 1 {
				t.Fatal("overflow demand was not recovered")
			}
		})
	}
}
