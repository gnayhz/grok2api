package egress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// 防风暴语义锁定(README 承诺的限流: 同节点 ≥10 分钟、全局每小时 ≤N):
//  1. 同节点突发隔离/入队被去重为单个队列条目;
//  2. 全局每小时配额耗尽后返回等待时长(小时内不再放行), 小时窗口滚动复位;
//  3. requeueAfter 的延迟节点不阻塞队列——其他节点立即可被消费;
//  4. 服务级突发: 3 个节点同时隔离而 maxGlobalPerHour=2 → 只有 2 次
//     webhook 真实发生, 第 3 个节点被限流重排(不燃烧尝试计数)。
func TestRotationRateGuardsPreventStorm(t *testing.T) {
	t.Run("queue deduplicates burst", func(t *testing.T) {
		scheduler := &rotationScheduler{set: map[uint64]struct{}{}, wake: make(chan struct{}, 1)}
		for i := 0; i < 10; i++ {
			scheduler.requeue(7)
		}
		if id, ok := scheduler.next(); !ok || id != 7 {
			t.Fatalf("first next = (%d,%v), want (7,true)", id, ok)
		}
		if _, ok := scheduler.next(); ok {
			t.Fatal("burst of 10 enqueues must deduplicate to a single queue entry")
		}
	})

	t.Run("global hourly cap", func(t *testing.T) {
		scheduler := &rotationScheduler{set: map[uint64]struct{}{}, wake: make(chan struct{}, 1)}
		for i := 0; i < 2; i++ {
			if wait := scheduler.allowGlobal(2); wait != 0 {
				t.Fatalf("allowance %d denied: wait=%v", i, wait)
			}
		}
		wait := scheduler.allowGlobal(2)
		if wait <= 0 || wait > time.Hour {
			t.Fatalf("exhausted cap wait = %v, want (0, 1h]", wait)
		}
		// 小时窗口滚动: 回拨窗口起点后配额复位。
		scheduler.mu.Lock()
		scheduler.hourStart = time.Now().Add(-time.Hour - time.Minute)
		scheduler.mu.Unlock()
		if wait := scheduler.allowGlobal(2); wait != 0 {
			t.Fatalf("rolled-over window still denied: wait=%v", wait)
		}
	})

	t.Run("delayed requeue does not block queue", func(t *testing.T) {
		scheduler := &rotationScheduler{set: map[uint64]struct{}{}, wake: make(chan struct{}, 1)}
		scheduler.requeueAfter(1, 10*time.Minute)
		scheduler.requeue(2)
		if id, ok := scheduler.next(); !ok || id != 2 {
			t.Fatalf("next = (%d,%v), want (2,true): a delayed node must not block the queue", id, ok)
		}
		if _, ok := scheduler.next(); ok {
			t.Fatal("delayed node must not be selectable before its delay elapses")
		}
	})

	t.Run("service burst honors global cap", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var webhookCalls atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			webhookCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(server.Close)
		cipher := newRotationCipher(t)

		repo := &multiNodeRotationRepo{nodes: map[uint64]domain.Node{}}
		for _, id := range []uint64{1, 2, 3} {
			encrypted, err := cipher.Encrypt(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			repo.nodes[id] = domain.Node{ID: id, Name: "burst", Enabled: true, Health: 1, RotationEnabled: true, EncryptedRotationURL: encrypted, ExitIP: "203.0.113.9"}
		}
		prober := &rotationTestProber{result: domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.77"}}
		canary := &canaryStub{result: EgressQualityProbeResult{Outcome: EgressQualityProbeClean}}
		service := &Service{
			repository: repo, cipher: cipher, qualityQuarantiner: &fakeQuarantiner{},
			qualityGuard: DefaultQualityGuardConfig(), qualityEvidence: map[uint64][]degradeObservation{},
		}
		service.operations = repo
		service.SetNodeProber(prober)
		cfg := fastRotationConfig()
		cfg.MaxGlobalPerHour = 2
		cfg.MinNodeInterval = 0 // 归一为 10m:首个节点无 LastRotatedAt 不受影响
		cfg.MaxAttemptsPerQuarantine = 3
		service.SetRotationConfig(cfg)
		service.SetEgressQualityProber(canary)

		workerCtx, workerCancel := context.WithCancel(ctx)
		defer workerCancel()
		go service.RunRotationWorker(workerCtx)

		// 3 个节点同时隔离入队, 全局配额 2。
		for _, id := range []uint64{1, 2, 3} {
			service.QuarantineForExitIP(ctx, id, 9000+id)
		}

		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if webhookCalls.Load() >= 2 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(500 * time.Millisecond) // 若防风暴失效, 第 3 次会立即发生
		if got := webhookCalls.Load(); got != 2 {
			t.Fatalf("burst of 3 quarantines with maxGlobalPerHour=2 produced %d webhook calls, want exactly 2", got)
		}
		// 被限流的第 3 个节点不燃烧尝试计数(未进入轮换周期)。
		repo.mu.Lock()
		attempts3 := repo.nodes[3].RotationAttempts
		repo.mu.Unlock()
		if attempts3 != 0 {
			t.Fatalf("rate-limited node burned an attempt: %d", attempts3)
		}
	})
}

// multiNodeRotationRepo 覆盖轮换调度器需要的多节点仓储面。
type multiNodeRotationRepo struct {
	ServiceRepository
	OperationsRepository
	mu    sync.Mutex
	nodes map[uint64]domain.Node
}

func (r *multiNodeRotationRepo) GetEgressNode(_ context.Context, id uint64) (domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	node, ok := r.nodes[id]
	if !ok {
		return domain.Node{}, ErrNotFound
	}
	return node, nil
}

func (r *multiNodeRotationRepo) UpdateEgressNodeRotationState(_ context.Context, id uint64, lastRotatedAt *time.Time, attempts int, lastError string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	node := r.nodes[id]
	node.RotationAttempts = attempts
	if lastRotatedAt != nil {
		node.LastRotatedAt = lastRotatedAt
	}
	node.LastRotationError = lastError
	r.nodes[id] = node
	return nil
}

func (r *multiNodeRotationRepo) UpdateEgressNodeProbe(context.Context, uint64, string, domain.ProbeResult) error {
	return nil
}

func (r *multiNodeRotationRepo) ListEgressNodes(_ context.Context, _ repository.SortQuery) ([]domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	nodes := make([]domain.Node, 0, len(r.nodes))
	for _, node := range r.nodes {
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// 重启恢复:轮换队列是进程内状态,而隔离持久在库。worker 启动时必须把
// "质量隔离中且配置了换 IP"的节点重新入队——否则进程在隔离与轮换之间
// 重启后, 坏出口滞留整个隔离周期, 冷却到期未经验证直接回池。非隔离/
// 无 webhook/冷却已过的节点不得入队。
func TestRotationRecoversPendingQuarantineAfterRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var webhookCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		webhookCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	cipher := newRotationCipher(t)
	encryptedWebhook, err := cipher.Encrypt(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	until := time.Now().Add(2 * time.Hour)
	repo := &multiNodeRotationRepo{nodes: map[uint64]domain.Node{}}
	repo.nodes[1] = domain.Node{ID: 1, Name: "quarantined", Enabled: true, Health: 1, RotationEnabled: true, EncryptedRotationURL: encryptedWebhook, CooldownUntil: &until, LastError: domain.LastErrorExitIPQuality, ExitIP: "203.0.113.7"}
	repo.nodes[2] = domain.Node{ID: 2, Name: "no-webhook", Enabled: true, Health: 1, CooldownUntil: &until, LastError: domain.LastErrorExitIPQuality}
	repo.nodes[3] = domain.Node{ID: 3, Name: "transport-cooling", Enabled: true, Health: 1, RotationEnabled: true, EncryptedRotationURL: encryptedWebhook, CooldownUntil: &until, LastError: domain.LastErrorTransport}
	repo.nodes[4] = domain.Node{ID: 4, Name: "expired", Enabled: true, Health: 1, RotationEnabled: true, EncryptedRotationURL: encryptedWebhook, LastError: domain.LastErrorExitIPQuality}

	prober := &rotationTestProber{result: domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.90"}}
	service := &Service{
		repository: repo, cipher: cipher, qualityQuarantiner: &fakeQuarantiner{},
		qualityGuard: DefaultQualityGuardConfig(), qualityEvidence: map[uint64][]degradeObservation{},
	}
	service.operations = repo
	service.SetNodeProber(prober)
	cfg := fastRotationConfig()
	service.SetRotationConfig(cfg)
	service.SetEgressQualityProber(&canaryStub{result: EgressQualityProbeResult{Outcome: EgressQualityProbeClean}})

	// 关键前置:不调用 QuarantineForExitIP——模拟"隔离发生在上一个进程
	// 生命周期内", 唯一的入队来源是 worker 启动恢复。
	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()
	go service.RunRotationWorker(workerCtx)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && webhookCalls.Load() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if webhookCalls.Load() == 0 {
		t.Fatal("quarantined node was never rotated after restart: pending rotation intent lost")
	}
	if webhookCalls.Load() > 2 {
		t.Fatalf("unexpected webhook storm: %d calls", webhookCalls.Load())
	}
	// 只有节点 1 恢复; 其余节点(无 webhook/传输冷却/冷却已过)不入队。
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.nodes[2].RotationAttempts != 0 || repo.nodes[3].RotationAttempts != 0 || repo.nodes[4].RotationAttempts != 0 {
		t.Fatalf("non-eligible nodes were rotated: %+v", repo.nodes)
	}
}

// 并发触发去重:手动轮换(API)与降智隔离并发到达时, 全部经由单 worker
// 队列串行化——慢 webhook(100ms)在途期间的新触发不得产生第二次调用
// (队列集合去重), 也不得并发执行 processRotation。
func TestConcurrentRotationTriggersDeduplicate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var webhookCalls atomic.Int64
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		webhookCalls.Add(1)
		<-release // 慢 webhook:在途期间堆积并发触发
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	cipher := newRotationCipher(t)
	encryptedWebhook, err := cipher.Encrypt(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	repo := &multiNodeRotationRepo{nodes: map[uint64]domain.Node{}}
	repo.nodes[1] = domain.Node{ID: 1, Name: "concurrent", Enabled: true, Health: 1, RotationEnabled: true, EncryptedRotationURL: encryptedWebhook, ExitIP: "203.0.113.7", CooldownUntil: &until, LastError: domain.LastErrorExitIPQuality}
	service := &Service{
		repository: repo, cipher: cipher, qualityQuarantiner: &fakeQuarantiner{},
		qualityGuard: DefaultQualityGuardConfig(), qualityEvidence: map[uint64][]degradeObservation{},
	}
	service.operations = repo
	service.SetNodeProber(&rotationTestProber{result: domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.5"}})
	service.SetRotationConfig(fastRotationConfig())
	service.SetEgressQualityProber(&canaryStub{result: EgressQualityProbeResult{Outcome: EgressQualityProbeClean}})

	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()
	go service.RunRotationWorker(workerCtx)

	// 10 个并发手动触发 + 3 个并发隔离事件, 全部在 webhook 在途时到达。
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = service.RotateNode(ctx, 1)
		}()
	}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			service.QuarantineForExitIP(ctx, 1, 7000)
		}()
	}
	time.Sleep(200 * time.Millisecond) // 等待首个轮换进入 webhook
	wg.Wait()
	close(release) // 放行慢 webhook

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		repo.mu.Lock()
		done := repo.nodes[1].LastRotatedAt != nil
		repo.mu.Unlock()
		if done {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if got := webhookCalls.Load(); got != 1 {
		t.Fatalf("concurrent triggers produced %d webhook calls, want exactly 1 (queue dedup + single worker)", got)
	}
}

// 出口指标词汇表:轮换四个终态(succeeded/failed/tentative_release/exhausted)
// 与 §4 日志事件表 1:1 对应地记入 egress_rotation_total; egress 域计数器
// 的 Subsystem 恒为 "egress"。本测试以注册器真实采样断言(重启恢复场景
// 走 canary clean → succeeded)。
func TestEgressRotationMetricVocabulary(t *testing.T) {
	before := perfmetrics.Default.CollectAndReset()
	_ = before

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var webhookCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		webhookCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	cipher := newRotationCipher(t)
	encryptedWebhook, err := cipher.Encrypt(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	repo := &multiNodeRotationRepo{nodes: map[uint64]domain.Node{}}
	repo.nodes[1] = domain.Node{ID: 1, Name: "metric", Enabled: true, Health: 1, RotationEnabled: true, EncryptedRotationURL: encryptedWebhook, CooldownUntil: &until, LastError: domain.LastErrorExitIPQuality, ExitIP: "203.0.113.7"}
	service := &Service{
		repository: repo, cipher: cipher, qualityQuarantiner: &fakeQuarantiner{},
		qualityGuard: DefaultQualityGuardConfig(), qualityEvidence: map[uint64][]degradeObservation{},
	}
	service.operations = repo
	service.SetNodeProber(&rotationTestProber{result: domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.21"}})
	service.SetRotationConfig(fastRotationConfig())
	service.SetEgressQualityProber(&canaryStub{result: EgressQualityProbeResult{Outcome: EgressQualityProbeClean}})

	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()
	go service.RunRotationWorker(workerCtx)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		repo.mu.Lock()
		done := repo.nodes[1].LastRotatedAt != nil
		repo.mu.Unlock()
		if done {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	samples := perfmetrics.Default.CollectAndReset()
	found := false
	for _, sample := range samples {
		if sample.Name != "egress_rotation_total" {
			continue
		}
		if sample.Labels.Subsystem != "egress" || sample.Labels.Operation != "rotation" {
			t.Fatalf("egress rotation metric mislabeled: %+v", sample.Labels)
		}
		if sample.Labels.Outcome == "succeeded" && sample.Count >= 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("egress_rotation_total{succeeded} not observed after a clean rotation: %+v", samples)
	}
}
