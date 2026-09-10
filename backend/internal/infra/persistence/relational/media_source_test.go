package relational

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func seedArchiveSource(t *testing.T, db *Database) media.Job {
	t.Helper()
	key, job := seedMediaDeletion(t, db, "archive_source", media.StatusInProgress)
	job.ClientKeyID = key.ID
	job.ClaimToken = "archive_source_current_claim"
	job.Execution = media.VideoExecution{Revision: 1, Phase: media.VideoExecutionGenerated, Route: "web", Endpoint: "https://example.test/video", GeneratedAt: &job.UpdatedAt}
	if err := NewMediaJobRepository(db).CreateMediaJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

func archiveAssetFixture(id, source string) media.Asset {
	return media.Asset{ID: id, Kind: "video", StorageKey: "videos/" + id + ".mp4", MIMEType: "video/mp4", SizeBytes: 140, SHA256: strings.Repeat("a", 64), CreatedAt: time.Now().UTC(), SourceJobID: source}
}

func TestMediaArchiveSourceMigrationPreservesExistingAssets(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			original := archiveAssetFixture("vid_legacy_source_migration", "")
			original.CreatedAt = original.CreatedAt.Truncate(time.Microsecond)
			if err := NewMediaAssetRepository(db).CreateMediaAsset(ctx, original); err != nil {
				t.Fatal(err)
			}
			m := db.db.Migrator()
			if err := m.DropConstraint(&mediaAssetModel{}, "chk_media_assets_source"); err != nil {
				t.Fatal(err)
			}
			// SQLite constraint removal can rebuild the table without indexes;
			// use portable, schema-local DROP IF EXISTS for the old-shape fixture.
			if err := db.db.Exec("DROP INDEX IF EXISTS idx_media_assets_source_job_id").Error; err != nil {
				t.Fatal(err)
			}
			if err := m.DropColumn(&mediaAssetModel{}, "source_job_id"); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := peer.InitializeSchema(ctx); err != nil {
					t.Fatal(err)
				}
			}
			got, err := NewMediaAssetRepository(peer).GetMediaAsset(ctx, original.ID)
			if err != nil || got.ID != original.ID || got.StorageKey != original.StorageKey || got.SHA256 != original.SHA256 || got.SourceJobID != "" || !got.CreatedAt.Equal(original.CreatedAt) {
				t.Fatalf("migration rewrote existing asset: %+v %v", got, err)
			}
			if !peer.db.Migrator().HasIndex(&mediaAssetModel{}, "idx_media_assets_source_job_id") || !peer.db.Migrator().HasConstraint(&mediaAssetModel{}, "chk_media_assets_source") {
				t.Fatal("source index/shape constraint missing")
			}
			job := seedArchiveSource(t, peer)
			asset := archiveAssetFixture("vid_source_after_migration", job.ID)
			if err := NewMediaAssetRepository(peer).CreateMediaAsset(ctx, asset); err != nil {
				t.Fatal(err)
			}
			got, err = NewMediaAssetRepository(db).GetMediaAsset(ctx, asset.ID)
			if err != nil || got.SourceJobID != job.ID {
				var raw []map[string]any
				_ = peer.db.Raw("SELECT id, source_job_id FROM media_assets WHERE id = ?", asset.ID).Scan(&raw).Error
				peerGot, peerErr := NewMediaAssetRepository(peer).GetMediaAsset(ctx, asset.ID)
				t.Logf("migration diagnostic: SQL=%v peer-source=%q peer-err=%v", raw, peerGot.SourceJobID, peerErr)
				var rawOnOriginal []map[string]any
				_ = db.db.Raw("SELECT * FROM media_assets WHERE id = ?", asset.ID).Scan(&rawOnOriginal).Error
				var modelOnOriginal mediaAssetModel
				_ = db.db.Select("id", "source_job_id").Where("id = ?", asset.ID).First(&modelOnOriginal).Error
				t.Logf("original connection: all=%v explicit=%q", rawOnOriginal, modelOnOriginal.SourceJobID)
				t.Fatalf("source not shared after migration: %+v %v", got, err)
			}
			for _, invalid := range []string{"image", "input", "whitespace", "short"} {
				row := mediaAssetModel{ID: "vid_invalid_source_" + invalid, Kind: "video", StorageKey: "invalid/" + invalid, MIMEType: "video/mp4", SizeBytes: 1, SHA256: strings.Repeat("0", 64), CreatedAt: time.Now(), SourceJobID: job.ID}
				switch invalid {
				case "image":
					row.Kind, row.MIMEType = "image", "image/png"
				case "input":
					row.ExpiresAt = &row.CreatedAt
				case "whitespace":
					row.SourceJobID += " "
				case "short":
					row.SourceJobID = "short"
				}
				if err := peer.db.Create(&row).Error; err == nil {
					t.Fatalf("invalid source shape accepted: %s", invalid)
				}
			}
		})
	}
}

