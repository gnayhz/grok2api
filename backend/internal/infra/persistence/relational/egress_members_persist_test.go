package relational

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// 回归: PUT members 200 但库里一行都没有的现象。
func TestSetPoolMembersPersists(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "members.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := &EgressRepository{db: database}
	pool, err := repo.CreateEgressPool(ctx, egress.Pool{ID: 3, Name: "t", Enabled: true})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if err := repo.SetEgressPoolMembers(ctx, pool.ID, []uint64{57, 58}); err != nil {
		t.Fatalf("set members: %v", err)
	}
	members, err := repo.EgressPoolMembers(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(members[pool.ID]) != 2 {
		t.Fatalf("members = %v, want 2 rows", members[pool.ID])
	}
	nodes, err := repo.ListEgressNodesByPool(ctx, pool.ID)
	if err != nil {
		t.Fatalf("list by pool: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("nodes = %d, want 0 (node ids need not exist)", len(nodes))
	}
}
