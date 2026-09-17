package relational

import (
	"context"
	"fmt"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"path/filepath"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

func BenchmarkClientKeyDeletion(b *testing.B) {
	for _, jobCount := range []int{0, 20} {
		b.Run(fmt.Sprintf("terminal_jobs_%d", jobCount), func(b *testing.B) {
			ctx := context.Background()
			db, err := OpenSQLite(ctx, filepath.Join(b.TempDir(), "deletion-bench.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = db.Close() })
			if err := db.InitializeSchema(ctx); err != nil {
				b.Fatal(err)
			}
			keys, jobs := NewClientKeyRepository(db), NewMediaJobRepository(db)
			service := clientkeyapp.NewService("benchmark", keys, nil, nil, 0, 0, nil, security.RandomTokenSource{})
			b.Cleanup(func() { _ = service.Close(ctx) })
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				key, err := keys.Create(ctx, clientkey.Key{Name: "benchmark", Prefix: fmt.Sprintf("deletion%d", i), SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true})
				if err != nil {
					b.Fatal(err)
				}
				now := time.Now().UTC()
				for j := range jobCount {
					job := media.Job{ID: fmt.Sprintf("video_delete_benchmark_%d_%d", i, j), RequestID: "benchmark", ClientKeyID: key.ID, ClientKeyName: key.Name, Provider: "grok_web", Model: "Web/grok-imagine-video", ModelRouteID: 1, UpstreamModel: "grok-imagine-video", Prompt: "synthetic", Seconds: 3, Quality: "720p", Status: media.StatusCompleted, CreatedAt: now, UpdatedAt: now, Quota: media.JobQuota{RecordedAt: &now}, UsageRecordedAt: &now}
					if err := jobs.CreateMediaJob(ctx, job); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
				if n, err := service.BatchDelete(ctx, []uint64{key.ID}); err != nil || n != 1 {
					b.Fatalf("delete=%d err=%v", n, err)
				}
			}
		})
	}
}
