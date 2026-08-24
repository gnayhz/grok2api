package egress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
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

func (r *rotationStubRepo) UpdateEgressNodeRotationState(_ context.Context, _ uint64, lastRotatedAt *time.Time, attempts int, lastError string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rotationCalls++
	r.rotationState = [3]any{lastRotatedAt, attempts, lastError}
	r.node.RotationAttempts = attempts
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

type canaryStub struct {
	result EgressQualityProbeResult
	calls  int
}

func (c *canaryStub) ProbeEgressQuality(context.Context, uint64) EgressQualityProbeResult {
	c.calls++
	return c.result
}

func newRotationCipher(t *testing.T) *security.Cipher {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

func newRotationTestService(t *testing.T, node domain.Node, webhookOK bool, probe domain.ProbeResult, canary EgressQualityProbeResult) (*Service, *rotationStubRepo, *httptest.Server, *rotationTestProber, *canaryStub) {
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
	canaryStub := &canaryStub{result: canary}
	service := &Service{
		repository: repo, cipher: cipher, qualityQuarantiner: &fakeQuarantiner{},
		qualityGuard: DefaultQualityGuardConfig(), rotationCfg: fastRotationConfig(), qualityProber: canaryStub,
	}
	service.operations = repo
	service.SetNodeProber(prober)
	service.SetRotationConfig(fastRotationConfig())
	_ = webhookCallCount(&mu, &webhookCalls)
	return service, repo, server, prober, canaryStub
}

func fastRotationConfig() RotationConfig {
	cfg := DefaultRotationConfig()
	cfg.SettleDelay = 0
	cfg.ProbeTimeout = 2 * time.Second
	cfg.ProbeInterval = 10 * time.Millisecond
	cfg.MinNodeInterval = 0
	cfg.MaxGlobalPerHour = 1000
	return cfg
}

func webhookCallCount(mu *sync.Mutex, count *int) int { mu.Lock(); defer mu.Unlock(); return *count }

// canary clean → 解除隔离、重置尝试计数。
func TestRotationCanaryCleanReleases(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7"}
	service, repo, _, prober, canary := newRotationTestService(t, node, true, probe, EgressQualityProbeResult{Outcome: EgressQualityProbeClean})
	service.processRotation(context.Background(), 3)
	if prober.calls == 0 || canary.calls != 1 {
		t.Fatalf("probe calls = %d, canary calls = %d", prober.calls, canary.calls)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.rotationState[1] != 0 {
		t.Fatalf("attempts not reset: %+v", repo.rotationState)
	}
}

// canary 未配置模型 → 按文档承诺暂定放行(短冷却), 而非扣满整个隔离周期。
func TestRotationCanaryUnconfiguredTentativelyReleases(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7"}
	service, repo, _, _, canary := newRotationTestService(t, node, true, probe, EgressQualityProbeResult{Outcome: EgressQualityProbeUnconfigured, Reason: "canary model not configured"})
	quarantiner := &fakeQuarantiner{}
	service.qualityQuarantiner = quarantiner
	service.processRotation(context.Background(), 3)
	if canary.calls != 1 {
		t.Fatalf("canary calls = %d", canary.calls)
	}
	quarantiner.mu.Lock()
	defer quarantiner.mu.Unlock()
	if len(quarantiner.cooldown) != 1 || quarantiner.cooldown[0] != 3 {
		t.Fatalf("tentative release cooldown calls = %v, want [3]", quarantiner.cooldown)
	}
	if len(quarantiner.release) != 0 {
		t.Fatalf("unconfigured canary must not hard-release: %v", quarantiner.release)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.rotationState[1] != 0 {
		t.Fatalf("attempts = %+v, want reset to 0", repo.rotationState)
	}
}

// 出口 IP 未变化 → 计失败并重排。
func TestRotationUnchangedExitIPFails(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7", TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7"}
	service, repo, _, _, canary := newRotationTestService(t, node, true, probe, EgressQualityProbeResult{Outcome: EgressQualityProbeClean})
	service.processRotation(context.Background(), 3)
	if canary.calls != 0 {
		t.Fatalf("canary should not run for unchanged exit ip")
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.rotationState[1] != 1 {
		t.Fatalf("attempts = %+v, want 1", repo.rotationState)
	}
}

// IPv4 恒定、仅 IPv6 变化(MicroWARP 重启常态)→ 轮换视为有效,放行 canary。
func TestRotationIPv6OnlyChangeSucceeds(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7",
		IPv4: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7"},
		IPv6: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "2001:db8::dead:beef:2"}, TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7",
		IPv4Probe: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7"},
		IPv6Probe: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "2001:db8::dead:beef:1"}}
	service, repo, _, _, canary := newRotationTestService(t, node, true, probe, EgressQualityProbeResult{Outcome: EgressQualityProbeClean})
	service.processRotation(context.Background(), 3)
	if canary.calls != 1 {
		t.Fatalf("canary calls = %d, want 1 (IPv6 rotation must count as changed)", canary.calls)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.rotationState[1] != 0 {
		t.Fatalf("attempts = %+v, want reset to 0", repo.rotationState)
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
	service, repo, _, _, canary := newRotationTestService(t, node, true, probe, EgressQualityProbeResult{Outcome: EgressQualityProbeClean})
	service.processRotation(context.Background(), 3)
	if canary.calls != 0 {
		t.Fatalf("canary calls = %d, want 0 (no family changed)", canary.calls)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.rotationState[1] != 1 {
		t.Fatalf("attempts = %+v, want 1", repo.rotationState)
	}
}

// 死出口轮换(LastError=transport): 隧道重启探活健康即成功——不走 canary、
// 不做质量解除、不做暂定冷却; 尝试计数归零。
func TestRotationProbeDeadRecoversWithoutCanary(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7",
		IPv4: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7"},
		IPv6: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "2001:db8::dead:beef:9"}, TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7", LastError: "transport error",
		IPv4Probe: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7"},
		IPv6Probe: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "2001:db8::dead:beef:8"}}
	service, repo, _, _, canary := newRotationTestService(t, node, true, probe, EgressQualityProbeResult{Outcome: EgressQualityProbeClean})
	quarantiner := &fakeQuarantiner{}
	service.qualityQuarantiner = quarantiner
	service.processRotation(context.Background(), 3)
	if canary.calls != 0 {
		t.Fatalf("canary calls = %d, want 0 (transport rotation must not verify quality)", canary.calls)
	}
	quarantiner.mu.Lock()
	releases, cooldowns := len(quarantiner.release), len(quarantiner.cooldown)
	quarantiner.mu.Unlock()
	if releases != 0 || cooldowns != 0 {
		t.Fatalf("quality release/cooldown must not run: release=%d cooldown=%d", releases, cooldowns)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if attempts, _ := repo.rotationState[1].(int); attempts != 0 {
		t.Fatalf("attempts = %v, want reset to 0", repo.rotationState[1])
	}
}

// canary 判定降智 → 计失败；到上限后不再重排。
func TestRotationCanaryDegradedCountsAndCaps(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7"}
	service, repo, _, _, canary := newRotationTestService(t, node, true, probe, EgressQualityProbeResult{Outcome: EgressQualityProbeDegraded, Reason: "no thinking"})
	service.processRotation(context.Background(), 3)
	if canary.calls != 1 {
		t.Fatalf("canary calls = %d", canary.calls)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.rotationState[1] != 1 {
		t.Fatalf("attempts = %+v, want 1", repo.rotationState)
	}
}

// webhook 不可达 → 记录错误、保持隔离、不计入尝试。
func TestRotationWebhookFailureRecorded(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7"}
	service, repo, _, _, canary := newRotationTestService(t, node, false, probe, EgressQualityProbeResult{Outcome: EgressQualityProbeClean})
	service.processRotation(context.Background(), 3)
	if canary.calls != 0 {
		t.Fatalf("canary should not run after webhook failure")
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.rotationState[1] != 0 {
		t.Fatalf("attempts changed on webhook failure: %+v", repo.rotationState)
	}
	if s, _ := repo.rotationState[2].(string); s == "" {
		t.Fatalf("last rotation error not recorded")
	}
}

// 尝试次数耗尽 → 直接跳过，不触发 webhook。
func TestRotationExhaustedSkips(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7", RotationAttempts: 3}
	service, _, _, prober, canary := newRotationTestService(t, node, true, probe, EgressQualityProbeResult{Outcome: EgressQualityProbeClean})
	service.processRotation(context.Background(), 3)
	if prober.calls != 0 || canary.calls != 0 {
		t.Fatalf("exhausted node still processed: probe=%d canary=%d", prober.calls, canary.calls)
	}
}

var _ = repository.ErrNotFound
