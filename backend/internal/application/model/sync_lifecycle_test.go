package model

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type blockedSyncFinalizer struct {
	repository.ModelRepository
	entered, release chan struct{}
	result           chan error
}

func (r blockedSyncFinalizer) CompleteAccountCapabilitySync(ctx context.Context, ref modeldomain.CapabilitySyncRef, result modeldomain.CapabilitySyncResult) error {
	close(r.entered)
	select {
	case <-r.release:
	case <-ctx.Done():
	}
	err := r.ModelRepository.CompleteAccountCapabilitySync(ctx, ref, result)
	r.result <- err
	return err
}

func TestFullSyncCloseRetainsOwnershipUntilFinalWrite(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "sync-close.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("sync-close")
	if err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(db)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "sync-close", SourceKey: "sync-close", EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &modelCapabilityAdapter{models: map[uint64][]string{credential.ID: {"grok-close"}}, entered: make(chan struct{}), release: make(chan struct{})}
	providers := provider.NewRegistry(adapter)
	accountService := accountapp.NewService(accounts, relational.NewAuditRepository(db), memory.NewDeviceSessionStore(), memory.NewStickyStore(), providers, cipher, nil)
	finalizer := blockedSyncFinalizer{ModelRepository: relational.NewModelRepository(db), entered: make(chan struct{}), release: make(chan struct{}), result: make(chan error, 1)}
	service := NewService(finalizer, accounts, accountService, providers)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(finalizer.release) })
	watchers := make(chan error, 8)
	for range cap(watchers) {
		go func() { _, err := service.SyncObserved(ctx, nil); watchers <- err }()
	}
	select {
	case <-adapter.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("full sync did not start")
	}
	closeCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := service.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unfinished final write did not retain ownership: %v", err)
	}
	if !service.SyncProgress().Active {
		t.Fatal("unfinished final write reported inactive")
	}
	if _, err := service.SyncObserved(ctx, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("closing service accepted work: %v", err)
	}
	select {
	case <-finalizer.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled run did not persist its failure")
	}
	closed := make(chan error, 2)
	for range cap(closed) {
		go func() { closed <- service.Close(ctx) }()
	}
	releaseOnce.Do(func() { close(finalizer.release) })
	if err := <-finalizer.result; err != nil {
		t.Fatalf("final write after close timeout: %v", err)
	}
	for range cap(closed) {
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	}
	for range cap(watchers) {
		if err := <-watchers; !errors.Is(err, context.Canceled) && !errors.Is(err, ErrClosed) {
			t.Fatalf("watcher result: %v", err)
		}
	}
	if adapter.attemptCount() != 1 || service.SyncProgress().Active || service.SyncProgress().Err == nil {
		t.Fatalf("shared close state: attempts=%d snapshot=%+v", adapter.attemptCount(), service.SyncProgress())
	}
}

func TestClosedOrCanceledFullSyncStartsNoWork(t *testing.T) {
	// Nil dependencies make accidental work visible, including a delayed
	// singleflight goroutine that might otherwise begin after Close returned.
	service := NewService(nil, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.SyncObserved(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled watcher started a run: %v", err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for range 32 {
		wait.Go(func() {
			if _, err := service.SyncObserved(context.Background(), nil); !errors.Is(err, ErrClosed) {
				t.Errorf("closed service result: %v", err)
			}
		})
	}
	wait.Wait()
	if service.SyncProgress().Active {
		t.Fatal("closed service has an active run")
	}
}
