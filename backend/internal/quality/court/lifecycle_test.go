package court

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

func TestCloseCancelsEvaluatorWaitingForSQL(t *testing.T) {
	reg, err := registry.Open(context.Background(), registry.Options{SQLitePath: filepath.Join(t.TempDir(), "quality.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	source, err := evidence.New(context.Background(), reg.DB(), qualitymodel.DefaultEvidenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	db, err := reg.DB().DB()
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s := newFixtureCourt(Config{EvaluateEvery: time.Second}, reg, storeSource{source}, nil)
	defer s.Close(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for db.Stats().WaitCount == 0 {
		if time.Now().After(deadline) {
			t.Fatal("evaluator did not start its SQL call")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() { s.Close(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		_ = conn.Close()
		<-done
		t.Fatal("Close could not cancel the evaluator's active SQL wait")
	}
}

type delayedCloseStore struct {
	StateStore
	started chan struct{}
	release chan struct{}
}

func (s delayedCloseStore) Coordinate(ctx context.Context, _ string) (context.Context, func(), error) {
	close(s.started)
	<-ctx.Done()
	<-s.release // Model a dependency finishing cleanup after cancellation.
	return ctx, nil, ctx.Err()
}

func TestRepeatedCloseWaitsForEvaluatorAfterTimeout(t *testing.T) {
	store := delayedCloseStore{started: make(chan struct{}), release: make(chan struct{})}
	s := New(Config{EvaluateEvery: time.Millisecond}, store, storeSource{}, nil, nil)
	s.Run(context.Background())
	defer s.Close(context.Background())
	defer close(store.release)
	select {
	case <-store.started:
	case <-time.After(3 * time.Second):
		t.Fatal("evaluator did not start")
	}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.Close(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("close %d returned %v while evaluator still owns its dependency", i+1, err)
		}
	}
}
