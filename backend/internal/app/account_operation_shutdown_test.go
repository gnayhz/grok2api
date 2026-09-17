package app

import (
	"context"
	"errors"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

type shutdownCredentialAdapter struct {
	entered, cleaning, release chan struct{}
	calls                      atomic.Int32
}

func (*shutdownCredentialAdapter) Provider() account.Provider { return account.ProviderBuild }
func (*shutdownCredentialAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: account.ProviderBuild, Credential: provider.CredentialSurface{Refresh: true}}
}
func (a *shutdownCredentialAdapter) RefreshCredential(ctx context.Context, _ account.Credential) (provider.RefreshedCredential, error) {
	a.calls.Add(1)
	close(a.entered)
	<-ctx.Done()
	close(a.cleaning)
	<-a.release
	// The Provider already knows its rotation succeeded. M07 must finish the
	// bounded SQL commit before the HTTP owner and Application can drain.
	return provider.RefreshedCredential{EncryptedAccessToken: "rotated-access", EncryptedRefreshToken: "rotated-refresh", ExpiresAt: time.Now().Add(time.Hour), RefreshTokenRotated: true}, nil
}

type shutdownOperationWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *shutdownOperationWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestApplicationDrainsCredentialOwnerAfterCanceledHTTPWaiter(t *testing.T) {
	a := newLifecycleApplication(t)
	a.httpDrainTimeout = 10 * time.Millisecond
	ctx := context.Background()
	repo := relational.NewAccountRepository(a.database)
	v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "operation-shutdown", SourceKey: "operation-shutdown", EncryptedAccessToken: "access", EncryptedRefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &shutdownCredentialAdapter{entered: make(chan struct{}), cleaning: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	finish := func() { once.Do(func() { close(adapter.release) }) }
	defer finish()
	s := accountapp.NewService(repo, nil, nil, nil, providerimpl.NewRegistry(adapter), nil, security.RandomTokenSource{}, nil, nil, nil)
	waitJoined := make(chan struct{})
	ownerDone, waiterDone := make(chan error, 1), make(chan error, 1)
	a.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		callCtx := r.Context()
		if r.URL.Path == "/wait" {
			callCtx = &shutdownOperationWaitContext{Context: callCtx, waiting: waitJoined}
		}
		_, err := s.EnsureCredential(callCtx, v, true)
		if r.URL.Path == "/wait" {
			waiterDone <- err
			return
		}
		if err == nil {
			current, readErr := repo.Get(context.Background(), v.ID)
			if readErr != nil {
				err = readErr
			} else if current.EncryptedRefreshToken != "rotated-refresh" || current.CredentialGeneration != v.CredentialGeneration+1 {
				err = fmt.Errorf("rotated material missing before HTTP owner returned")
			}
		}
		ownerDone <- err
	})
	_, runDone := startLifecycleApplication(t, a)
	response := lifecycleGET(t, a)
	defer response.Body.Close()
	select {
	case <-adapter.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not reach Provider")
	}
	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	request, err := http.NewRequestWithContext(waitCtx, http.MethodGet, "http://"+a.server.Addr+"/wait", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	waitResponse, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer waitResponse.Body.Close()
	select {
	case <-waitJoined:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP waiter did not join")
	}
	cancelWait()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("HTTP waiter=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP waiter did not honor client cancellation")
	}
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	select {
	case <-adapter.cleaning:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not cancel actual credential owner")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close detached credential cleanup: %v", err)
	default:
	}
	if _, err := repo.Get(ctx, v.ID); err != nil {
		t.Fatalf("SQL closed while credential owner was still finishing: %v", err)
	}
	finish()
	select {
	case err := <-ownerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not finish rotation commit")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not finish")
	}
	awaitRun(t, runDone)
	if adapter.calls.Load() != 1 {
		t.Fatalf("canceled HTTP waiter triggered extra rotation: %d", adapter.calls.Load())
	}
}

type shutdownDeviceAdapter struct{ entered, cleaning, release chan struct{} }

func (*shutdownDeviceAdapter) Provider() account.Provider { return account.ProviderBuild }
func (*shutdownDeviceAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: account.ProviderBuild, Credential: provider.CredentialSurface{AuthType: account.AuthTypeOAuth, DeviceOAuth: true}}
}
func (*shutdownDeviceAdapter) StartDeviceAuthorization(context.Context) (provider.DeviceAuthorization, error) {
	return provider.DeviceAuthorization{}, errors.New("unused")
}
func (a *shutdownDeviceAdapter) PollDeviceAuthorization(ctx context.Context, _ string) (provider.CredentialSeed, error) {
	close(a.entered)
	<-ctx.Done()
	close(a.cleaning)
	<-a.release
	return provider.CredentialSeed{Name: "shutdown-device", SourceKey: "shutdown-device", AccessToken: "issued-access", RefreshToken: "issued-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func TestApplicationDrainsKnownDeviceGrantBeforeStorageClose(t *testing.T) {
	a := newLifecycleApplication(t)
	a.httpDrainTimeout = 10 * time.Millisecond
	ctx := context.Background()
	repo := relational.NewAccountRepository(a.database)
	store := memory.NewDeviceSessionStore()
	if err := store.Create(ctx, account.DeviceSession{ID: "shutdown", DeviceCode: "shutdown-device", Interval: time.Second, ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	adapter := &shutdownDeviceAdapter{entered: make(chan struct{}), cleaning: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	finish := func() { once.Do(func() { close(adapter.release) }) }
	defer finish()
	service := accountapp.NewService(repo, relational.NewAuditRepository(a.database), store, nil, providerimpl.NewRegistry(adapter), cipher, security.RandomTokenSource{}, nil, nil, nil)
	ownerDone := make(chan error, 1)
	a.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		view, err := service.PollDeviceLogin(r.Context(), "shutdown")
		if err == nil {
			current, readErr := repo.Get(context.Background(), view.Credential.ID)
			if readErr != nil {
				err = readErr
			} else if token, decryptErr := cipher.Decrypt(current.EncryptedRefreshToken); decryptErr != nil || token != "issued-refresh" {
				err = fmt.Errorf("known grant missing before handler drained: %v", decryptErr)
			}
		}
		ownerDone <- err
	})
	_, runDone := startLifecycleApplication(t, a)
	response := lifecycleGET(t, a)
	defer response.Body.Close()
	select {
	case <-adapter.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("device provider not entered")
	}
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	select {
	case <-adapter.cleaning:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not cancel device provider")
	}
	select {
	case err := <-closed:
		t.Fatalf("shutdown detached known grant owner: %v", err)
	default:
	}
	if _, err := repo.ListEnabled(ctx, account.ProviderBuild); err != nil {
		t.Fatalf("SQL closed before device commit: %v", err)
	}
	finish()
	select {
	case err := <-ownerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("device owner did not finish")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	awaitRun(t, runDone)
}
