package media

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type sweepConcurrentAssets struct {
	repository.MediaAssetRepository
	afterRead func(context.Context) error
}

func (r *sweepConcurrentAssets) changed(ctx context.Context) error {
	if r.afterRead == nil {
		return nil
	}
	f := r.afterRead
	r.afterRead = nil
	return f(ctx)
}
func (r *sweepConcurrentAssets) ListOldestMediaAssets(ctx context.Context, offset, limit int) ([]mediadomain.Asset, error) {
	values, err := r.MediaAssetRepository.ListOldestMediaAssets(ctx, offset, limit)
	if err == nil {
		err = r.changed(ctx)
	}
	return values, err
}

// The new candidate lookup is selected only when the implementation supports it;
// the same regression also compiles on the baseline's offset scanner.
func (r *sweepConcurrentAssets) FindMediaAssetStorageKeys(ctx context.Context, keys []string) (map[string]struct{}, error) {
	source, ok := r.MediaAssetRepository.(interface {
		FindMediaAssetStorageKeys(context.Context, []string) (map[string]struct{}, error)
	})
	if !ok {
		return nil, errors.New("candidate lookup unavailable")
	}
	values, err := source.FindMediaAssetStorageKeys(ctx, keys)
	if err == nil {
		err = r.changed(ctx)
	}
	return values, err
}

func TestSweepConcurrentMetadataDeletionPreservesLiveObjects(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) { testSweepConcurrentDeletion(t, dialect) })
	}
}

func sweepDatabasePair(t *testing.T, dialect string) (*relational.Database, *relational.Database) {
	t.Helper()
	ctx := context.Background()
	var open func() (*relational.Database, error)
	if dialect == "sqlite" {
		path := filepath.Join(t.TempDir(), "sweep.db")
		open = func() (*relational.Database, error) { return relational.OpenSQLite(ctx, path) }
	} else {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("isolated TEST_POSTGRES_DSN required")
		}
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		schema := fmt.Sprintf("g43_sweep_%d", time.Now().UnixNano())
		if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			_ = admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			defer admin.Close()
			if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
				t.Error(err)
			}
		})
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := parsed.Query()
		q.Set("search_path", schema)
		parsed.RawQuery = q.Encode()
		open = func() (*relational.Database, error) { return relational.OpenPostgres(ctx, parsed.String(), 4, 2) }
	}
	db, err := open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	peer, err := open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	return db, peer
}

func testSweepConcurrentDeletion(t *testing.T, dialect string) {
	ctx := context.Background()
	db, peer := sweepDatabasePair(t, dialect)
	root := t.TempDir()
	disk, err := localmedia.NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80}
	admin := NewService(relational.NewMediaAssetRepository(peer), nil, disk, nil, cfg)
	rows := &sweepConcurrentAssets{MediaAssetRepository: relational.NewMediaAssetRepository(db)}
	subject := NewService(rows, nil, disk, nil, cfg)
	raw, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}
	assets := make([]mediadomain.Asset, cleanupAssetBatchSize+1)
	old := time.Now().Add(-48 * time.Hour)
	for i := range assets {
		assets[i], err = admin.SaveImage(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(root, filepath.FromSlash(assets[i].StorageKey))
		if err = os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	shifted := false
	rows.afterRead = func(ctx context.Context) error {
		n, err := admin.AdminDeleteImages(ctx, []string{assets[0].ID})
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("concurrent admin deletion did not remove one image")
		}
		shifted = true
		return nil
	}
	deleted, err := subject.sweepOrphanObjects(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !shifted {
		t.Fatal("interleaving was not exercised")
	}
	for _, asset := range assets[1:] {
		if _, err = rows.GetMediaAsset(ctx, asset.ID); err != nil {
			t.Fatal(err)
		}
		_, body, err := admin.OpenImage(ctx, asset.ID)
		if err != nil {
			t.Errorf("live asset lost during reconciliation: id=%s sweep-deleted=%d err=%v", asset.ID, deleted, err)
			continue
		}
		if err = body.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
