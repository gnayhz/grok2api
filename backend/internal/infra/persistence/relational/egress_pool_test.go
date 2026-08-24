package relational

import (
	"context"
	"path/filepath"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func TestEgressPoolCRUDAndMembership(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "egress-pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewEgressRepository(database)

	created, err := repo.CreateEgressPool(ctx, domain.Pool{Name: "warp-premium", Enabled: true, Strategy: domain.PoolStrategyAffinity, FallbackMode: domain.PoolFallbackNone})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || created.Name != "warp-premium" || created.Strategy != domain.PoolStrategyAffinity {
		t.Fatalf("created pool = %#v", created)
	}
	if _, err := repo.CreateEgressPool(ctx, domain.Pool{Name: "cheap", Enabled: true, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: created.ID}); err != nil {
		t.Fatal(err)
	}
	pools, err := repo.ListEgressPools(ctx)
	if err != nil || len(pools) != 2 {
		t.Fatalf("list pools = %v err=%v", len(pools), err)
	}

	// 节点入池 → 按池列出 → 删除池自动脱离
	node, err := repo.CreateEgressNode(ctx, domain.Node{Name: "warp-a", Enabled: true, Health: 1, EncryptedProxyURL: "enc"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetEgressPoolMembers(ctx, created.ID, []uint64{node.ID}); err != nil {
		t.Fatal(err)
	}
	members, err := repo.ListEgressNodesByPool(ctx, created.ID)
	if err != nil || len(members) != 1 || members[0].ID != node.ID {
		t.Fatalf("pool members = %+v err=%v", members, err)
	}
	// 多对多：同一节点可同时属于第二个池
	if _, err := repo.CreateEgressPool(ctx, domain.Pool{Name: "shared", Enabled: true, FallbackMode: domain.PoolFallbackNone}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetEgressPoolMembers(ctx, 3, []uint64{node.ID}); err != nil {
		t.Fatal(err)
	}
	dual, err := repo.GetEgressNode(ctx, node.ID)
	if err != nil || len(dual.PoolIDs) != 2 {
		t.Fatalf("node should belong to two pools: %+v err=%v", dual.PoolIDs, err)
	}
	repo.SetEgressPoolMembers(ctx, 3, nil)
	// 更新池(改回退模式与策略)
	updated, err := repo.UpdateEgressPool(ctx, domain.Pool{ID: created.ID, Name: "warp-premium", Enabled: true, Strategy: domain.PoolStrategySticky, FallbackMode: domain.PoolFallbackDirect})
	if err != nil || updated.FallbackMode != domain.PoolFallbackDirect || updated.Strategy != domain.PoolStrategySticky {
		t.Fatalf("updated pool = %#v err=%v", updated, err)
	}
	// 删除池:成员脱离、引用它的回退清零
	if err := repo.DeleteEgressPool(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	orphan, err := repo.GetEgressNode(ctx, node.ID)
	if err != nil || len(orphan.PoolIDs) != 0 {
		t.Fatalf("member not detached: %+v err=%v", orphan.PoolIDs, err)
	}
	cheap, err := repo.GetEgressPool(ctx, 2)
	if err != nil || cheap.FallbackPoolID != 0 || cheap.FallbackMode != domain.PoolFallbackNone {
		t.Fatalf("dangling fallback not cleared: %#v err=%v", cheap, err)
	}
}

// 池策略必须原样落库:affinity/random/sticky 三种策略 round-trip。
func TestEgressPoolStrategyRoundTrips(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewEgressRepository(database)
	for _, strategy := range []domain.PoolStrategy{domain.PoolStrategyAffinity, domain.PoolStrategyRandom, domain.PoolStrategySticky} {
		pool, err := repo.CreateEgressPool(ctx, domain.Pool{Name: "strategy-" + string(strategy), Enabled: true, Strategy: strategy, FallbackMode: domain.PoolFallbackNone})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := repo.GetEgressPool(ctx, pool.ID)
		if err != nil || stored.Strategy != strategy {
			t.Fatalf("strategy %q round trip = %#v err=%v", strategy, stored, err)
		}
	}
	// 零值策略读回后按 domain 归一化保持 affinity 语义。
	blank, err := repo.CreateEgressPool(ctx, domain.Pool{Name: "blank", Enabled: true, FallbackMode: domain.PoolFallbackNone})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetEgressPool(ctx, blank.ID)
	if err != nil || stored.Strategy.Normalized() != domain.PoolStrategyAffinity {
		t.Fatalf("blank strategy normalized = %#v err=%v", stored, err)
	}
}
