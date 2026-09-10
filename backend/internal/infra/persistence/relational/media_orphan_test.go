package relational

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestMediaAssetCandidateLookupCurrentScopeAndCancellation(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			db, peer := settingsDatabasePair(t, dialect)
			source, writer := NewMediaAssetRepository(db), NewMediaAssetRepository(peer)
			keys := make([]string, repository.MaxMediaAssetLookupKeys)
			for i := range keys {
				keys[i] = fmt.Sprintf("images/%03d/img_candidate_0123456789.png", i)
				asset := media.Asset{ID: fmt.Sprintf("img_candidate_%020d", i), Kind: "image", StorageKey: keys[i], MIMEType: "image/png", SHA256: strings.Repeat("0", 64), SizeBytes: 1, CreatedAt: time.Now().UTC()}
				if err := writer.CreateMediaAsset(ctx, asset); err != nil {
					t.Fatal(err)
				}
			}
			got, err := source.FindMediaAssetStorageKeys(ctx, keys)
			if err != nil || len(got) != len(keys) {
				t.Fatalf("all candidates: %d %v", len(got), err)
			}
			if err = writer.DeleteMediaAsset(ctx, "img_candidate_00000000000000000000"); err != nil {
				t.Fatal(err)
			}
			got, err = source.FindMediaAssetStorageKeys(ctx, []string{keys[0], keys[1], keys[1], "unknown", " " + keys[1]})
			if _, ok := got[keys[1]]; err != nil || len(got) != 1 || !ok {
				t.Fatalf("exact current subset: %v %v", got, err)
			}
			release := func() {}
			if dialect == "sqlite" {
				pool, err := db.db.DB()
				if err != nil {
					t.Fatal(err)
				}
				pool.SetMaxOpenConns(1)
				conn, err := pool.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				release = func() { _ = conn.Close() }
			} else {
				tx := peer.db.Begin()
				if tx.Error != nil {
					t.Fatal(tx.Error)
				}
				if err := tx.Exec("LOCK TABLE media_assets IN ACCESS EXCLUSIVE MODE").Error; err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				release = func() {
					if err := tx.Rollback().Error; err != nil {
						t.Error(err)
					}
				}
			}
			blocked, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
			got, err = source.FindMediaAssetStorageKeys(blocked, []string{keys[1]})
			cancel()
			release()
			if !errors.Is(err, context.DeadlineExceeded) || got != nil {
				t.Fatalf("cancelled read claimed complete subset: %v %v", got, err)
			}
			if got, err = source.FindMediaAssetStorageKeys(ctx, []string{keys[1]}); err != nil || len(got) != 1 {
				t.Fatalf("retry: %v %v", got, err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err = source.FindMediaAssetStorageKeys(ctx, []string{keys[1]}); err == nil {
				t.Fatal("closed SQL claimed absence")
			}
			if got, err = source.FindMediaAssetStorageKeys(ctx, nil); err != nil || len(got) != 0 {
				t.Fatalf("empty query touched SQL: %v %v", got, err)
			}
			if _, err = source.FindMediaAssetStorageKeys(ctx, make([]string, 201)); !errors.Is(err, repository.ErrLimitExceeded) {
				t.Fatalf("oversized query was not rejected before SQL: %v", err)
			}
		})
	}
}
