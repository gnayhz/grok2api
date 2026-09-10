package account

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	redisclient "github.com/redis/go-redis/v9"
)

// Identical before/after fixture: public demand, worker execution, actual Web
// wire through M13, SQL quota revision/CAS and shared publish/read/clear/lock.
func BenchmarkQuotaQueueCost(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, runtimeKind := range []string{"memory", "redis"} {
			b.Run(dialect+"/"+runtimeKind, func(b *testing.B) {
				ctx := context.Background()
				db := accountImportCostDatabase(b, dialect)
				repo := relational.NewAccountRepository(db)
				cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
				if err != nil {
					b.Fatal(err)
				}
				token, err := cipher.Encrypt("synthetic")
				if err != nil {
					b.Fatal(err)
				}
				v, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, SourceKey: "queue-cost", Name: "queue-cost", EncryptedAccessToken: token, AuthStatus: accountdomain.AuthStatusActive})
				if err != nil {
					b.Fatal(err)
				}
				var coordinator repository.QuotaRefreshCoordinator = memory.NewQuotaRefreshCoordinator()
				var lock repository.DistributedLock = memory.NewLockStore()
				if runtimeKind == "redis" {
					address := os.Getenv("TEST_REDIS_ADDRESS")
					if address == "" {
						b.Skip("isolated TEST_REDIS_ADDRESS required")
					}
					prefix := fmt.Sprintf("g27-cost-%d:", time.Now().UnixNano())
					store, err := redisruntime.Open(ctx, redisruntime.Config{Address: address, Database: 15, KeyPrefix: prefix})
					if err != nil {
						b.Fatal(err)
					}
					coordinator, lock = store, redisruntime.NewLockStore(store)
					b.Cleanup(func() {
						defer store.Close()
						client := redisclient.NewClient(&redisclient.Options{Addr: address, DB: 15})
						defer client.Close()
						keys, err := client.Keys(ctx, prefix+"*").Result()
						if err != nil {
							b.Error(err)
							return
						}
						if len(keys) > 0 {
							if err = client.Del(ctx, keys...).Err(); err != nil {
								b.Error(err)
							}
						}
					})
				}
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"remainingQueries":5,"totalQueries":30,"windowSizeSeconds":3600}`)
				}))
				b.Cleanup(server.Close)
				adapter, closeNetwork := NewQuotaQueueWebFixture(db, cipher, server.URL)
				b.Cleanup(func() { _ = closeNetwork(ctx) })
				service := NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), cipher, lock)
				service.SetQuotaRefreshCoordinator(coordinator)
				service.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					service.QueueQuotaRefresh(v.ID, "fast")
					request := <-service.quotaRefreshQueue
					state := service.quotaRefreshes[request.key]
					state.queued = false
					state.running = true
					state.pending = false
					service.runQuotaRefresh(ctx, request)
					if len(service.quotaRefreshes) != 0 {
						b.Fatalf("quota query did not complete: %+v", service.QuotaRefreshStats())
					}
				}
				if calls.Load() != int64(b.N) {
					b.Fatalf("HTTP calls=%d operations=%d", calls.Load(), b.N)
				}
			})
		}
	}
}
