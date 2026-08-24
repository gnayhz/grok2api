package egress

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// TestEgressSoakResourceStability 是长时运行维度的回归锁:真实本地代理 + 真实
// HTTP 出站 + 反馈/失效/证据/游标搅动循环之后, goroutine 数、进程 FD 数、
// 堆(强制 GC 后)与池统计容量必须回落到基线附近——任何随迭代单调增长的项
// 都是泄漏(连接、goroutine、缓存无界)。运行预算约 15-25s, -race 下跳过
// (吞吐过低, 该维度由泄漏专项测试在 -race 下覆盖)。
func TestEgressSoakResourceStability(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test needs sustained runtime")
	}
	const iterations = 3000
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var proxyAHits, proxyBHits atomic.Int64
	origin := newEchoOrigin(t)
	originHits := origin.hits
	defer origin.server.Close()
	proxyA := newForwardProxy(t, origin.server, &proxyAHits)
	proxyB := newForwardProxy(t, origin.server, &proxyBHits)

	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := &e2eRepo{pools: map[uint64]domain.Pool{}}
	config := domain.DefaultOperationsConfig()
	config.ClassTargets = map[domain.TrafficClass]domain.RoutingTarget{
		domain.TrafficClassInference: {Mode: domain.RoutingTargetPool, PoolID: 1},
	}
	repo.config = config
	manager := NewManager(repo, cipher)
	nodeA := domain.Node{ID: 10, Name: "soak-a", Enabled: true, Health: 1}
	nodeB := domain.Node{ID: 20, Name: "soak-b", Enabled: true, Health: 1}
	nodeA.EncryptedProxyURL = encryptedProxy(t, cipher, proxyA.URL)
	nodeB.EncryptedProxyURL = encryptedProxy(t, cipher, proxyB.URL)
	repo.nodes = []domain.Node{nodeA, nodeB}
	repo.pools[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyRotation, FallbackMode: domain.PoolFallbackNone}

	fdCount := func() int {
		entries, readErr := os.ReadDir("/proc/self/fd")
		if readErr != nil {
			return -1
		}
		return len(entries)
	}
	poolStatEntries := func() int {
		poolNodeStats.mu.RLock()
		defer poolNodeStats.mu.RUnlock()
		total := len(poolNodeStats.failures)
		for _, nodes := range poolNodeStats.pools {
			total += len(nodes)
		}
		return total
	}
	baseline := func() (goroutines, fds, heap, entries int) {
		runtime.GC()
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return runtime.NumGoroutine(), fdCount(), int(stats.HeapAlloc), poolStatEntries()
	}

	// 预热:建立客户端缓存/连接池与统计条目, 基线在稳态后采样。
	warmup := 200
	for i := 0; i < warmup; i++ {
		runSoakIteration(t, ctx, manager, origin, i, false)
	}
	goroutineBase, fdBase, heapBase, entriesBase := baseline()

	for i := 0; i < iterations; i++ {
		runSoakIteration(t, ctx, manager, origin, i, i%97 == 0)
		// 搅动进程内守卫状态:证据标记/解除、池缓存失效(游标簿记重置)、
		// 粘性判定记忆失效(代理 URL 变更→记忆表逐出重建)。
		if i%50 == 0 {
			manager.MarkDegradeEvidence(10)
			manager.ClearDegradeEvidence(10)
		}
		if i%200 == 0 {
			manager.InvalidatePoolCache()
		}
		if i%400 == 0 {
			nodeA.EncryptedProxyURL = encryptedProxy(t, cipher, proxyA.URL)
			repo.mu.Lock()
			repo.nodes[0] = nodeA
			repo.mu.Unlock()
			manager.invalidateNodes()
		}
	}

	settled := func() (goroutines, fds, heap, entries int) {
		time.Sleep(200 * time.Millisecond)
		return baseline()
	}
	goroutinesAfter, fdAfter, heapAfter, entriesAfter := settled()

	if originHits.Load() < int64(iterations+warmup) {
		t.Fatalf("origin hits = %d, expected at least %d real round trips", originHits.Load(), iterations+warmup)
	}
	if proxyAHits.Load()+proxyBHits.Load() != originHits.Load() {
		t.Fatalf("proxy hits (%d+%d) must equal origin hits (%d): every request traverses a node proxy", proxyAHits.Load(), proxyBHits.Load(), originHits.Load())
	}
	if goroutinesAfter > goroutineBase+5 {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutine leak: baseline=%d after=%d (+%d)\n%s", goroutineBase, goroutinesAfter, goroutinesAfter-goroutineBase, buf[:n])
	}
	if fdBase >= 0 && fdAfter > fdBase+16 {
		t.Fatalf("file descriptor leak: baseline=%d after=%d (+%d)", fdBase, fdAfter, fdAfter-fdBase)
	}
	if heapAfter > heapBase*2 && heapAfter-heapBase > 32<<20 {
		t.Fatalf("heap grew: baseline=%d after=%d", heapBase, heapAfter)
	}
	if entriesAfter > entriesBase+64 {
		t.Fatalf("pool stats entries grew unbounded: baseline=%d after=%d", entriesBase, entriesAfter)
	}
	t.Logf("soak: iterations=%d goroutines %d->%d fds %d->%d heap %d->%d stats %d->%d",
		iterations, goroutineBase, goroutinesAfter, fdBase, fdAfter, heapBase, heapAfter, entriesBase, entriesAfter)
}

func runSoakIteration(t *testing.T, ctx context.Context, manager *Manager, origin *echoOrigin, i int, degradeFeedback bool) {
	t.Helper()
	lease, err := manager.Acquire(WithTrafficClass(ctx, domain.TrafficClassInference), domain.ScopeBuild, fmt.Sprintf("soak-%d", i%32))
	if err != nil {
		t.Fatalf("acquire %d: %v", i, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.url+"/soak", nil)
	if err != nil {
		lease.Release()
		t.Fatal(err)
	}
	response, err := lease.Do(request)
	if err != nil {
		lease.Release()
		t.Fatalf("round trip %d: %v", i, err)
	}
	response.Body.Close()
	lease.Release()
	if degradeFeedback {
		// 模拟一次瞬时降智反馈(随后下轮迭代恢复):反馈只记录状态, 不产生
		// 网络 I/O; 使用成功状态保持节点可调度。
		manager.FeedbackForScope(ctx, domain.ScopeBuild, lease.NodeID, http.StatusOK, nil)
	}
}

// echoOrigin 是最简真实源站, 记录命中数以证明真实出站。
type echoOrigin struct {
	server *httptest.Server
	url    string
	hits   *atomic.Int64
}

func newEchoOrigin(t *testing.T) *echoOrigin {
	t.Helper()
	hits := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprintf(w, "ok %s", strings.TrimSpace(r.URL.Path))
	}))
	t.Cleanup(server.Close)
	return &echoOrigin{server: server, url: server.URL, hits: hits}
}
