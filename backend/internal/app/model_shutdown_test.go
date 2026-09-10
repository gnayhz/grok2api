package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

type closingModelRepository struct {
	repository.ModelRepository
	application *Application
	finished    chan error
	entered     chan struct{}
	release     chan struct{}
}

func (r closingModelRepository) CompleteAccountCapabilitySync(ctx context.Context, ref modeldomain.CapabilitySyncRef, result modeldomain.CapabilitySyncResult) error {
	close(r.entered)
	select {
	case <-r.release:
	case <-ctx.Done():
	}
	err := r.ModelRepository.CompleteAccountCapabilitySync(ctx, ref, result)
	if r.application.database.Stats().OpenConnections == 0 {
		err = errors.Join(err, errors.New("SQL closed before detached model sync finalized"))
	}
	r.finished <- err
	return err
}

// The actual Build catalog call is blocked while the watching request leaves.
// Closing the application must cancel and join the detached run before closing
// the repository needed to persist its failure snapshot.
func TestCloseStopsDetachedModelSyncBeforeDependencies(t *testing.T) {
	testDetachedModelSyncShutdown(t, false)
}

func TestRunCancellationWaitsForDetachedModelSync(t *testing.T) {
	testDetachedModelSyncShutdown(t, true)
}

func testDetachedModelSyncShutdown(t *testing.T, cancelRun bool) {
	entered, upstreamDone := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		close(entered)
		defer close(upstreamDone)
		select {
		case <-r.Context().Done():
		case <-release:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(origin.Close)
	a := newLifecycleApplication(t, func(cfg *config.Config) {
		cfg.Provider.Build.BaseURL = origin.URL + "/v1"
	})
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("model-shutdown-fixture")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	credential, _, err := a.accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "closing-model", SourceKey: "closing-model", EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// The startup catchup already has a fresh snapshot; only the explicit
	// detached sync under test should reach the gated catalog server.
	if err := testsupport.Capabilities(ctx, a.modelRepo, a.accountRepo, credential.ID, []string{"grok-existing"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Keep production construction, credential service, Build adapter and SQL;
	// the repository decorator only observes the existing failure write.
	finished := make(chan error, 1)
	writeEntered, writeRelease := make(chan struct{}), make(chan struct{})
	var writeOnce sync.Once
	defer writeOnce.Do(func() { close(writeRelease) })
	a.models = modelapp.NewService(closingModelRepository{ModelRepository: a.modelRepo, application: a, finished: finished, entered: writeEntered, release: writeRelease}, a.accountRepo, a.accounts, a.providers)
	a.models.SetLogger(a.logger)
	closeApplication := a.Close
	if cancelRun {
		cancel, runDone := startLifecycleApplication(t, a)
		response := lifecycleGET(t, a)
		_ = response.Body.Close()
		closeApplication = func() error {
			cancel()
			return <-runDone
		}
	}
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	watchDone := make(chan error, 1)
	go func() { _, err := a.models.SyncObserved(watchCtx, nil); watchDone <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Build catalog not reached")
	}
	cancelWatch()
	if err := <-watchDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("watch cancellation: %v", err)
	}
	if !a.models.SyncProgress().Active {
		t.Fatal("watcher disconnect canceled shared sync")
	}
	closed := make(chan error, 1)
	go func() { closed <- closeApplication() }()
	select {
	case <-writeEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not cancel the active catalog call")
	}
	returned := false
	select {
	case err := <-closed:
		returned = true
		t.Errorf("Application.Close returned while detached model sync was still finalizing: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	writeOnce.Do(func() { close(writeRelease) })
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-finished:
		if err != nil {
			t.Error(fmt.Errorf("model sync finalized after dependency close: %w", err))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("detached model sync did not finalize")
	}
	if !returned {
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	}
	if a.models.SyncProgress().Active {
		t.Error("Application.Close returned while detached model sync was still active")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-upstreamDone:
	case <-time.After(time.Second):
		t.Fatal("upstream catalog request was not released")
	}
}
