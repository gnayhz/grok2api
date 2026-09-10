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

func waitMediaDeletionLock(t *testing.T, ctx context.Context, db *Database, table string, done <-chan error) {
	t.Helper()
	if db.Dialect() == "sqlite" {
		select {
		case err := <-done:
			t.Fatalf("second writer completed before transaction release: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		return
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := db.db.WithContext(ctx).Raw("SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock' AND query LIKE ?)", "%"+table+"%").Scan(&waiting).Error; err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("second writer completed without expected database lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestClientKeyDeletionSerializesConcurrentJobCreation(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, order := range []string{"delete_first", "create_first", "cancel_delete"} {
			t.Run(dialect+"/"+order, func(t *testing.T) {
				a, b := settingsDatabasePair(t, dialect)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				key, job := seedMediaDeletion(t, a, "concurrent", media.StatusQueued)
				job.UsageRecordedAt, job.CompletedAt = nil, nil
				held, resume := make(chan struct{}), make(chan struct{})
				var once sync.Once
				release := func() { once.Do(func() { close(resume) }) }
				defer release()
				var gated atomic.Bool
				gate := func(tx *gorm.DB) {
					if (order == "delete_first" && tx.Statement.Table != "client_keys") || (order != "delete_first" && tx.Statement.Table != "media_jobs") || !gated.CompareAndSwap(false, true) {
						return
					}
					close(held)
					select {
					case <-resume:
					case <-ctx.Done():
						tx.AddError(ctx.Err())
					}
				}
				if order == "delete_first" {
					if err := a.db.Callback().Query().After("gorm:query").Register("deletion_key_lock", gate); err != nil {
						t.Fatal(err)
					}
					defer a.db.Callback().Query().Remove("deletion_key_lock")
				} else {
					if err := a.db.Callback().Create().After("gorm:create").Before("gorm:commit_or_rollback_transaction").Register("deletion_create_lock", gate); err != nil {
						t.Fatal(err)
					}
					defer a.db.Callback().Create().Remove("deletion_create_lock")
				}
				first := make(chan error, 1)
				go func() {
					if order == "delete_first" {
						first <- NewClientKeyRepository(a).Delete(ctx, key.ID)
					} else {
						first <- NewMediaJobRepository(a).CreateMediaJob(ctx, job)
					}
				}()
				select {
				case <-held:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				secondCtx, secondCancel := context.WithCancel(ctx)
				defer secondCancel()
				second := make(chan error, 1)
				go func() {
					if order == "delete_first" {
						second <- NewMediaJobRepository(b).CreateMediaJob(secondCtx, job)
					} else {
						second <- NewClientKeyRepository(b).Delete(secondCtx, key.ID)
					}
				}()
				table := "client_keys"
				if order == "delete_first" {
					table = "media_jobs"
				}
				waitMediaDeletionLock(t, ctx, b, table, second)
				if order == "cancel_delete" {
					secondCancel()
				}
				release()
				if err := <-first; err != nil {
					t.Fatalf("first transaction failed: %v", err)
				}
				err := <-second
				if order == "create_first" && !errors.Is(err, repository.ErrConflict) {
					t.Fatalf("committed active job did not prevent deletion: %v", err)
				} else if order != "create_first" && err == nil {
					t.Fatal("second operation unexpectedly succeeded")
				}
				_, keyErr := NewClientKeyRepository(b).Get(ctx, key.ID)
				_, jobErr := NewMediaJobRepository(b).GetMediaJob(ctx, job.ID, key.ID)
				if order == "delete_first" {
					if !errors.Is(keyErr, repository.ErrNotFound) || !errors.Is(jobErr, repository.ErrNotFound) {
						t.Fatalf("deleted identity gained a new job: key=%v job=%v", keyErr, jobErr)
					}
				} else if keyErr != nil || jobErr != nil {
					t.Fatalf("rejected/cancelled deletion lost existing state: key=%v job=%v", keyErr, jobErr)
				}
			})
		}
	}
}

func TestMediaDeletionSerializesUsageAcknowledgement(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, order := range []string{"mark_first", "delete_first"} {
			t.Run(dialect+"/"+order, func(t *testing.T) {
				a, b := settingsDatabasePair(t, dialect)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				key, job := seedMediaDeletion(t, a, "mark_race", media.StatusCompleted)
				job.UsageRecordedAt = nil
				if err := NewMediaJobRepository(a).CreateMediaJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				held, resume := make(chan struct{}), make(chan struct{})
				var once sync.Once
				release := func() { once.Do(func() { close(resume) }) }
				defer release()
				var gated atomic.Bool
				gate := func(tx *gorm.DB) {
					if tx.Statement.Table != "media_jobs" || !gated.CompareAndSwap(false, true) {
						return
					}
					close(held)
					select {
					case <-resume:
					case <-ctx.Done():
						tx.AddError(ctx.Err())
					}
				}
				if order == "mark_first" {
					if err := a.db.Callback().Update().After("gorm:update").Before("gorm:commit_or_rollback_transaction").Register("deletion_mark_lock", gate); err != nil {
						t.Fatal(err)
					}
					defer a.db.Callback().Update().Remove("deletion_mark_lock")
				} else {
					if err := a.db.Callback().Query().After("gorm:query").Register("deletion_job_lock", gate); err != nil {
						t.Fatal(err)
					}
					defer a.db.Callback().Query().Remove("deletion_job_lock")
				}
				first := make(chan error, 1)
				go func() {
					if order == "mark_first" {
						first <- NewMediaJobRepository(a).MarkMediaJobUsageRecorded(ctx, job.ID, time.Now().UTC())
					} else {
						first <- NewClientKeyRepository(a).Delete(ctx, key.ID)
					}
				}()
				select {
				case <-held:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				second := make(chan error, 1)
				go func() {
					if order == "mark_first" {
						second <- NewClientKeyRepository(b).Delete(ctx, key.ID)
					} else {
						second <- NewMediaJobRepository(b).MarkMediaJobUsageRecorded(ctx, job.ID, time.Now().UTC())
					}
				}()
				waitMediaDeletionLock(t, ctx, b, "media_jobs", second)
				release()
				firstErr, secondErr := <-first, <-second
				if secondErr != nil {
					t.Fatalf("second operation failed: %v", secondErr)
				}
				if order == "mark_first" && firstErr != nil {
					t.Fatal(firstErr)
				}
				if order == "delete_first" {
					if !errors.Is(firstErr, repository.ErrConflict) {
						t.Fatalf("unacknowledged job deleted: %v", firstErr)
					}
					if err := NewClientKeyRepository(b).Delete(ctx, key.ID); err != nil {
						t.Fatalf("retry after acknowledgement failed: %v", err)
					}
				}
			})
		}
	}
}
