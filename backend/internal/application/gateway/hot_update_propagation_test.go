package gateway

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/repository"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	relational "github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// TestRoutingConfigHotUpdatePropagation 锁定配置热更新的传播链:管理端保存
// 路由/池配置后, infra manager 必须经失效器立即丢弃缓存——下一次 Acquire
// 就按新配置选路, 而不是等 manager 自身的 1s TTL 过期。若 app 装配处漏接
// SetOperationsConfigInvalidator/SetPoolCacheInvalidator, 本测试在 TTL 窗口
// 内立即失败(每个用例都先做一次 Acquire 预热缓存, 再保存, 再零延迟断言)。
func TestRoutingConfigHotUpdatePropagation(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "hot-update.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewEgressRepository(database)
	for _, id := range []uint64{10, 20} {
		encrypted, encryptErr := cipher.Encrypt("http://10.0.0." + string(rune('0'+id/10)) + ":1080")
		if encryptErr != nil {
			t.Fatal(encryptErr)
		}
		if _, err := repo.CreateEgressNode(ctx, egressdomain.Node{Name: "hot-" + string(rune('0'+id/10)), Enabled: true, EncryptedProxyURL: encrypted, Health: 1}); err != nil {
			t.Fatal(err)
		}
	}
	nodes, err := repo.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil || len(nodes) != 2 {
		t.Fatalf("nodes=%d err=%v, want 2", len(nodes), err)
	}
	first, second := nodes[0], nodes[1]
	pool, err := repo.CreateEgressPool(ctx, egressdomain.Pool{Name: "hot-pool", Enabled: true, Strategy: egressdomain.PoolStrategyAffinity, FallbackMode: egressdomain.PoolFallbackNone})
	if err != nil {
		t.Fatal(err)
	}

	manager := infraegress.NewManager(repo, cipher)
	service := egressapp.NewService(repo, cipher)
	// 与 app.go 相同的失效器装配——本测试守护的正是这两根线。
	service.SetOperationsConfigInvalidator(manager)
	service.SetPoolCacheInvalidator(manager)

	save := func(t *testing.T, scopeTarget *egressapp.RoutingTargetInput) {
		t.Helper()
		if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
			ProbeProvider:        egressdomain.ProbeProviderCloudflare,
			ProbeIntervalSeconds: 900,
			ScopeTargets:         map[egressdomain.Scope]egressapp.RoutingTargetInput{egressdomain.ScopeBuild: *scopeTarget},
		}); err != nil {
			t.Fatal(err)
		}
	}
	acquireNode := func(t *testing.T) uint64 {
		t.Helper()
		lease, _, acquireErr := manager.AcquireIfConfigured(ctx, egressdomain.ScopeBuild, "hot-update")
		if acquireErr != nil || lease == nil {
			t.Fatalf("acquire: lease=%v err=%v", lease, acquireErr)
		}
		defer lease.Release()
		return lease.NodeID
	}

	// 预热 manager 的路由配置缓存(未配置 → 自动调度)。
	if got := acquireNode(t); got != first.ID && got != second.ID {
		t.Fatalf("pre-warm acquire landed on unknown node %d", got)
	}

	t.Run("scope target to fixed node takes effect immediately", func(t *testing.T) {
		save(t, &egressapp.RoutingTargetInput{Mode: egressdomain.RoutingTargetNode, NodeID: second.ID})
		if got := acquireNode(t); got != second.ID {
			t.Fatalf("immediately after save, acquire landed on node %d, want fixed target %d (stale manager cache: invalidator wiring broken)", got, second.ID)
		}
	})

	t.Run("retarget to pool takes effect immediately", func(t *testing.T) {
		if err := repo.SetEgressPoolMembers(ctx, pool.ID, []uint64{first.ID}); err != nil {
			t.Fatal(err)
		}
		save(t, &egressapp.RoutingTargetInput{Mode: egressdomain.RoutingTargetPool, PoolID: pool.ID})
		if got := acquireNode(t); got != first.ID {
			t.Fatalf("immediately after retarget to pool, acquire landed on node %d, want pool member %d", got, first.ID)
		}
	})

	t.Run("pool membership edit takes effect immediately", func(t *testing.T) {
		// 池路由不变; 池成员由 [first] 换为 [second]——池缓存失效器必须让
		// 下一次 Acquire 立即看到新成员。
		if err := repo.SetEgressPoolMembers(ctx, pool.ID, []uint64{second.ID}); err != nil {
			t.Fatal(err)
		}
		if err := service.SetPoolMembers(ctx, pool.ID, []uint64{second.ID}); err != nil {
			t.Fatal(err)
		}
		if got := acquireNode(t); got != second.ID {
			t.Fatalf("immediately after membership edit, acquire landed on node %d, want new member %d (stale pool cache)", got, second.ID)
		}
	})

}
