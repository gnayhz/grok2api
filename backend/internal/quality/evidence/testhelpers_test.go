package evidence

import (
	"path/filepath"
	"testing"

	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openTestSQLiteAt 在指定路径打开测试用 SQLite(gorm 句柄)。
func openTestSQLiteAt(path string) (*gorm.DB, error) {
	dsn := "file:" + filepath.Clean(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := gorm.Open(glebarezsqlite.Open(dsn), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		TranslateError: true,
	})
	if err != nil {
		return nil, err
	}
	return db, nil
}

func openTestSQLite(t *testing.T) (*gorm.DB, error) {
	t.Helper()
	dir := t.TempDir()
	return openTestSQLiteAt(filepath.Join(dir, "evidence.db"))
}

func closeTestSQLite(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
