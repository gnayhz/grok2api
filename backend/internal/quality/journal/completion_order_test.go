package journal_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	"gorm.io/gorm"
)

// Delay the admission caller after the real database commit, while allowing
// other transactions to complete on the same store.
type delayedCommitPool struct {
	*sql.DB
	first           atomic.Bool
	entered, resume chan struct{}
}

func (p *delayedCommitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &delayedCommitTx{Tx: tx, pool: p}, nil
}

type delayedCommitTx struct {
	*sql.Tx
	pool *delayedCommitPool
}

func (tx *delayedCommitTx) Commit() error {
	err := tx.Tx.Commit()
	if err == nil && tx.pool.first.CompareAndSwap(false, true) {
		close(tx.pool.entered)
		<-tx.pool.resume
	}
	return err
}

func TestCompletionBeforeAdmissionReturnsDoesNotResurrectLease(t *testing.T) {
	for _, failCompletion := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "retry_after_storage_failure"}[failCompletion], func(t *testing.T) {
			r, _ := setup(t)
			sqlDB, err := r.DB().DB()
			if err != nil {
				t.Fatal(err)
			}
			pool := &delayedCommitPool{DB: sqlDB, entered: make(chan struct{}), resume: make(chan struct{})}
			var once sync.Once
			resume := func() { once.Do(func() { close(pool.resume) }) }
			defer resume()
			db := r.DB().Session(&gorm.Session{NewDB: true})
			db.Statement.ConnPool = pool
			store := journal.New(db)
			var lookups atomic.Int32
			if err := db.Callback().Query().Before("gorm:query").Register("maturity:completion_lookup", func(tx *gorm.DB) {
				if tx.Statement.Table == "q_guard_completion" {
					lookups.Add(1)
				}
			}); err != nil {
				t.Fatal(err)
			}
			defer db.Callback().Query().Remove("maturity:completion_lookup")
			now := time.Now().UTC()
			admission := event("commit-race", now)
			admission.Outcome, admission.HoldUntil = "delivered", time.Time{}
			admissionDone := make(chan error, 1)
			go func() { admissionDone <- store.Record(context.Background(), admission) }()
			select {
			case <-pool.entered:
			case <-time.After(time.Second):
				t.Fatal("admission did not commit")
			}
			lookups.Store(0)
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			cancelDone := make(chan error, 1)
			go func() { cancelDone <- store.Record(cancelled, event("unrelated", now)) }()
			select {
			case err := <-cancelDone:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatal("slow admission blocked unrelated cancellation")
			}
			if failCompletion {
				if err := r.DB().Exec("CREATE TRIGGER fail_racing_terminal BEFORE INSERT ON q_guard_event WHEN NEW.stage = 'completion' BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
					t.Fatal(err)
				}
			}
			completion := admission
			completion.Stage, completion.Outcome = "completion", "completed"
			err = store.Record(context.Background(), completion)
			if (err != nil) != failCompletion {
				t.Fatalf("completion: %v", err)
			}
			resume()
			if err := <-admissionDone; err != nil {
				t.Fatal(err)
			}
			if lookups.Load() != 0 {
				t.Fatal("recording queried completion obligations after commit")
			}
			if failCompletion {
				if err := r.DB().Exec("DROP TRIGGER fail_racing_terminal").Error; err != nil {
					t.Fatal(err)
				}
				// The failed finalizer must suppress renewal even though the
				// admission's local bookkeeping ran later.
				later := now.Add(journal.CompletionLease + time.Second)
				if err := store.RenewCompletions(context.Background(), later); err != nil {
					t.Fatal(err)
				}
				if err := store.RecoverCompletions(context.Background(), later); err != nil {
					t.Fatal(err)
				}
				stats, err := store.Stats(context.Background())
				if err != nil || stats.Unconfirmed != 1 || stats.InFlight != 0 {
					t.Fatalf("lease resurrected: %+v %v", stats, err)
				}
				if err := store.RetryCompletions(context.Background(), now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Record(context.Background(), completion); err != nil {
				t.Fatal(err)
			}
			var rows int64
			if err := r.DB().Model(&journal.CompletionRow{}).Count(&rows).Error; err != nil || rows != 0 {
				t.Fatalf("obligations=%d %v", rows, err)
			}
		})
	}
}
