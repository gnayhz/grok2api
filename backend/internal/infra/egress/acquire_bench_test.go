package egress

import (
	"context"
	"fmt"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// 出口获取热路径基准:路由解析(类别/作用域/总出口阶梯)→ 节点/池调度 →
// 租约构造与释放。这是每个出站请求都要走的路径, 是出口子系统的延迟底线。
// 场景固定 100 个健康固定节点, 分别测量:
//   - auto:      无路由配置, 全量自动调度兜底
//   - pool:      类别规则 → 100 成员 affinity 池
//   - fixed:     作用域规则 → 固定节点目标
//
// 稳态运行(快照/池缓存 TTL 内), DB 读为零, 反映纯内存决策成本。
func newAcquireBenchManager(b *testing.B) (*Manager, *e2eRepo) {
	b.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		b.Fatal(err)
	}
	repo := &e2eRepo{pools: map[uint64]domain.Pool{}}
	for i := uint64(1); i <= 100; i++ {
		encrypted, cipherErr := cipher.Encrypt(fmt.Sprintf("http://10.0.%d.%d:8080", i/256, i%256))
		if cipherErr != nil {
			b.Fatal(cipherErr)
		}
		repo.nodes = append(repo.nodes, domain.Node{ID: i, Enabled: true, Health: 1, EncryptedProxyURL: encrypted})
	}
	return NewManager(repo, cipher), repo
}

func BenchmarkAcquireAutoSchedule(b *testing.B) {
	manager, _ := newAcquireBenchManager(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lease, err := manager.Acquire(ctx, domain.ScopeBuild, fmt.Sprintf("acct-%d", i%64))
		if err != nil {
			b.Fatal(err)
		}
		lease.Release()
	}
}

func BenchmarkAcquirePoolTarget(b *testing.B) {
	manager, repo := newAcquireBenchManager(b)
	repo.pools[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyAffinity, FallbackMode: domain.PoolFallbackNone}
	config := domain.DefaultOperationsConfig()
	config.ClassTargets = map[domain.TrafficClass]domain.RoutingTarget{
		domain.TrafficClassInference: {Mode: domain.RoutingTargetPool, PoolID: 1},
	}
	repo.config = config
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lease, err := manager.Acquire(WithTrafficClass(ctx, domain.TrafficClassInference), domain.ScopeBuild, fmt.Sprintf("acct-%d", i%64))
		if err != nil {
			b.Fatal(err)
		}
		lease.Release()
	}
}

func BenchmarkAcquireFixedNodeTarget(b *testing.B) {
	manager, repo := newAcquireBenchManager(b)
	config := domain.DefaultOperationsConfig()
	config.ScopeTargets = map[domain.Scope]domain.RoutingTarget{
		domain.ScopeBuild: {Mode: domain.RoutingTargetNode, NodeID: 42},
	}
	repo.config = config
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lease, err := manager.Acquire(ctx, domain.ScopeBuild, fmt.Sprintf("acct-%d", i%64))
		if err != nil {
			b.Fatal(err)
		}
		lease.Release()
	}
}

// 并发基准:64 goroutine 同时获取+释放, 观察锁竞争下的伸缩性
// (节点快照 RLock、inflight 计数、统计记账都在热路径上)。
func BenchmarkAcquireConcurrent64(b *testing.B) {
	manager, _ := newAcquireBenchManager(b)
	ctx := context.Background()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			lease, err := manager.Acquire(ctx, domain.ScopeBuild, fmt.Sprintf("acct-%d", i%64))
			if err != nil {
				b.Fatal(err)
			}
			lease.Release()
			i++
		}
	})
}
