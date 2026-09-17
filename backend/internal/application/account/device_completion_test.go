package account

import (
	"context"
	"errors"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

type deviceCompletionAdapter struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
	after   func()
	pending bool
}

func (*deviceCompletionAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderBuild
}
func (*deviceCompletionAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: accountdomain.ProviderBuild, Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeOAuth, DeviceOAuth: true}}
}
func (*deviceCompletionAdapter) StartDeviceAuthorization(context.Context) (provider.DeviceAuthorization, error) {
	return provider.DeviceAuthorization{}, errors.New("not used")
}
func (a *deviceCompletionAdapter) PollDeviceAuthorization(context.Context, string) (provider.CredentialSeed, error) {
	a.calls.Add(1)
	if a.entered != nil {
		a.entered <- struct{}{}
		<-a.release
	}
	if a.after != nil {
		a.after()
	}
	if a.pending {
		return provider.CredentialSeed{}, provider.ErrAuthorizationPending
	}
	return provider.CredentialSeed{Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "device-grant", SourceKey: "device-grant", AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func deviceCompletionService(t *testing.T, a *deviceCompletionAdapter) (*Service, *relational.AccountRepository, *memory.DeviceSessionStore) {
	t.Helper()
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "device.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(db)
	store := memory.NewDeviceSessionStore()
	if err := store.Create(ctx, accountdomain.DeviceSession{ID: "device", DeviceCode: "synthetic-device", Interval: time.Second, NextPollAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	return NewService(repo, relational.NewAuditRepository(db), store, nil, providerimpl.NewRegistry(a), cipher, security.RandomTokenSource{}, nil, nil, nil), repo, store
}
func TestDevicePollingOwnsOneGrant(t *testing.T) {
	adapter := &deviceCompletionAdapter{entered: make(chan struct{}, 2), release: make(chan struct{}), pending: true}
	first, repo, store := deviceCompletionService(t, adapter)
	second := NewService(repo, nil, store, nil, providerimpl.NewRegistry(adapter), first.cipher, security.RandomTokenSource{}, nil, nil, nil)
	var once sync.Once
	release := func() { once.Do(func() { close(adapter.release) }) }
	defer release()
	owner := make(chan error, 1)
	waiter := make(chan error, 1)
	go func() { _, err := first.PollDeviceLogin(context.Background(), "device"); owner <- err }()
	select {
	case <-adapter.entered:
	case <-time.After(time.Second):
		t.Fatal("first poll did not start")
	}
	go func() { _, err := second.PollDeviceLogin(context.Background(), "device"); waiter <- err }()
	secondFinished := false
	select {
	case <-adapter.entered:
	case <-waiter:
		secondFinished = true
	case <-time.After(time.Second):
		t.Error("second call did not settle")
	}
	release()
	<-owner
	if !secondFinished {
		<-waiter
	}
	if got := adapter.calls.Load(); got != 1 {
		t.Errorf("one due device session issued %d concurrent upstream polls; want one", got)
	}
}
func TestDeviceKnownGrantSurvivesClientCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter := &deviceCompletionAdapter{after: cancel}
	service, repo, _ := deviceCompletionService(t, adapter)
	_, callErr := service.PollDeviceLogin(ctx, "device")
	values, err := repo.ListEnabled(context.Background(), accountdomain.ProviderBuild)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 {
		t.Fatalf("known issued grant lost after cancellation: accounts=%d service_error=%v", len(values), callErr)
	}
	if values[0].EncryptedRefreshToken == "" {
		t.Fatal("known grant refresh material missing")
	}
}

type conversionCompletionAdapter struct{ after func() }

func (*conversionCompletionAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderWeb
}
func (a *conversionCompletionAdapter) ConvertToBuild(context.Context, accountdomain.Credential) (provider.CredentialSeed, error) {
	a.after()
	return provider.CredentialSeed{Name: "converted", SourceKey: "converted", AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func TestConversionKnownGrantSurvivesClientCancellation(t *testing.T) {
	service, repo, _ := deviceCompletionService(t, &deviceCompletionAdapter{})
	v, _, err := repo.UpsertByIdentity(context.Background(), accountdomain.Credential{Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "web", EncryptedAccessToken: "synthetic", AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.providers = providerimpl.NewRegistry(&conversionCompletionAdapter{after: cancel})
	service.refreshLock = memory.NewLockStore()
	_, _, _, callErr := service.convertWebAccountToBuild(ctx, v.ID, BuildConversionAll)
	values, err := repo.ListEnabled(context.Background(), accountdomain.ProviderBuild)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 {
		t.Fatalf("known conversion grant lost after cancellation: accounts=%d service_error=%v", len(values), callErr)
	}
	if values[0].EncryptedRefreshToken == "" {
		t.Fatal("known conversion refresh material missing")
	}
}
