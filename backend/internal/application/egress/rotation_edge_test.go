package egress

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// 轮换调度器的过程边界覆盖补齐:此前既有测试集中在终态(canary/ExitIP/
// exhausted/webhook 失败),以下路径从未执行——
//   - SetRotationConfig 禁用时丢弃排队工作(热更新语义)
//   - processRotation 的四个跳过路径(节点停用/无 webhook/节点级关闭/解密失败)
//   - requeueAfter 的定时器路径(MinNodeInterval 非阻塞重排的核心机制)
//   - waitNodeHealthy 的重试循环与超时返回(上次结果)
//   - 探活仓储错误 → failRotation(隔离保持 + 预算内重排)

// nonNilTime:recordRotationState 把 (*time.Time)(nil) 存进 any 槽——
// 带类型的 nil 接口本身非 nil。只有真实记录了时间戳才返回 true。
func nonNilTime(value any) bool {
	pointer, ok := value.(*time.Time)
	return ok && pointer != nil
}

// 序列探活器:按预设序列返回,越界重复最后一个。
type sequenceProber struct {
	mu      sync.Mutex
	results []domain.ProbeResult
	calls   int
}

func (p *sequenceProber) ProbeEgressNode(context.Context, domain.Node) (domain.ProbeResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	index := p.calls
	p.calls++
	if index < len(p.results) {
		return p.results[index], nil
	}
	return p.results[len(p.results)-1], nil
}

// 手工装配的轮换服务(绕过 harness 对空 webhook 的自动填充)。
type bareRotationRepo interface {
	ServiceRepository
	OperationsRepository
}

func newBareRotationService(t *testing.T, repo bareRotationRepo, cfg RotationConfig, prober NodeProber) *Service {
	t.Helper()
	service := &Service{
		repository: repo, cipher: newRotationCipher(t), qualityQuarantiner: &fakeQuarantiner{},
		qualityGuard: DefaultQualityGuardConfig(), rotationCfg: cfg,
	}
	service.operations = repo
	if prober != nil {
		service.SetNodeProber(prober)
	}
	service.SetRotationConfig(cfg)
	return service
}

