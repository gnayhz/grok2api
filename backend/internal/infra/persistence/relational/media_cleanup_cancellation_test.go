package relational

import (
	"context"
	"errors"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
)

func TestMediaCleanupStartupCancellationReleasesDatabaseWait(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			workerCtx, stop := context.WithCancel(ctx)
			defer stop()
			objects, err := localmedia.NewLocalStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			service := mediaapp.NewServiceWithTickets(NewMediaAssetRepository(db), NewMediaJobRepository(db), nil, objects, nil, mediaapp.Config{MaxImageBytes: 1 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute})
			pool, err := db.db.DB()
			if err != nil {
				t.Fatal(err)
			}
			waits := pool.Stats().WaitCount
			var unlock func()
			if dialect == "sqlite" {
				pool.SetMaxOpenConns(1)
				conn, err := pool.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				unlock = func() { _ = conn.Close() }
			} else {
				tx := peer.db.WithContext(ctx).Begin()
				if tx.Error != nil {
					t.Fatal(tx.Error)
				}
				if err := tx.Exec("LOCK TABLE media_assets IN ACCESS EXCLUSIVE MODE").Error; err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				unlock = func() { _ = tx.Rollback() }
			}
			defer unlock()
			reported := make(chan error, 4)
			done := make(chan error, 1)
			go func() { service.RunCleanup(workerCtx, func(err error) { reported <- err }); done <- nil }()
			if dialect == "postgres" {
				waitMediaDeletionLock(t, ctx, peer, "media_assets", done)
			} else {
				for pool.Stats().WaitCount == waits {
					select {
					case <-done:
						t.Fatal("cleanup did not wait")
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-time.After(time.Millisecond):
					}
				}
			}
			stop()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("startup cleanup retained a database wait after cancellation")
			}
			select {
			case err := <-reported:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation outcome=%v", err)
				}
			default:
				t.Fatal("cleanup hid the interrupted query")
			}
			unlock()
			deadline := time.Now().Add(time.Second)
			for pool.Stats().InUse != 0 {
				if time.Now().After(deadline) {
					t.Fatal("cleanup retained a SQL connection")
				}
				time.Sleep(time.Millisecond)
			}
			if _, err := service.Cleanup(ctx); err != nil {
				t.Fatalf("canceled owner prevented next cleanup: %v", err)
			}
		})
	}
}
