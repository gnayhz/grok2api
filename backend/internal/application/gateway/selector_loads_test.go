package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// The gate runs the actual repository query first. Only its first return is
// delayed, making sharing/cancellation deterministic without replacing SQL.
type routingReadGate struct {
	*relational.AccountRepository
	layer   string
	entered chan struct{}
	resume  chan struct{}
	reads   atomic.Int32
	failure error
	abort   string
}

func (r *routingReadGate) afterRead(ctx context.Context, layer string, err error) error {
	if err != nil || layer != r.layer || r.reads.Add(1) != 1 {
		return err
	}
	close(r.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.resume:
	}
	switch r.abort {
	case "panic":
		panic("routing loader fixture")
	case "goexit":
		runtime.Goexit()
	}
	return r.failure
}

func (r *routingReadGate) ListRoutingAccountBases(ctx context.Context, provider account.Provider, mode string) ([]account.RoutingAccountBase, error) {
	values, err := r.AccountRepository.ListRoutingAccountBases(ctx, provider, mode)
	return values, r.afterRead(ctx, "base", err)
}

func (r *routingReadGate) ListRoutingAccountOverlays(ctx context.Context, provider account.Provider, route uint64, model string) (account.RoutingOverlaySnapshot, error) {
	values, err := r.AccountRepository.ListRoutingAccountOverlays(ctx, provider, route, model)
	return values, r.afterRead(ctx, "overlay", err)
}

func (r *routingReadGate) ListRoutingCandidates(ctx context.Context, provider account.Provider, route uint64, model, mode string) ([]account.RoutingCandidate, error) {
	values, err := r.AccountRepository.ListRoutingCandidates(ctx, provider, route, model, mode)
	return values, r.afterRead(ctx, "combined", err)
}

// Err does not signal. Done signals when the follower enters its cancellable
// wait, avoiding sleeps and scheduler-dependent assumptions about joining.
type routingWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *routingWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func awaitRoutingSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("routing operation did not reach expected stage")
	}
}

func awaitRoutingError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("routing operation did not return")
		return nil
	}
}

func openRoutingCancellationDB(t *testing.T, dialect string) *relational.Database {
	t.Helper()
	var db *relational.Database
	var err error
	if dialect == "postgres" {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("isolated TEST_POSTGRES_DSN required")
		}
		db, err = relational.OpenPostgres(context.Background(), dsn, 8, 4)
	} else {
		db, err = relational.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "routing-cancel.db"))
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

func routingCancellationAccount(t *testing.T, repo *relational.AccountRepository) account.Credential {
	t.Helper()
	value, _, err := repo.UpsertByIdentity(context.Background(), account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierBasic,
		Name: "routing cancel", SourceKey: fmt.Sprintf("routing-cancel-%d", time.Now().UnixNano()),
		EncryptedAccessToken: "synthetic", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Delete(context.Background(), value.ID); err != nil {
			t.Error(err)
		}
	})
	return value
}

