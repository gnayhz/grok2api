package media

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/png"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

// Both revisions coordinate the same already-retrieved image and perform real
// asset registration. This isolates the moved URL/use-case boundary and local
// storage cost; DNS and network timing are deliberately outside this benchmark.
func BenchmarkImageImportCoordinationAndStorage(b *testing.B) {
	picture := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	random := rand.New(rand.NewSource(49))
	for i := 0; i < len(picture.Pix); i += 4 {
		picture.Pix[i] = byte(random.Intn(256))
		picture.Pix[i+1] = byte(random.Intn(256))
		picture.Pix[i+2] = byte(random.Intn(256))
		picture.Pix[i+3] = 255
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		b.Fatal(err)
	}
	for _, dialect := range []string{"sqlite", "postgres"} {
		b.Run(dialect, func(b *testing.B) {
			ctx := context.Background()
			var db *relational.Database
			var err error
			if dialect == "sqlite" {
				db, err = relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "import.db"))
			} else {
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					b.Skip("isolated TEST_POSTGRES_DSN required")
				}
				admin, err := sql.Open("pgx", dsn)
				if err != nil {
					b.Fatal(err)
				}
				schema := fmt.Sprintf("g49_import_cost_%d", time.Now().UnixNano())
				if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
					_ = admin.Close()
					b.Fatal(err)
				}
				b.Cleanup(func() {
					defer admin.Close()
					if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
						b.Error(err)
					}
				})
				parsed, err := url.Parse(dsn)
				if err != nil {
					b.Fatal(err)
				}
				q := parsed.Query()
				q.Set("search_path", schema)
				parsed.RawQuery = q.Encode()
				db, err = relational.OpenPostgres(ctx, parsed.String(), 4, 2)
				if err != nil {
					b.Fatal(err)
				}
			}
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = db.Close() })
			if err := db.InitializeSchema(ctx); err != nil {
				b.Fatal(err)
			}
			objects, err := localmedia.NewLocalStore(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			assets := relational.NewMediaAssetRepository(db)
			service := mediaapp.NewServiceWithTickets(assets, nil, nil, objects, nil, mediaapp.Config{MaxImageBytes: 1 << 20, MaxTotalBytes: 1 << 40, CleanupThresholdPercent: 80})
			runImport := imageImportCostOperation(service, "https://example.com/image.png", encoded.Bytes())
			b.ReportAllocs()
			b.SetBytes(int64(encoded.Len()))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				asset, err := runImport(ctx)
				b.StopTimer()
				if err != nil || asset.SizeBytes != int64(encoded.Len()) || asset.ExpiresAt == nil {
					b.Fatalf("incomplete import: size=%d err=%v", asset.SizeBytes, err)
				}
				if err := objects.Delete(ctx, asset.StorageKey); err != nil {
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
