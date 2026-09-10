package account

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	redisclient "github.com/redis/go-redis/v9"
)

type exhaustedRetryAdapter struct{ quotaCountingAdapter }

func (a *exhaustedRetryAdapter) SyncQuotaMode(context.Context, accountdomain.Credential, string) (accountdomain.QuotaWindow, error) {
	a.modeCalls.Add(1)
	return accountdomain.QuotaWindow{}, errors.New("quota endpoint unavailable")
}

func TestSharedQuotaRetryBudget(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS required")
	}
	for _, durable := range []bool{false, true} {
		t.Run(fmt.Sprint("durable=", durable), func(t *testing.T) {
			ctx := context.Background()
			prefix := fmt.Sprintf("g27-budget-%d:", time.Now().UnixNano())
			cfg := redisruntime.Config{Address: address, Database: 15, KeyPrefix: prefix, ConcurrencyLease: time.Minute}
			runtime, err := redisruntime.Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			cleanup := redisclient.NewClient(&redisclient.Options{Addr: address, DB: 15})
			defer cleanup.Close()
			defer func() {
				var cursor uint64
				for {
					keys, next, err := cleanup.Scan(ctx, cursor, prefix+"*", 100).Result()
					if err != nil {
						t.Error(err)
						return
					}
					if len(keys) > 0 {
						if err := cleanup.Del(ctx, keys...).Err(); err != nil {
							t.Error(err)
						}
					}
					cursor = next
					if cursor == 0 {
						return
					}
				}
			}()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "retry.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			v, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, SourceKey: "retry", Name: "retry", EncryptedAccessToken: "synthetic", AuthStatus: accountdomain.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			adapter := &exhaustedRetryAdapter{}
			service := NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), nil, redisruntime.NewLockStore(runtime))
			service.SetQuotaRefreshCoordinator(runtime)
			now := time.Now().UTC()
			service.now = func() time.Time { return now }
			if durable {
				if _, err := repo.ConsumeQuota(ctx, accountdomain.QuotaConsumption{EventID: "pending", AccountID: v.ID, Mode: "fast", Units: 1}, now); err != nil {
					t.Fatal(err)
				}
				service.recoverDurableQuotaRefreshes(ctx, 0)
			} else {
				service.QueueQuotaRefresh(v.ID, "fast")
			}
			for attempt := 0; attempt < quotaRefreshFailureBudget+1; attempt++ {
				if len(service.quotaRefreshQueue) == 0 {
					break
				}
				request := <-service.quotaRefreshQueue
				state := service.quotaRefreshes[request.key]
				state.queued, state.running, state.pending = false, true, false
				service.runQuotaRefresh(ctx, request)
				now = now.Add(quotaRefreshBackoffMax + time.Minute)
				service.requeueQuotaRefreshes()
				service.recoverSharedQuotaRefreshes(ctx, now)
				service.requeueQuotaRefreshes()
			}
			if got := adapter.modeCalls.Load(); got != int64(quotaRefreshFailureBudget) {
				t.Fatalf("same shared demand exceeded failure budget: attempts=%d budget=%d pending_queue=%d", got, quotaRefreshFailureBudget, len(service.quotaRefreshQueue))
			}
			if len(service.quotaRefreshQueue) != 0 {
				t.Fatal("parked shared demand queued again")
			}
		})
	}
}
