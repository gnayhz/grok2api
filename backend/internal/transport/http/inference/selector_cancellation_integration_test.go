package inference

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type httpSelectorReadPause struct {
	entered chan struct{}
	resume  chan struct{}
	reads   atomic.Int32
}

func (p *httpSelectorReadPause) after(ctx context.Context, err error) error {
	if err == nil && p.reads.Add(1) == 1 {
		close(p.entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.resume:
		}
	}
	return err
}

type httpRoutingReadGate struct {
	*relational.AccountRepository
	*httpSelectorReadPause
}

func (r *httpRoutingReadGate) ListRoutingAccountBases(ctx context.Context, provider account.Provider, mode string) ([]account.RoutingAccountBase, error) {
	values, err := r.AccountRepository.ListRoutingAccountBases(ctx, provider, mode)
	return values, r.after(ctx, err)
}

type httpCapacityReadGate struct {
	repository.ConcurrencyLimiter
	*httpSelectorReadPause
}

// Wait for this HTTP handler's goroutine to reach the existing selector wait.
// The gateway derives a cancellation context, so observing the root HTTP
// context's Done method cannot identify that stage. Inspect only this test's
// handler; no production callback or timing assumption is needed.
func waitHTTPRoutingFollower(t *testing.T, goroutine string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	buffer := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buffer, true)
		if n == len(buffer) {
			if len(buffer) >= 4<<20 {
				t.Fatal("HTTP selector goroutine snapshot exceeds test bound")
			}
			buffer = make([]byte, len(buffer)*2)
			continue
		}
		for _, stack := range strings.Split(string(buffer[:n]), "\n\n") {
			if strings.HasPrefix(stack, goroutine+" [select]:\n") && strings.Contains(stack, "/selector.(*routingLoadGroup).Do(") {
				return
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatal("HTTP follower did not join selector read")
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *httpCapacityReadGate) CurrentMany(ctx context.Context, keys []string) (map[string]int, error) {
	values, err := r.ConcurrencyLimiter.(repository.ConcurrencySnapshotReader).CurrentMany(ctx, keys)
	return values, r.after(ctx, err)
}
func httpSelectorCancellationLimiter(t *testing.T, backend string) repository.ConcurrencyLimiter {
	t.Helper()
	if backend == "memory" {
		return memory.NewConcurrencyLimiter()
	}
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("isolated TEST_REDIS_ADDRESS required")
	}
	store, err := redisruntime.Open(context.Background(), redisruntime.Config{Address: address, KeyPrefix: fmt.Sprintf("g18-http-%d:", time.Now().UnixNano()), ConcurrencyLease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return redisruntime.NewConcurrencyLimiter(store)
}

func TestHTTPSelectorSharedReadDisconnect(t *testing.T) {
	runHTTPSelectorReadDisconnect(t, "candidate", "memory")
}
func TestHTTPSelectorCapacityReadDisconnect(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) { runHTTPSelectorReadDisconnect(t, "capacity", backend) })
	}
}
func runHTTPSelectorReadDisconnect(t *testing.T, stage, backend string) {
	for _, operation := range []string{"responses", "chat/completions", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, canceled := range []string{"waiter", "owner", "all"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", operation, stream, canceled), func(t *testing.T) {
					ctx := context.Background()
					db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-http.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					if err := db.InitializeSchema(ctx); err != nil {
						t.Fatal(err)
					}
					cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
					if err != nil {
						t.Fatal(err)
					}
					token, err := cipher.Encrypt("synthetic-selector-token")
					if err != nil {
						t.Fatal(err)
					}
					accounts := relational.NewAccountRepository(db)
					models := relational.NewModelRepository(db)
					audits := relational.NewAuditRepository(db)
					credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth,
						Name: "routing", SourceKey: "routing", EncryptedAccessToken: token, Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 2, ExpiresAt: time.Now().Add(time.Hour)})
					if err != nil {
						t.Fatal(err)
					}
					const model = "grok-4.5"
					if err := testsupport.Discover(ctx, models, credential.Provider, []string{model}); err != nil {
						t.Fatal(err)
					}
					if err := testsupport.Capabilities(ctx, models, accounts, credential.ID, []string{model}, time.Now()); err != nil {
						t.Fatal(err)
					}
					var generated atomic.Int32
					upstream := completionHTTPUpstream(t, model, &generated)
					defer upstream.Close()
					registry := providerimpl.NewRegistry(cli.NewAdapter(cli.Config{BaseURL: upstream.URL + "/v1"}, cipher))
					concurrency, sticky := httpSelectorCancellationLimiter(t, backend), memory.NewStickyStore()
					accountService := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
					clients := clientkeyapp.NewService("selector-owner", relational.NewClientKeyRepository(db), memory.NewRateLimiter(), concurrency, 120, 4, cipher, security.RandomTokenSource{})
					defer closeClientKeyService(t, clients)
					created, err := clients.Create(ctx, clientkeyapp.CreateInput{Name: "routing", Enabled: true, RPMLimit: 120, MaxConcurrent: 4})
					if err != nil {
						t.Fatal(err)
					}
					gate := &httpSelectorReadPause{entered: make(chan struct{}), resume: make(chan struct{})}
					var selectorAccounts repository.AccountRepository = accounts
					var selectorConcurrency repository.ConcurrencyLimiter = concurrency
					if stage == "candidate" {
						selectorAccounts = &httpRoutingReadGate{AccountRepository: accounts, httpSelectorReadPause: gate}
					} else {
						selectorConcurrency = &httpCapacityReadGate{ConcurrencyLimiter: concurrency, httpSelectorReadPause: gate}
					}
					var release sync.Once
					unblock := func() { release.Do(func() { close(gate.resume) }) }
					selector := selector.NewSelector(selectorAccounts, selectorConcurrency, sticky, registry, time.Hour, time.Second, time.Minute)
					service := gateway.NewService(models, audits, accountService, clients, registry, selector, historyapp.NewResponseResources(relational.NewResponseRepository(db)), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 2)
					gin.SetMode(gin.TestMode)
					router := gin.New()
					router.Use(middleware.RequestID(nil), middleware.ClientAuth(clients))
					NewHandler(service, nil, 1<<20).Register(router.Group("/v1"))
					started := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
					finished := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
					followerGoroutine := make(chan string, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						index := 0
						if r.Header.Get("X-Test-Request") == "waiter" {
							index = 1
						} else if r.Header.Get("X-Test-Request") == "next" {
							index = 2
						}
						close(started[index])
						defer close(finished[index])
						if index == 1 {
							// Deliberately exceed the old 50ms assumption. Cancellation
							// must still occur after this request reaches selection.
							time.Sleep(100 * time.Millisecond)
							var stack [128]byte
							n := runtime.Stack(stack[:], false)
							followerGoroutine <- strings.SplitN(string(stack[:n]), " [", 2)[0]
						}
						router.ServeHTTP(w, r)
					}))
					defer server.Close()
					defer unblock()
					wait := func(signal <-chan struct{}, stage string) {
						t.Helper()
						select {
						case <-signal:
						case <-time.After(5 * time.Second):
							t.Fatalf("HTTP %s did not finish", stage)
						}
					}
					payload := map[string]any{"model": model, "stream": stream}
					if operation == "responses" {
						payload["input"] = "synthetic routing cancellation"
					} else {
						payload["messages"] = []any{map[string]any{"role": "user", "content": "synthetic routing cancellation"}}
					}
					if operation == "messages" {
						payload["max_tokens"] = 128
					}
					data, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					request := func(ctx context.Context, name string, done chan error) {
						req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/"+operation, bytes.NewReader(data))
						if err != nil {
							done <- err
							return
						}
						req.Header.Set("Authorization", "Bearer "+created.Secret)
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("anthropic-version", "2023-06-01")
						req.Header.Set("X-Test-Request", name)
						req.Header.Set("X-Request-ID", "routing-"+name)
						response, err := server.Client().Do(req)
						if err == nil {
							body, readErr := io.ReadAll(response.Body)
							response.Body.Close()
							err = readErr
							if err == nil && (response.StatusCode != 200 || !strings.Contains(string(body), "completion answer")) {
								err = fmt.Errorf("response %s: status=%d body=%s", name, response.StatusCode, body)
							}
						}
						done <- err
					}
					ownerCtx, cancelOwner := context.WithCancel(ctx)
					defer cancelOwner()
					waiterCtx, cancelWaiter := context.WithCancel(ctx)
					defer cancelWaiter()
					owner, waiter := make(chan error, 1), make(chan error, 1)
					go request(ownerCtx, "owner", owner)
					wait(gate.entered, "owner "+stage+" read")
					go request(waiterCtx, "waiter", waiter)
					wait(started[1], "waiter entry")
					select {
					case goroutine := <-followerGoroutine:
						waitHTTPRoutingFollower(t, goroutine)
					case <-time.After(5 * time.Second):
						t.Fatal("HTTP follower handler did not start")
					}
					// Both real requests have reached selection, with no attempt.
					select {
					case <-finished[1]:
						t.Fatal("waiter completed while shared read was held")
					default:
					}
					if generated.Load() != 0 || gate.reads.Load() != 1 {
						t.Fatalf("before cancellation: generations=%d reads=%d", generated.Load(), gate.reads.Load())
					}
					if canceled != "owner" {
						cancelWaiter()
						wait(finished[1], "disconnected waiter handler")
					}
					if canceled != "waiter" {
						cancelOwner()
						wait(finished[0], "disconnected owner handler")
					} else {
						// The canceled request must release before its peer is unblocked.
						if generated.Load() != 0 {
							t.Fatal("canceled waiter generated while owner was held")
						}
						unblock()
					}
					wait(finished[0], "owner cleanup")
					wait(finished[1], "waiter cleanup")
					for index, result := range []chan error{owner, waiter} {
						err := <-result
						wasCanceled := canceled == "all" || index == 0 && canceled == "owner" || index == 1 && canceled == "waiter"
						if wasCanceled && !errors.Is(err, context.Canceled) || !wasCanceled && err != nil {
							t.Fatalf("request %d canceled=%t: %v", index, wasCanceled, err)
						}
					}
					for _, key := range []string{repository.AccountConcurrencyKey(credential.ID), fmt.Sprintf("client:%d", created.Key.ID)} {
						current, err := concurrency.Current(ctx, key)
						if err != nil || current != 0 {
							t.Fatalf("capacity %s after disconnect: %d %v", key, current, err)
						}
					}
					records, count, err := audits.List(ctx, 0, 10)
					if err != nil || count != 2 {
						t.Fatalf("completion records after disconnect: count=%d error=%v", count, err)
					}
					for _, summary := range records {
						record, err := audits.Get(ctx, summary.ID)
						if err != nil {
							t.Fatal(err)
						}
						wasCanceled := canceled == "all" || record.RequestID == "routing-"+canceled
						if wasCanceled && (record.AttemptCount != 0 || len(record.GenerationUsages) != 0 || record.CostInUSDTicks != 0 || record.AccountID != nil) {
							t.Fatalf("canceled selection recorded generation/cost/account: %+v", record)
						}
					}
					wantGenerated := int32(1)
					if canceled == "all" {
						wantGenerated = 0
					}
					if generated.Load() != wantGenerated {
						t.Fatalf("disconnected request made an upstream attempt: %d, want %d", generated.Load(), wantGenerated)
					}
					unblock()
					next := make(chan error, 1)
					go request(ctx, "next", next)
					wait(finished[2], "next request")
					if err := <-next; err != nil {
						t.Fatalf("request after disconnect: %v", err)
					}
					if generated.Load() != wantGenerated+1 {
						t.Fatalf("next request physical attempts=%d", generated.Load())
					}
				})
			}
		}
	}
}
