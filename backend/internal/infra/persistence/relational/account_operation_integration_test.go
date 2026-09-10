package relational

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type accountOperationWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *accountOperationWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func awaitAccountOperation(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("account operation did not finish")
		return nil
	}
}

func TestAccountBillingHTTPSharedOperationAcrossSQL(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			for _, cancelOwner := range []bool{false, true} {
				t.Run(fmt.Sprintf("cancel_owner=%t", cancelOwner), func(t *testing.T) {
					ctx := context.Background()
					cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
					if err != nil {
						t.Fatal(err)
					}
					encrypted, err := cipher.Encrypt("operation-access")
					if err != nil {
						t.Fatal(err)
					}
					v, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: t.Name(), SourceKey: t.Name(), EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive})
					if err != nil {
						t.Fatal(err)
					}
					entered, release := make(chan struct{}), make(chan struct{})
					var once sync.Once
					finish := func() { once.Do(func() { close(release) }) }
					var calls atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/v1/billing":
							if r.Header.Get("Authorization") != "Bearer operation-access" || r.URL.Query().Get("format") != "credits" {
								t.Error("Billing request lost captured material or protocol")
							}
							if calls.Add(1) == 1 {
								close(entered)
								select {
								case <-release:
								case <-r.Context().Done():
									return
								}
							}
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, `{"monthlyLimit":100,"used":43}`)
						case "/v1/user":
							fmt.Fprint(w, `{"subscriptionTier":"SuperGrok"}`)
						default:
							http.NotFound(w, r)
						}
					}))
					defer server.Close()
					defer finish()
					s := accountapp.NewService(ra, nil, nil, nil, provider.NewRegistry(cli.NewAdapter(cli.Config{BaseURL: server.URL + "/v1"}, cipher)), cipher, nil)
					ownerCtx, cancel := context.WithCancel(ctx)
					defer cancel()
					owner := make(chan error, 1)
					go func() { _, err := s.RefreshBilling(ownerCtx, v.ID); owner <- err }()
					select {
					case <-entered:
					case err := <-owner:
						t.Fatalf("owner failed: %v", err)
					case <-time.After(5 * time.Second):
						t.Fatal("Billing did not start")
					}
					waitBase, stop := context.WithCancel(ctx)
					defer stop()
					waitCtx := &accountOperationWaitContext{Context: waitBase, waiting: make(chan struct{})}
					waiter := make(chan error, 1)
					go func() { _, err := s.RefreshBilling(waitCtx, v.ID); waiter <- err }()
					select {
					case <-waitCtx.waiting:
					case <-time.After(5 * time.Second):
						t.Fatal("waiter did not join Billing")
					}
					if cancelOwner {
						cancel()
						if err := awaitAccountOperation(t, owner); !errors.Is(err, context.Canceled) {
							t.Fatalf("owner=%v", err)
						}
						if err := awaitAccountOperation(t, waiter); err != nil {
							t.Fatal(err)
						}
					} else {
						stop()
						if err := awaitAccountOperation(t, waiter); !errors.Is(err, context.Canceled) {
							t.Fatalf("waiter=%v", err)
						}
						if _, err := rb.GetBilling(ctx, v.ID); err == nil {
							t.Fatal("waiting cancellation fabricated Billing before upstream response")
						}
						finish()
						if err := awaitAccountOperation(t, owner); err != nil {
							t.Fatal(err)
						}
					}
					billing, err := rb.GetBilling(ctx, v.ID)
					if err != nil || billing.Used != 43 || billing.MonthlyLimit != 100 {
						t.Fatalf("committed Billing missing: %v", err)
					}
					wantCalls := int32(1)
					if cancelOwner {
						wantCalls++
					}
					if calls.Load() != wantCalls {
						t.Fatalf("Billing calls=%d want=%d", calls.Load(), wantCalls)
					}
					current, err := rb.Get(ctx, v.ID)
					if err != nil || current.CredentialGeneration != v.CredentialGeneration || current.AuthStatus != v.AuthStatus || current.RefreshFailureCount != 0 {
						t.Fatalf("Billing lifecycle changed credential health: %v", err)
					}
				})
			}
		})
	}
}

type accountOperationRefreshAdapter struct {
	calls                      atomic.Int32
	entered, cleaning, release chan struct{}
	cancelOwner                bool
}

func (*accountOperationRefreshAdapter) Provider() account.Provider { return account.ProviderBuild }
func (*accountOperationRefreshAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: account.ProviderBuild, Credential: provider.CredentialSurface{Refresh: true}}
}
func (a *accountOperationRefreshAdapter) RefreshCredential(ctx context.Context, _ account.Credential) (provider.RefreshedCredential, error) {
	if a.calls.Add(1) == 1 {
		close(a.entered)
		if a.cancelOwner {
			<-ctx.Done()
			close(a.cleaning)
		}
		<-a.release
		if a.cancelOwner {
			return provider.RefreshedCredential{}, ctx.Err()
		}
	}
	return provider.RefreshedCredential{EncryptedAccessToken: "committed-access", EncryptedRefreshToken: "committed-refresh", ExpiresAt: time.Now().Add(time.Hour), RefreshTokenRotated: true}, nil
}

