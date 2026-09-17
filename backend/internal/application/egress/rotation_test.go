package egress

import (
	"context"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// rotationStubRepo 覆盖轮换调度器需要的仓储方法。
type rotationStubRepo struct {
	ServiceRepository
	OperationsRepository
	mu            sync.Mutex
	node          domain.Node
	rotationCalls int
	rotationState [3]any // lastRotatedAt, attempts, lastErr
}

func (r *rotationStubRepo) GetEgressNode(context.Context, uint64) (domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.node, nil
}

func (r *rotationStubRepo) UpdateEgressNodeProbe(context.Context, uint64, string, domain.ProbeResult) error {
	return nil
}

func (r *rotationStubRepo) BeginEgressNodeProbe(context.Context, uint64, string) (uint64, error) {
	return 1, nil
}

func (r *rotationStubRepo) UpdateEgressNodeRotationStateForBinding(_ context.Context, _ domain.Node, lastRotatedAt *time.Time, attempts int, lastError string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rotationCalls++
	r.rotationState = [3]any{lastRotatedAt, attempts, lastError}
	r.node.RotationAttempts = attempts
	if lastRotatedAt != nil {
		r.node.LastRotatedAt = lastRotatedAt
	}
	if lastError != "" {
		r.node.LastRotationError = lastError
	} else {
		r.node.LastRotationError = ""
	}
	return nil
}

type rotationTestProber struct {
	result domain.ProbeResult
	calls  int
}

func (p *rotationTestProber) ProbeEgressNode(context.Context, domain.Node) (domain.ProbeResult, error) {
	p.calls++
	return p.result, nil
}

func newRotationCipher(t *testing.T) security.Cryptor {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

// newRotationTestService 构建轮换测试服务。金丝雀验证已废除(G16):
// webhook→探活→IP 变化判定即终局,无二次推理验证。
func newRotationTestService(t *testing.T, node domain.Node, webhookOK bool, probe domain.ProbeResult) (*Service, *rotationStubRepo, *httptest.Server, *rotationTestProber) {
	t.Helper()
	webhookCalls := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		webhookCalls++
		mu.Unlock()
		if webhookOK {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	cipher := newRotationCipher(t)
	if node.EncryptedRotationURL == "" {
		encrypted, err := cipher.Encrypt(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		node.EncryptedRotationURL = encrypted
	}
	node.RotationEnabled = true
	repo := &rotationStubRepo{node: node}
	prober := &rotationTestProber{result: probe}
	service := &Service{
		repository: repo, cipher: cipher, qualityQuarantiner: &fakeQuarantiner{},
		rotationCfg: fastRotationConfig(),
	}
	service.operations = repo
	service.SetNodeProber(prober)
	setTestRotationConfig(service, fastRotationConfig())
	_ = webhookCallCount(&mu, &webhookCalls)
	return service, repo, server, prober
}

func fastRotationConfig() RotationConfig {
	// 与已删除的 DefaultRotationConfig() 等值的保守默认,显式构造便于审查。
	cfg := RotationConfig{
		Enabled:                  true,
		MaxAttemptsPerQuarantine: 3,
		MinNodeInterval:          3 * time.Minute,
		MaxGlobalPerHour:         6,
		WebhookTimeout:           15 * time.Second,
		WebhookRetries:           2,
		SettleDelay:              20 * time.Second,
		ProbeTimeout:             2 * time.Minute,
		ProbeInterval:            5 * time.Second,
	}
	cfg.SettleDelay = 0
	cfg.ProbeTimeout = 2 * time.Second
	cfg.ProbeInterval = 10 * time.Millisecond
	cfg.MinNodeInterval = 0
	cfg.MaxGlobalPerHour = 1000
	return cfg
}

func webhookCallCount(mu *sync.Mutex, count *int) int { mu.Lock(); defer mu.Unlock(); return *count }

// 出口 IP 已变化 → 轮换成功、重置尝试计数(G16:无金丝雀,IP 变即成功)。
func TestRotationIPChangeSucceeds(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7"}
	service, repo, _, prober := newRotationTestService(t, node, true, probe)
	service.processRotation(context.Background(), 3)
	if prober.calls == 0 {
		t.Fatal("probe never ran")
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.rotationState[1] != 0 {
		t.Fatalf("attempts not reset: %+v", repo.rotationState)
	}
}

// 出口 IP 未变化 → 计失败并重排。
func TestRotationUnchangedExitIPFails(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7", TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7"}
	service, repo, _, _ := newRotationTestService(t, node, true, probe)
	service.processRotation(context.Background(), 3)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.rotationState[1] != 1 {
		t.Fatalf("attempts = %+v, want 1", repo.rotationState)
	}
}

// IPv4 恒定、仅 IPv6 变化(MicroWARP 重启常态)→ 轮换视为有效。
func TestRotationIPv6OnlyChangeSucceeds(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7",
		IPv4: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7"},
		IPv6: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "2001:db8::dead:beef:2"}, TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7",
		IPv4Probe: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7"},
		IPv6Probe: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "2001:db8::dead:beef:1"}}
	service, repo, _, _ := newRotationTestService(t, node, true, probe)
	service.processRotation(context.Background(), 3)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.rotationState[1] != 0 {
		t.Fatalf("attempts = %+v, want reset 0 (IPv6 rotation must count as changed)", repo.rotationState)
	}
}

// 双栈均未变化 → 仍然失败(防假 webhook 语义保留)。
func TestRotationBothFamiliesUnchangedFails(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7",
		IPv4: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7"},
		IPv6: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "2001:db8::dead:beef:1"}, TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7",
		IPv4Probe: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7"},
		IPv6Probe: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "2001:db8::dead:beef:1"}}
	service, repo, _, _ := newRotationTestService(t, node, true, probe)
	service.processRotation(context.Background(), 3)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.rotationState[1] != 1 {
		t.Fatalf("attempts = %+v, want 1", repo.rotationState)
	}
}

