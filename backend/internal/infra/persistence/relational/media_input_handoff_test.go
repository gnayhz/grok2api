package relational

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
)

func TestMediaJobInputHandoffOrders(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, order := range []string{"release_first", "create_first"} {
			t.Run(dialect+"/"+order, func(t *testing.T) {
				db, peer := settingsDatabasePair(t, dialect)
				ctx := context.Background()
				objects, err := localmedia.NewLocalStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				cfg := mediaapp.Config{MaxImageBytes: 1 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80}
				owner := mediaapp.NewService(NewMediaAssetRepository(db), NewMediaJobRepository(db), objects, nil, cfg)
				releaser := mediaapp.NewService(NewMediaAssetRepository(peer), NewMediaJobRepository(peer), objects, nil, cfg)
				picture, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
				input, err := owner.SaveInputImage(ctx, picture)
				if err != nil {
					t.Fatal(err)
				}
				_, body, err := owner.OpenInputAsset(ctx, input.ID)
				if err != nil {
					t.Fatal(err)
				}
				_ = body.Close()
				_, job := seedMediaDeletion(t, db, "input_handoff", media.StatusQueued)
				job.InputJSON = fmt.Sprintf(`{"image_urls":[%q]}`, media.InputReference(input.ID))
				if order == "release_first" {
					if err := releaser.ReleaseInputAssets(ctx, []string{media.InputReference(input.ID)}); err != nil {
						t.Fatal(err)
					}
				}
				createErr := NewMediaJobRepository(db).CreateMediaJob(ctx, job)
				if order == "release_first" {
					if createErr == nil {
						t.Fatal("job accepted an input already released after precheck")
					}
					return
				}
				if createErr != nil {
					t.Fatal(createErr)
				}
				if err := releaser.ReleaseInputAssets(ctx, []string{media.InputReference(input.ID)}); err != nil {
					t.Fatal(err)
				}
				_, body, err = owner.OpenInputAsset(ctx, input.ID)
				if err != nil {
					t.Fatalf("active job lost input: %v", err)
				}
				var stored bytes.Buffer
				_, err = stored.ReadFrom(body)
				_ = body.Close()
				if err != nil || !bytes.Equal(stored.Bytes(), picture) {
					t.Fatalf("active input changed: %v", err)
				}
			})
		}
	}
}
