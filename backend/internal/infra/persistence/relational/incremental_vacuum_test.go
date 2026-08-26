package relational

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteIncrementalVacuumReturnsFreelist(t *testing.T) {
	ctx := context.Background()
	db, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "incvac.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if trimmed, err := db.SQLiteIncrementalVacuum(ctx); err != nil || trimmed {
		t.Fatalf("empty db no-op expected, trimmed=%v err=%v", trimmed, err)
	}

	if err := db.db.WithContext(ctx).Exec("CREATE TABLE churn (id INTEGER PRIMARY KEY, payload TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 2000)
	for i := 0; i < 50; i++ {
		if err := db.db.WithContext(ctx).Exec("INSERT INTO churn (payload) VALUES (?)", payload).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.db.WithContext(ctx).Exec("DELETE FROM churn").Error; err != nil {
		t.Fatal(err)
	}
	var freelist int
	if err := db.db.WithContext(ctx).Raw("PRAGMA freelist_count").Scan(&freelist).Error; err != nil {
		t.Fatal(err)
	}
	if freelist == 0 {
		t.Fatal("setup error: expected freelist pages after mass delete")
	}
	before := incVacPageCount(t, db)
	trimmed, err := db.SQLiteIncrementalVacuum(ctx)
	if err != nil || !trimmed {
		t.Fatalf("vacuum with freelist: trimmed=%v err=%v", trimmed, err)
	}
	after := incVacPageCount(t, db)
	if after >= before {
		t.Fatalf("pages not returned: before=%d after=%d", before, after)
	}
	// 增长封顶契约（round 56 实证）：freelist 页被后续写入复用，
	// 重新写入同等数据后文件页数只允许头部/ptrmap 级别的微小增长。
	regrow := 0
	for regrow < 50 && incVacPageCount(t, db) < before {
		if err := db.db.WithContext(ctx).Exec("INSERT INTO churn (payload) VALUES (?)", payload).Error; err != nil {
			t.Fatal(err)
		}
		regrow++
	}
	if grown := incVacPageCount(t, db) - before; grown > 2 {
		t.Fatalf("freelist pages not reused: page_count grew by %d after reinserting %d rows (before=%d)", grown, regrow, before)
	}
}

func incVacPageCount(t *testing.T, db *Database) int {
	t.Helper()
	var pages int
	if err := db.db.Raw("PRAGMA page_count").Scan(&pages).Error; err != nil {
		t.Fatal(err)
	}
	return pages
}
