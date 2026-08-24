package relational

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// 回归: 保存成员后 priority 必须保留(之前先删后读,恒读空集,星标每次保存丢失)。
func TestSetPoolMembersPreservesPriority(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "prio.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := NewEgressRepository(database)
	pool, err := repo.CreateEgressPool(ctx, egress.Pool{Name: "prio", Enabled: true})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	var ids []uint64
	for _, name := range []string{"m1", "m2"} {
		node, err := repo.CreateEgressNode(ctx, egress.Node{Name: name, Enabled: true, Health: 1})
		if err != nil {
			t.Fatalf("create node %s: %v", name, err)
		}
		ids = append(ids, node.ID)
	}
	if err := repo.SetEgressPoolMembers(ctx, pool.ID, ids); err != nil {
		t.Fatalf("set members: %v", err)
	}
	if err := repo.SetEgressPoolMemberPriority(ctx, pool.ID, ids[0], 1); err != nil {
		t.Fatalf("set priority: %v", err)
	}
	// 同样成员再保存一次 —— priority 必须保留
	if err := repo.SetEgressPoolMembers(ctx, pool.ID, ids); err != nil {
		t.Fatalf("re-save members: %v", err)
	}
	preferred, err := repo.EgressPoolPreferredNodes(ctx)
	if err != nil {
		t.Fatalf("preferred: %v", err)
	}
	if preferred[pool.ID] != ids[0] {
		t.Fatalf("preferred after re-save = %v, want %d (priority must survive membership save)", preferred, ids[0])
	}
}

// 回归: 按代理排序的 SQL 必须可执行(历史上引号未闭合,排序即报错)。
func TestListNodesSortByProxyExecutes(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "sort.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := NewEgressRepository(database)
	if _, err := repo.CreateEgressNode(ctx, egress.Node{Name: "n", Enabled: true, Health: 1}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := repo.ListEgressNodePage(ctx, repository.EgressNodeListQuery{Page: repository.PageQuery{Limit: 10, Offset: 0, Sort: repository.SortQuery{Field: "proxy"}}}); err != nil {
		t.Fatalf("sort by proxy: %v", err)
	}
}
