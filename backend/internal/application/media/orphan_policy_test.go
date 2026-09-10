package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type sweepCandidateQuery struct {
	repository.MediaAssetRepository
	lookup func(context.Context, []string) (map[string]struct{}, error)
}

func (q sweepCandidateQuery) FindMediaAssetStorageKeys(ctx context.Context, keys []string) (map[string]struct{}, error) {
	return q.lookup(ctx, keys)
}

type sweepObjects struct {
	repository.MediaObjectStorage
	objects, temps map[string]time.Time
	deleted        []string
	deleteErr      error
}

func (s *sweepObjects) ListMediaObjectFiles(context.Context) (map[string]time.Time, map[string]time.Time, error) {
	return s.objects, s.temps, nil
}
func (s *sweepObjects) Delete(_ context.Context, key string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, key)
	return nil
}

func TestOrphanCandidatesAreBoundedAndFailuresStopDeletion(t *testing.T) {
	cause := errors.New("SQL is unavailable")
	now := time.Now().UTC()
	for _, scenario := range []string{"success", "lookup-failed", "cancel-after-lookup", "delete-failed", "unconfigured"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			objects := &sweepObjects{objects: map[string]time.Time{}, temps: map[string]time.Time{"old-temp": now.Add(-48 * time.Hour), "fresh-temp": now}}
			for i := 0; i < 401; i++ {
				objects.objects[fmt.Sprintf("old-%03d", i)] = now.Add(-48 * time.Hour)
			}
			objects.objects["fresh-object"] = now
			queries := 0
			queried := map[string]struct{}{}
			var assets repository.MediaAssetRepository = sweepCandidateQuery{lookup: func(ctx context.Context, keys []string) (map[string]struct{}, error) {
				queries++
				if len(keys) == 0 || len(keys) > repository.MaxMediaAssetLookupKeys {
					t.Fatalf("unbounded query: %d", len(keys))
				}
				for _, k := range keys {
					if k == "fresh-object" {
						t.Fatal("fresh object queried")
					}
					if _, duplicate := queried[k]; duplicate {
						t.Fatalf("duplicate candidate %s", k)
					}
					queried[k] = struct{}{}
				}
				if scenario == "lookup-failed" {
					return nil, cause
				}
				if scenario == "cancel-after-lookup" {
					cancel()
				}
				return map[string]struct{}{}, nil
			}}
			if scenario == "unconfigured" {
				assets = nil
			}
			if scenario == "delete-failed" {
				objects.deleteErr = cause
			}
			service := NewService(assets, nil, objects, nil, Config{})
			n, err := service.sweepOrphanObjects(ctx, now)
			if scenario == "success" {
				if err != nil || n != 402 || queries != 3 || len(queried) != 401 {
					t.Fatalf("n=%d queries=%d keys=%d err=%v", n, queries, len(queried), err)
				}
				for _, key := range objects.deleted {
					if key == "fresh-object" || key == "fresh-temp" {
						t.Fatal("fresh file deleted")
					}
				}
			} else {
				if err == nil || n != 0 || len(objects.deleted) != 0 {
					t.Fatalf("failure permitted deletion: n=%d deleted=%v err=%v", n, objects.deleted, err)
				}
				if scenario != "unconfigured" && queries != 1 {
					t.Fatalf("failure continued queries: %d", queries)
				}
				if (scenario == "lookup-failed" || scenario == "delete-failed") && !errors.Is(err, cause) {
					t.Fatal("lost cause")
				}
				if scenario == "cancel-after-lookup" && !errors.Is(err, context.Canceled) {
					t.Fatal("lost cancellation")
				}
			}
		})
	}
	objects := &sweepObjects{objects: map[string]time.Time{"fresh": now}}
	service := NewService(nil, nil, objects, nil, Config{})
	if n, err := service.sweepOrphanObjects(context.Background(), now); n != 0 || err != nil {
		t.Fatalf("fresh-only needs no SQL: %d %v", n, err)
	}
}

func TestOrphanSQLCloseRetainsObjectsAndRebuildRetries(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			db, peer := sweepDatabasePair(t, dialect)
			root := t.TempDir()
			disk, err := localmedia.NewLocalStore(root)
			if err != nil {
				t.Fatal(err)
			}
			key, err := disk.SaveImage(ctx, "img_orphan_sql_012345678901", "image/png", []byte("crash residue"))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, filepath.FromSlash(key))
			stamp := time.Now().Add(-48 * time.Hour)
			if err = os.Chtimes(path, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			subject := NewService(relational.NewMediaAssetRepository(db), nil, disk, nil, Config{})
			if n, err := subject.sweepOrphanObjects(ctx, time.Now().UTC()); err == nil || n != 0 {
				t.Fatalf("closed SQL permitted deletion: n=%d err=%v", n, err)
			}
			if _, err = os.Stat(path); err != nil {
				t.Fatalf("unverified orphan lost: %v", err)
			}
			fresh := NewService(relational.NewMediaAssetRepository(peer), nil, disk, nil, Config{})
			if n, err := fresh.sweepOrphanObjects(ctx, time.Now().UTC()); err != nil || n != 1 {
				t.Fatalf("rebuild did not reclaim orphan: n=%d err=%v", n, err)
			}
			if n, err := fresh.sweepOrphanObjects(ctx, time.Now().UTC()); err != nil || n != 0 {
				t.Fatalf("retry not idempotent: n=%d err=%v", n, err)
			}
		})
	}
}
