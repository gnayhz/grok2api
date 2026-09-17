package media

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type sweepBenchmarkQueries struct {
	repository.MediaAssetRepository
	calls int
}

func (q *sweepBenchmarkQueries) ListOldestMediaAssets(ctx context.Context, offset, limit int) ([]mediadomain.Asset, error) {
	q.calls++
	return q.MediaAssetRepository.ListOldestMediaAssets(ctx, offset, limit)
}
func (q *sweepBenchmarkQueries) FindMediaAssetStorageKeys(ctx context.Context, keys []string) (map[string]struct{}, error) {
	q.calls++
	return q.MediaAssetRepository.(interface {
		FindMediaAssetStorageKeys(context.Context, []string) (map[string]struct{}, error)
	}).FindMediaAssetStorageKeys(ctx, keys)
}

// Shared metadata may contain objects stored on other instances. Measure a real
// local sweep with 16 referenced files and 4096 total SQLite metadata rows.
func BenchmarkOrphanSweepSharedMetadata(b *testing.B) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "sweep.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	if err = db.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	root := b.TempDir()
	disk, err := localmedia.NewLocalStore(root)
	if err != nil {
		b.Fatal(err)
	}
	source := &sweepBenchmarkQueries{MediaAssetRepository: relational.NewMediaAssetRepository(db)}
	old := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 4096; i++ {
		id := fmt.Sprintf("img_benchmark_%020d", i)
		key := "images/remote/" + id + ".png"
		if i < 16 {
			key, err = disk.SaveImage(ctx, id, "image/png", []byte("asset"))
			if err != nil {
				b.Fatal(err)
			}
			p := filepath.Join(root, filepath.FromSlash(key))
			if err = os.Chtimes(p, old, old); err != nil {
				b.Fatal(err)
			}
		}
		if err = source.CreateMediaAsset(ctx, mediadomain.Asset{ID: id, Kind: "image", StorageKey: key, MIMEType: "image/png", SizeBytes: 5, SHA256: strings.Repeat("0", 64), CreatedAt: old}); err != nil {
			b.Fatal(err)
		}
	}
	service := NewServiceWithTickets(source, nil, nil, disk, nil, Config{})
	now := time.Now().UTC()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if n, err := service.sweepOrphanObjects(ctx, now); err != nil || n != 0 {
			b.Fatalf("sweep: %d %v", n, err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(source.calls)/float64(b.N), "queries/op")
}
