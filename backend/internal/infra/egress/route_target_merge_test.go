package egress

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// targetGatedRepo 在固定目标节点读取上门闩并计数进入者。
type targetGatedRepo struct {
	poolStubRepo
	entered  atomic.Int64
	blocking atomic.Bool
	gate     chan struct{}
	node     domain.Node
}

func (r *targetGatedRepo) GetEgressNode(_ context.Context, id uint64) (domain.Node, error) {
	if id != r.node.ID {
		return domain.Node{}, repository.ErrNotFound
	}
	r.entered.Add(1)
	if r.blocking.Load() {
		<-r.gate
	}
	return r.node, nil
}

// TestCachedRoutingTargetNodeMergesConcurrentReloads 锁定固定路由目标
// 缓存的合并契约:1s TTL 过期瞬间,并发请求只产生一次 GetEgressNode。
func TestCachedRoutingTargetNodeMergesConcurrentReloads(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := &targetGatedRepo{gate: make(chan struct{}), node: domain.Node{ID: 42, Name: "fixed", Enabled: true, Health: 1}}
	manager := NewManager(repo, cipher)
	ctx := context.Background()

	if _, ok, err := manager.cachedRoutingTargetNode(ctx, 42); err != nil || !ok {
		t.Fatalf("warmup: ok=%v err=%v", ok, err)
	}

	// 缓存条目置为已过期。
	manager.routeRuleNodeMu.Lock()
	stale := manager.routeRuleNodeCache[42]
	stale.expiresAt = time.Now().UTC().Add(-time.Second)
	manager.routeRuleNodeCache[42] = stale
	manager.routeRuleNodeMu.Unlock()
	repo.blocking.Store(true)

	const concurrency = 24
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			node, ok, err := manager.cachedRoutingTargetNode(ctx, 42)
			if err != nil || !ok || node.ID != 42 {
				t.Errorf("reload: ok=%v err=%v node=%d", ok, err, node.ID)
			}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for repo.entered.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(repo.gate)
	wg.Wait()

	if got := repo.entered.Load(); got != 2 {
		t.Fatalf("%d concurrent reloads produced %d repository entries, want 2 (warmup + merged)", concurrency, got)
	}
}
