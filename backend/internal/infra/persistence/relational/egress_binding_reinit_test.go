package relational

import (
	"context"
	"path/filepath"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestEgressBindingSurvivesSchemaReinit：账号-出口绑定是仓储层仍在活跃
// 查询的功能面（路由排除/绑定列举）。重新 InitializeSchema（升级路径）
// 不得清空绑定数据。
func TestEgressBindingSurvivesSchemaReinit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "binding.db")
	db, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewAccountRepository(db)
	created, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "bind", SourceKey: "bind",
		EncryptedAccessToken: "enc", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 建真实出口节点满足外键，再绑定。
	if err := db.db.WithContext(ctx).Exec("INSERT INTO egress_nodes (name, enabled, health, created_at, updated_at) VALUES ('probe-node', true, 1.0, datetime('now'), datetime('now'))").Error; err != nil {
		t.Fatal(err)
	}
	var nodeIDValue uint64
	if err := db.db.WithContext(ctx).Raw("SELECT id FROM egress_nodes ORDER BY id DESC LIMIT 1").Row().Scan(&nodeIDValue); err != nil {
		t.Fatal(err)
	}
	if err := db.db.WithContext(ctx).Exec("UPDATE provider_accounts SET egress_node_id = ?, egress_assignment_mode = 'manual' WHERE id = ?", nodeIDValue, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	// 升级路径：再次 InitializeSchema（真实进程重启即此语义）。
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var nodeID *uint64
	var mode string
	if err := db.db.WithContext(ctx).Raw("SELECT egress_node_id, egress_assignment_mode FROM provider_accounts WHERE id = ?", created.ID).Row().Scan(&nodeID, &mode); err != nil {
		t.Fatal(err)
	}
	if nodeID == nil || *nodeID != nodeIDValue || mode != "manual" {
		t.Fatalf("egress binding lost across re-init: nodeID=%v mode=%q", nodeID, mode)
	}
}