// 死出口轮换(LastError=transport): 隧道重启探活健康即成功; 尝试计数归零。
func TestRotationProbeDeadRecovers(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7",
		IPv4: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7"},
		IPv6: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "2001:db8::dead:beef:9"}, TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7", LastError: "transport error",
		IPv4Probe: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7"},
		IPv6Probe: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "2001:db8::dead:beef:8"}}
	service, repo, _, _ := newRotationTestService(t, node, true, probe)
	service.processRotation(context.Background(), 3)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if attempts, _ := repo.rotationState[1].(int); attempts != 0 {
		t.Fatalf("attempts = %v, want reset to 0", repo.rotationState[1])
	}
}

// webhook 不可达 → 记录错误、保持隔离、消耗一次已预留尝试。
func TestRotationWebhookFailureRecorded(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7"}
	service, repo, _, _ := newRotationTestService(t, node, false, probe)
	service.processRotation(context.Background(), 3)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.rotationState[1] != 1 {
		t.Fatalf("webhook failure must consume a reserved attempt: %+v", repo.rotationState)
	}
	if s, _ := repo.rotationState[2].(string); s == "" {
		t.Fatalf("last rotation error not recorded")
	}
}

// 尝试次数耗尽 → 直接跳过，不触发 webhook。
func TestRotationExhaustedSkips(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7", RotationAttempts: 3}
	service, _, _, prober := newRotationTestService(t, node, true, probe)
	service.processRotation(context.Background(), 3)
	if prober.calls != 0 {
		t.Fatalf("exhausted node still processed: probe=%d", prober.calls)
	}
}

var _ = repository.ErrNotFound

func setTestRotationConfig(service *Service, cfg RotationConfig) {
	if service.rotationLock == nil {
		service.SetRotationCoordination(memory.NewLockStore(), memory.NewRateLimiter())
	}
	service.SetRotationConfig(cfg)
	service.SetWebhookExecutor(infraegress.NewRotationWebhookExecutor(nil))
}
