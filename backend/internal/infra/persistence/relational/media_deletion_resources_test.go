package relational

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type mediaDeletionObjectFailure struct {
	repository.MediaObjectStorage
	fail bool
}

func (s *mediaDeletionObjectFailure) Delete(ctx context.Context, key string) error {
	if s.fail {
		return errors.New("injected media object deletion failure")
	}
	return s.MediaObjectStorage.Delete(ctx, key)
}

func TestMediaAdminDeletionRetriesResourceFailures(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, fault := range []string{"media_upload_tickets", "object", "media_assets", "media_jobs", "pending_selection", "key_retention"} {
			t.Run(dialect+"/"+fault, func(t *testing.T) {
				db, peer := settingsDatabasePair(t, dialect)
				ctx := context.Background()
				key, job := seedMediaDeletion(t, db, "resources", media.StatusFailed)
				store, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
				if err != nil {
					t.Fatal(err)
				}
				objects := &mediaDeletionObjectFailure{MediaObjectStorage: store}
				assets, jobs, tickets := NewMediaAssetRepository(db), NewMediaJobRepository(db), NewMediaUploadTicketRepository(db)
				service := mediaapp.NewServiceWithTickets(assets, jobs, tickets, objects, nil, mediaapp.Config{MaxTotalBytes: 1 << 20, CleanupThresholdPercent: 80})
				payload := append([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{3}, 64)...)
				asset, err := service.SaveVideo(ctx, "", "video/mp4", bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}
				job.ResultAssetID = asset.ID
				if err := jobs.CreateMediaJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				ticket := repository.MediaUploadTicket{TokenHash: strings.Repeat("a", 64), AssetID: asset.ID, JobID: job.ID, MaxBytes: 1024, AllowedMIME: "video/mp4", CreatedAt: job.CreatedAt, ExpiresAt: job.CreatedAt.Add(time.Hour)}
				if err := tickets.CreateUploadTicket(ctx, ticket); err != nil {
					t.Fatal(err)
				}
				assertObject := func(wantExists bool) {
					t.Helper()
					body, err := store.Open(ctx, asset.StorageKey)
					if body != nil {
						_ = body.Close()
					}
					if wantExists && err != nil || !wantExists && !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("object presence want=%v err=%v", wantExists, err)
					}
				}
				if fault == "key_retention" {
					if err := NewClientKeyRepository(db).Delete(ctx, key.ID); err != nil {
						t.Fatal(err)
					}
					assertObject(true)
					if _, err := NewMediaAssetRepository(peer).GetMediaAsset(ctx, asset.ID); err != nil {
						t.Fatalf("key deletion changed media retention: %v", err)
					}
					// Retained media continues to participate in M17's ordinary
					// capacity cleanup after its job/ticket protection is removed.
					service.UpdateConfig(mediaapp.Config{MaxTotalBytes: 1, CleanupThresholdPercent: 80})
					if n, err := service.Cleanup(ctx); n != 1 || err != nil {
						t.Fatalf("retained media lost cleanup owner: %d %v", n, err)
					}
					assertObject(false)
					return
				}
				ids := []string{job.ID}
				if fault == "pending_selection" {
					other := job
					other.ID, other.UsageRecordedAt, other.ResultAssetID = "video_deletion_pending_other", nil, ""
					if err := jobs.CreateMediaJob(ctx, other); err != nil {
						t.Fatal(err)
					}
					ids = append(ids, other.ID)
				} else if fault == "object" {
					objects.fail = true
				} else {
					if err := db.db.Callback().Delete().Before("gorm:delete").Register("media_resource_failure", func(tx *gorm.DB) {
						if tx.Statement.Table == fault {
							tx.AddError(errors.New("injected media resource SQL failure"))
						}
					}); err != nil {
						t.Fatal(err)
					}
				}
				n, err := service.AdminDeleteVideoJobs(ctx, ids)
				if fault != "pending_selection" && fault != "object" {
					if err := db.db.Callback().Delete().Remove("media_resource_failure"); err != nil {
						t.Fatal(err)
					}
				}
				objects.fail = false
				if n != 0 || err == nil {
					t.Fatalf("failed cleanup acknowledged removal: %d %v", n, err)
				}
				if _, err := NewMediaJobRepository(peer).GetMediaJob(ctx, job.ID, key.ID); err != nil {
					t.Fatalf("failed cleanup lost retry source: %v", err)
				}
				if fault == "pending_selection" || fault == "media_upload_tickets" {
					if _, err := NewMediaUploadTicketRepository(peer).GetUploadTicketByHash(ctx, ticket.TokenHash); err != nil {
						t.Fatalf("preflight/ticket failure revoked input: %v", err)
					}
				}
				assertObject(fault == "pending_selection" || fault == "media_upload_tickets" || fault == "object")
				if n, err := service.AdminDeleteVideoJobs(ctx, []string{job.ID}); n != 1 || err != nil {
					t.Fatalf("cleanup could not resume after failure: %d %v", n, err)
				}
				assertObject(false)
				if _, err := assets.GetMediaAsset(ctx, asset.ID); !errors.Is(err, repository.ErrNotFound) {
					t.Fatalf("retry retained asset metadata: %v", err)
				}
				if total, err := assets.TotalMediaAssetBytes(ctx); total != 0 || err != nil {
					t.Fatalf("deleted resources retained capacity: %d %v", total, err)
				}
				if _, err := NewMediaJobRepository(peer).GetMediaJob(ctx, job.ID, key.ID); !errors.Is(err, repository.ErrNotFound) {
					t.Fatalf("retry retained job: %v", err)
				}
			})
		}
	}
}
