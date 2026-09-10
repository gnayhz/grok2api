package relational

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
)

// The same fixture runs on both revisions. It measures a complete native archive
// and immediate readable handoff using an active checkpoint job, SQL, and disk.
// Setup and deleting the previous iteration's asset are outside the timer.
func BenchmarkVideoArchiveAndOpen(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, size := range []int{64 << 10, 1 << 20} {
			b.Run(fmt.Sprintf("%s/bytes_%d", dialect, size), func(b *testing.B) {
				ctx := context.Background()
				db := webProfileCostDatabase(b, dialect)
				key, err := NewClientKeyRepository(db).Create(ctx, clientkey.Key{Name: "archive-cost", Prefix: "archive-cost", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true})
				if err != nil {
					b.Fatal(err)
				}
				now := time.Now().UTC()
				job := media.Job{ID: "video_archive_benchmark", RequestID: "request_archive_benchmark", ClientKeyID: key.ID, ClientKeyName: key.Name, Provider: "grok_web", Model: "Web/grok-imagine-video", ModelRouteID: 1, UpstreamModel: "grok-imagine-video", Prompt: "synthetic", Seconds: 3, Quality: "720p", Status: media.StatusInProgress, CreatedAt: now, UpdatedAt: now, ClaimToken: "archive_benchmark_claim", Execution: media.VideoExecution{Revision: 1, Phase: media.VideoExecutionGenerated, Route: "web", Endpoint: "https://example.test/video", GeneratedAt: &now}}
				jobs, assets := NewMediaJobRepository(db), NewMediaAssetRepository(db)
				if err := jobs.CreateMediaJob(ctx, job); err != nil {
					b.Fatal(err)
				}
				disk, err := localmedia.NewLocalStore(b.TempDir())
				if err != nil {
					b.Fatal(err)
				}
				service := mediaapp.NewServiceWithTickets(assets, jobs, NewMediaUploadTicketRepository(db), disk, nil, mediaapp.Config{MaxTotalBytes: 1 << 40, CleanupThresholdPercent: 80})
				payload := bytes.Repeat([]byte{1}, size)
				copy(payload, []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'})
				b.ReportAllocs()
				b.SetBytes(int64(size))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					asset, err := service.SaveVideo(ctx, job.ID, "video/mp4", bytes.NewReader(payload))
					if err != nil {
						b.Fatal(err)
					}
					_, body, err := service.OpenVideo(ctx, asset.ID)
					if err != nil {
						b.Fatal(err)
					}
					n, readErr := io.Copy(io.Discard, body)
					closeErr := body.Close()
					b.StopTimer()
					if readErr != nil || closeErr != nil || n != int64(size) {
						b.Fatalf("archive not readable: %d %v %v", n, readErr, closeErr)
					}
					if err := disk.Delete(ctx, asset.StorageKey); err != nil {
						b.Fatal(err)
					}
					if err := assets.DeleteMediaAsset(ctx, asset.ID); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
				}
			})
		}
	}
}
