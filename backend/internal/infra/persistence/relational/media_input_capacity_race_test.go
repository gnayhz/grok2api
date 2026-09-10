package relational

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type inputCapacityObservedAssets struct {
	repository.MediaAssetRepository
	observed chan<- struct{}
	proceed  <-chan struct{}
}

func (r *inputCapacityObservedAssets) TotalMediaAssetBytes(ctx context.Context) (int64, error) {
	total, err := r.MediaAssetRepository.TotalMediaAssetBytes(ctx)
	if err != nil {
		return total, err
	}
	r.observed <- struct{}{}
	select {
	case <-r.proceed:
		return total, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func TestInputAssetAdmissionSerializesSharedCapacity(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, pair := range []string{"images", "videos", "mixed"} {
			t.Run(dialect+"/"+pair, func(t *testing.T) {
				db, peer := settingsDatabasePair(t, dialect)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				objects, err := localmedia.NewLocalStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				var encoded bytes.Buffer
				if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
					t.Fatal(err)
				}
				picture := encoded.Bytes()
				video := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 128)...)
				sizes := []int{len(picture), len(picture)}
				if pair == "videos" {
					sizes[0], sizes[1] = len(video), len(video)
				}
				if pair == "mixed" {
					sizes[1] = len(video)
				}
				capacity := int64(max(sizes[0], sizes[1]) + min(sizes[0], sizes[1])/2)
				observed, proceed := make(chan struct{}, 2), make(chan struct{})
				cfg := mediaapp.Config{MaxImageBytes: 1 << 20, MaxTotalBytes: capacity, CleanupThresholdPercent: 100}
				services := []*mediaapp.Service{
					mediaapp.NewService(&inputCapacityObservedAssets{NewMediaAssetRepository(db), observed, proceed}, nil, objects, nil, cfg),
					mediaapp.NewService(&inputCapacityObservedAssets{NewMediaAssetRepository(peer), observed, proceed}, nil, objects, nil, cfg),
				}
				type result struct {
					asset media.Asset
					err   error
				}
				results := make(chan result, 2)
				for i, service := range services {
					go func(i int, service *mediaapp.Service) {
						var asset media.Asset
						var err error
						if pair == "videos" || (pair == "mixed" && i == 1) {
							asset, err = service.SaveInputVideo(ctx, "video/mp4", bytes.NewReader(video))
						} else {
							asset, err = service.SaveInputImage(ctx, picture)
						}
						results <- result{asset, err}
					}(i, service)
				}
				for range 2 {
					select {
					case <-observed:
					case <-ctx.Done():
						close(proceed)
						t.Fatal(ctx.Err())
					}
				}
				close(proceed)
				accepted, refused := 0, 0
				var winner media.Asset
				for range 2 {
					result := <-results
					if result.err == nil {
						accepted++
						winner = result.asset
					} else if errors.Is(result.err, mediaapp.ErrMediaCapacity) {
						refused++
					} else {
						t.Errorf("unexpected admission failure: %v", result.err)
					}
				}
				total, err := NewMediaAssetRepository(peer).TotalMediaAssetBytes(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if accepted != 1 || refused != 1 || total > capacity {
					t.Fatalf("shared input capacity exceeded: accepted=%d refused=%d total=%d limit=%d", accepted, refused, total, capacity)
				}
				files, temps, err := objects.ListMediaObjectFiles(ctx)
				if err != nil || len(files) != 1 || len(temps) != 0 {
					t.Fatalf("refused input retained objects: %d/%d %v", len(files), len(temps), err)
				}
				retry := mediaapp.NewService(NewMediaAssetRepository(peer), nil, objects, nil, cfg)
				if err := retry.ReleaseInputAssets(ctx, []string{media.InputReference(winner.ID)}); err != nil {
					t.Fatal(err)
				}
				if _, err := retry.SaveInputImage(ctx, picture); err != nil {
					t.Fatalf("peer cannot reuse released capacity: %v", err)
				}

			})
		}
	}
}
