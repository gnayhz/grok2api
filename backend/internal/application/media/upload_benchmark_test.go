package media

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Measure the full accepted upload against real SQLite and local files. Ticket
// issuance and explicit fixture cleanup are outside the measured operation.
func BenchmarkReceiveVideoUpload(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("bytes_%d", size), func(b *testing.B) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "upload.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = db.Close() })
			if err = db.InitializeSchema(ctx); err != nil {
				b.Fatal(err)
			}
			disk, err := localmedia.NewLocalStore(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			assets := relational.NewMediaAssetRepository(db)
			tickets := relational.NewMediaUploadTicketRepository(db)
			service := NewServiceWithTickets(assets, nil, tickets, disk, nil, Config{MaxTotalBytes: 1 << 40, CleanupThresholdPercent: 80})
			token := strings.Repeat("c", 64)
			hash := hashUploadToken(token)
			payload := bytes.Repeat([]byte{1}, size)
			copy(payload, []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'})
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				now := time.Now().UTC()
				id := fmt.Sprintf("vid_benchmark_%020d", i)
				if err := tickets.CreateUploadTicket(ctx, repository.MediaUploadTicket{TokenHash: hash, AssetID: id, JobID: "benchmark", AllowedMIME: "video/mp4", MaxBytes: int64(size), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
					b.Fatal(err)
				}
				body := bytes.NewReader(payload)
				b.StartTimer()
				asset, err := service.ReceiveVideoUpload(ctx, token, "video/mp4", body)
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				if err = disk.Delete(ctx, asset.StorageKey); err != nil {
					b.Fatal(err)
				}
				if err = assets.DeleteMediaAsset(ctx, asset.ID); err != nil {
					b.Fatal(err)
				}
				if err = tickets.DeleteUploadTicketByHash(ctx, hash); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}