func TestMediaArchiveSourceProtectsUntilTerminalAndRejectsLateRegistration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			job := seedArchiveSource(t, db)
			store, err := localmedia.NewLocalStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			assets, jobs := NewMediaAssetRepository(db), NewMediaJobRepository(db)
			service := mediaapp.NewServiceWithTickets(assets, jobs, NewMediaUploadTicketRepository(db), store, nil, mediaapp.Config{MaxTotalBytes: 64, CleanupThresholdPercent: 50})
			payload := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 128)...)
			asset, err := service.SaveVideo(ctx, job.ID, "video/mp4", bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			if asset.SourceJobID != job.ID {
				t.Fatalf("source missing from committed asset: %+v", asset)
			}
			current, err := NewMediaJobRepository(peer).GetMediaJob(ctx, job.ID, job.ClientKeyID)
			if err != nil || current.ResultAssetID != "" || current.Execution.Revision != job.Execution.Revision || current.ClaimToken != job.ClaimToken {
				t.Fatalf("archive selected result or rewrote execution: %+v %v", current, err)
			}
			if n, err := service.Cleanup(ctx); err != nil || n != 0 {
				t.Fatalf("active source lost protection: %d %v", n, err)
			}
			job.Status = media.StatusFailed
			job.ErrorCode = "delivery_failed"
			if err := NewMediaJobRepository(peer).UpdateMediaJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if n, err := service.Cleanup(ctx); err != nil || n != 1 {
				t.Fatalf("terminal source permanently protected: %d %v", n, err)
			}
			if _, err := service.SaveVideo(ctx, job.ID, "video/mp4", bytes.NewReader(payload)); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("late terminal archive accepted: %v", err)
			}
			if err := NewMediaJobRepository(peer).DeleteMediaJob(ctx, job.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := service.SaveVideo(ctx, job.ID, "video/mp4", bytes.NewReader(payload)); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("deleted source archive accepted: %v", err)
			}
			files, temps, err := store.ListMediaObjectFiles(ctx)
			if err != nil || len(files) != 0 || len(temps) != 0 {
				t.Fatalf("rejected registration retained object/staging: %d/%d %v", len(files), len(temps), err)
			}
		})
	}
}

func TestMediaAdminDeletionRemovesAllSourceArchivesInBoundedPages(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			job := seedArchiveSource(t, db)
			store, err := localmedia.NewLocalStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			assets, jobs := NewMediaAssetRepository(db), NewMediaJobRepository(db)
			for i := 0; i < 201; i++ {
				asset := archiveAssetFixture(fmt.Sprintf("vid_source_delete_%020d", i), job.ID)
				upload, err := store.BeginVideoUpload(ctx, asset.ID, asset.MIMEType)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = upload.Write(bytes.Repeat([]byte{1}, int(asset.SizeBytes))); err != nil {
					t.Fatal(err)
				}
				asset.StorageKey, err = upload.Commit(ctx)
				abortErr := upload.Abort(ctx)
				if err != nil || abortErr != nil {
					t.Fatalf("fixture commit: %v/%v", err, abortErr)
				}
				if err = assets.CreateMediaAsset(ctx, asset); err != nil {
					t.Fatal(err)
				}
			}
			job.Status = media.StatusFailed
			if err := NewMediaJobRepository(peer).UpdateMediaJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			objects := &mediaDeletionObjectFailure{MediaObjectStorage: store, fail: true}
			service := mediaapp.NewServiceWithTickets(assets, jobs, NewMediaUploadTicketRepository(db), objects, nil, mediaapp.Config{})
			if _, err := service.AdminDeleteVideoJobs(ctx, []string{job.ID}); err == nil {
				t.Fatal("failed object deletion acknowledged")
			}
			if _, err := jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID); err != nil {
				t.Fatalf("failed deletion removed source job: %v", err)
			}
			objects.fail = false
			if n, err := service.AdminDeleteVideoJobs(ctx, []string{job.ID}); err != nil || n != 1 {
				t.Fatalf("source delete retry: %d %v", n, err)
			}
			remaining, err := NewMediaAssetRepository(peer).ListMediaAssetsBySourceJob(ctx, job.ID, 200)
			files, temps, fileErr := store.ListMediaObjectFiles(ctx)
			if err != nil || fileErr != nil || len(remaining)+len(files)+len(temps) != 0 {
				t.Fatalf("source archives survived deletion: assets=%d files=%d/%d err=%v/%v", len(remaining), len(files), len(temps), err, fileErr)
			}
			for _, limit := range []int{0, 201} {
				if _, err := assets.ListMediaAssetsBySourceJob(ctx, job.ID, limit); !errors.Is(err, repository.ErrLimitExceeded) {
					t.Fatalf("unbounded source listing accepted: %d %v", limit, err)
				}
			}
		})
	}
}

func TestMediaArchiveSourceRetainsObjectAfterClientKeyDeletion(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			job := seedArchiveSource(t, db)
			objects, err := localmedia.NewLocalStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			assets, jobs := NewMediaAssetRepository(db), NewMediaJobRepository(db)
			service := mediaapp.NewServiceWithTickets(assets, jobs, NewMediaUploadTicketRepository(db), objects, nil, mediaapp.Config{})
			payload := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 128)...)
			asset, err := service.SaveVideo(ctx, job.ID, "video/mp4", bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			job.Status = media.StatusFailed
			if err := jobs.UpdateMediaJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := NewClientKeyRepository(peer).Delete(ctx, job.ClientKeyID); err != nil {
				t.Fatalf("archive source prevented eligible key deletion: %v", err)
			}
			if _, err := NewMediaJobRepository(peer).GetMediaJob(ctx, job.ID, job.ClientKeyID); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("key deletion left source job: %v", err)
			}
			stored, body, err := service.OpenVideo(ctx, asset.ID)
			if err != nil {
				t.Fatalf("key deletion removed retained archive: %v", err)
			}
			if err := body.Close(); err != nil {
				t.Fatal(err)
			}
			if stored.SourceJobID != job.ID {
				t.Fatalf("deleting key rewrote immutable provenance: %q", stored.SourceJobID)
			}
			protected, err := NewMediaAssetRepository(peer).ListProtectedMediaAssetIDs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, present := protected[asset.ID]; present {
				t.Fatal("deleted source permanently protects retained object")
			}
		})
	}
}
