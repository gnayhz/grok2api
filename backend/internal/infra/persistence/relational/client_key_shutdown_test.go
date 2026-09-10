package relational

import (
	"context"
	"database/sql"
	"encoding/base64"
	"sync"
	"testing"
	"time"

	keyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type observedTouchRepository struct {
	repository.ClientKeyRepository
	entered  chan struct{}
	resume   <-chan struct{}
	finished chan touchSQLResult
}

type touchSQLResult struct{ err, contextErr error }

func (r observedTouchRepository) Touch(ctx context.Context, id uint64) error {
	close(r.entered)
	if r.resume != nil {
		select {
		case <-r.resume:
		case <-ctx.Done():
		}
	}
	err := r.ClientKeyRepository.Touch(ctx, id)
	r.finished <- touchSQLResult{err: err, contextErr: ctx.Err()}
	return err
}

func TestClientKeyCloseCancelsRealSQLWait(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db, other := settingsDatabasePair(t, driver)
			ctx := context.Background()
			cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
			if err != nil {
				t.Fatal(err)
			}
			resume := make(chan struct{})
			var resumeOnce sync.Once
			defer resumeOnce.Do(func() { close(resume) })
			repo := observedTouchRepository{ClientKeyRepository: NewClientKeyRepository(db), entered: make(chan struct{}), resume: resume, finished: make(chan touchSQLResult, 1)}
			service := keyapp.NewService("sql-touch-owner", repo, nil, nil, 0, 0, cipher)
			defer service.Close(ctx)
			created, err := service.Create(ctx, keyapp.CreateInput{Name: "touch", Enabled: true, RPMUnlimited: true, ConcurrencyUnlimited: true})
			if err != nil {
				t.Fatal(err)
			}
			_, release, err := service.Authenticate(ctx, created.Secret)
			if err != nil {
				t.Fatal(err)
			}
			release()
			select {
			case <-repo.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("authentication did not start Touch")
			}
			sqlDB, err := db.db.DB()
			if err != nil {
				t.Fatal(err)
			}
			var held *sql.Conn
			var unlock func()
			if driver == "sqlite" {
				sqlDB.SetMaxOpenConns(1)
				held, err = sqlDB.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				unlock = func() { _ = held.Close() }
			} else {
				lock := other.db.Begin()
				if lock.Error != nil {
					t.Fatal(lock.Error)
				}
				unlock = func() { _ = lock.Rollback().Error }
				if err := lock.Exec("LOCK TABLE client_keys IN SHARE MODE").Error; err != nil {
					unlock()
					t.Fatal(err)
				}
			}
			defer unlock()
			before := sqlDB.Stats().WaitCount
			resumeOnce.Do(func() { close(resume) })
			deadline := time.Now().Add(2 * time.Second)
			for {
				waiting := sqlDB.Stats().WaitCount > before
				if driver == "postgres" {
					if err := other.db.Raw("SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock' AND query LIKE '%UPDATE%client_keys%')").Scan(&waiting).Error; err != nil {
						t.Fatal(err)
					}
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("Touch did not enter SQL wait")
				}
				time.Sleep(time.Millisecond)
			}
			closeCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			if err := service.Close(closeCtx); err != nil {
				t.Fatalf("SQL cancellation did not join: %v", err)
			}
			select {
			case result := <-repo.finished:
				// GORM may combine cancellation with rollback's sql.ErrTxDone,
				// losing errors.Is; require both the canceled context and SQL failure.
				if result.contextErr != context.Canceled || result.err == nil {
					t.Fatalf("SQL Touch cancellation: %+v", result)
				}
			default:
				t.Fatal("service returned before SQL Touch")
			}
			unlock()
			value, err := NewClientKeyRepository(db).Get(ctx, created.Key.ID)
			if err != nil {
				t.Fatal(err)
			}
			if value.LastUsedAt != nil {
				t.Fatal("canceled Touch unexpectedly persisted usage time")
			}
			// A newly constructed service can still persist successful display
			// writes; shutdown only retires the old service's admission.
			nextRepo := observedTouchRepository{ClientKeyRepository: NewClientKeyRepository(db), entered: make(chan struct{}), finished: make(chan touchSQLResult, 1)}
			next := keyapp.NewService("next-owner", nextRepo, nil, nil, 0, 0, cipher)
			defer next.Close(ctx)
			_, release, err = next.Authenticate(ctx, created.Secret)
			if err != nil {
				t.Fatal(err)
			}
			release()
			select {
			case result := <-nextRepo.finished:
				if result.err != nil {
					t.Fatal(result.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("successful Touch did not return")
			}
			if err := next.Close(closeCtx); err != nil {
				t.Fatal(err)
			}
			value, err = NewClientKeyRepository(other).Get(ctx, created.Key.ID)
			if err != nil || value.LastUsedAt == nil {
				t.Fatalf("successful Touch was not persisted: %+v %v", value.LastUsedAt, err)
			}
		})
	}
}
