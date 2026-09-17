package relational

import (
	"context"
	"errors"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	redisclient "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type deviceGrantAdapter struct {
	key              string
	calls            atomic.Int32
	entered, release chan struct{}
	after            func()
	pending, panics  bool
}

func (*deviceGrantAdapter) Provider() account.Provider { return account.ProviderBuild }
func (*deviceGrantAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: account.ProviderBuild, Credential: provider.CredentialSurface{AuthType: account.AuthTypeOAuth, DeviceOAuth: true}}
}
func (*deviceGrantAdapter) StartDeviceAuthorization(context.Context) (provider.DeviceAuthorization, error) {
	return provider.DeviceAuthorization{}, errors.New("unused")
}
func (a *deviceGrantAdapter) PollDeviceAuthorization(ctx context.Context, _ string) (provider.CredentialSeed, error) {
	n := a.calls.Add(1)
	if _, ok := ctx.Deadline(); !ok {
		return provider.CredentialSeed{}, errors.New("poll has no deadline")
	}
	if n == 1 && a.entered != nil {
		close(a.entered)
		<-a.release
	}
	if a.after != nil {
		a.after()
	}
	if a.panics {
		panic("device provider panic")
	}
	if a.pending {
		return provider.CredentialSeed{}, provider.ErrAuthorizationPending
	}
	return provider.CredentialSeed{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: a.key, SourceKey: a.key, AccessToken: fmt.Sprintf("issued-%d", n), RefreshToken: fmt.Sprintf("refresh-%d", n), ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type deviceGrantStore struct {
	repository.DeviceSessionRepository
	lease     time.Duration
	finishErr error
}

func (s *deviceGrantStore) ClaimPoll(ctx context.Context, id, token string, now, until time.Time) (account.DeviceSession, error) {
	if s.lease > 0 {
		until = now.Add(s.lease)
	}
	return s.DeviceSessionRepository.ClaimPoll(ctx, id, token, now, until)
}
func (s *deviceGrantStore) FinishPoll(ctx context.Context, receipt account.DevicePollReceipt, event account.DevicePollCompletion) (bool, error) {
	if s.finishErr != nil {
		return false, s.finishErr
	}
	return s.DeviceSessionRepository.FinishPoll(ctx, receipt, event)
}

func deviceGrantStores(t *testing.T, kind string) (repository.DeviceSessionRepository, repository.DeviceSessionRepository) {
	t.Helper()
	if kind == "memory" {
		s := memory.NewDeviceSessionStore()
		return s, s
	}
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS required")
	}
	cfg := redisruntime.Config{Address: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), KeyPrefix: fmt.Sprintf("g24-grant-%d:", time.Now().UnixNano())}
	if raw := os.Getenv("TEST_REDIS_DATABASE"); raw != "" {
		var err error
		cfg.Database, err = strconv.Atoi(raw)
		if err != nil {
			t.Fatal(err)
		}
	}
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
	t.Cleanup(func() {
		c := redisclient.NewClient(&redisclient.Options{Addr: cfg.Address, Username: cfg.Username, Password: cfg.Password, DB: cfg.Database})
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		iter := c.Scan(ctx, 0, cfg.KeyPrefix+"*", 100).Iterator()
		for iter.Next(ctx) {
			if err := c.Del(ctx, iter.Val()).Err(); err != nil {
				t.Error(err)
			}
		}
		if err := iter.Err(); err != nil {
			t.Error(err)
		}
	})
	return redisruntime.NewDeviceSessionStore(first), redisruntime.NewDeviceSessionStore(second)
}

