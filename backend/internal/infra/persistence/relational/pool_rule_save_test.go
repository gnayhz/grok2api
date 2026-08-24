package relational

import (
	"context"
	"path/filepath"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// 池目标路由(如某流量类别固定走一个专属池)必须随 operations config 一起
// 落库并通过存储层校验。
func TestSaveOperationsConfigWithPoolRoutingTarget(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "pool-rule.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewEgressRepository(database)

	pool, err := repo.CreateEgressPool(ctx, domain.Pool{Name: "p1", Enabled: true, Strategy: domain.PoolStrategyRandom})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	config := domain.OperationsConfig{
		ProbeProvider: domain.ProbeProviderCloudflare, ProbeIntervalSeconds: 900,
		ClassTargets: map[domain.TrafficClass]domain.RoutingTarget{
			domain.TrafficClassVideo: {Mode: domain.RoutingTargetPool, PoolID: pool.ID},
		},
	}
	if err := domain.ValidateRoutingTargets(config.DefaultTarget, config.ScopeTargets, config.ClassTargets); err != nil {
		t.Fatalf("validate: %v", err)
	}
	saved, err := repo.SaveEgressOperationsConfig(ctx, config)
	if err != nil {
		t.Fatalf("save with pool target: %v", err)
	}
	if got := saved.TargetFor(domain.ScopeBuild, domain.TrafficClassVideo); got.Mode != domain.RoutingTargetPool || got.PoolID != pool.ID {
		t.Fatalf("pool target lost in roundtrip: %+v", got)
	}
	// 指向不存在池的目标在存储层被拒绝。
	broken := config
	broken.ClassTargets = map[domain.TrafficClass]domain.RoutingTarget{
		domain.TrafficClassVideo: {Mode: domain.RoutingTargetPool, PoolID: pool.ID + 1000},
	}
	if _, err := repo.SaveEgressOperationsConfig(ctx, broken); err == nil {
		t.Fatal("missing pool target must be rejected")
	}
}
