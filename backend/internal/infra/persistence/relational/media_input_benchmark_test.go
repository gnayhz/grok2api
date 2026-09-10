package relational

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"math/rand"
	"testing"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
)

// Both revisions run the same successful input admission with real SQL and
// local files. Encoding, initialization, and cleanup are outside the timer.
func BenchmarkMediaInputAdmission(b *testing.B) {
	picture := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	random := rand.New(rand.NewSource(47))
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
	video := func(size int) []byte {
		data := bytes.Repeat([]byte{1}, size)
		copy(data, []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'})
		return data
	}
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, fixture := range []struct {
			name    string
			payload []byte
			image   bool
		}{
			{"image_256x256", encoded.Bytes(), true}, {"video_65536", video(64 << 10), false}, {"video_1048576", video(1 << 20), false},
		} {
			b.Run(dialect+"/"+fixture.name, func(b *testing.B) {
				ctx := context.Background()
				db := webProfileCostDatabase(b, dialect)
				objects, err := localmedia.NewLocalStore(b.TempDir())
				if err != nil {
					b.Fatal(err)
				}
				assets := NewMediaAssetRepository(db)
				service := mediaapp.NewService(assets, nil, objects, nil, mediaapp.Config{MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 40, CleanupThresholdPercent: 80})
				b.ReportAllocs()
				b.SetBytes(int64(len(fixture.payload)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var asset media.Asset
					var err error
					if fixture.image {
						asset, err = service.SaveInputImage(ctx, fixture.payload)
					} else {
						asset, err = service.SaveInputVideo(ctx, "video/mp4", bytes.NewReader(fixture.payload))
					}
					b.StopTimer()
					if err != nil {
						b.Fatal(err)
					}
					if asset.ExpiresAt == nil || !media.IsInputAssetID(asset.ID) || asset.SizeBytes != int64(len(fixture.payload)) {
						b.Fatal("benchmark did not admit a complete private input")
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
}
