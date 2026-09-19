package investigator

import (
	"context"

	"fmt"

	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func executorRegistries(t *testing.T, driver string) (*registry.Registry, *registry.Registry) {
	t.Helper()
	ctx := context.Background()
	opts := registry.Options{Driver: driver, SQLitePath: filepath.Join(t.TempDir(), "quality.db")}
	if driver == "postgres" {
		dsn := os.Getenv("GROK_EVOLUTION_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("requires isolated GROK_EVOLUTION_POSTGRES_DSN")
		}
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal(err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		schema := fmt.Sprintf("probe_executor_%d", time.Now().UnixNano())
		if err := db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
			sqlDB.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Exec("DROP SCHEMA " + schema + " CASCADE"); sqlDB.Close() })
		separator := "?"
		if strings.Contains(dsn, "?") {
			separator = "&"
		}
		opts.PostgresDSN = dsn + separator + "search_path=" + schema
	}
	first, err := registry.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	for _, node := range []uint64{8, 10} {
		if _, _, err := first.AdvanceEpoch(ctx, node, model.ExitIdentityFromAggregate(fmt.Sprintf("198.51.100.%d", node))); err != nil {
			t.Fatal(err)
		}
	}
	second, err := registry.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	return first, second
}
