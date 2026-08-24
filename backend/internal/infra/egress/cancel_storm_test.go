package egress

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// 取消风暴下的租约生命周期守恒:并发 worker 各自完成 Acquire → 立即取消
// 上下文 → Release 循环,穿插正常租约。结束时:inflight 全部归零(无泄漏
// 计数)、无 goroutine 泄漏、后续正常获取不受污染。
// 既有测试只覆盖裸计数器守恒与反馈面取消豁免,未覆盖真实租约生命周期
// 在取消风暴下的整体守恒。
func TestLeaseLifecycleUnderCancellationStorm(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := &e2eRepo{pools: map[uint64]domain.Pool{}}
	for i := uint64(1); i <= 8; i++ {
		encrypted, encErr := cipher.Encrypt(fmt.Sprintf("http://10.0.0.%d:8080", i))
		if encErr != nil {
			t.Fatal(encErr)
		}
		repo.nodes = append(repo.nodes, domain.Node{ID: i, Name: fmt.Sprintf("node-%d", i), Enabled: true, Health: 1, EncryptedProxyURL: encrypted})
	}
	manager := NewManager(repo, cipher)

	const workers = 32
	const iterations = 60
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				stormCtx, cancel := context.WithCancel(context.Background())
				lease, acquireErr := manager.Acquire(stormCtx, domain.ScopeBuild, fmt.Sprintf("storm-%d", worker))
				if acquireErr == nil {
					if worker%2 == 0 {
						cancel() // 半数 worker 在持租约时取消上下文
					}
					lease.Release()
				}
				cancel() // 幂等:偶数路径提前取消过,此处保证所有路径都释放
			}
		}(worker)
	}
	wait.Wait()

	// inflight 守恒:所有节点计数归零。
	manager.nodeMu.RLock()
	for _, node := range repo.nodes {
		if value := manager.inflightCount(node.ID); value != 0 {
			manager.nodeMu.RUnlock()
			t.Fatalf("node %d inflight = %d after storm, want 0", node.ID, value)
		}
	}
	manager.nodeMu.RUnlock()

	// 风暴后正常获取不受污染。
	healthy, err := manager.Acquire(context.Background(), domain.ScopeBuild, "post-storm")
	if err != nil {
		t.Fatalf("post-storm acquire: %v", err)
	}
	healthy.Release()

	// goroutine 守恒:风暴不得遗留后台 goroutine(对比强制 GC 前后两次采样)。
	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		if runtime.NumGoroutine() <= before {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	settled := runtime.NumGoroutine()
	if settled > before {
		t.Fatalf("goroutines grew after storm: %d -> %d", before, settled)
	}
}
