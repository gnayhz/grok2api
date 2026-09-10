package registry

import (
	"context"
	"errors"
	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

func TestRegistrySharesExactSQLitePathWithMainStore(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"hash#part.db", "question?part.db", "percent%25.db", "space 中文.db", "options?_pragma=foreign_keys(0)#.db"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(root, name)
			main, err := relational.OpenSQLite(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer main.Close()
			if err := main.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			reg, err := Open(ctx, Options{SQLitePath: path})
			if err != nil {
				t.Fatal(err)
			}
			defer reg.Close()
			if !reg.DB().Migrator().HasTable("provider_accounts") {
				t.Fatal("quality and main store opened different physical files")
			}
			var databases []struct {
				Seq  int
				Name string
				File string
			}
			if err := reg.DB().Raw("PRAGMA database_list").Scan(&databases).Error; err != nil {
				t.Fatal(err)
			}
			found := false
			var foreignKeys int
			if err := reg.DB().Raw("PRAGMA foreign_keys").Scan(&foreignKeys).Error; err != nil || foreignKeys != 1 {
				t.Fatalf("foreign key policy changed: %d %v", foreignKeys, err)
			}
			for _, db := range databases {
				if db.Name == "main" {
					found = true
				}
				if db.Name == "main" && db.File != path {
					t.Fatalf("opened %q, expected literal path %q", db.File, path)
				}
			}
			if !found {
				t.Fatal("main database identity missing")
			}
			if err := reg.RecordExitIP(ctx, 17, "198.51.100.17"); err != nil {
				t.Fatal(err)
			}
			second, err := Open(ctx, Options{SQLitePath: path})
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			archives, err := second.ListNodeIPArchives(ctx, 10)
			if err != nil || len(archives) != 1 || archives[0].CurrentIP != "198.51.100.17" {
				t.Fatalf("reopen lost persisted state: %+v %v", archives, err)
			}
		})
	}
}

func TestRegistryCancelledConstructionReleasesStorage(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor check requires procfs")
	}
	path := filepath.Join(t.TempDir(), "cancelled.db")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if reg, err := Open(ctx, Options{SQLitePath: path}); err == nil {
		reg.Close()
		t.Fatal("cancelled constructor succeeded")
	}
	assertNoRegistryFileHandles(t, filepath.Dir(path))
}

func assertNoRegistryFileHandles(t *testing.T, dir string) {
	t.Helper()
	files, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		path, err := os.Readlink(filepath.Join("/proc/self/fd", file.Name()))
		if err == nil && strings.HasPrefix(path, dir+string(filepath.Separator)) {
			t.Fatalf("failed construction retained descriptor for %s", path)
		}
	}
}

func TestRegistryRefusesToAbandonLegacyAliasedQualityState(t *testing.T) {
	for _, kind := range []string{"empty", "state", "unrelated"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			path := filepath.Join(dir, "quality#legacy.db")
			source := filepath.Join(dir, "quality")
			legacy, err := gorm.Open(glebarezsqlite.Open("file:"+source), qualityGormConfig())
			if err != nil {
				t.Fatal(err)
			}
			pool, err := legacy.DB()
			if err != nil {
				t.Fatal(err)
			}
			if err := legacy.Exec("CREATE TABLE q_case (id INTEGER PRIMARY KEY)").Error; err != nil {
				t.Fatal(err)
			}
			if kind == "state" {
				if err := legacy.Exec("INSERT INTO q_case(id) VALUES (7)").Error; err != nil {
					t.Fatal(err)
				}
			}
			if kind == "unrelated" {
				if err := legacy.Exec("CREATE TABLE unrelated (id INTEGER PRIMARY KEY)").Error; err != nil {
					t.Fatal(err)
				}
				if err := legacy.Exec("INSERT INTO unrelated VALUES(9)").Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := pool.Close(); err != nil {
				t.Fatal(err)
			}
			reg, err := Open(ctx, Options{SQLitePath: path})
			if kind == "state" {
				if reg != nil {
					reg.Close()
				}
				if !errors.Is(err, ErrLegacySQLitePath) {
					t.Fatalf("legacy restrictions silently abandoned: %v", err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("conflict created new empty quality file: %v", err)
				}
				if runtime.GOOS == "linux" {
					assertNoRegistryFileHandles(t, dir)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				reg.Close()
			}
		})
	}
}

func TestRegistryRelativePathAndSchemaFailureRelease(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor verification requires procfs")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	const path = "nested/relative #%.db"
	reg, err := Open(context.Background(), Options{SQLitePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoRegistryFileHandles(t, dir)
	broken := filepath.Join(dir, "broken.db")
	db, err := gorm.Open(glebarezsqlite.Open("file:"+broken), qualityGormConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE VIEW q_case AS SELECT 1 AS id").Error; err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if reg, err := Open(context.Background(), Options{SQLitePath: broken}); err == nil {
		reg.Close()
		t.Fatal("conflicting view allowed schema construction")
	}
	assertNoRegistryFileHandles(t, dir)
}
