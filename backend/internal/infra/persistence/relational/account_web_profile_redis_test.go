package relational

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
)

type webProfileRedisAdapter struct {
	entered, release chan struct{}
	calls            atomic.Int32
}

func (*webProfileRedisAdapter) Provider() account.Provider { return account.ProviderWeb }
func (a *webProfileRedisAdapter) AcceptTerms(context.Context, account.Credential) error {
	if a.calls.Add(1) == 1 {
		close(a.entered)
		<-a.release
	}
	// A successful upstream result may become known after the client canceled.
	return nil
}
func (*webProfileRedisAdapter) SetBirthDate(context.Context, account.Credential, time.Time) error {
	return nil
}
func (*webProfileRedisAdapter) EnableNSFW(context.Context, account.Credential) error { return nil }

func TestAccountWebProfileRedisLockOwnsCommitAcrossSQL(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS is not configured")
	}
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			for _, replace := range []bool{false, true} {
				t.Run(fmt.Sprintf("replace=%t", replace), func(t *testing.T) {
					ctx := context.Background()
					cfg := redisruntime.Config{Address: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), KeyPrefix: fmt.Sprintf("g23-profile-%d:", time.Now().UnixNano()), ConcurrencyLease: time.Minute}
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
					v, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: t.Name(), SourceKey: t.Name(), UserID: "old-user-" + strconv.FormatBool(replace), EncryptedAccessToken: "old", AuthStatus: account.AuthStatusActive})
					if err != nil {
						t.Fatal(err)
					}
					adapter := &webProfileRedisAdapter{entered: make(chan struct{}), release: make(chan struct{})}
					var once sync.Once
					finish := func() { once.Do(func() { close(adapter.release) }) }
					defer finish()
					registry := provider.NewRegistry(adapter)
					first := accountapp.NewService(ra, nil, nil, nil, registry, nil, lockA)
					second := accountapp.NewService(rb, nil, nil, nil, registry, nil, lockB)
					ownerCtx, cancel := context.WithCancel(ctx)
					defer cancel()
					owner := make(chan error, 1)
					go func() { owner <- first.AcceptWebTerms(ownerCtx, v.ID) }()
					select {
					case <-adapter.entered:
					case err := <-owner:
						t.Fatalf("profile failed before upstream: %v", err)
					case <-time.After(5 * time.Second):
						t.Fatal("profile upstream did not start")
					}
					cancel()
					if replace {
						replacement := v
						replacement.UserID, replacement.EncryptedAccessToken = "new-user", "new"
						if _, _, err := rb.UpsertByIdentity(ctx, replacement); err != nil {
							t.Fatal(err)
						}
					}
					// Even after cancellation, the owner must retain its actual
					// script lease until the observed result is saved or fenced.
					if err := second.AcceptWebTerms(ctx, v.ID); !errors.Is(err, accountapp.ErrWebAccountScriptBusy) {
						t.Fatalf("second instance bypassed owner: %v", err)
					}
					select {
					case err := <-owner:
						t.Fatalf("owner detached before completion: %v", err)
					default:
					}
					finish()
					ownerErr := awaitAccountOperation(t, owner)
					if replace {
						if !errors.Is(ownerErr, accountapp.ErrConflict) {
							t.Fatalf("stale canceled completion=%v", ownerErr)
						}
					} else if ownerErr != nil {
						t.Fatalf("known success lost on cancellation: %v", ownerErr)
					}
					stored, err := rb.Get(ctx, v.ID)
					if err != nil || (stored.WebTermsAcceptedAt != nil) == replace || adapter.calls.Load() != 1 {
						t.Fatalf("wrong completion after owner: %v calls=%d", err, adapter.calls.Load())
					}
					// A second process reads the committed marker or executes the
					// replacement's missing step, with no leaked lock or stale skip.
					if err := second.AcceptWebTerms(ctx, v.ID); err != nil {
						t.Fatal(err)
					}
					want := int32(1)
					if replace {
						want++
					}
					stored, err = rb.Get(ctx, v.ID)
					if err != nil || stored.WebTermsAcceptedAt == nil || stored.WebTermsAcceptedVersion != account.CurrentWebTermsVersion || adapter.calls.Load() != want {
						t.Fatalf("second instance could not resume: %v calls=%d", err, adapter.calls.Load())
					}
					unlock, acquired, err := lockB.Acquire(ctx, "web-account-script:"+strconv.FormatUint(v.ID, 10), time.Minute)
					if err != nil || !acquired {
						t.Fatalf("completed script leaked Redis lease: %v", err)
					}
					unlock()
				})
			}
		})
	}
}
