package egress

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// poolGatedRepo 在首个池行读取上阻塞,直到测试放行;同时统计进入
// GetEgressPool 的调用方数量——singleflight 合并下只有领头会进入,
// 未合并时所有并发调用方都会进入(确定性判别,不受缓存写回时序影响)。
type poolGatedRepo struct {
	poolStubRepo
	entered  atomic.Int64
	reads    atomic.Int64
	blocking atomic.Bool
	gate     chan struct{}
}

func (r *poolGatedRepo) GetEgressPool(ctx context.Context, id uint64) (domain.Pool, error) {
	r.entered.Add(1)
	if r.blocking.Load() {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return domain.Pool{}, ctx.Err()
		}
	}
	r.reads.Add(1)
	return r.poolStubRepo.GetEgressPool(ctx, id)
}

func (r *poolGatedRepo) ListEgressNodesByPool(ctx context.Context, id uint64) ([]domain.Node, error) {
	return r.poolStubRepo.ListEgressNodesByPool(ctx, id)
}

// TestCachedPoolMembersMergesConcurrentReloads 锁定回源合并契约:
// 1s TTL 过期瞬间,N 个并发请求共享一次 DB 回源(只有领头进入仓储层),
// 而不是每个请求各打 2 条 SQL。严格绑定语义下池读失败=请求直接失败,
// 惊群会把一次 DB 抖动放大成批的池路由失败——singleflight 是既有
// listNodes 范式在此处的对齐。
func TestCachedPoolMembersMergesConcurrentReloads(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := &poolGatedRepo{poolStubRepo: poolStubRepo{
		pool:   map[uint64]domain.Pool{7: {ID: 7, Name: "p", Enabled: true}},
		member: map[uint64][]domain.Node{7: {{ID: 71, Name: "m1", Enabled: true, Health: 1}}},
	}, gate: make(chan struct{})}
	manager := NewManager(repo, cipher)

	ctx := context.Background()
	if _, _, err := manager.cachedPoolMembers(ctx, 7, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// 缓存条目直接置为已过期(领头的内部复查用真实时间,伪造调用方 now
	// 骗不过它——内部复查本身是合并正确性的一部分)。
	manager.fallbackMu.Lock()
	stale := manager.poolFallbacks[7]
	stale.expiresAt = time.Now().UTC().Add(-time.Second)
	manager.poolFallbacks[7] = stale
	manager.fallbackMu.Unlock()
	repo.blocking.Store(true)

	const concurrency = 32
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool, members, err := manager.cachedPoolMembers(ctx, 7, time.Now().UTC())
			if err != nil {
				t.Errorf("cachedPoolMembers: %v", err)
				return
			}
			if pool.ID != 7 || len(members) != 1 || members[0].ID != 71 {
				t.Errorf("unexpected result: pool=%d members=%v", pool.ID, members)
			}
		}()
	}

	// 等全部调用方到达回源路径(合并:等在 singleflight 上;未合并:阻塞在
	// 仓储门闩上),放行领头的读取。
	deadline := time.Now().Add(2 * time.Second)
	for repo.entered.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(repo.gate)
	wg.Wait()

	// 合并契约:预热 1 次 + 合并回源 1 次,共 2 次进入;未合并时会是 1+32。
	if got := repo.entered.Load(); got != 2 {
		t.Fatalf("%d concurrent reloads produced %d repository entries, want 2 (warmup + singleflight merged)", concurrency, got)
	}
	if repo.reads.Load() != 2 {
		t.Fatalf("pool row reads = %d, want 2 (warmup + merged reload)", repo.reads.Load())
	}

	// 合并回源写入的新缓存对后续请求生效:零额外进入。
	if _, _, err := manager.cachedPoolMembers(ctx, 7, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if repo.entered.Load() != 2 || repo.reads.Load() != 2 {
		t.Fatalf("post-reload entries/reads = %d/%d, want 2/2 (cache hit)", repo.entered.Load(), repo.reads.Load())
	}
}
