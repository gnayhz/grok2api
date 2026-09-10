package relational

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	"image/png"
	"sync/atomic"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type inputAdmissionBoundary struct {
	repository.MediaAssetRepository
	before func(context.Context) error
}

func (r *inputAdmissionBoundary) CreateMediaInputAsset(ctx context.Context, asset media.Asset, limit int64) error {
	if err := r.before(ctx); err != nil {
		return err
	}
	return r.MediaAssetRepository.CreateMediaInputAsset(ctx, asset, limit)
}

func TestInputAssetAdmissionFailureReleasesObjectAndCapacity(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []string{"image", "video"} {
			for _, fault := range []string{"insert_rollback", "cancel_lock_wait"} {
				t.Run(dialect+"/"+kind+"/"+fault, func(t *testing.T) {
					db, peer := settingsDatabasePair(t, dialect)
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					objects, err := localmedia.NewLocalStore(t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					var picture bytes.Buffer
					if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
						t.Fatal(err)
					}
					payload := picture.Bytes()
					if kind == "video" {
						payload = append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 128)...)
					}
					cfg := mediaapp.Config{MaxImageBytes: 1 << 20, MaxTotalBytes: int64(len(payload)), CleanupThresholdPercent: 100}
					operationCtx, cancelOperation := context.WithCancel(ctx)
					defer cancelOperation()
					boundary := &inputAdmissionBoundary{MediaAssetRepository: NewMediaAssetRepository(db), before: func(context.Context) error { return nil }}
					if fault == "insert_rollback" {
						var failed atomic.Bool
						if err := db.db.Callback().Create().After("gorm:create").Before("gorm:commit_or_rollback_transaction").Register("input_registration_failure", func(tx *gorm.DB) {
							if tx.Statement.Table == "media_assets" && failed.CompareAndSwap(false, true) {
								tx.AddError(errors.New("injected asset commit failure"))
							}
						}); err != nil {
							t.Fatal(err)
						}
						defer db.db.Callback().Create().Remove("input_registration_failure")
					}
					locked := make(chan func(), 1)
					var pool *sql.DB
					var waits int64
					if fault == "cancel_lock_wait" {
						pool, err = db.db.DB()
						if err != nil {
							t.Fatal(err)
						}
						if dialect == "sqlite" {
							pool.SetMaxOpenConns(1)
							waits = pool.Stats().WaitCount
						}
						boundary.before = func(context.Context) error {
							if dialect == "sqlite" {
								conn, err := pool.Conn(ctx)
								if err != nil {
									return err
								}
								locked <- func() { _ = conn.Close() }
							} else {
								tx := peer.db.WithContext(ctx).Begin()
								if tx.Error != nil {
									return tx.Error
								}
								if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", mediaInputCapacityLockID).Error; err != nil {
									_ = tx.Rollback()
									return err
								}
								locked <- func() { _ = tx.Rollback() }
							}
							return nil
						}
					}
					service := mediaapp.NewService(boundary, nil, objects, nil, cfg)
					save := func(ctx context.Context, service *mediaapp.Service) error {
						if kind == "image" {
							_, err := service.SaveInputImage(ctx, payload)
							return err
						}
						_, err := service.SaveInputVideo(ctx, "video/mp4", bytes.NewReader(payload))
						return err
					}
					done := make(chan error, 1)
					go func() { done <- save(operationCtx, service) }()
					if fault == "cancel_lock_wait" {
						var release func()
						select {
						case release = <-locked:
						case err := <-done:
							t.Fatalf("registration did not wait: %v", err)
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						}
						defer release()
						if dialect == "postgres" {
							waitMediaDeletionLock(t, ctx, peer, "pg_advisory_xact_lock", done)
						} else {
							for pool.Stats().WaitCount == waits {
								select {
								case err := <-done:
									t.Fatalf("registration did not wait for pool: %v", err)
								case <-ctx.Done():
									t.Fatal(ctx.Err())
								case <-time.After(time.Millisecond):
								}
							}
						}
						cancelOperation()
						release()
					}
					if err := <-done; err == nil {
						t.Fatal("failed/canceled admission succeeded")
					}
					total, err := NewMediaAssetRepository(peer).TotalMediaAssetBytes(ctx)
					files, temps, fileErr := objects.ListMediaObjectFiles(ctx)
					if err != nil || fileErr != nil || total != 0 || len(files)+len(temps) != 0 {
						t.Fatalf("failed admission retained resources: total=%d files=%d/%d err=%v/%v", total, len(files), len(temps), err, fileErr)
					}
					retry := mediaapp.NewService(NewMediaAssetRepository(peer), nil, objects, nil, cfg)
					if err := save(ctx, retry); err != nil {
						t.Fatalf("peer could not retry full capacity: %v", err)
					}
				})
			}
		}
	}
}
