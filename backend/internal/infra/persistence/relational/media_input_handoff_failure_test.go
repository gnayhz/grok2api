package relational

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"gorm.io/gorm"
)

func TestMediaJobInputHandoffRollbackReleasesOwnership(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, fault := range []string{"sql_insert_rollback", "expired_during_insert"} {
			t.Run(dialect+"/"+fault, func(t *testing.T) {
				fx := newMediaInputHandoffFixture(t, dialect)
				ctx := context.Background()
				if fault == "expired_during_insert" {
					expiry := time.Now().UTC().Add(150 * time.Millisecond)
					fx.input.ExpiresAt = &expiry
					if err := fx.db.db.Model(&mediaAssetModel{}).Where("id = ?", fx.input.ID).Update("expires_at", expiry).Error; err != nil {
						t.Fatal(err)
					}
				}
				injected := errors.New("actual SQL insert rolled back")
				var reached atomic.Bool
				if err := fx.db.db.Callback().Create().After("gorm:create").Before("gorm:commit_or_rollback_transaction").Register("handoff_insert_failure", func(tx *gorm.DB) {
					if tx.Statement.Table != "media_jobs" {
						return
					}
					reached.Store(true)
					if fault == "sql_insert_rollback" {
						tx.AddError(injected)
					} else {
						time.Sleep(time.Until(*fx.input.ExpiresAt) + time.Millisecond)
					}
				}); err != nil {
					t.Fatal(err)
				}
				defer fx.db.db.Callback().Create().Remove("handoff_insert_failure")
				err := NewMediaJobRepository(fx.db).CreateMediaJob(ctx, fx.job)
				if !reached.Load() {
					t.Fatal("test did not reach actual SQL insert")
				}
				want := injected
				if fault == "expired_during_insert" {
					want = media.ErrVideoInputUnavailable
				}
				if !errors.Is(err, want) {
					t.Fatalf("create=%v want=%v", err, want)
				}
				assertNoHandoffJob(t, fx)
				if err := fx.release(ctx); err != nil {
					t.Fatalf("rollback retained lock or active reference: %v", err)
				}
				fx.assertInput(t, false)
			})
		}
	}
}

func TestMediaJobInputHandoffCancellationDoesNotReleaseOrCreate(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, operation := range []string{"create", "release"} {
			t.Run(dialect+"/"+operation, func(t *testing.T) {
				fx := newMediaInputHandoffFixture(t, dialect)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				operationCtx, cancelOperation := context.WithCancel(ctx)
				defer cancelOperation()
				target := fx.db
				if operation == "release" {
					target = fx.peer
				}
				pool, err := target.db.DB()
				if err != nil {
					t.Fatal(err)
				}
				var unlock func()
				waits := pool.Stats().WaitCount
				if dialect == "sqlite" {
					pool.SetMaxOpenConns(1)
					conn, err := pool.Conn(ctx)
					if err != nil {
						t.Fatal(err)
					}
					unlock = func() { _ = conn.Close() }
				} else {
					locker := fx.peer
					if operation == "release" {
						locker = fx.db
					}
					tx := locker.db.WithContext(ctx).Begin()
					if tx.Error != nil {
						t.Fatal(tx.Error)
					}
					if err := tx.Exec("SELECT id FROM media_assets WHERE id = ? FOR UPDATE", fx.input.ID).Error; err != nil {
						_ = tx.Rollback()
						t.Fatal(err)
					}
					unlock = func() { _ = tx.Rollback() }
				}
				defer unlock()
				done := make(chan error, 1)
				go func() {
					if operation == "create" {
						done <- NewMediaJobRepository(target).CreateMediaJob(operationCtx, fx.job)
					} else {
						done <- fx.release(operationCtx)
					}
				}()
				if dialect == "postgres" {
					waitMediaDeletionLock(t, ctx, fx.db, "media_assets", done)
				} else {
					for pool.Stats().WaitCount == waits {
						select {
						case err := <-done:
							t.Fatalf("did not block: %v", err)
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						case <-time.After(time.Millisecond):
						}
					}
				}
				cancelOperation()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("canceled operation succeeded")
					}
				case <-ctx.Done():
					t.Fatal("canceled operation did not return while lock held")
				}
				unlock()
				assertNoHandoffJob(t, fx)
				fx.assertInput(t, true)
				// A peer can acquire the same input after cancellation, and the active job
				// still prevents subsequent release. No process-local ownership is retained.
				if err := NewMediaJobRepository(fx.peer).CreateMediaJob(ctx, fx.job); err != nil {
					t.Fatalf("retry=%v", err)
				}
				if err := fx.release(ctx); err != nil {
					t.Fatal(err)
				}
				fx.assertInput(t, true)
			})
		}
	}
}

func TestMediaJobInputHandoffConcurrentReversedReferences(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			fx := newMediaInputHandoffFixture(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			second, err := fx.owner.SaveInputImage(ctx, fx.picture)
			if err != nil {
				t.Fatal(err)
			}
			firstRefs := []string{media.InputReference(fx.input.ID), media.InputReference(second.ID)}
			fx.job.InputJSON, err = (media.VideoInput{ReferenceURLs: firstRefs}).Encode()
			if err != nil {
				t.Fatal(err)
			}
			peerJob := fx.job
			peerJob.ID += "_peer"
			peerJob.RequestID += "_peer"
			peerJob.InputJSON, err = (media.VideoInput{ReferenceURLs: []string{firstRefs[1], firstRefs[0]}}).Encode()
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			done := make(chan error, 2)
			go func() { <-start; done <- NewMediaJobRepository(fx.db).CreateMediaJob(ctx, fx.job) }()
			go func() { <-start; done <- NewMediaJobRepository(fx.peer).CreateMediaJob(ctx, peerJob) }()
			close(start)
			for range 2 {
				if err := <-done; err != nil {
					t.Fatalf("shared multi-input create=%v", err)
				}
			}
			if err := fx.releaser.ReleaseInputAssets(ctx, firstRefs); err != nil {
				t.Fatal(err)
			}
			fx.assertInput(t, true)
			_, body, err := fx.owner.OpenInputAsset(ctx, second.ID)
			if err != nil {
				t.Fatal(err)
			}
			_ = body.Close()
		})
	}
}