func TestSelectorSharedReadCancellation(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := openRoutingCancellationDB(t, dialect)
			repo := relational.NewAccountRepository(db)
			routingCancellationAccount(t, repo)
			for _, entry := range []string{"combined", "assembled", "base", "overlay"} {
				t.Run(entry, func(t *testing.T) {
					for _, scenario := range []string{"pre_canceled", "waiter_canceled", "owner_canceled", "all_canceled", "result_and_cancel", "shared_success", "shared_error", "panic", "goexit"} {
						t.Run(scenario, func(t *testing.T) {
							gate := &routingReadGate{AccountRepository: repo, layer: entry, entered: make(chan struct{}), resume: make(chan struct{})}
							if entry == "assembled" {
								gate.layer = "base"
							}
							storageErr := errors.New("routing storage unavailable")
							if scenario == "shared_error" {
								gate.failure = storageErr
							}
							if scenario == "panic" || scenario == "goexit" {
								gate.abort = scenario
							}
							var release sync.Once
							unblock := func() { release.Do(func() { close(gate.resume) }) }
							t.Cleanup(unblock)
							selector := NewSelector(gate, memory.NewConcurrencyLimiter(), nil, nil, time.Hour, time.Second, time.Minute)
							load := func(ctx context.Context) error {
								now := time.Now().UTC()
								switch entry {
								case "combined":
									_, err := selector.loadCombinedCandidates(ctx, account.ProviderWeb, 0, "", "", now)
									return err
								case "assembled":
									_, err := selector.loadLayeredCandidates(ctx, account.ProviderWeb, 0, "", "", now)
									return err
								case "base":
									_, _, err := selector.loadRoutingBases(ctx, gate, account.ProviderWeb, "", now)
									return err
								default:
									_, _, err := selector.loadRoutingOverlay(ctx, gate, account.ProviderWeb, 0, "", now)
									return err
								}
							}
							ownerCtx, cancelOwner := context.WithCancel(context.Background())
							defer cancelOwner()
							owner := make(chan error, 1)
							go func() {
								err := errRoutingLoadInterrupted
								defer func() {
									if recovered := recover(); recovered != nil && recovered != "routing loader fixture" {
										err = fmt.Errorf("unexpected panic: %v", recovered)
									}
									owner <- err
								}()
								err = load(ownerCtx)
							}()
							awaitRoutingSignal(t, gate.entered)
							waiters := make([]chan error, 3)
							cancels := make([]context.CancelFunc, len(waiters))
							for index := range waiters {
								ctx, cancel := context.WithCancel(context.Background())
								cancels[index] = cancel
								defer cancel()
								if scenario == "pre_canceled" && index == 0 {
									cancel()
								}
								tracked := &routingWaitContext{Context: ctx, waiting: make(chan struct{})}
								waiters[index] = make(chan error, 1)
								go func(result chan error) { result <- load(tracked) }(waiters[index])
								if scenario != "pre_canceled" || index != 0 {
									awaitRoutingSignal(t, tracked.waiting)
								}
							}
							canceledWaiters := 0
							switch scenario {
							case "pre_canceled", "waiter_canceled", "result_and_cancel":
								canceledWaiters = 1
							case "all_canceled":
								canceledWaiters = len(waiters)
							}
							for index := range canceledWaiters {
								cancels[index]()
							}
							if scenario == "result_and_cancel" {
								unblock()
							}
							for index := range canceledWaiters {
								if err := awaitRoutingError(t, waiters[index]); !errors.Is(err, context.Canceled) {
									t.Fatalf("canceled waiter: %v", err)
								}
							}
							if scenario == "owner_canceled" || scenario == "all_canceled" {
								cancelOwner()
							} else {
								unblock()
							}
							ownerErr := awaitRoutingError(t, owner)
							var wantOwner, wantWaiter error
							wantReads := int32(1)
							switch scenario {
							case "owner_canceled":
								wantOwner, wantReads = context.Canceled, 2
							case "all_canceled":
								wantOwner = context.Canceled
							case "shared_error":
								wantOwner, wantWaiter = storageErr, storageErr
							case "panic", "goexit":
								wantOwner, wantWaiter = errRoutingLoadInterrupted, errRoutingLoadInterrupted
							}
							if !errors.Is(ownerErr, wantOwner) {
								t.Fatalf("owner: %v, want %v", ownerErr, wantOwner)
							}
							for _, result := range waiters[canceledWaiters:] {
								if err := awaitRoutingError(t, result); !errors.Is(err, wantWaiter) {
									t.Fatalf("live waiter: %v, want %v", err, wantWaiter)
								}
							}
							if got := gate.reads.Load(); got != wantReads {
								t.Fatalf("actual SQL reads: %d, want %d", got, wantReads)
							}
							if db.Stats().InUse != 0 {
								t.Fatal("SQL connection remains in use after all callers returned")
							}
							// After an error or aborted owner, the next independent request
							// must be able to load, proving cleanup and no cached failure.
							unblock()
							if err := load(context.Background()); err != nil {
								t.Fatalf("next request: %v", err)
							}
							reads := gate.reads.Load()
							if err := load(context.Background()); err != nil || gate.reads.Load() != reads {
								t.Fatalf("warm request queried SQL or failed: %v", err)
							}
						})
					}
				})
			}
		})
	}
}

