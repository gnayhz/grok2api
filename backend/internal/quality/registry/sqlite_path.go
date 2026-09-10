package registry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// ErrLegacySQLitePath prevents a URI-escaping upgrade from silently leaving
// restrictions/cases in the unintended file opened by the older constructor.
var ErrLegacySQLitePath = errors.New("quality: legacy SQLite path requires reconciliation")

func checkLegacySQLitePath(ctx context.Context, path string) error {
	legacy, err := url.Parse("file:" + path + "?_pragma=busy_timeout(5000)")
	if err != nil || legacy.Path == "" || legacy.Path == path {
		return nil
	}
	stat, err := os.Stat(legacy.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect legacy quality file: %w", err)
	}
	if !stat.Mode().IsRegular() || stat.Size() == 0 {
		return nil
	}
	if target, err := os.Stat(path); err == nil && os.SameFile(stat, target) {
		return nil
	}
	source := url.URL{Scheme: "file", Path: legacy.Path, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	db, err := gorm.Open(glebarezsqlite.Open(source.String()), qualityGormConfig())
	if err != nil {
		return fmt.Errorf("inspect legacy quality file %q: %w", legacy.Path, err)
	}
	pool, err := db.DB()
	if err != nil {
		return err
	}
	defer pool.Close()
	var tables []string
	if err := db.WithContext(ctx).Raw("SELECT name FROM sqlite_master WHERE type = 'table'").Scan(&tables).Error; err != nil {
		return err
	}
	for _, table := range tables {
		if !strings.HasPrefix(table, "q_") {
			continue
		}
		var found int
		if err := db.WithContext(ctx).Raw(`SELECT 1 FROM "` + strings.ReplaceAll(table, `"`, `""`) + `" LIMIT 1`).Scan(&found).Error; err != nil {
			return err
		}
		if found == 1 {
			return fmt.Errorf("%w: configured %q, earlier URI opened %q; preserve both files and reconcile quality state before starting", ErrLegacySQLitePath, path, legacy.Path)
		}
	}
	return nil
}
