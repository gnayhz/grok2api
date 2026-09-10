package relational

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestMediaArchiveRegistrationSerializesSourceLifecycle(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, order := range []string{"archive_first", "terminal_first", "delete_first", "cancel_archive"} {
			t.Run(dialect+"/"+order, func(t *testing.T) {
				db, peer := settingsDatabasePair(t, dialect)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				job := seedArchiveSource(t, db)
				terminal := job
				terminal.Status = media.StatusFailed
				terminal.ErrorCode = "delivery_failed"
				if order == "delete_first" {
					if err := NewMediaJobRepository(db).UpdateMediaJob(ctx, terminal); err != nil {
						t.Fatal(err)
					}
				}
				asset := archiveAssetFixture("vid_concurrent_source_archive", job.ID)
				held, resume := make(chan struct{}), make(chan struct{})
				var once sync.Once
				release := func() { once.Do(func() { close(resume) }) }
				defer release()
				var gated atomic.Bool
				gate := func(tx *gorm.DB) {
					table := "media_jobs"
					if order == "archive_first" {
						table = "media_assets"
					}
					if tx.Statement.Table != table || !gated.CompareAndSwap(false, true) {
						return
					}
					close(held)
					select {
					case <-resume:
					case <-ctx.Done():
						tx.AddError(ctx.Err())
					}
				}
				switch order {
				case "archive_first":
					if err := db.db.Callback().Create().After("gorm:create").Before("gorm:commit_or_rollback_transaction").Register("source_archive_gate", gate); err != nil {
						t.Fatal(err)
					}
					defer db.db.Callback().Create().Remove("source_archive_gate")
				case "delete_first":
					if err := db.db.Callback().Query().After("gorm:query").Register("source_delete_gate", gate); err != nil {
						t.Fatal(err)
					}
					defer db.db.Callback().Query().Remove("source_delete_gate")
				default:
					if err := db.db.Callback().Update().After("gorm:update").Before("gorm:commit_or_rollback_transaction").Register("source_terminal_gate", gate); err != nil {
						t.Fatal(err)
					}
					defer db.db.Callback().Update().Remove("source_terminal_gate")
				}
				first := make(chan error, 1)
				go func() {
					switch order {
					case "archive_first":
						first <- NewMediaAssetRepository(db).CreateMediaAsset(ctx, asset)
					case "delete_first":
						first <- NewMediaJobRepository(db).DeleteMediaJob(ctx, job.ID)
					default:
						first <- NewMediaJobRepository(db).UpdateMediaJob(ctx, terminal)
					}
				}()
				select {
				case <-held:
				case err := <-first:
					t.Fatalf("first operation did not hold transaction: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				secondCtx, cancelSecond := context.WithCancel(ctx)
				defer cancelSecond()
				second := make(chan error, 1)
				go func() {
					if order == "archive_first" {
						second <- NewMediaJobRepository(peer).UpdateMediaJob(secondCtx, terminal)
					} else {
						second <- NewMediaAssetRepository(peer).CreateMediaAsset(secondCtx, asset)
					}
				}()
				waitMediaDeletionLock(t, ctx, peer, "media_jobs", second)
				if order == "cancel_archive" {
					cancelSecond()
				}
				release()
				if err := <-first; err != nil {
					t.Fatalf("first operation failed: %v", err)
				}
				err := <-second
				switch order {
				case "archive_first":
					if err != nil {
						t.Fatalf("terminal handoff failed after committed archive: %v", err)
					}
				case "terminal_first":
					if !errors.Is(err, repository.ErrConflict) {
						t.Fatalf("terminal source admitted archive: %v", err)
					}
				case "delete_first":
					if !errors.Is(err, repository.ErrNotFound) {
						t.Fatalf("deleted source admitted archive: %v", err)
					}
				case "cancel_archive":
					if err == nil {
						t.Fatal("canceled registration succeeded")
					}
				}
				got, assetErr := NewMediaAssetRepository(peer).GetMediaAsset(ctx, asset.ID)
				if order == "archive_first" {
					if assetErr != nil || got.SourceJobID != job.ID {
						t.Fatalf("committed provenance lost: %+v %v", got, assetErr)
					}
				} else if !errors.Is(assetErr, repository.ErrNotFound) {
					t.Fatalf("rejected registration left metadata: %v", assetErr)
				}
				current, jobErr := NewMediaJobRepository(peer).GetMediaJob(ctx, job.ID, job.ClientKeyID)
				if order == "delete_first" {
					if !errors.Is(jobErr, repository.ErrNotFound) {
						t.Fatalf("deleted source reappeared: %v", jobErr)
					}
				} else if jobErr != nil || current.Status != media.StatusFailed || current.ResultAssetID != "" || current.ClaimToken != job.ClaimToken || current.Execution.Revision != job.Execution.Revision {
					t.Fatalf("archive changed execution authority: %+v %v", current, jobErr)
				}
			})
		}
	}
}
