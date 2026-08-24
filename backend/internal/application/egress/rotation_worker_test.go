package egress

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// RotateNode 服务路径(手动轮换入口,handler POST /egress-nodes/:id/rotate):
// 缺失 404 归一 / 轮换未启用的显式错误 / 启用后入队。此前 61.5%,前两个
// 错误分支与真实入队路径从未在服务层执行。
func TestRotateNodeServicePath(t *testing.T) {
	ctx := context.Background()
	cipher := newRotationCipher(t)
	encrypted, err := cipher.Encrypt("https://rotate.example/hook")
	if err != nil {
		t.Fatal(err)
	}
	repo := &multiNodeRotationRepo{nodes: map[uint64]domain.Node{
		5: {ID: 5, Name: "warp", Enabled: true, Health: 1, RotationEnabled: true, EncryptedRotationURL: encrypted},
	}}
	healthy := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	service := newBareRotationService(t, repo, fastRotationConfig(), &rotationTestProber{result: healthy})

	// 缺失节点:repository.ErrNotFound → 应用层 ErrNotFound(404),不透传 500。
	// (multiNodeRotationRepo 对缺失 id 返回应用层 ErrNotFound,语义等价归一结果。)
	if err := service.RotateNode(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotate missing node = %v, want ErrNotFound", err)
	}

	// 配置禁用:显式可解释错误,不是静默无操作。
	repo.mu.Lock()
	saved := repo.nodes[5]
	repo.mu.Unlock()
	_ = saved
	disabled := fastRotationConfig()
	disabled.Enabled = false
	service.SetRotationConfig(disabled)
	if err := service.RotateNode(ctx, 5); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("rotate while rotation disabled = %v, want explicit error", err)
	}

	// 启用:成功入队(经调度器集合)。
	service.SetRotationConfig(fastRotationConfig())
	if err := service.RotateNode(ctx, 5); err != nil {
		t.Fatalf("rotate enabled = %v", err)
	}
	if id, ok := service.rotation.next(); !ok || id != 5 {
		t.Fatalf("manual rotate did not enqueue: %d,%v", id, ok)
	}
}

// panic 隔离:processRotation 内任一 panic(webhook/解密/探测/canary)不得
// 击穿 worker 循环——后续节点继续被处理,batch.Do 捕获后记错误日志。
type panicRotationRepo struct {
	*multiNodeRotationRepo
	panicFor atomic.Uint64 // 1 = 下一次 GetEgressNode panic
}

func (r *panicRotationRepo) GetEgressNode(_ context.Context, id uint64) (domain.Node, error) {
	if r.panicFor.Load() == id {
		panic("simulated probe stack panic")
	}
	return r.multiNodeRotationRepo.GetEgressNode(context.Background(), id)
}

func newCountingWebhook(calls *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
}

func TestRotationWorkerSurvivesProcessingPanic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var webhookCalls atomic.Int64
	server := newCountingWebhook(&webhookCalls)
	t.Cleanup(server.Close)
	cipher := newRotationCipher(t)
	encrypted, err := cipher.Encrypt(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := &multiNodeRotationRepo{nodes: map[uint64]domain.Node{}}
	for _, id := range []uint64{1, 2} {
		base.nodes[id] = domain.Node{ID: id, Name: "warp", Enabled: true, Health: 1, RotationEnabled: true, EncryptedRotationURL: encrypted, ExitIP: "203.0.113.9"}
	}
	repo := &panicRotationRepo{multiNodeRotationRepo: base}
	repo.panicFor.Store(1) // 节点 1 的处理 panic
	healthy := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.77", TestedAt: time.Now()}
	service := &Service{
		repository: repo, cipher: cipher, qualityQuarantiner: &fakeQuarantiner{},
		qualityGuard: DefaultQualityGuardConfig(), qualityEvidence: map[uint64][]degradeObservation{},
	}
	service.operations = repo
	service.SetNodeProber(&rotationTestProber{result: healthy})
	service.SetRotationConfig(fastRotationConfig())
	service.SetEgressQualityProber(&canaryStub{result: EgressQualityProbeResult{Outcome: EgressQualityProbeClean}})

	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()
	done := make(chan struct{})
	go func() { service.RunRotationWorker(workerCtx); close(done) }()

	// 节点 1(panic)先入队,节点 2 随后——worker 必须两处都消费且存活。
	service.enqueueRotation(1)
	service.enqueueRotation(2)

	deadline := time.Now().Add(8 * time.Second)
	for webhookCalls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if webhookCalls.Load() < 1 {
		t.Fatalf("worker died before processing the healthy node after a panic: calls=%d", webhookCalls.Load())
	}
	workerCancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not exit after cancel")
	}
}