func TestAccountCredentialSharedOperationOwnsRedisLockAcrossSQL(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS is not configured")
	}
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			for _, cancelOwner := range []bool{false, true} {
				t.Run(fmt.Sprintf("cancel_owner=%t", cancelOwner), func(t *testing.T) {
					ctx := context.Background()
					cfg := redisruntime.Config{Address: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), KeyPrefix: fmt.Sprintf("g22-operation-%d:", time.Now().UnixNano()), ConcurrencyLease: time.Minute}
					if raw := os.Getenv("TEST_REDIS_DATABASE"); raw != "" {
						var err error
						cfg.Database, err = strconv.Atoi(raw)
						if err != nil {
							t.Fatal(err)
						}
					}
					runtimeA, err := redisruntime.Open(ctx, cfg)
					if err != nil {
						t.Fatal(err)
					}
					defer runtimeA.Close()
					runtimeB, err := redisruntime.Open(ctx, cfg)
					if err != nil {
						t.Fatal(err)
					}
					defer runtimeB.Close()
					lockA, lockB := redisruntime.NewLockStore(runtimeA), redisruntime.NewLockStore(runtimeB)
					v, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: t.Name(), SourceKey: t.Name(), EncryptedAccessToken: "access", EncryptedRefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive})
					if err != nil {
						t.Fatal(err)
					}
					adapter := &accountOperationRefreshAdapter{entered: make(chan struct{}), cleaning: make(chan struct{}), release: make(chan struct{}), cancelOwner: cancelOwner}
					var once sync.Once
					finish := func() { once.Do(func() { close(adapter.release) }) }
					defer finish()
					registry := provider.NewRegistry(adapter)
					first := accountapp.NewService(ra, nil, nil, nil, registry, nil, lockA)
					second := accountapp.NewService(rb, nil, nil, nil, registry, nil, lockB)
					ownerCtx, cancel := context.WithCancel(ctx)
					defer cancel()
					owner := make(chan error, 1)
					go func() { _, err := first.EnsureCredential(ownerCtx, v, true); owner <- err }()
					select {
					case <-adapter.entered:
					case <-time.After(5 * time.Second):
						t.Fatal("credential Provider did not start")
					}
					// Both a local waiter and another instance must honor their own
					// deadlines while the owner retains its actual Redis lease.
					for _, service := range []*accountapp.Service{first, second} {
						waitCtx, stop := context.WithTimeout(ctx, 40*time.Millisecond)
						_, err := service.EnsureCredential(waitCtx, v, true)
						stop()
						if !errors.Is(err, context.DeadlineExceeded) {
							t.Fatalf("canceled credential waiter=%v", err)
						}
					}
					waiter := make(chan error, 1)
					if cancelOwner {
						go func() { _, err := first.EnsureCredential(ctx, v, true); waiter <- err }()
						cancel()
						select {
						case <-adapter.cleaning:
						case <-time.After(5 * time.Second):
							t.Fatal("canceled owner did not clean up")
						}
						select {
						case err := <-owner:
							t.Fatalf("owner detached from cleanup: %v", err)
						default:
						}
					}
					key := "credential-refresh:" + strconv.FormatUint(v.ID, 10)
					releaseLock, acquired, err := lockB.Acquire(ctx, key, time.Minute)
					if acquired {
						releaseLock()
					}
					if err != nil || acquired || adapter.calls.Load() != 1 {
						t.Fatalf("waiter released/replaced owner's Redis lease: %v", err)
					}
					finish()
					ownerErr := awaitAccountOperation(t, owner)
					wantCalls := int32(1)
					if cancelOwner {
						if !errors.Is(ownerErr, context.Canceled) {
							t.Fatalf("owner=%v", ownerErr)
						}
						if err := awaitAccountOperation(t, waiter); err != nil {
							t.Fatal(err)
						}
						wantCalls++
					} else if ownerErr != nil {
						t.Fatal(ownerErr)
					}
					if _, err := second.EnsureCredential(ctx, v, true); err != nil {
						t.Fatal(err)
					}
					current, err := rb.Get(ctx, v.ID)
					if err != nil || current.EncryptedRefreshToken != "committed-refresh" || current.CredentialGeneration != v.CredentialGeneration+1 || current.RefreshFailureCount != 0 || adapter.calls.Load() != wantCalls {
						t.Fatalf("cross-instance completion did not preserve one rotation: %v calls=%d", err, adapter.calls.Load())
					}
					releaseLock, acquired, err = lockB.Acquire(ctx, key, time.Minute)
					if err != nil || !acquired {
						t.Fatalf("completed owner leaked Redis lock: %v", err)
					}
					releaseLock()
				})
			}
		})
	}
}
