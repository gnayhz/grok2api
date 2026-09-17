package evidence

import (
	"context"
	"path/filepath"
	"testing"

	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openTestStoreB(b *testing.B) *Store {
	b.Helper()
	dsn := "file:" + filepath.Join(b.TempDir(), "bench.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := gorm.Open(glebarezsqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	store, err := New(context.Background(), db, qualitymodel.DefaultEvidenceConfig())
	if err != nil {
		b.Fatal(err)
	}
	return store
}
