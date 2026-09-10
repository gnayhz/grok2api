package egress

import (
	"context"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// sharedCursorRepo 把节点轮询游标持久化进池行(CAS 语义), 模拟共享数据库。
type sharedCursorRepo struct {
	poolStubRepo
	sync.Mutex
}

func (r *sharedCursorRepo) UpdateEgressPoolRotationCursor(_ context.Context, poolID, fromNodeID, nodeID uint64) error {
	r.Lock()
	defer r.Unlock()
	pool, ok := r.pool[poolID]
	if !ok {
		return repository.ErrNotFound
	}
	if pool.RotationCursorNodeID != fromNodeID && pool.RotationCursorNodeID != 0 {
		return repository.ErrNotFound
	}
	pool.RotationCursorNodeID = nodeID
	r.pool[poolID] = pool
	return nil
}

func (r *sharedCursorRepo) storedCursor(poolID uint64) uint64 {
	r.Lock()
	defer r.Unlock()
	return r.pool[poolID].RotationCursorNodeID
}

// poolSnapshot 返回池行副本。selectRotationNode 异步持久化游标
// （fire-and-forget goroutine），测试侧直接读 map 会与其加锁写竞争
// （规模轮 170 race 实证）——一切读取必须走锁内快照。
func (r *sharedCursorRepo) poolSnapshot(poolID uint64) domain.Pool {
	r.Lock()
	defer r.Unlock()
	return r.pool[poolID]
}

func newSharedReplicaManagers(t *testing.T) (*Manager, *Manager, *sharedCursorRepo) {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := &sharedCursorRepo{}
	repo.pool = map[uint64]domain.Pool{}
	repo.member = map[uint64][]domain.Node{}
	return NewManager(repo, cipher), NewManager(repo, cipher), repo
}

// 节点轮询游标:热游标进程内,持久游标 CAS 落共享库。副本 A 推进后,
// 新副本(或重启后的副本)从持久游标续位,不回退到首个成员。
func TestRotationCursorContinuityAcrossReplicas(t *testing.T) {
	managerA, managerB, repo := newSharedReplicaManagers(t)

	members := []domain.Node{
		{ID: 10, Enabled: true, Health: 1},
		{ID: 20, Enabled: true, Health: 1},
		{ID: 30, Enabled: true, Health: 1},
	}
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyRotation, FallbackMode: domain.PoolFallbackNone}

	// 副本 A:冷启动选首成员 10;10 失效后续位 20 并持久化游标。
	first := managerA.routing.selectRotationNode(repo.poolSnapshot(1), members, members)
	if first.ID != 10 {
		t.Fatalf("cold-start rotation member = %d, want 10", first.ID)
	}
	without10 := members[1:]
	second := managerA.routing.selectRotationNode(repo.poolSnapshot(1), without10, members)
	if second.ID != 20 {
		t.Fatalf("advanced rotation member = %d, want 20", second.ID)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && repo.storedCursor(1) != 20 {
		time.Sleep(5 * time.Millisecond)
	}
	if repo.storedCursor(1) != 20 {
		t.Fatalf("rotation cursor not persisted to shared store: got %d, want 20", repo.storedCursor(1))
	}

	// 新副本 B:热游标为空,必须从共享持久游标(20)续位;20 失效后到 30,
	// 绝不回退到 10(节点轮询"不回头"跨副本保持)。
	poolB := domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyRotation, FallbackMode: domain.PoolFallbackNone, RotationCursorNodeID: repo.storedCursor(1)}
	without20 := []domain.Node{members[0], members[2]}
	continued := managerB.routing.selectRotationNode(poolB, without20, members)
	if continued.ID != 30 {
		t.Fatalf("replica B rotation continuation = %d, want 30 (persisted cursor 20, no regression to 10)", continued.ID)
	}
}

// inflight 计数条目随节点删除回收:订阅换血持续产生新节点 ID, 计数器永不
// 删除会无界累积(与 poolNodeStats 同类)。快照重建时清理"已不存在且计数为
// 零"的条目; 在途租约(count>0)的条目保留; 迟来递减被 Load 守卫吞掉。
func TestInflightCountersSweptForDeletedNodes(t *testing.T) {
	manager, _, repo := newSharedReplicaManagers(t)
	ctx := context.Background()

	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyAffinity, FallbackMode: domain.PoolFallbackNone}
	repo.member[1] = []domain.Node{{ID: 10, Enabled: true, Health: 1}, {ID: 20, Enabled: true, Health: 1}}
	gone := []domain.Node{{ID: 30, Enabled: true, Health: 1}, {ID: 40, Enabled: true, Health: 1}}

	// 历史流量在 30/40 上留下零计数的 inflight 条目(获取后立即释放)。
	for _, id := range []uint64{30, 40} {
		manager.incrementInflight(id)
		manager.decrementInflight(id)
	}
	// 30/40 随后从仓储消失(换血/删除), 快照重建为 10/20 + 残留条目。
	repo.nodes = append(append([]domain.Node(nil), repo.member[1]...), gone...)
	rebuildSnapshot(t, ctx, manager)
	if _, ok := manager.routing.inflight.Load(uint64(30)); !ok {
		t.Fatal("precondition: inflight entry for node 30 must exist before sweep trigger")
	}

	// 换血移除 30/40:快照重建后零计数条目被清理。
	repo.nodes = append([]domain.Node(nil), repo.member[1]...)
	rebuildSnapshot(t, ctx, manager)
	for _, id := range []uint64{30, 40} {
		if _, ok := manager.routing.inflight.Load(id); ok {
			t.Fatalf("zero-count inflight entry for deleted node %d not swept", id)
		}
	}
	// 存活节点条目不受影响(10/20 经真实获取创建)。
	lease, _, err := manager.AcquireIfConfigured(ctx, domain.ScopeBuild, "sweep")
	if err != nil || lease == nil {
		t.Fatalf("acquire after sweep: %v", err)
	}
	lease.Release()
	if _, ok := manager.routing.inflight.Load(lease.NodeID); !ok {
		t.Fatal("live node inflight entry must survive the sweep")
	}

	// 在途租约(count>0)的已删节点条目必须保留, 避免丢计数。
	manager.incrementInflight(uint64(50))
	repo.nodes = append([]domain.Node(nil), repo.member[1]...) // 50 不入快照
	rebuildSnapshot(t, ctx, manager)
	if _, ok := manager.routing.inflight.Load(uint64(50)); !ok {
		t.Fatal("in-flight (count>0) entry for deleted node must be retained")
	}
	manager.decrementInflight(uint64(50)) // 迟来递减: Load 守卫吞掉或正常归零
}

// rebuildSnapshot 触发一次真实的快照重建:invalidateNodes 只删缓存, 重建
// 发生在下一次 listNodes(sharedCursorRepo 内嵌的 stub 提供 ListEgressNodes)。
func rebuildSnapshot(t *testing.T, ctx context.Context, manager *Manager) {
	t.Helper()
	manager.invalidateNodes() // TTL 内命中缓存不会重建, 先显式失效
	if _, err := manager.listNodes(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("listNodes: %v", err)
	}
}