func TestSelectorCanceledSQLReadReleasesConnection(t *testing.T) {
	db := openRoutingCancellationDB(t, "postgres")
	repo := relational.NewAccountRepository(db)
	credential := routingCancellationAccount(t, repo)
	control, err := sql.Open("pgx", os.Getenv("TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	for _, scenario := range []string{"waiter_canceled", "owner_canceled", "all_canceled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			lock, err := control.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback()
			if _, err := lock.ExecContext(ctx, "LOCK TABLE provider_accounts IN ACCESS EXCLUSIVE MODE"); err != nil {
				t.Fatal(err)
			}
			concurrency := memory.NewConcurrencyLimiter()
			selector := NewSelector(repo, concurrency, nil, nil, time.Hour, time.Second, time.Minute)
			acquire := func(ctx context.Context, result chan error) {
				lease, err := selector.AcquirePinned(ctx, credential.Provider, credential.ID, 0, "", "", false)
				if lease != nil {
					lease.Release()
				}
				result <- err
			}
			ownerCtx, cancelOwner := context.WithCancel(ctx)
			defer cancelOwner()
			owner := make(chan error, 1)
			go acquire(ownerCtx, owner)
			// Observe a real PostgreSQL lock wait before starting the follower.
			for {
				var waiting bool
				err := control.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%provider_accounts%' AND pid <> pg_backend_pid())").Scan(&waiting)
				if err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err := <-owner:
					t.Fatalf("owner returned before SQL lock wait: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			followerCtx, cancelFollower := context.WithCancel(ctx)
			defer cancelFollower()
			tracked := &routingWaitContext{Context: followerCtx, waiting: make(chan struct{})}
			follower := make(chan error, 1)
			go acquire(tracked, follower)
			awaitRoutingSignal(t, tracked.waiting)
			if scenario != "owner_canceled" {
				cancelFollower()
				if err := awaitRoutingError(t, follower); !errors.Is(err, context.Canceled) {
					t.Fatalf("follower cancellation while SQL blocked: %v", err)
				}
			}
			if scenario != "waiter_canceled" {
				cancelOwner()
				if err := awaitRoutingError(t, owner); !errors.Is(err, context.Canceled) {
					t.Fatalf("owner SQL cancellation: %v", err)
				}
			}
			if scenario == "all_canceled" && db.Stats().InUse != 0 {
				t.Fatal("all callers returned before their SQL connections were released")
			}
			if err := lock.Rollback(); err != nil {
				t.Fatal(err)
			}
			if scenario == "waiter_canceled" {
				if err := awaitRoutingError(t, owner); err != nil {
					t.Fatalf("live owner: %v", err)
				}
			}
			if scenario == "owner_canceled" {
				if err := awaitRoutingError(t, follower); err != nil {
					t.Fatalf("live follower takeover: %v", err)
				}
			}
			if db.Stats().InUse != 0 {
				t.Fatal("completed callers retained SQL connections")
			}
			current, err := concurrency.Current(ctx, repository.AccountConcurrencyKey(credential.ID))
			if err != nil || current != 0 {
				t.Fatalf("account capacity leaked: %d %v", current, err)
			}
		})
	}
}

func TestSelectorSharedBaseCancellationRetainsInvalidation(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := openRoutingCancellationDB(t, dialect)
			repo := relational.NewAccountRepository(db)
			credential := routingCancellationAccount(t, repo)
			gate := &routingReadGate{AccountRepository: repo, layer: "base", entered: make(chan struct{}), resume: make(chan struct{})}
			defer close(gate.resume)
			a := NewSelector(gate, nil, nil, nil, time.Hour, time.Second, time.Minute)
			b := NewSelector(repo, nil, nil, nil, time.Hour, time.Second, time.Minute)
			peer := relational.NewAccountRepository(db)
			peer.SetInvalidationObserver(func(_ context.Context, event repository.InvalidationEvent) {
				a.ApplyInvalidation(event)
				b.ApplyInvalidation(event)
			})
			load := func(ctx context.Context, s *Selector, model string) (int, error) {
				values, err := s.loadCandidates(ctx, credential.Provider, 0, model, "", time.Now().UTC())
				if err != nil {
					return 0, err
				}
				for _, value := range values {
					if value.Credential.ID == credential.ID {
						return value.Credential.Priority, nil
					}
				}
				return 0, errors.New("routing candidate missing")
			}
			if _, err := load(context.Background(), b, "model-b"); err != nil {
				t.Fatal(err)
			}
			ownerCtx, cancelOwner := context.WithCancel(context.Background())
			defer cancelOwner()
			owner := make(chan error, 1)
			go func() { _, err := load(ownerCtx, a, "model-a"); owner <- err }()
			awaitRoutingSignal(t, gate.entered)
			followerCtx := &routingWaitContext{Context: context.Background(), waiting: make(chan struct{})}
			follower := make(chan error, 1)
			priority := 17
			go func() {
				got, err := load(followerCtx, a, "model-b")
				if err == nil && got != priority {
					err = fmt.Errorf("shared base returned old priority: %d", got)
				}
				follower <- err
			}()
			awaitRoutingSignal(t, followerCtx.waiting)
			if _, err := peer.UpdateAdministration(context.Background(), credential.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Priority: &priority}}); err != nil {
				t.Fatal(err)
			}
			cancelOwner()
			if err := awaitRoutingError(t, owner); !errors.Is(err, context.Canceled) {
				t.Fatalf("old owner: %v", err)
			}
			if err := awaitRoutingError(t, follower); err != nil {
				t.Fatal(err)
			}
			for _, s := range []*Selector{a, b} {
				for _, model := range []string{"model-a", "model-b"} {
					if got, err := load(context.Background(), s, model); err != nil || got != priority {
						t.Fatalf("next model reused old generation: priority=%d error=%v", got, err)
					}
				}
			}
			if got := gate.reads.Load(); got != 2 {
				t.Fatalf("model takeover base reads=%d, want 2", got)
			}
		})
	}
}
