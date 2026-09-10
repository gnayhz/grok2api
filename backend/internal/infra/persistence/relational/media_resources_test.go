package relational

import (
	"context"
	"errors"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

func TestVideoResourceSQLCancelCloseAndRetry(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			db, peer := settingsDatabasePair(t, dialect)
			key, job := seedMediaDeletion(t, db, "resource", media.StatusCompleted)
			if err := NewMediaJobRepository(db).CreateMediaJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			resources := mediaapp.NewVideoResources(NewMediaJobRepository(db), nil)
			got, err := resources.Lookup(ctx, job.ID, key.ID)
			if err != nil || got.ID != job.ID {
				t.Fatalf("initial read: %+v %v", got, err)
			}
			// Exhaust the actual SQLite connection pool, or hold a PostgreSQL table lock,
			// so cancellation happens within the repository call, not only at its entry.
			release := func() {}
			if dialect == "sqlite" {
				pool, err := db.db.DB()
				if err != nil {
					t.Fatal(err)
				}
				pool.SetMaxOpenConns(1)
				conn, err := pool.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				release = func() { _ = conn.Close() }
			} else {
				tx := peer.db.Begin()
				if tx.Error != nil {
					t.Fatal(tx.Error)
				}
				if err := tx.Exec("LOCK TABLE media_jobs IN ACCESS EXCLUSIVE MODE").Error; err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				release = func() {
					if err := tx.Rollback().Error; err != nil {
						t.Error(err)
					}
				}
			}
			blocked, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
			_, err = resources.Lookup(blocked, job.ID, key.ID)
			cancel()
			release()
			if !errors.Is(err, media.ErrVideoResourceRead) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, media.ErrVideoNotFound) {
				t.Fatalf("cancelled SQL was not availability failure: %v", err)
			}
			if got, err = resources.Lookup(ctx, job.ID, key.ID); err != nil || got.ID != job.ID {
				t.Fatalf("retry: %+v %v", got, err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err = resources.Lookup(ctx, job.ID, key.ID); !errors.Is(err, media.ErrVideoResourceRead) || errors.Is(err, media.ErrVideoNotFound) {
				t.Fatalf("closed SQL was not availability failure: %v", err)
			}
			fresh := mediaapp.NewVideoResources(NewMediaJobRepository(peer), nil)
			if got, err = fresh.Lookup(ctx, job.ID, key.ID); err != nil || got.Status != job.Status {
				t.Fatalf("rebuild: %+v %v", got, err)
			}
		})
	}
}
