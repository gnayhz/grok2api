package relational

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestLiveShapeDBUpgradeRestoresBindingColumns：生产旧库形态——出口绑定列
// 被旧迁移整列删除（round 41 修复前）。修复后的代码在此形态上初始化必须
// 重建绑定列（AutoMigrate），且不破坏既有行。
func TestLiveShapeDBUpgradeRestoresBindingColumns(t *testing.T) {
	source := os.Getenv("LIVE_SHAPE_DB")
	if source == "" {
		t.Skip("LIVE_SHAPE_DB not set")
	}
	copy := filepath.Join(t.TempDir(), "live.db")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copy, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := OpenSQLite(ctx, copy)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatalf("live-shape upgrade failed: %v", err)
	}
	for _, col := range []string{"egress_node_id", "egress_assignment_mode", "egress_assigned_at"} {
		var count int
		if err := db.db.WithContext(ctx).Raw("SELECT COUNT(*) FROM pragma_table_info('provider_accounts') WHERE name=?", col).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("binding column %s not restored on live-shape db", col)
		}
	}
	var accounts int64
	if err := db.db.WithContext(ctx).Raw("SELECT COUNT(*) FROM provider_accounts").Scan(&accounts).Error; err != nil {
		t.Fatal(err)
	}
	if accounts == 0 {
		t.Error("accounts lost during live-shape upgrade")
	}
}
