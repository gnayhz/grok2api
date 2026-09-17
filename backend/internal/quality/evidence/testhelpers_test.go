package evidence

import (
	"path/filepath"
	"testing"

	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openTestSQLiteAt 在指定路径打开测试用 SQLite(gorm 句柄)。建表对齐
// registry 的统一迁移语义:直接 AutoMigrate evidence.Models()(生产中
// 由 registry.New 完成,evidence.New 不再自跑迁移)。
func openTestSQLiteAt(path string) (*gorm.DB, error) {
	dsn := "file:" + filepath.Clean(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := gorm.Open(glebarezsqlite.Open(dsn), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		TranslateError: true,
	})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(Models()...); err != nil {
		sqlDB, closeErr := db.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
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