func TestAccountDeviceGrantCompletionAcrossSQLAndRuntime(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			for _, kind := range []string{"memory", "redis"} {
				t.Run(kind, func(t *testing.T) {
					first, second := deviceGrantStores(t, kind)
					for _, scenario := range []string{"cancel_known_success", "cancel_pending_owner", "old_lease_success", "completion_store_failed", "sql_install_failed", "panic_releases_claim"} {
						t.Run(scenario, func(t *testing.T) {
							ctx, cancel := context.WithCancel(context.Background())
							defer cancel()
							key := kind + "-" + scenario
							session := account.DeviceSession{ID: key, DeviceCode: "synthetic-device", Interval: time.Millisecond, ExpiresAt: time.Now().Add(time.Minute)}
							if err := first.Create(ctx, session); err != nil {
								t.Fatal(err)
							}
							adapter := &deviceGrantAdapter{key: key}
							store := &deviceGrantStore{DeviceSessionRepository: first}
							injected := errors.New("device persistence unavailable")
							switch scenario {
							case "cancel_known_success":
								adapter.after = cancel
							case "cancel_pending_owner", "old_lease_success":
								adapter.entered, adapter.release = make(chan struct{}), make(chan struct{})
								if scenario == "old_lease_success" {
									store.lease = 150 * time.Millisecond
								} else {
									adapter.pending = true
								}
							case "completion_store_failed":
								store.finishErr = injected
							case "sql_install_failed":
								if err := a.db.Callback().Create().Before("gorm:create").Register("g24-device-sql", func(tx *gorm.DB) {
									if tx.Statement.Table == "account_credentials" {
										tx.AddError(injected)
									}
								}); err != nil {
									t.Fatal(err)
								}
								defer a.db.Callback().Create().Remove("g24-device-sql")
							case "panic_releases_claim":
								adapter.panics = true
							}
							registry := providerimpl.NewRegistry(adapter)
							owner := accountapp.NewService(ra, NewAuditRepository(a), store, nil, registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
							peer := accountapp.NewService(rb, NewAuditRepository(b), second, nil, registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
							var callErr error
							if adapter.entered != nil {
								var once sync.Once
								release := func() { once.Do(func() { close(adapter.release) }) }
								defer release()
								done := make(chan error, 1)
								go func() { _, err := owner.PollDeviceLogin(ctx, key); done <- err }()
								select {
								case <-adapter.entered:
								case err := <-done:
									t.Fatalf("owner failed before upstream: %v", err)
								case <-time.After(time.Second):
									t.Fatal("provider did not start")
								}
								if scenario == "cancel_pending_owner" {
									cancel()
									select {
									case err := <-done:
										t.Fatalf("owner detached from in-flight provider: %v", err)
									default:
									}
									if _, err := peer.PollDeviceLogin(context.Background(), key); !errors.Is(err, accountapp.ErrDeviceSlowDown) {
										t.Fatalf("cancel released in-flight claim: %v", err)
									}
								} else {
									claimed, err := second.Get(context.Background(), key, time.Now())
									if err != nil {
										t.Fatal(err)
									}
									time.Sleep(time.Until(claimed.PollLeaseUntil.Add(20 * time.Millisecond)))
									if _, err := peer.PollDeviceLogin(context.Background(), key); err != nil {
										t.Fatalf("new lease grant: %v", err)
									}
								}
								release()
								select {
								case callErr = <-done:
								case <-time.After(5 * time.Second):
									t.Fatal("owner did not finish")
								}
							} else if scenario == "panic_releases_claim" {
								var recovered any
								func() { defer func() { recovered = recover() }(); _, callErr = owner.PollDeviceLogin(ctx, key) }()
								if recovered == nil {
									t.Fatal("provider did not panic")
								}
							} else {
								_, callErr = owner.PollDeviceLogin(ctx, key)
							}
							expected := ""
							switch scenario {
							case "cancel_known_success":
								if callErr != nil {
									t.Fatal(callErr)
								}
								expected = "issued-1"
							case "old_lease_success":
								if !errors.Is(callErr, accountapp.ErrConflict) {
									t.Fatalf("stale owner result: %v", callErr)
								}
								expected = "issued-2"
							case "cancel_pending_owner":
								if !errors.Is(callErr, accountapp.ErrDevicePending) {
									t.Fatalf("pending result: %v", callErr)
								}
							case "completion_store_failed", "sql_install_failed":
								if !errors.Is(callErr, injected) {
									t.Fatalf("storage failure hidden: %v", callErr)
								}
							}
							var rows []accountModel
							if err := b.db.Where("provider = ? AND source_key = ?", account.ProviderBuild, key).Find(&rows).Error; err != nil {
								t.Fatal(err)
							}
							if expected == "" {
								if len(rows) != 0 {
									t.Fatal("failed owner installed material")
								}
							} else {
								if len(rows) != 1 {
									t.Fatalf("known material count=%d", len(rows))
								}
								saved, err := rb.Get(context.Background(), rows[0].ID)
								if err != nil {
									t.Fatal(err)
								}
								token, err := cipher.Decrypt(saved.EncryptedAccessToken)
								if err != nil || token != expected {
									t.Fatalf("wrong grant persisted: %v", err)
								}
							}
							stored, err := second.Get(context.Background(), key, time.Now())
							switch scenario {
							case "cancel_known_success", "old_lease_success", "sql_install_failed":
								if !errors.Is(err, repository.ErrNotFound) {
									t.Fatalf("terminal session reused: %v", err)
								}
							case "cancel_pending_owner", "panic_releases_claim":
								if err != nil || stored.PollToken != "" {
									t.Fatalf("claim not released: %v", err)
								}
							case "completion_store_failed":
								if err != nil || stored.PollToken == "" {
									t.Fatalf("failed runtime write fabricated completion: %v", err)
								}
							}
						})
					}
				})
			}
		})
	}
}
