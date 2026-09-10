package inference

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/repository"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	"github.com/gin-gonic/gin"
	redisclient "github.com/redis/go-redis/v9"
)

func TestHTTPConsoleQuotaTimingBelongsToAccountOwner(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		for _, runtime := range []string{"memory", "redis"} {
			for _, entry := range []string{"full", "mode"} {
				for _, exhausted := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/exhausted=%t", driver, runtime, entry, exhausted), func(t *testing.T) {
						var calls atomic.Int32
						f := newResponseRetentionFixture(t, driver, account.ProviderConsole, responseRetentionHooks{beforeUsage: func(w http.ResponseWriter, r *http.Request) bool {
							calls.Add(1)
							chat, image, video := 9, 5, 2
							if exhausted {
								chat, image, video = 0, 0, 0
							}
							w.Header().Set("Content-Type", "application/json")
							_, _ = fmt.Fprintf(w, `{"quotas":[{"kind":"chat","limit":10,"remaining":%d},{"kind":"image","limit":5,"remaining":%d},{"kind":"video","limit":2,"remaining":%d}]}`, chat, image, video)
							return true
						}})
						ctx := context.Background()
						credential, err := f.accounts.Get(ctx, f.accountID)
						if err != nil {
							t.Fatal(err)
						}
						raw, err := f.adapter.(provider.QuotaAdapter).SyncQuota(ctx, credential)
						if err != nil || len(raw.Windows) != 3 {
							t.Fatalf("raw windows=%+v err=%v", raw.Windows, err)
						}
						for _, window := range raw.Windows {
							if window.WindowSeconds != 0 || window.ResetAt != nil || window.SyncedAt == nil || window.Source != account.QuotaSourceUpstream {
								t.Fatalf("provider invented timing: %+v", window)
							}
						}
						writer, reader := consoleQuotaOwnerQueues(t, runtime)
						f.accountService.SetQuotaRecoveryQueue(writer)
						started := time.Now().UTC()
						if entry == "full" {
							router := gin.New()
							accounthttp.NewHandler(f.accountService, nil).Register(router.Group("/api/admin/v1"))
							output := httptest.NewRecorder()
							router.ServeHTTP(output, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/admin/v1/accounts/%d/refresh-quota", f.accountID), nil))
							if output.Code != 200 {
								t.Fatalf("full=%d: %s", output.Code, output.Body.String())
							}
						} else {
							window, err := f.accountService.RefreshQuotaMode(ctx, f.accountID, "console_image")
							if err != nil || window.Mode != "console_image" {
								t.Fatalf("mode=%+v err=%v", window, err)
							}
						}
						finished := time.Now().UTC()
						windows, err := f.accounts.GetQuotaWindows(ctx, []uint64{f.accountID})
						if err != nil {
							t.Fatal(err)
						}
						if len(windows[f.accountID]) != 3 || calls.Load() != 2 {
							t.Fatalf("windows=%d calls=%d", len(windows[f.accountID]), calls.Load())
						}
						for _, window := range windows[f.accountID] {
							if window.SyncedAt == nil || window.Source != account.QuotaSourceUpstream {
								t.Fatalf("lost upstream amount facts: %+v", window)
							}
							if window.Mode == "console" {
								if window.WindowSeconds != 86400 || (window.ResetAt != nil) != exhausted {
									t.Fatalf("chat timing=%+v", window)
								}
								if exhausted {
									want := window.SyncedAt.Add(24 * time.Hour)
									if delta := window.ResetAt.Sub(want); delta < -time.Microsecond || delta > time.Microsecond {
										t.Fatalf("prediction drift=%v", delta)
									}
								}
							} else if window.ResetAt != nil || window.WindowSeconds != 0 {
								t.Fatalf("media acquired fabricated window: %+v", window)
							}
						}
						early, err := reader.ClaimDueQuotaRecoveries(ctx, started.Add(24*time.Hour-time.Millisecond), 4, time.Minute)
						if err != nil || len(early) != 0 {
							t.Fatalf("early=%+v err=%v", early, err)
						}
						events, err := reader.ClaimDueQuotaRecoveries(ctx, finished.Add(24*time.Hour+time.Millisecond), 4, time.Minute)
						want := 0
						if exhausted {
							want = 3
						}
						if err != nil || len(events) != want {
							t.Fatalf("events=%+v err=%v want=%d", events, err, want)
						}
						for _, event := range events {
							if event.AccountID != f.accountID || event.ClaimToken == "" {
								t.Fatalf("bad claim=%+v", event)
							}

							if err := reader.AckQuotaRecovery(ctx, event); err != nil {
								t.Fatal(err)
							}
						}
						if f.generated.Load() != 0 {
							t.Fatal("quota operation generated inference")
						}
					})
				}
			}
		}
	}
}

func consoleQuotaOwnerQueues(t *testing.T, kind string) (repository.QuotaRecoveryQueue, repository.QuotaRecoveryQueue) {
	t.Helper()
	if kind == "memory" {
		q := memory.NewQuotaRecoveryQueue()
		return q, q
	}
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("requires isolated TEST_REDIS_ADDRESS")
	}
	number := 0
	if value := os.Getenv("TEST_REDIS_DATABASE"); value != "" {
		var err error
		number, err = strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	prefix := fmt.Sprintf("g69-owner-%d:", time.Now().UnixNano())
	cfg := redisruntime.Config{Address: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), Database: number, KeyPrefix: prefix, ConcurrencyLease: time.Minute}
	cleanup := redisclient.NewClient(&redisclient.Options{Addr: address, Username: cfg.Username, Password: cfg.Password, DB: number})
	t.Cleanup(func() {
		defer cleanup.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
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
					return
				}
			}
			cursor = next
			if cursor == 0 {
				return
			}
		}
	})
	first, err := redisruntime.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := redisruntime.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	return first, second
}
