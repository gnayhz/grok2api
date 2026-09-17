package clientkey

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/pkg/tokenhash"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// The gate delays delivery only after the real SQL snapshot has completed.
// Later reads are free to observe and cache the newly committed policy.
type heldAuthRead struct {
	repository.ClientKeyRepository
	read, resume chan struct{}
	held         atomic.Bool
	calls        atomic.Int32
}

func (r *heldAuthRead) GetByPrefix(ctx context.Context, prefix string) (clientkeydomain.Key, error) {
	r.calls.Add(1)
	key, err := r.ClientKeyRepository.GetByPrefix(ctx, prefix)
	if r.held.CompareAndSwap(false, true) {
		close(r.read)
		select {
		case <-r.resume:
		case <-ctx.Done():
			return clientkeydomain.Key{}, ctx.Err()
		}
	}
	return key, err
}

func newHeldAuthService(t *testing.T) (*Service, *relational.ClientKeyRepository, *heldAuthRead, func()) {
	t.Helper()
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "key-cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	base := relational.NewClientKeyRepository(db)
	held := &heldAuthRead{ClientKeyRepository: base, read: make(chan struct{}), resume: make(chan struct{})}
	resume := sync.OnceFunc(func() { close(held.resume) })
	t.Cleanup(resume)
	service := NewService("auth-test", held, nil, nil, 0, 0, testCipher(t), security.RandomTokenSource{})
	t.Cleanup(func() { closeKeyService(t, service) })
	return service, base, held, resume
}

func awaitAuthRead(t *testing.T, held *heldAuthRead) {
	t.Helper()
	select {
	case <-held.read:
	case <-time.After(5 * time.Second):
		t.Fatal("SQL authentication read did not reach gate")
	}
}

func authResult(service *Service, ctx context.Context, raw string) (clientkeydomain.Key, error) {
	value, release, err := service.Authenticate(ctx, raw)
	if release != nil {
		release()
	}
	return value, err
}

