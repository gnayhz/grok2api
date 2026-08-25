package relational

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Database 持有关系型数据库连接和各仓储实现共享的 GORM 实例。
type Database struct {
	db      *gorm.DB
	dialect string
}

func (d *Database) Stats() sql.DBStats {
	if d == nil {
		return sql.DBStats{}
	}
	sqlDB, err := d.db.DB()
	if err != nil {
		return sql.DBStats{}
	}
	return sqlDB.Stats()
}

func (d *Database) Dialect() string {
	if d == nil {
		return ""
	}
	return d.dialect
}

// OpenSQLite 打开纯 Go SQLite 数据库并启用 WAL、外键与 busy timeout。
// 显式事务使用 IMMEDIATE，避免并发读后写事务在锁升级时直接返回 SQLITE_BUSY。
func OpenSQLite(ctx context.Context, path string) (*Database, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建数据库目录: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=auto_vacuum(INCREMENTAL)&_txlock=immediate", path)
	db, err := gorm.Open(glebarezsqlite.Open(dsn), gormConfig())
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite: %w", err)
	}
	database, err := configureDatabase(ctx, db, "sqlite", 16, 16)
	if err != nil {
		return nil, err
	}
	// 存量库迁移：auto_vacuum 模式只在 VACUUM 后生效（round 72 实测
	// 长跑库 83% 页面为删除空洞——retention/媒体作业清理留下的空间
	// 从不回收，页扫描与备份体积虚胖）。仅当模式尚未生效时执行一次。
	if mode := database.sqliteAutoVacuumMode(ctx); mode != incrementalAutoVacuum {
		if vacuumErr := database.sqliteVacuumOnce(ctx); vacuumErr != nil {
			return nil, fmt.Errorf("迁移 SQLite auto_vacuum: %w", vacuumErr)
		}
	}
	return database, nil
}

// OpenPostgres 打开 PostgreSQL 数据库并配置连接池。
func OpenPostgres(ctx context.Context, dsn string, maxOpenConns, maxIdleConns int) (*Database, error) {
	db, err := gorm.Open(postgres.Open(dsn), gormConfig())
	if err != nil {
		return nil, &postgresConnectionError{operation: "打开 PostgreSQL", err: err, dsn: dsn}
	}
	database, err := configureDatabase(ctx, db, "postgres", maxOpenConns, maxIdleConns)
	if err != nil {
		return nil, &postgresConnectionError{operation: "配置 PostgreSQL", err: err, dsn: dsn}
	}
	return database, nil
}

type postgresConnectionError struct {
	operation string
	err       error
	dsn       string
}

func (e *postgresConnectionError) Error() string {
	return e.operation + ": " + redactPostgresErrorMessage(e.err, e.dsn)
}

func (e *postgresConnectionError) Unwrap() error { return e.err }

var (
	postgresURLPasswordPattern = regexp.MustCompile(`(?i)(postgres(?:ql)?://[^:/\s]+:)[^@\s]+(@)`)
	postgresDSNPasswordPattern = regexp.MustCompile(`(?i)(password\s*=\s*)(?:'[^']*'|"[^"]*"|[^\s]+)`)
)

func redactPostgresErrorMessage(err error, dsn string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if value := strings.TrimSpace(dsn); value != "" {
		message = strings.ReplaceAll(message, value, "<redacted PostgreSQL DSN>")
	}
	message = postgresURLPasswordPattern.ReplaceAllString(message, `${1}<redacted>${2}`)
	return postgresDSNPasswordPattern.ReplaceAllString(message, `${1}<redacted>`)
}

func gormConfig() *gorm.Config {
	return &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		TranslateError: true,
		NowFunc:        func() time.Time { return time.Now().UTC() },
	}
}

func configureDatabase(ctx context.Context, db *gorm.DB, dialect string, maxOpenConns, maxIdleConns int) (*Database, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxIdleConns)
	sqlDB.SetConnMaxLifetime(time.Hour)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("连接 %s: %w", dialect, err)
	}
	return &Database{db: db, dialect: dialect}, nil
}

// Close 关闭底层数据库连接。
func (d *Database) Close() error {
	sqlDB, err := d.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

const incrementalAutoVacuum = "incremental"

// sqliteAutoVacuumMode 返回当前库的 auto_vacuum 模式（none/full/incremental）。
// PRAGMA 返回数字（0=none/1=full/2=incremental），归一化为名称。
func (d *Database) sqliteAutoVacuumMode(ctx context.Context) string {
	var raw sql.NullString
	d.db.WithContext(ctx).Raw("PRAGMA auto_vacuum").Scan(&raw)
	switch strings.TrimSpace(raw.String) {
	case "1", "full":
		return "full"
	case "2", "incremental":
		return incrementalAutoVacuum
	default:
		return "none"
	}
}

// sqliteVacuumOnce 执行一次 VACUUM——这是让已存在库切换到 INCREMENTAL
// auto_vacuum 的唯一途径（pragma 只对新库生效）。8.8MB 实测库秒级完成；
// 大库也仅在首次迁移时付出一次成本。
func (d *Database) sqliteVacuumOnce(ctx context.Context) error {
	return d.db.WithContext(ctx).Exec("VACUUM").Error
}

// SQLiteIncrementalVacuum 归还累积的 freelist 页给操作系统。设置
// auto_vacuum=INCREMENTAL 只启用机制；真正归还页需要周期性执行本
// pragma（round 61 实证：DELETE 后 freelist 页滞留文件，直到显式
// incremental_vacuum 才缩小）。非 SQLite 方言为 no-op。
func (d *Database) SQLiteIncrementalVacuum(ctx context.Context) (bool, error) {
	if d.dialect != "sqlite" {
		return false, nil
	}
	var freelist int
	if err := d.db.WithContext(ctx).Raw("PRAGMA freelist_count").Scan(&freelist).Error; err != nil {
		return false, err
	}
	if freelist == 0 {
		return false, nil
	}
	if err := d.db.WithContext(ctx).Exec("PRAGMA incremental_vacuum").Error; err != nil {
		return false, err
	}
	return true, nil
}
