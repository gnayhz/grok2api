package app

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type closingClientKeyRepository struct {
	repository.ClientKeyRepository
	database *relational.Database
	entered  chan context.Context
	release  chan struct{}
	finished chan error
}

func (r *closingClientKeyRepository) Touch(ctx context.Context, id uint64) error {
	r.entered <- ctx
	<-r.release // Simulate a driver finalizing after cancellation.
	var err error
	if r.database.Stats().OpenConnections == 0 {
		err = errors.New("SQL closed before client key Touch returned")
	} else {
		err = r.ClientKeyRepository.Touch(context.WithoutCancel(ctx), id)
	}
	r.finished <- err
	return err
}

func TestCloseRetainsDependenciesUntilClientKeyTouchReturns(t *testing.T) {
	a := newLifecycleApplication(t)
	a.shutdownJoinBudget = 40 * time.Millisecond
	repo := &closingClientKeyRepository{
		ClientKeyRepository: relational.NewClientKeyRepository(a.database), database: a.database,
		entered: make(chan context.Context, 1), release: make(chan struct{}), finished: make(chan error, 1),
	}
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(repo.release) })
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	a.clientKeys = clientkeyapp.NewService("shutdown-owner", repo, nil, nil, 0, 0, cipher, security.RandomTokenSource{})
	created, err := a.clientKeys.Create(context.Background(), clientkeyapp.CreateInput{Name: "closing-key", Enabled: true, RPMUnlimited: true, ConcurrencyUnlimited: true})
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := a.clientKeys.Authenticate(context.Background(), created.Secret)
	if err != nil {
		t.Fatal(err)
	}
	release()
	var touchCtx context.Context
	select {
	case touchCtx = <-repo.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("authentication did not start Touch")
	}
	if err := a.Close(); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close must report unfinished Touch: %v", err)
	}
	if touchCtx.Err() == nil {
		t.Error("Close did not cancel Touch")
	}
	if a.database.Stats().OpenConnections == 0 {
		t.Error("Close released SQL while Touch still owned it")
	}
	releaseOnce.Do(func() { close(repo.release) })
	select {
	case err := <-repo.finished:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Touch did not finish")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if a.database.Stats().OpenConnections != 0 {
		t.Error("retry did not close SQL after Touch finished")
	}
}

func TestRunCancellationWaitsForClientKeyTouch(t *testing.T) {
	a := newLifecycleApplication(t)
	repo := &closingClientKeyRepository{
		ClientKeyRepository: relational.NewClientKeyRepository(a.database), database: a.database,
		entered: make(chan context.Context, 1), release: make(chan struct{}), finished: make(chan error, 1),
	}
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(repo.release) })
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	a.clientKeys = clientkeyapp.NewService("run-shutdown-owner", repo, nil, nil, 0, 0, cipher, security.RandomTokenSource{})
	created, err := a.clientKeys.Create(context.Background(), clientkeyapp.CreateInput{Name: "run-closing-key", Enabled: true, RPMUnlimited: true, ConcurrencyUnlimited: true})
	if err != nil {
		t.Fatal(err)
	}
	a.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, release, err := a.clientKeys.Authenticate(r.Context(), created.Secret)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		release()
		w.WriteHeader(200)
	})
	cancel, runDone := startLifecycleApplication(t, a)
	response := lifecycleGET(t, a)
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("HTTP authentication: %d", response.StatusCode)
	}
	var touchCtx context.Context
	select {
	case touchCtx = <-repo.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP did not start Touch")
	}
	if touchCtx.Err() != nil {
		t.Fatal("HTTP completion canceled Touch")
	}
	cancel()
	select {
	case <-touchCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not cancel Touch")
	}
	select {
	case err := <-runDone:
		t.Fatalf("Run returned while Touch still owns SQL: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(repo.release) })
	select {
	case err := <-repo.finished:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Touch did not finalize")
	}
	awaitRun(t, runDone)
	if a.database.Stats().OpenConnections == 0 {
		t.Fatal("Run unexpectedly released SQL")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}