func TestAuthenticationInvalidationRejectsEarlierCacheFills(t *testing.T) {
	for _, mutation := range []string{"disable", "delete", "batch_disable", "batch_delete", "restrict_empty", "account_scope"} {
		t.Run(mutation, func(t *testing.T) {
			service, _, held, resume := newHeldAuthService(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			created, err := service.Create(ctx, CreateInput{Name: "cached", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			oldDone := make(chan error, 1)
			go func() { _, err := authResult(service, ctx, created.Secret); oldDone <- err }()
			awaitAuthRead(t, held)
			disabled := false
			restricted := clientkeydomain.ModelScopeRestricted
			provider := clientkeydomain.ProviderScopeWeb
			tier := clientkeydomain.TierScopeFree
			switch mutation {
			case "disable":
				_, err = service.Update(ctx, created.Key.ID, UpdateInput{Enabled: &disabled})
			case "delete":
				err = service.Delete(ctx, created.Key.ID)
			case "batch_disable":
				_, err = service.BatchSetEnabled(ctx, []uint64{created.Key.ID}, false)
			case "batch_delete":
				_, err = service.BatchDelete(ctx, []uint64{created.Key.ID})
			case "restrict_empty":
				_, err = service.Update(ctx, created.Key.ID, UpdateInput{ModelScope: &restricted})
			case "account_scope":
				_, err = service.Update(ctx, created.Key.ID, UpdateInput{ProviderScope: &provider, TierScope: &tier})
			}
			if err != nil {
				t.Fatal(err)
			}
			check := func() {
				t.Helper()
				value, err := authResult(service, ctx, created.Secret)
				switch mutation {
				case "restrict_empty":
					if err != nil || value.ModelScope != restricted || value.AllowsModel(1) {
						t.Fatalf("new request retained old model authorization: scope=%q, err=%v", value.ModelScope, err)
					}
				case "account_scope":
					if err != nil || value.ProviderScope != provider || value.TierScope != tier {
						t.Fatalf("new request retained old account scope: %+v, %v", value.AccountScope(), err)
					}
				default:
					if !errors.Is(err, ErrInvalidKey) {
						t.Fatalf("new request accepted revoked key: %v", err)
					}
				}
			}
			check() // A newer SQL result can be cached while the old read is held.
			resume()
			if err := <-oldDone; err != nil {
				t.Fatalf("pre-invalidation in-flight request lost its permitted snapshot: %v", err)
			}
			check() // Releasing the old result must not replace the newer policy.
		})
	}
}

func TestAuthenticationCreateInvalidatesEarlierNegativeResult(t *testing.T) {
	for _, oldReturned := range []bool{false, true} {
		t.Run(map[bool]string{false: "in_flight", true: "already_cached"}[oldReturned], func(t *testing.T) {
			service, base, held, resume := newHeldAuthService(t)
			base.SetInvalidationObserver(func(_ context.Context, event repository.InvalidationEvent) { service.ApplyInvalidation(event) })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			prefix := "123456789abc"
			raw := clientkeydomain.FormatClientKey(prefix, "synthetic-test-key")
			oldDone := make(chan error, 1)
			go func() { _, err := authResult(service, ctx, raw); oldDone <- err }()
			awaitAuthRead(t, held)
			if oldReturned {
				resume()
				if err := <-oldDone; !errors.Is(err, ErrInvalidKey) {
					t.Fatal(err)
				}
			}
			if _, err := base.Create(ctx, clientkeydomain.Key{Name: "created", Prefix: prefix, SecretHash: tokenhash.HashToken(raw), EncryptedSecret: "test", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			if !oldReturned {
				resume()
				if err := <-oldDone; !errors.Is(err, ErrInvalidKey) {
					t.Fatal(err)
				}
			}
			if _, err := authResult(service, ctx, raw); err != nil {
				t.Fatalf("created key hidden by stale negative result: %v", err)
			}
		})
	}
}

func TestFailedManagementKeepsUnchangedAuthenticationCacheGeneration(t *testing.T) {
	service, _, held, resume := newHeldAuthService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	created, err := service.Create(ctx, CreateInput{Name: "unchanged", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := authResult(service, ctx, created.Secret); done <- err }()
	awaitAuthRead(t, held)
	unknown := []uint64{999}
	if _, err := service.Update(ctx, created.Key.ID, UpdateInput{AllowedModels: &unknown}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown model management failure: %v", err)
	}
	resume()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if value, err := authResult(service, ctx, created.Secret); err != nil || value.ModelScope != clientkeydomain.ModelScopeAll {
		t.Fatalf("failed change affected authorization: %q, %v", value.ModelScope, err)
	}
	if held.calls.Load() != 1 {
		t.Fatal("failed SQL command invalidated an unchanged cache fill")
	}
}

func TestCancelledAuthenticationReadDoesNotFillCache(t *testing.T) {
	service, _, held, resume := newHeldAuthService(t)
	created, err := service.Create(context.Background(), CreateInput{Name: "cancelled read", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := authResult(service, ctx, created.Secret); done <- err }()
	awaitAuthRead(t, held)
	cancel()
	if err := <-done; !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("cancelled SQL read: %v", err)
	}
	resume()
	if _, err := authResult(service, context.Background(), created.Secret); err != nil {
		t.Fatal(err)
	}
	if held.calls.Load() != 2 {
		t.Fatal("cancelled read populated authorization cache")
	}
}

func TestAuthenticationTTLRecoversMissedNotification(t *testing.T) {
	service, base, held, resume := newHeldAuthService(t)
	resume()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	created, err := service.Create(ctx, CreateInput{Name: "missed notification", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authResult(service, ctx, created.Secret); err != nil {
		t.Fatal(err)
	}
	disabled := false
	// This repository intentionally has no observer, as when the remote event
	// is lost. No cache invalidation call is substituted for the missing event.
	if _, err := base.Patch(ctx, created.Key.ID, clientkeydomain.ManagementPatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := authResult(service, ctx, created.Secret); err != nil || held.calls.Load() != 1 {
		t.Fatalf("cache did not preserve its documented fallback window: %v", err)
	}
	timer := time.NewTimer(keyAuthCacheTTL)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := authResult(service, ctx, created.Secret); !errors.Is(err, ErrInvalidKey) || held.calls.Load() != 2 {
		t.Fatalf("missed revocation did not converge at TTL: %v", err)
	}
}

func TestCachedAuthenticationStillVerifiesCredentialExpiryAndLimits(t *testing.T) {
	for _, rule := range []string{"secret", "expiry", "rpm", "concurrency"} {
		t.Run(rule, func(t *testing.T) {
			service, _, held, resume := newHeldAuthService(t)
			resume()
			service.rateLimiter, service.concurrency = memory.NewRateLimiter(), memory.NewConcurrencyLimiter()
			input := CreateInput{Name: rule, Enabled: true}
			switch rule {
			case "expiry":
				expires := time.Now().Add(500 * time.Millisecond)
				input.ExpiresAt = &expires
			case "rpm":
				input.RPMLimit = 1
			case "concurrency":
				input.MaxConcurrent = 1
			}
			created, err := service.Create(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			_, release, err := service.Authenticate(context.Background(), created.Secret)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			raw, want := created.Secret, ErrInvalidKey
			switch rule {
			case "secret":
				raw = clientkeydomain.FormatClientKey(created.Key.Prefix, "wrong-secret")
			case "expiry":
				timer := time.NewTimer(time.Until(*input.ExpiresAt))
				defer timer.Stop()
				<-timer.C
			case "rpm":
				want = ErrRateLimited
			case "concurrency":
				want = ErrConcurrencyLimit
			}
			if _, err := authResult(service, context.Background(), raw); !errors.Is(err, want) {
				t.Fatalf("cache bypassed %s: %v", rule, err)
			}
			if held.calls.Load() != 1 {
				t.Fatal("verification did not exercise an actual cache hit")
			}
		})
	}
}
