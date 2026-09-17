package gateway

import (
	"context"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

func BenchmarkVideoResourceProjection(b *testing.B) {
	for _, scenario := range []string{"queued", "local", "unconfigured"} {
		b.Run(scenario, func(b *testing.B) {
			job := media.Job{ID: "video", ClientKeyID: 7, Status: media.StatusCompleted, ResultAssetID: "asset"}
			if scenario == "queued" {
				job.Status = media.StatusQueued
			}
			service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory()}
			service.ConfigureMedia(&videoUsageRepository{job: job}, mediaapp.NewVideoResources(&videoUsageRepository{job: job}, nil), 1)
			if scenario == "local" {
				service.ConfigureMediaAssets(&videoAssetStoreStub{openAsset: media.Asset{ID: "asset", Kind: "video"}, openData: []byte("video")})
			}
			ctx := context.Background()
			key := clientkey.Key{ID: 7, ModelScope: clientkey.ModelScopeAll}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, err := service.GetVideo(ctx, "video", key)
				if err != nil || result.ID != "video" {
					b.Fatalf("%+v %v", result, err)
				}
			}
		})
	}
}
