package relational

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

// Identical fixture on both revisions, using the public repository entry points.
// Schema, fixture setup, job deletion and expiry reset are outside the timer.
func BenchmarkMediaInputHandoff(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, scenario := range []string{"create_remote", "create_data_1MiB", "create_local_1", "create_local_8", "release_free", "release_active", "release_escaped_128"} {
			b.Run(dialect+"/"+scenario, func(b *testing.B) {
				ctx := context.Background()
				db := webProfileCostDatabase(b, dialect)
				jobs, assets := NewMediaJobRepository(db), NewMediaAssetRepository(db)
				key, err := NewClientKeyRepository(db).Create(ctx, clientkey.Key{Name: "input-cost", Prefix: "input-cost", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true})
				if err != nil {
					b.Fatal(err)
				}
				now := time.Now().UTC()
				expiry := now.Add(time.Hour)
				ids := make([]string, 8)
				refs := make([]string, 8)
				for i := range ids {
					ids[i] = "input_" + strings.Repeat(string(rune('A'+i)), 32)
					refs[i] = media.InputReference(ids[i])
					asset := mediaAssetModel{ID: ids[i], Kind: "image", StorageKey: fmt.Sprintf("input-cost-%d.png", i), MIMEType: "image/png", SizeBytes: 64, SHA256: strings.Repeat("a", 64), ExpiresAt: &expiry, CreatedAt: now}
					if err := db.db.Create(&asset).Error; err != nil {
						b.Fatal(err)
					}
				}
				job := media.Job{ID: "video_input_cost", RequestID: "input_cost", ClientKeyID: key.ID, ClientKeyName: key.Name, Provider: "grok_web", Model: "Web/grok-imagine-video", ModelRouteID: 1, UpstreamModel: "grok-imagine-video", Prompt: "cost", Seconds: 3, Quality: "720p", Status: media.StatusQueued, CreatedAt: now, UpdatedAt: now}
				var payload map[string]any
				switch scenario {
				case "create_remote":
					payload = map[string]any{"image_url": "https://example.invalid/picture.png"}
				case "create_data_1MiB":
					payload = map[string]any{"image_url": "data:image/png;base64," + strings.Repeat("A", 1<<20)}
				case "create_local_8":
					payload = map[string]any{"reference_urls": refs}
				default:
					payload = map[string]any{"image_url": refs[0]}
				}
				encoded, err := json.Marshal(payload)
				if err != nil {
					b.Fatal(err)
				}
				job.InputJSON = string(encoded)
				if scenario == "release_active" {
					if err := jobs.CreateMediaJob(ctx, job); err != nil {
						b.Fatal(err)
					}
				}
				if scenario == "release_escaped_128" {
					for i := range 128 {
						other := job
						other.ID += fmt.Sprintf("_%d", i)
						other.RequestID += fmt.Sprintf("_%d", i)
						other.InputJSON = `{"image_url":"https://example.invalid/image?a=1\u0026b=` + strings.Repeat("b", 1024) + `"}`
						if err := jobs.CreateMediaJob(ctx, other); err != nil {
							b.Fatal(err)
						}
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if strings.HasPrefix(scenario, "create_") {
						err := jobs.CreateMediaJob(ctx, job)
						b.StopTimer()
						if err != nil {
							b.Fatal(err)
						}
						if err := db.db.Where("id = ?", job.ID).Delete(&mediaJobModel{}).Error; err != nil {
							b.Fatal(err)
						}
					} else {
						expired, err := assets.ExpireMediaInputIfUnreferenced(ctx, ids[0], now)
						b.StopTimer()
						if err != nil || expired == (scenario == "release_active") {
							b.Fatalf("release result=%v err=%v", expired, err)
						}
						if err := db.db.Model(&mediaAssetModel{}).Where("id = ?", ids[0]).Update("expires_at", expiry).Error; err != nil {
							b.Fatal(err)
						}
					}
					b.StartTimer()
				}
			})
		}
	}
}
