package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type closingHistoryRetentionStore struct {
	repository.ResponseRepository
	db       *relational.Database
	entered  chan context.Context
	release  chan struct{}
	finished chan error
}

func (s *closingHistoryRetentionStore) DeleteExpired(ctx context.Context, now time.Time, ownership, web int) (repository.ResponseCleanupResult, error) {
	s.entered <- ctx
	<-s.release // Driver cleanup can outlive its cancellation notification.
	if s.db.Stats().OpenConnections == 0 {
		s.finished <- errors.New("SQL closed under history cleanup")
		return repository.ResponseCleanupResult{}, ctx.Err()
	}
	result, err := s.ResponseRepository.DeleteExpired(context.WithoutCancel(ctx), now, ownership, web)
	s.finished <- err
	return result, ctx.Err()
}
func TestCloseWaitsForHistoryRetentionBeforeStorage(t *testing.T) {
	a := newLifecycleApplication(t)
	a.httpDrainTimeout = 5 * time.Millisecond
	a.shutdownJoinBudget = 10 * time.Millisecond
	store := &closingHistoryRetentionStore{ResponseRepository: relational.NewResponseRepository(a.database), db: a.database, entered: make(chan context.Context, 1), release: make(chan struct{}), finished: make(chan error, 1)}
	var once sync.Once
	defer once.Do(func() { close(store.release) })
	a.historyRetention = historyapp.NewRetention(store, nil, nil, a.logger)
	ctx, err := a.beginRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.backgroundDone = make(chan struct{})
	// Exercise the same scheduled work and lifecycle barriers at a short test
	// interval; production's five-minute cadence is checked independently.
	go func() {
		a.runPeriodicTask(ctx, time.Millisecond, "response_ownership_cleanup", func(runCtx context.Context) error {
			return a.historyRetention.CleanupResponses(runCtx, time.Now().UTC())
		})
		close(a.backgroundDone)
		close(a.runDone)
	}()
	var callCtx context.Context
	select {
	case callCtx = <-store.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("history cleanup never entered")
	}
	if err = a.Close(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unfinished cleanup close=%v", err)
	}
	if callCtx.Err() == nil {
		t.Fatal("Close did not cancel cleanup")
	}
	if a.database.Stats().OpenConnections == 0 {
		t.Fatal("storage closed before cleanup returned")
	}
	once.Do(func() { close(store.release) })
	if err = <-store.finished; err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if a.database.Stats().OpenConnections != 0 {
		t.Fatal("retry did not close storage")
	}
}
