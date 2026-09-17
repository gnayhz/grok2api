package model

import (
	"context"
	"errors"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func backgroundModelFixture(t *testing.T, count int, adapter provider.Adapter) (*Service, []uint64) {
	t.Helper()
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "background-model.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("background-model-test")
	if err != nil {
		t.Fatal(err)
	}
	accounts, models := relational.NewAccountRepository(db), relational.NewModelRepository(db)
	ids := make([]uint64, 0, count)
	for index := range count {
		credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: fmt.Sprintf("model-%d", index), SourceKey: fmt.Sprintf("model-%d", index), EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, credential.ID)
	}
	registry := providerimpl.NewRegistry(adapter)
	accountService := accountapp.NewService(accounts, relational.NewAuditRepository(db), memory.NewDeviceSessionStore(), memory.NewStickyStore(), registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
	s := NewService(models, accounts, accountService, registry)
	s.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := s.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	return s, ids
}

func waitBackgroundModelRun(t *testing.T, s *Service, id uint64) {
	t.Helper()
	s.syncRunMu.RLock()
	run, active := s.accountSyncRuns[id]
	s.syncRunMu.RUnlock()
	if !active {
		return
	}
	select {
	case <-run.done:
	case <-time.After(3 * time.Second):
		t.Fatal("background model refresh did not release ownership")
	}
}

func TestBackgroundAccountSyncSharesRefreshAndJoinsFinalWrite(t *testing.T) {
	adapter := &modelCapabilityAdapter{entered: make(chan struct{}, 2), release: make(chan struct{})}
	s, ids := backgroundModelFixture(t, 1, adapter)
	finalizer := blockedSyncFinalizer{ModelRepository: s.models, entered: make(chan struct{}), release: make(chan struct{}), result: make(chan error, 1)}
	s.models = finalizer
	var once sync.Once
	defer once.Do(func() { close(finalizer.release) })
	var callers sync.WaitGroup
	for range 32 {
		callers.Go(func() {
			if !s.QueueAccountSync(ids[0]) {
				t.Error("active service rejected account refresh")
			}
		})
	}
	callers.Wait()
	select {
	case <-adapter.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("account refresh did not reach Provider")
	}
	if adapter.attemptCount() != 1 {
		t.Fatalf("duplicate catalog calls=%d", adapter.attemptCount())
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unfinished failure write released ownership: %v", err)
	}
	select {
	case <-finalizer.entered:
	case <-time.After(time.Second):
		t.Fatal("canceled refresh did not enter failure persistence")
	}
	if s.QueueAccountSync(ids[0]) {
		t.Fatal("closing service accepted account refresh")
	}
	closed := make(chan error, 8)
	for range cap(closed) {
		go func() { closed <- s.Close(context.Background()) }()
	}
	once.Do(func() { close(finalizer.release) })
	if err := <-finalizer.result; err != nil {
		t.Fatal(err)
	}
	for range cap(closed) {
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	}
	s.syncRunMu.RLock()
	remaining := len(s.accountSyncRuns)
	s.syncRunMu.RUnlock()
	if remaining != 0 || s.bulkPool.Snapshot().Active != 0 {
		t.Fatal("completed refresh retained run or pool ownership")
	}
}

func TestBackgroundAccountSyncCancelsQueuedSharedPoolWork(t *testing.T) {
	adapter := &modelCapabilityAdapter{entered: make(chan struct{}, 8), release: make(chan struct{})}
	s, ids := backgroundModelFixture(t, 8, adapter)
	s.SetBulkPool(batch.NewPool(1))
	if !s.QueueAccountSync(ids[0]) {
		t.Fatal("first refresh rejected")
	}
	select {
	case <-adapter.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first refresh did not hold shared pool")
	}
	for _, id := range ids[1:] {
		if !s.QueueAccountSync(id) {
			t.Fatal("queued refresh rejected")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if adapter.attemptCount() != 1 || s.bulkPool.Snapshot().Active != 0 || s.bulkPool.Snapshot().Queued != 0 {
		t.Fatalf("close left work or started a queued catalog: calls=%d pool=%+v", adapter.attemptCount(), s.bulkPool.Snapshot())
	}
}

type panicBackgroundCatalog struct {
	*modelCapabilityAdapter
	calls atomic.Int32
}

func (a *panicBackgroundCatalog) ListModels(context.Context, account.Credential) ([]string, error) {
	if a.calls.Add(1) == 1 {
		panic("catalog fixture panic")
	}
	return []string{"grok-4.5"}, nil
}

func TestBackgroundAccountSyncPanicReleasesRunForNextHint(t *testing.T) {
	adapter := &panicBackgroundCatalog{modelCapabilityAdapter: &modelCapabilityAdapter{}}
	s, ids := backgroundModelFixture(t, 1, adapter)
	if !s.QueueAccountSync(ids[0]) {
		t.Fatal("first refresh rejected")
	}
	waitBackgroundModelRun(t, s, ids[0])
	if !s.QueueAccountSync(ids[0]) {
		t.Fatal("panic retained account refresh ownership")
	}
	waitBackgroundModelRun(t, s, ids[0])
	if adapter.calls.Load() != 2 {
		t.Fatalf("retry calls=%d", adapter.calls.Load())
	}
	if synced, err := s.HasSuccessfulAccountSync(context.Background(), ids[0]); err != nil || !synced {
		t.Fatalf("next refresh did not publish: synced=%t err=%v", synced, err)
	}
}

func TestBackgroundAccountSyncRejectsAfterClose(t *testing.T) {
	s := NewService(nil, nil, nil, nil)
	if s.QueueAccountSync(0) {
		t.Fatal("zero account accepted")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var callers sync.WaitGroup
	for id := uint64(1); id <= 32; id++ {
		callers.Go(func() {
			if s.QueueAccountSync(id) {
				t.Error("closed service accepted detached work")
			}
		})
	}
	callers.Wait()
}

func TestBackgroundAccountSyncUsesSharedCapacityAndCancelsWaitingInstance(t *testing.T) {
	for _, runtime := range []string{"memory", "redis"} {
		t.Run(runtime, func(t *testing.T) {
			ctx := context.Background()
			var firstLimiter, secondLimiter repository.ConcurrencyLimiter
			if runtime == "redis" {
				address := os.Getenv("TEST_REDIS_ADDRESS")
				if address == "" {
					t.Skip("requires isolated TEST_REDIS_ADDRESS")
				}
				cfg := redisruntime.Config{Address: address, KeyPrefix: fmt.Sprintf("g77-model:%d:", time.Now().UnixNano())}
				first, err := redisruntime.Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = first.Close() })
				second, err := redisruntime.Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = second.Close() })
				firstLimiter, secondLimiter = redisruntime.NewConcurrencyLimiter(first), redisruntime.NewConcurrencyLimiter(second)
			} else {
				firstLimiter = memory.NewConcurrencyLimiter()
				secondLimiter = firstLimiter
			}
			adapter := &modelCapabilityAdapter{entered: make(chan struct{}, 2), release: make(chan struct{})}
			first, ids := backgroundModelFixture(t, 2, adapter)
			second := NewService(first.models, first.accounts, first.account, first.providers)
			second.SetLogger(first.logger)
			t.Cleanup(func() { _ = second.Close(context.Background()) })
			const key = "bulk:sync"
			first.SetBulkPool(batch.NewSharedPool(1, firstLimiter, key))
			second.SetBulkPool(batch.NewSharedPool(1, secondLimiter, key))
			if !first.QueueAccountSync(ids[0]) {
				t.Fatal("first instance rejected refresh")
			}
			select {
			case <-adapter.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("first instance did not acquire shared capacity")
			}
			if !second.QueueAccountSync(ids[1]) {
				t.Fatal("second instance rejected refresh")
			}
			deadline := time.Now().Add(3 * time.Second)
			for second.bulkPool.Snapshot().Queued != 1 {
				if time.Now().After(deadline) {
					t.Fatal("second instance did not enter pool wait")
				}
				time.Sleep(time.Millisecond)
			}
			closeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if err := second.Close(closeCtx); err != nil {
				t.Fatal(err)
			}
			if count, err := secondLimiter.Current(ctx, key); err != nil || count != 1 || adapter.attemptCount() != 1 {
				t.Fatalf("waiting instance disturbed active capacity: count=%d calls=%d err=%v", count, adapter.attemptCount(), err)
			}
			if err := first.Close(closeCtx); err != nil {
				t.Fatal(err)
			}
			if count, err := secondLimiter.Current(ctx, key); err != nil || count != 0 {
				t.Fatalf("active instance retained capacity: count=%d err=%v", count, err)
			}
		})
	}
}