// SetRotationConfig 禁用必须丢弃已排队工作,且禁用后入队为无操作。
func TestRotationConfigDisableDropsQueue(t *testing.T) {
	healthy := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	repo := &rotationStubRepo{node: domain.Node{ID: 5, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7"}}
	cfg := fastRotationConfig()
	service := newBareRotationService(t, repo, cfg, &rotationTestProber{result: healthy})

	service.enqueueRotation(5)
	service.enqueueRotation(6)
	if id, ok := service.rotation.next(); !ok || id != 5 {
		t.Fatalf("pre-disable next = %d,%v", id, ok)
	}

	disabled := fastRotationConfig()
	disabled.Enabled = false
	service.SetRotationConfig(disabled)
	if _, ok := service.rotation.next(); ok {
		t.Fatal("disable must drop queued work")
	}
	// 禁用后入队同样必须是无操作。
	service.enqueueRotation(6)
	if _, ok := service.rotation.next(); ok {
		t.Fatal("enqueue while disabled must be a no-op")
	}
}

// 四个跳过路径各自的前置语义:什么都没发生(无探活/无 canary/无 webhook),
// 且写下的状态与"未真正轮换"一致(rotated=false 不推进 LastRotatedAt)。
func TestRotationSkipPaths(t *testing.T) {
	healthy := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	lastRotated := time.Now().UTC().Add(-time.Hour)
	base := domain.Node{ID: 9, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7", RotationEnabled: true, LastRotatedAt: &lastRotated, RotationAttempts: 2}

	// (1) 节点停用:静默返回,连状态都不写。
	disabledNode := base
	disabledNode.Enabled = false
	repo := &rotationStubRepo{node: disabledNode}
	service := newBareRotationService(t, repo, fastRotationConfig(), &rotationTestProber{result: healthy})
	service.processRotation(context.Background(), 9)
	repo.mu.Lock()
	if repo.rotationCalls != 0 {
		t.Fatalf("disabled node wrote rotation state: %d", repo.rotationCalls)
	}
	repo.mu.Unlock()

	// (2) 无 webhook:记状态、不计尝试、不探活。
	noWebhook := base
	repo = &rotationStubRepo{node: noWebhook}
	service = newBareRotationService(t, repo, fastRotationConfig(), &rotationTestProber{result: healthy})
	service.processRotation(context.Background(), 9)
	repo.mu.Lock()
	rotatedAt, attempts, lastErr := repo.rotationState[0], repo.rotationState[1].(int), repo.rotationState[2].(string)
	repo.mu.Unlock()
	if repo.rotationCalls != 1 || nonNilTime(rotatedAt) || attempts != 2 || lastErr != "no rotation webhook configured" {
		t.Fatalf("no-webhook state = %+v calls=%d", repo.rotationState, repo.rotationCalls)
	}

	// (3) 节点级轮换关闭:同上,理由不同(需要有效 webhook 密文,该检查在前)。
	configuredURL, err := newRotationCipher(t).Encrypt("https://rotate.example/hook")
	if err != nil {
		t.Fatal(err)
	}
	perNodeOff := base
	perNodeOff.EncryptedRotationURL = configuredURL
	perNodeOff.RotationEnabled = false
	repo = &rotationStubRepo{node: perNodeOff}
	service = newBareRotationService(t, repo, fastRotationConfig(), &rotationTestProber{result: healthy})
	service.processRotation(context.Background(), 9)
	repo.mu.Lock()
	_, attempts, lastErr = repo.rotationState[0], repo.rotationState[1].(int), repo.rotationState[2].(string)
	repo.mu.Unlock()
	if attempts != 2 || lastErr != "rotation disabled for this node" {
		t.Fatalf("per-node-off state = %+v", repo.rotationState)
	}

	// (4) webhook 密文损坏:记状态、不探活、不触发 webhook。
	corrupt := base
	corrupt.EncryptedRotationURL = "not-valid-ciphertext"
	repo = &rotationStubRepo{node: corrupt}
	service = newBareRotationService(t, repo, fastRotationConfig(), &rotationTestProber{result: healthy})
	service.processRotation(context.Background(), 9)
	repo.mu.Lock()
	_, attempts, lastErr = repo.rotationState[0], repo.rotationState[1].(int), repo.rotationState[2].(string)
	repo.mu.Unlock()
	if attempts != 2 || lastErr == "" {
		t.Fatalf("corrupt-url state = %+v", repo.rotationState)
	}
}

// requeueAfter 定时器:未到期不出队,到期后节点回到队尾并唤醒 worker。
func TestRotationRequeueAfterTimer(t *testing.T) {
	scheduler := &rotationScheduler{set: make(map[uint64]struct{}), wake: make(chan struct{}, 1)}
	scheduler.requeueAfter(7, 60*time.Millisecond)
	if _, ok := scheduler.next(); ok {
		t.Fatal("node must not be consumable before the delay elapses")
	}
	select {
	case <-scheduler.wake:
		t.Fatal("wake must not fire before the delay elapses")
	default:
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if id, ok := scheduler.next(); ok {
			if id != 7 {
				t.Fatalf("requeued id = %d, want 7", id)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node was never requeued after the delay")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 零延迟退化为立即重排。
	scheduler.requeueAfter(8, 0)
	if id, ok := scheduler.next(); !ok || id != 8 {
		t.Fatalf("zero-delay requeue = %d,%v", id, ok)
	}
}

// MinNodeInterval 未到:不调 webhook、不推进 LastRotatedAt,经 requeueAfter
// 延迟重排——worker 不被原地阻塞。
func TestRotationMinIntervalDefersViaRequeue(t *testing.T) {
	healthy := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	recent := time.Now().UTC().Add(-20 * time.Millisecond)
	configuredURL, err := newRotationCipher(t).Encrypt("https://rotate.example/hook")
	if err != nil {
		t.Fatal(err)
	}
	node := domain.Node{ID: 11, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7", RotationEnabled: true, EncryptedRotationURL: configuredURL, LastRotatedAt: &recent}
	cfg := fastRotationConfig()
	cfg.MinNodeInterval = 120 * time.Millisecond
	repo := &rotationStubRepo{node: node}
	prober := &rotationTestProber{result: healthy}
	service := newBareRotationService(t, repo, cfg, prober)

	service.processRotation(context.Background(), 11)

	repo.mu.Lock()
	rotatedAt, lastErr := repo.rotationState[0], repo.rotationState[2].(string)
	repo.mu.Unlock()
	if nonNilTime(rotatedAt) {
		t.Fatal("deferred rotation must not advance LastRotatedAt")
	}
	if lastErr != "min interval not elapsed" {
		t.Fatalf("deferred state = %+v", repo.rotationState)
	}
	if prober.calls != 0 {
		t.Fatalf("probe ran during min-interval deferral: %d", prober.calls)
	}
	if _, ok := service.rotation.next(); ok {
		t.Fatal("node must not be immediately consumable during the deferral window")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if id, ok := service.rotation.next(); ok {
			if id != 11 {
				t.Fatalf("deferred requeue id = %d", id)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node never returned to the queue after min interval")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitNodeHealthy:先不健康后健康 → 重试后返回健康;持续不健康 → 超时返回
// 最后一次(不健康)结果且错误为 nil,由上层按探活不健康处理。
func TestRotationWaitNodeHealthyRetryAndDeadline(t *testing.T) {
	unhealthy := domain.ProbeResult{Status: domain.ProbeStatusUnhealthy, Error: "warming up", TestedAt: time.Now()}
	healthy := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	repo := &rotationStubRepo{node: domain.Node{ID: 13, Name: "warp", Enabled: true, Health: 1}}
	cfg := fastRotationConfig()
	cfg.ProbeTimeout = 2 * time.Second
	cfg.ProbeInterval = 10 * time.Millisecond

	// 重试到健康。
	flaky := &sequenceProber{results: []domain.ProbeResult{unhealthy, unhealthy, healthy}}
	service := newBareRotationService(t, repo, cfg, flaky)
	result, err := service.waitNodeHealthy(context.Background(), 13, cfg)
	if err != nil || result.Status != domain.ProbeStatusHealthy {
		t.Fatalf("retry result = %+v err = %v", result, err)
	}
	flaky.mu.Lock()
	calls := flaky.calls
	flaky.mu.Unlock()
	if calls != 3 {
		t.Fatalf("probe calls = %d, want 3 (retry loop)", calls)
	}

	// 超时:返回最后的不健康结果,错误为 nil。
	stuck := &sequenceProber{results: []domain.ProbeResult{unhealthy}}
	cfg.ProbeTimeout = 40 * time.Millisecond
	service = newBareRotationService(t, repo, cfg, stuck)
	result, err = service.waitNodeHealthy(context.Background(), 13, cfg)
	if err != nil || result.Status != domain.ProbeStatusUnhealthy {
		t.Fatalf("deadline result = %+v err = %v, want unhealthy/nil", result, err)
	}
	stuck.mu.Lock()
	calls = stuck.calls
	stuck.mu.Unlock()
	if calls < 2 {
		t.Fatalf("deadline calls = %d, want retry at least twice", calls)
	}
}

// 探活仓储错误 → failRotation:计一次尝试、保持隔离、预算内重排队尾。
type errorProbeRepo struct{ *rotationStubRepo }

func (r *errorProbeRepo) UpdateEgressNodeProbe(context.Context, uint64, string, domain.ProbeResult) error {
	return errors.New("probe backend down")
}

func TestRotationProbeRepositoryErrorFailsRotation(t *testing.T) {
	webhookCalls := 0
	var webhookMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		webhookMu.Lock()
		webhookCalls++
		webhookMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	cipher := newRotationCipher(t)
	encrypted, err := cipher.Encrypt(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	node := domain.Node{ID: 15, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7", RotationEnabled: true, RotationAttempts: 1, EncryptedRotationURL: encrypted}
	base := &rotationStubRepo{node: node}
	repo := &errorProbeRepo{base}
	healthy := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	service := newBareRotationService(t, repo, fastRotationConfig(), &rotationTestProber{result: healthy})
	service.repository = repo

	service.processRotation(context.Background(), 15)

	base.mu.Lock()
	attempts, lastErr := base.rotationState[1].(int), base.rotationState[2].(string)
	base.mu.Unlock()
	if attempts != 2 {
		t.Fatalf("probe error attempts = %d, want 2", attempts)
	}
	if lastErr == "" {
		t.Fatal("probe error must be recorded")
	}
	webhookMu.Lock()
	calls := webhookCalls
	webhookMu.Unlock()
	if calls != 1 {
		t.Fatalf("webhook calls = %d, want exactly 1", calls)
	}
	// 预算内(2 < default max)必须重排。
	if id, ok := service.rotation.next(); !ok || id != 15 {
		t.Fatalf("failed rotation not requeued: %d,%v", id, ok)
	}
}

// SetRotationLogger:nil 拒绝,非 nil 安装(此前 0%)。
func TestRotationLoggerInstall(t *testing.T) {
	healthy := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	repo := &rotationStubRepo{node: domain.Node{ID: 17, Name: "warp", Enabled: true, Health: 1}}
	service := newBareRotationService(t, repo, fastRotationConfig(), &rotationTestProber{result: healthy})
	before := service.rotationLog()
	service.SetRotationLogger(nil)
	if service.rotationLog() != before {
		t.Fatal("nil logger must be ignored")
	}
	installed := slog.New(slog.DiscardHandler)
	service.SetRotationLogger(installed)
	if service.rotationLog() != installed {
		t.Fatal("logger was not installed")
	}
}
