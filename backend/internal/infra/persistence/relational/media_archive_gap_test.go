package relational

import (
	"bytes"
	"context"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type archiveCleanupAssets struct {
	repository.MediaAssetRepository
	after func() error
}

func (r *archiveCleanupAssets) CreateMediaAsset(ctx context.Context, asset mediadomain.Asset) error {
	if err := r.MediaAssetRepository.CreateMediaAsset(ctx, asset); err != nil {
		return err
	}
	return r.after()
}

func TestActiveVideoArchiveSurvivesConcurrentCapacityCleanup(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []string{"legacy", "checkpoint"} {
			t.Run(dialect+"/"+kind, func(t *testing.T) {
				db, peer := settingsDatabasePair(t, dialect)
				ctx := context.Background()
				key := clientKeyModel{Name: "archive", Prefix: "archive", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
				if err := db.db.Create(&key).Error; err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				job := testMediaJob("video_archive_cleanup", 0, key.ID, mediadomain.StatusInProgress, now)
				if kind == "checkpoint" {
					job.Execution = mediadomain.VideoExecution{Revision: 1, Phase: mediadomain.VideoExecutionGenerated, Route: "web", Endpoint: "https://example.test/video", GeneratedAt: &now}
					job.ClaimToken = "archive_claim_000000000000000000000001"
				}
				if err := NewMediaJobRepository(db).CreateMediaJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				store, err := localmedia.NewLocalStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				cfg := mediaapp.Config{MaxTotalBytes: 64, CleanupThresholdPercent: 50}
				cleaner := mediaapp.NewServiceWithTickets(NewMediaAssetRepository(peer), NewMediaJobRepository(peer), NewMediaUploadTicketRepository(peer), store, nil, cfg)
				deleted := -1
				assets := &archiveCleanupAssets{MediaAssetRepository: NewMediaAssetRepository(db), after: func() error {
					var err error
					deleted, err = cleaner.Cleanup(ctx)
					return err
				}}
				service := mediaapp.NewServiceWithTickets(assets, NewMediaJobRepository(db), NewMediaUploadTicketRepository(db), store, nil, cfg)
				payload := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 128)...)
				asset, err := service.SaveVideo(ctx, job.ID, "video/mp4", bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}
				if deleted != 0 {
					t.Errorf("active archive deleted before handoff: %d", deleted)
				}
				_, body, err := service.OpenVideo(ctx, asset.ID)
				if err != nil {
					t.Fatalf("successful archive is unreadable before result handoff: %v", err)
				}
				_ = body.Close()
			})
		}
	}
}
