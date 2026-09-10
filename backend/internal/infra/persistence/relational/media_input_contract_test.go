package relational

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestMediaInputAssetRegistrationRequiresExplicitCapacity(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repo := NewMediaAssetRepository(db)
			expiry := time.Now().UTC().Add(time.Hour)
			asset := media.Asset{ID: "input_" + strings.Repeat("A", 32), Kind: "image", StorageKey: "images/input-contract.png", MIMEType: "image/png", SizeBytes: 100, SHA256: strings.Repeat("0", 64), ExpiresAt: &expiry, CreatedAt: time.Now().UTC()}
			if err := repo.CreateMediaAsset(ctx, asset); !errors.Is(err, repository.ErrInvalidRecord) {
				t.Fatalf("ordinary registration bypassed input admission: %v", err)
			}
			noTTL := asset
			noTTL.ExpiresAt = nil
			if err := repo.CreateMediaAsset(ctx, noTTL); !errors.Is(err, repository.ErrInvalidRecord) {
				t.Fatalf("input namespace became a public asset: %v", err)
			}
			if err := repo.CreateMediaInputAsset(ctx, noTTL, 100); !errors.Is(err, repository.ErrInvalidRecord) {
				t.Fatalf("private registration without TTL: %v", err)
			}
			for _, limit := range []int64{-1, 0, 99} {
				if err := repo.CreateMediaInputAsset(ctx, asset, limit); !errors.Is(err, repository.ErrLimitExceeded) {
					t.Fatalf("capacity %d admitted %d bytes: %v", limit, asset.SizeBytes, err)
				}
			}
			invalid := asset
			invalid.SHA256 = "invalid"
			if err := repo.CreateMediaInputAsset(ctx, invalid, 100); err == nil {
				t.Fatal("actual SQL constraint violation accepted")
			}
			if total, err := NewMediaAssetRepository(peer).TotalMediaAssetBytes(ctx); err != nil || total != 0 {
				t.Fatalf("rejected registration retained capacity: %d %v", total, err)
			}
			if err := NewMediaAssetRepository(peer).CreateMediaInputAsset(ctx, asset, 100); err != nil {
				t.Fatalf("peer cannot admit at exact capacity: %v", err)
			}
			got, err := repo.GetMediaAsset(ctx, asset.ID)
			if err != nil || got.ExpiresAt == nil || got.SizeBytes != 100 || got.SourceJobID != "" {
				t.Fatalf("input facts changed: %+v %v", got, err)
			}
		})
	}
}
