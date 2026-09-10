package court

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

func TestCloseCancelsEvaluatorWaitingForSQL(t *testing.T) {
	reg, err := registry.Open(context.Background(), registry.Options{SQLitePath: filepath.Join(t.TempDir(), "quality.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	source, err := evidence.New(context.Background(), reg.DB(), evidence.DefaultConfig())
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
