package journal_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/events"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type uncertainCommitPool struct {
	*sql.DB
	failed atomic.Bool
}

func (p *uncertainCommitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &uncertainCommitTx{Tx: tx, pool: p}, nil
}

type uncertainCommitTx struct {
	*sql.Tx
	pool *uncertainCommitPool
}

func (t *uncertainCommitTx) Commit() error {
	err := t.Tx.Commit()
	if err == nil && t.pool.failed.CompareAndSwap(false, true) {
		return driver.ErrBadConn
	}
	return err
}

func uncertainCommitDB(t *testing.T, dialect string) *gorm.DB {
	t.Helper()
	if dialect == "sqlite" {
		r, _ := setup(t)
		return r.DB()
	}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	adminDB := db
	schema := fmt.Sprintf("synthetic_receipt_%d", time.Now().UnixNano())
	if err := db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := adminDB.WithContext(ctx).Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err = gorm.Open(postgres.Open(u.String()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := db.AutoMigrate(journal.Models()...); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestReceiptRetryAfterCommittedConnectionFailureKeepsLiveOwner(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := uncertainCommitDB(t, dialect)
			connection, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool := &uncertainCommitPool{DB: connection}
			wrapped := db.Session(&gorm.Session{NewDB: true})
			wrapped.Statement.ConnPool = pool
			store := journal.New(wrapped)
			svc := events.New(store, nil, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			now := time.Now().UTC()
			e := event("synthetic-uncertain", now)
			e.Outcome, e.HoldUntil = "delivered", time.Time{}
			if err := svc.RecordQualityEvent(ctx, events.Receipt{Attempt: e.Attempt, Outcome: events.Admitted, At: e.At}, 0); err != nil {
				t.Fatal(err)
			}
			if !pool.failed.Load() {
				t.Fatal("commit failure was not exercised")
			}
			var count int64
			if err := db.Model(&journal.EventRow{}).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("duplicate events: %d %v", count, err)
			}
			later := now.Add(journal.CompletionLease + time.Second)
			if err := store.RenewCompletions(ctx, later); err != nil {
				t.Fatal(err)
			}
			other := journal.New(db)
			if err := other.RecoverCompletions(ctx, later); err != nil {
				t.Fatal(err)
			}
			stats, err := store.Stats(ctx)
			if err != nil || stats.InFlight != 1 || stats.Unconfirmed != 0 {
				t.Fatalf("retried live admission lost its owner: %+v %v", stats, err)
			}
			if err := other.Record(ctx, e); err != nil {
				t.Fatal(err)
			}
			later = later.Add(journal.CompletionLease + time.Second)
			if err := other.RenewCompletions(ctx, later); err != nil {
				t.Fatal(err)
			}
			if err := other.RecoverCompletions(ctx, later); err != nil {
				t.Fatal(err)
			}
			stats, err = other.Stats(ctx)
			if err != nil || stats.InFlight != 0 || stats.Unconfirmed != 1 {
				t.Fatalf("another process adopted the obligation: %+v %v", stats, err)
			}
		})
	}
}
