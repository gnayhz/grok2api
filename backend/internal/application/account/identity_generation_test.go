package account

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type identityContextAdapter struct {
	*webAccountSettingsAdapterStub
	observe func(context.Context, accountdomain.Credential) (provider.AccountIdentity, error)
}

func (a identityContextAdapter) SyncAccountIdentity(ctx context.Context, value accountdomain.Credential) (provider.AccountIdentity, error) {
	return a.observe(ctx, value)
}

func identityAccountFixture(t *testing.T, service *Service, adapter *webAccountSettingsAdapterStub, observe func(context.Context, accountdomain.Credential) (provider.AccountIdentity, error)) accountdomain.Credential {
	t.Helper()
	value, _, err := service.accounts.UpsertByIdentity(context.Background(), accountdomain.Credential{Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "source", EncryptedAccessToken: "synthetic", AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	service.providers = provider.NewRegistry(identityContextAdapter{webAccountSettingsAdapterStub: adapter, observe: observe})
	return value
}

func awaitIdentityResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("identity caller did not complete")
		return nil
	}
}

func TestIdentitySyncOwnsCleanupAndWaitersCancelIndependently(t *testing.T) {
	service, repo, adapter := newWebAccountSettingsTestService(t)
	entered, cleaning, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	defer finish()
	var calls atomic.Int32
	identity := provider.AccountIdentity{Email: "current@example.test", UserID: "11111111-1111-4111-8111-111111111111"}
	value := identityAccountFixture(t, service, adapter, func(ctx context.Context, _ accountdomain.Credential) (provider.AccountIdentity, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			close(cleaning)
			<-release
			return provider.AccountIdentity{}, ctx.Err()
		}
		return identity, nil
	})
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	owner := make(chan error, 1)
	go func() { owner <- service.SyncAccountIdentity(ownerCtx, value.ID) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not start")
	}
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() { waiter <- service.SyncAccountIdentity(waiterCtx, value.ID) }()
	cancelWaiter()
	if err := awaitIdentityResult(t, waiter); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter cancellation: %v", err)
	}
	live := make(chan error, 1)
	go func() { live <- service.SyncAccountIdentity(context.Background(), value.ID) }()
	cancelOwner()
	select {
	case <-cleaning:
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not enter cleanup")
	}
	select {
	case err := <-owner:
		t.Fatalf("owner returned before cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	finish()
	if err := awaitIdentityResult(t, owner); !errors.Is(err, context.Canceled) {
		t.Fatalf("owner cancellation: %v", err)
	}
	if err := awaitIdentityResult(t, live); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(context.Background(), value.ID)
	if err != nil || stored.UserID != identity.UserID || calls.Load() != 2 {
		t.Fatalf("live waiter did not resume: %v calls=%d", err, calls.Load())
	}
}

func TestIdentitySyncNewMaterialDoesNotWaitForOldProvider(t *testing.T) {
	service, repo, adapter := newWebAccountSettingsTestService(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	defer finish()
	var firstGeneration uint64
	oldIdentity := provider.AccountIdentity{Email: "old@example.test", UserID: "11111111-1111-4111-8111-111111111111"}
	newIdentity := provider.AccountIdentity{Email: "new@example.test", UserID: "22222222-2222-4222-8222-222222222222"}
	value := identityAccountFixture(t, service, adapter, func(_ context.Context, observed accountdomain.Credential) (provider.AccountIdentity, error) {
		if observed.CredentialGeneration == firstGeneration {
			close(entered)
			<-release
			return oldIdentity, nil
		}
		return newIdentity, nil
	})
	firstGeneration = value.CredentialGeneration
	oldResult := make(chan error, 1)
	go func() { oldResult <- service.SyncAccountIdentity(context.Background(), value.ID) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("old Provider did not start")
	}
	replacement := value
	replacement.EncryptedAccessToken = "new"
	if _, _, err := repo.UpsertByIdentity(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	newResult := make(chan error, 1)
	go func() { newResult <- service.SyncAccountIdentity(context.Background(), value.ID) }()
	if err := awaitIdentityResult(t, newResult); err != nil {
		t.Fatal(err)
	}
	finish()
	if err := awaitIdentityResult(t, oldResult); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(context.Background(), value.ID)
	if err != nil || stored.UserID != newIdentity.UserID || stored.Email != newIdentity.Email {
		t.Fatalf("new generation blocked or overwritten: %v", err)
	}
}

type identityGenerationAdapter struct {
	*webAccountSettingsAdapterStub
	complete func(accountdomain.Credential) provider.AccountIdentity
}

func (a identityGenerationAdapter) SyncAccountIdentity(_ context.Context, observed accountdomain.Credential) (provider.AccountIdentity, error) {
	return a.complete(observed), nil
}

func TestIdentityCompletionCannotOverwriteNewMaterial(t *testing.T) {
	ctx := context.Background()
	service, repo, adapter := newWebAccountSettingsTestService(t)
	web, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "source", EncryptedAccessToken: "old-material", AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	oldIdentity := provider.AccountIdentity{Email: "old@example.test", UserID: "11111111-1111-4111-8111-111111111111", TeamID: "old-team"}
	newIdentity := provider.AccountIdentity{Email: "new@example.test", UserID: "22222222-2222-4222-8222-222222222222", TeamID: "new-team"}
	staleBuild, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "old-build", SourceKey: "old-build", Email: oldIdentity.Email, UserID: oldIdentity.UserID, EncryptedAccessToken: "synthetic", AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	service.providers = provider.NewRegistry(identityGenerationAdapter{webAccountSettingsAdapterStub: adapter, complete: func(observed accountdomain.Credential) provider.AccountIdentity {
		if observed.CredentialGeneration != web.CredentialGeneration {
			t.Fatal("unexpected observed material")
		}
		replacement := web
		replacement.Email, replacement.UserID, replacement.TeamID = newIdentity.Email, newIdentity.UserID, newIdentity.TeamID
		replacement.EncryptedAccessToken = "new-material"
		if _, _, err := repo.UpsertByIdentity(ctx, replacement); err != nil {
			t.Fatal(err)
		}
		return oldIdentity
	}})
	if err := service.SyncAccountIdentity(ctx, web.ID); err != nil {
		t.Fatal(err)
	}
	current, err := repo.Get(ctx, web.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.CredentialGeneration != web.CredentialGeneration+1 || current.EncryptedAccessToken != "new-material" {
		t.Fatal("replacement did not commit")
	}
	if current.Email != newIdentity.Email || current.UserID != newIdentity.UserID || current.TeamID != newIdentity.TeamID || current.LinkedAccountID == staleBuild.ID {
		t.Fatalf("old identity completion overwrote current material identity: generation=%d email=%s uid=%s team=%s stale_link=%t", current.CredentialGeneration, current.Email, current.UserID, current.TeamID, current.LinkedAccountID == staleBuild.ID)
	}
}
