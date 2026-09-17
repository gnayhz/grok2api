package account

import (
	"context"
	"errors"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestCredentialCommittedAfterCancellationIsSharedWithoutAnotherRotation(t *testing.T) {
	s, value, base := newCredentialRefreshTestService(t, time.Now())
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	ownerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.providers = providerimpl.NewRegistry(operationRefreshAdapter{base, func(ctx context.Context, v accountdomain.Credential) (provider.RefreshedCredential, error) {
		close(entered)
		<-release
		cancel()
		return base.RefreshCredential(ctx, v)
	}})
	owner := make(chan error, 1)
	go func() { _, err := s.EnsureCredential(ownerCtx, value, true); owner <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not start")
	}
	waitCtx := &identityWaitContext{Context: context.Background(), waiting: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() {
		current, err := s.EnsureCredential(waitCtx, value, true)
		if err == nil && current.EncryptedAccessToken != "access-1" {
			err = errors.New("waiter did not receive committed material")
		}
		waiter <- err
	}()
	select {
	case <-waitCtx.waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not join")
	}
	finish()
	if err := awaitIdentityResult(t, owner); err != nil {
		t.Fatal(err)
	}
	if err := awaitIdentityResult(t, waiter); err != nil {
		t.Fatal(err)
	}
	if base.refreshCount.Load() != 1 {
		t.Fatal("committed canceled owner triggered an extra token rotation")
	}
	stored, err := s.accounts.Get(context.Background(), value.ID)
	if err != nil || stored.EncryptedRefreshToken != "refresh-1" {
		t.Fatalf("rotated token not saved: %v", err)
	}
}

func TestAccountOperationGoexitReleasesWaiters(t *testing.T) {
	var group OperationGroup[string]
	entered, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		_, _ = group.Do(context.Background(), "goexit", func() (any, error) { close(entered); <-release; runtime.Goexit(); return nil, nil })
	}()
	<-entered
	waitCtx := &identityWaitContext{Context: context.Background(), waiting: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { _, err := group.Do(waitCtx, "goexit", func() (any, error) { return nil, nil }); waiter <- err }()
	select {
	case <-waitCtx.waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not join")
	}
	close(release)
	<-stopped
	if err := awaitIdentityResult(t, waiter); err == nil {
		t.Fatal("interrupted owner reported success")
	}
	if _, err := group.Do(context.Background(), "goexit", func() (any, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
}

type operationRefreshAdapter struct {
	*credentialRefreshAdapter
	refresh func(context.Context, accountdomain.Credential) (provider.RefreshedCredential, error)
}

func (a operationRefreshAdapter) RefreshCredential(ctx context.Context, v accountdomain.Credential) (provider.RefreshedCredential, error) {
	return a.refresh(ctx, v)
}

func TestCredentialOperationWaitsForCanceledOwnerCleanupBeforeTakingOver(t *testing.T) {
	s, value, base := newCredentialRefreshTestService(t, time.Now())
	entered, cleaning, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	var calls atomic.Int32
	s.providers = providerimpl.NewRegistry(operationRefreshAdapter{base, func(ctx context.Context, v accountdomain.Credential) (provider.RefreshedCredential, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			close(cleaning)
			<-release
			return provider.RefreshedCredential{}, ctx.Err()
		}
		return base.RefreshCredential(ctx, v)
	}})
	ownerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := make(chan error, 1)
	go func() { _, err := s.EnsureCredential(ownerCtx, value, true); owner <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("owner never called Provider")
	}
	waitCtx := &identityWaitContext{Context: context.Background(), waiting: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { _, err := s.EnsureCredential(waitCtx, value, true); waiter <- err }()
	select {
	case <-waitCtx.waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("live waiter never joined")
	}
	cancel()
	<-cleaning
	select {
	case err := <-owner:
		t.Fatalf("owner returned before cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if calls.Load() != 1 {
		t.Fatal("waiter started before owner cleanup")
	}
	finish()
	if err := awaitIdentityResult(t, owner); !errors.Is(err, context.Canceled) {
		t.Fatalf("owner=%v", err)
	}
	if err := awaitIdentityResult(t, waiter); err != nil {
		t.Fatal(err)
	}
	stored, err := s.accounts.Get(context.Background(), value.ID)
	if err != nil || stored.EncryptedAccessToken != "access-1" || stored.CredentialGeneration != value.CredentialGeneration+1 || calls.Load() != 2 {
		t.Fatalf("live takeover did not commit current material: %v calls=%d", err, calls.Load())
	}
}

func TestCredentialCanceledOwnerDoesNotGrantWaiterPermanentRetry(t *testing.T) {
	s, value, base := newCredentialRefreshTestService(t, time.Now())
	base.refreshErr = &provider.CredentialRefreshError{Status: 400, Code: "invalid_grant", Message: "refresh expired", Permanent: true}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	ownerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.providers = providerimpl.NewRegistry(operationRefreshAdapter{base, func(ctx context.Context, v accountdomain.Credential) (provider.RefreshedCredential, error) {
		close(entered)
		<-release
		cancel()
		return base.RefreshCredential(ctx, v)
	}})
	owner := make(chan error, 1)
	go func() { _, err := s.EnsureCredential(ownerCtx, value, true); owner <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not start")
	}
	waitCtx := &identityWaitContext{Context: context.Background(), waiting: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { _, err := s.EnsureCredential(waitCtx, value, true); waiter <- err }()
	select {
	case <-waitCtx.waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not join")
	}
	finish()
	if err := awaitIdentityResult(t, owner); !errors.Is(err, context.Canceled) {
		t.Fatalf("owner=%v", err)
	}
	if err := awaitIdentityResult(t, waiter); !errors.Is(err, ErrCredentialRefreshPermanent) {
		t.Fatalf("waiter bypassed permanent failure: %v", err)
	}
	current, err := s.accounts.Get(context.Background(), value.ID)
	if err != nil || !current.RefreshPermanent || current.LastRefreshErrorCode != "invalid_grant" || current.RefreshFailureCount != 1 || base.refreshCount.Load() != 1 {
		t.Fatalf("automatic takeover repeated terminal Provider failure: %v calls=%d", err, base.refreshCount.Load())
	}
}

type observedWriteGate struct {
	repository.AccountRepository
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (r *observedWriteGate) UpdateObservedModelIfNewer(ctx context.Context, id uint64, model string, at time.Time) (bool, error) {
	if r.calls.Add(1) == 1 {
		close(r.entered)
		<-r.release
	}
	return r.AccountRepository.(repository.ObservedModelWriter).UpdateObservedModelIfNewer(ctx, id, model, at)
}

func TestObservedModelSharedWriteWaiterCancelsWithoutLosingOwnerCommit(t *testing.T) {
	s, value, _ := newCredentialRefreshTestService(t, time.Now())
	repo := &observedWriteGate{AccountRepository: s.accounts, entered: make(chan struct{}), release: make(chan struct{})}
	s.accounts = repo
	var once sync.Once
	finish := func() { once.Do(func() { close(repo.release) }) }
	defer finish()
	owner := make(chan error, 1)
	go func() { owner <- s.ObserveResponseModel(context.Background(), value.ID, "grok-test") }()
	select {
	case <-repo.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not reach actual model writer")
	}
	waitCtx, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	waiter := make(chan error, 1)
	go func() { waiter <- s.ObserveResponseModel(waitCtx, value.ID, "grok-test") }()
	if err := awaitIdentityResult(t, waiter); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter=%v", err)
	}
	finish()
	if err := awaitIdentityResult(t, owner); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(context.Background(), value.ID)
	if err != nil || stored.ObservedModel != "grok-test" || repo.calls.Load() != 1 {
		t.Fatalf("owner model observation lost: %v calls=%d", err, repo.calls.Load())
	}
}

func TestQuotaFullModeAndProbeWaitersCancelIndependently(t *testing.T) {
	for _, operation := range []string{"full", "mode", "probe"} {
		t.Run(operation, func(t *testing.T) {
			s, repo, _ := newWebAccountSettingsTestService(t)
			v, _, err := repo.UpsertByIdentity(context.Background(), accountdomain.Credential{Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO, Name: "console", SourceKey: "console", EncryptedAccessToken: "synthetic", AuthStatus: accountdomain.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			a := &consoleQuotaSnapshotAdapter{fullStarted: make(chan struct{}, 1), fullRelease: make(chan struct{})}
			s.providers = providerimpl.NewRegistry(a)
			var once sync.Once
			finish := func() { once.Do(func() { close(a.fullRelease) }) }
			defer finish()
			owner := make(chan error, 1)
			go func() { _, err := s.RefreshQuota(context.Background(), v.ID); owner <- err }()
			select {
			case <-a.fullStarted:
			case <-time.After(3 * time.Second):
				t.Fatal("full quota owner did not start")
			}
			waitCtx, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer stop()
			waiter := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "full":
					_, err = s.RefreshQuota(waitCtx, v.ID)
				case "mode":
					_, err = s.RefreshQuotaMode(waitCtx, v.ID, "console")
				case "probe":
					_, err = s.ProbeQuotaMode(waitCtx, v.ID, "console")
				}
				waiter <- err
			}()
			if err := awaitIdentityResult(t, waiter); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("waiter=%v", err)
			}
			finish()
			if err := awaitIdentityResult(t, owner); err != nil {
				t.Fatal(err)
			}
			windows, err := repo.GetQuotaWindows(context.Background(), []uint64{v.ID})
			if err != nil || !completeConsoleUsageSnapshot(windows[v.ID]) || a.fullCalls.Load() != 1 {
				t.Fatalf("canceled waiter changed owner quota: %v calls=%d", err, a.fullCalls.Load())
			}
		})
	}
}
