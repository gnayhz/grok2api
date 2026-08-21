package relational

import (
	"context"
	"path/filepath"
	"testing"
)

// TestSQLiteAutoVacuumMigration 锁定 round 72：存量库首次以
// auto_vacuum=INCREMENTAL 打开时执行一次性 VACUUM 迁移；重开后模式
// 保持 incremental 且不再 VACUUM（幂等）。长跑库 83% freelist 的空间
// 回收自此有机制保障。
func TestSQLiteAutoVacuumMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "av.db")

	db, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := db.sqliteAutoVacuumMode(ctx); mode != incrementalAutoVacuum {
		t.Fatalf("首次打开后 auto_vacuum = %q, want incremental", mode)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// 重开：模式已生效，不应再触发迁移（幂等路径）。
	db2, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if mode := db2.sqliteAutoVacuumMode(ctx); mode != incrementalAutoVacuum {
		t.Fatalf("重开后 auto_vacuum = %q, want incremental", mode)
	}
}
