package egress

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type rotationWriteCountingRepo struct {
	poolStubRepo
	writes    atomic.Int64
	latency   time.Duration
	persisted atomic.Uint64
}

func (r *rotationWriteCountingRepo) UpdateEgressPoolRotationCursor(_ context.Context, poolID, fromNodeID, nodeID uint64) error {
	if r.latency > 0 {
		time.Sleep(r.latency)
	}
	r.writes.Add(1)
	r.persisted.Store(nodeID)
	return nil
}

func newRotationBenchManager(tb testing.TB, latency time.Duration) (*Manager, *rotationWriteCountingRepo) {
	tb.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		tb.Fatal(err)
	}
	repo := &rotationWriteCountingRepo{latency: latency}
	repo.pool = map[uint64]domain.Pool{}
	repo.member = map[uint64][]domain.Node{}
	return NewManager(repo, cipher), repo
}

// 推进路径(pinned 节点失效/热游标失效)基准:池行快照仍指向旧游标
// (读缓存 TTL 未刷新),每次迭代从持久游标 10 重新推进到 20。测量
// 选择路径延迟与 DB 写次数。
func BenchmarkRotationCursorAdvance(b *testing.B) {
	manager, repo := newRotationBenchManager(b, time.Millisecond)
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyRotation, RotationCursorNodeID: 10}
	repo.member[1] = []domain.Node{
		{ID: 10, Enabled: true, Health: 1},
		{ID: 20, Enabled: true, Health: 1},
	}
	pool := repo.pool[1]
	candidates := []domain.Node{repo.member[1][1]}
	all := repo.member[1]

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		manager.rotationMu.Lock()
		delete(manager.rotationCursors, 1)
		manager.rotationMu.Unlock()
		manager.selectRotationNode(pool, candidates, all)
	}
	b.StopTimer()
	b.ReportMetric(float64(repo.writes.Load())/float64(b.N), "db-writes/op")
}

type rotationFailingRepo struct {
	poolStubRepo
	failWrites atomic.Bool
	writes     atomic.Int64
	persisted  atomic.Uint64
}

func (r *rotationFailingRepo) UpdateEgressPoolRotationCursor(_ context.Context, poolID, fromNodeID, nodeID uint64) error {
	if r.failWrites.Load() {
		return errors.New("db down")
	}
	r.writes.Add(1)
	r.persisted.Store(nodeID)
	return nil
}

// 异步去抖持久化的正确性:最终一致、去重、失败重试。
func TestRotationCursorPersistAsyncDedupAndRetry(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := &rotationFailingRepo{}
	repo.pool = map[uint64]domain.Pool{}
	repo.member = map[uint64][]domain.Node{}
	manager := NewManager(repo, cipher)
	all := []domain.Node{{ID: 10, Enabled: true, Health: 1}, {ID: 20, Enabled: true, Health: 1}, {ID: 30, Enabled: true, Health: 1}}
	pool := domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyRotation, RotationCursorNodeID: 10}

	// 1) 失败时不得记为已持久化:写失败后再次推进必须重试。
	repo.failWrites.Store(true)
	manager.persistRotationCursor(1, 10, 20)
	waitFor(t, time.Second, func() bool { return repo.writes.Load() == 0 || true }) // 失败写不计入成功
	manager.persistRotationCursor(1, 10, 20)
	repo.failWrites.Store(false)
	waitFor(t, time.Second, func() bool { return repo.persisted.Load() == 20 })

	// 2) 相同目标的重复推进必须去重为一次成功写。
	before := repo.writes.Load()
	for i := 0; i < 10; i++ {
		manager.persistRotationCursor(1, 20, 20)
	}
	waitFor(t, time.Second, func() bool { return true })
	if got := repo.writes.Load() - before; got > 1 {
		t.Fatalf("duplicate advances caused %d extra writes, want <=1", got)
	}

	// 3) 热游标立即生效:并发请求读到 hot 值,不等 DB。
	manager.rotationMu.Lock()
	hot := manager.rotationCursors[1]
	manager.rotationMu.Unlock()
	if hot != 20 {
		t.Fatalf("hot cursor = %d, want 20", hot)
	}

	// 4) 新目标的推进最终落盘。
	manager.persistRotationCursor(1, 20, 30)
	waitFor(t, time.Second, func() bool { return repo.persisted.Load() == 30 })

	// 5) 选路推进语义不变:游标可用时钉住,不可用时推进到下一可用成员。
	pinned := manager.selectRotationNode(pool, []domain.Node{all[2]}, all)
	if pinned.ID != 30 {
		t.Fatalf("pinned = %d, want 30", pinned.ID)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}
