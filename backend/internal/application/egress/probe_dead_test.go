package egress

import (
	"context"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func deadProbeResult() domain.ProbeResult {
	return domain.ProbeResult{Status: domain.ProbeStatusUnhealthy, TestedAt: time.Now(),
		IPv4: domain.ProbeFamilyResult{Status: domain.ProbeStatusUnhealthy},
		IPv6: domain.ProbeFamilyResult{Status: domain.ProbeStatusUnhealthy}}
}

func aliveProbeResult() domain.ProbeResult {
	return domain.ProbeResult{Status: domain.ProbeStatusHealthy, TestedAt: time.Now(),
		IPv4: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7"},
		IPv6: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "2001:db8::1"}, ExitIP: "198.51.100.7"}
}

type switchableProber struct {
	mu     sync.Mutex
	result domain.ProbeResult
	calls  int
}

func (p *switchableProber) ProbeEgressNode(context.Context, domain.Node) (domain.ProbeResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.result, nil
}

func (p *switchableProber) set(result domain.ProbeResult) {
	p.mu.Lock()
	p.result = result
	p.mu.Unlock()
}

func withProbeDeadConfirmDelay(t *testing.T, d time.Duration) {
	t.Helper()
	previous := probeDeadConfirmDelay
	probeDeadConfirmDelay = d
	t.Cleanup(func() { probeDeadConfirmDelay = previous })
}

func newProbeDeadTestService(t *testing.T, node domain.Node, probe domain.ProbeResult) (*Service, *rotationStubRepo, *rotationTestProber, *fakeQuarantiner) {
	t.Helper()
	repo := &rotationStubRepo{node: node}
	prober := &rotationTestProber{result: probe}
	quarantiner := &fakeQuarantiner{}
	service := &Service{
		repository: repo, cipher: newRotationCipher(t), qualityQuarantiner: quarantiner,
		qualityGuard: DefaultQualityGuardConfig(), rotationCfg: fastRotationConfig(),
	}
	service.operations = repo
	service.SetNodeProber(prober)
	service.SetRotationConfig(fastRotationConfig())
	return service, repo, prober, quarantiner
}

func probeDeadTestNode(t *testing.T, id uint64, mutate func(*domain.Node)) domain.Node {
	t.Helper()
	cipher := newRotationCipher(t)
	proxy, err := cipher.Encrypt("socks5://10.0.0.2:41081")
	if err != nil {
		t.Fatal(err)
	}
	rotation, err := cipher.Encrypt("http://127.0.0.1:9000/rotate/41081?token=x")
	if err != nil {
		t.Fatal(err)
	}
	node := domain.Node{ID: id, Name: "warp1", Enabled: true, Health: 1,
		EncryptedProxyURL: proxy, EncryptedRotationURL: rotation, RotationEnabled: true}
	if mutate != nil {
		mutate(&node)
	}
	return node
}

func probeCooldownCalls(f *fakeQuarantiner) []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint64(nil), f.probeCooldown...)
}

func rotationQueueLength(s *Service) int {
	s.rotation.mu.Lock()
	defer s.rotation.mu.Unlock()
	return len(s.rotation.queue)
}

// 连续两次双族探活失败 → transport 冷却 + 轮换入队; 尝试计数按新事件重置。
func TestProbeDeadTwoObservationsMarkAndRotate(t *testing.T) {
	withProbeDeadConfirmDelay(t, time.Hour)
	node := probeDeadTestNode(t, 7, func(n *domain.Node) { n.RotationAttempts = 3 })
	service, repo, _, quarantiner := newProbeDeadTestService(t, node, deadProbeResult())
	if _, err := service.TestNode(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if calls := probeCooldownCalls(quarantiner); len(calls) != 0 {
		t.Fatalf("single observation must not mark: %v", calls)
	}
	if _, err := service.TestNode(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if calls := probeCooldownCalls(quarantiner); len(calls) != 1 || calls[0] != 7 {
		t.Fatalf("confirmed dead exit not cooled: %v", calls)
	}
	repo.mu.Lock()
	attempts := repo.node.RotationAttempts
	repo.mu.Unlock()
	if attempts != 0 {
		t.Fatalf("fresh episode must reset attempts, got %d", attempts)
	}
	if queued := rotationQueueLength(service); queued != 1 {
		t.Fatalf("rotation queue length = %d, want 1", queued)
	}
}

// 抖动序列(失败-成功-失败)不标记; 只有再次连续两次失败才标记一次。
func TestProbeDeadFlakySequenceDoesNotMarkEarly(t *testing.T) {
	withProbeDeadConfirmDelay(t, time.Hour)
	node := probeDeadTestNode(t, 8, nil)
	repo := &rotationStubRepo{node: node}
	prober := &switchableProber{result: deadProbeResult()}
	quarantiner := &fakeQuarantiner{}
	service := &Service{
		repository: repo, cipher: newRotationCipher(t), qualityQuarantiner: quarantiner,
		qualityGuard: DefaultQualityGuardConfig(), rotationCfg: fastRotationConfig(),
	}
	service.operations = repo
	service.SetNodeProber(prober)
	service.SetRotationConfig(fastRotationConfig())
	ctx := context.Background()
	_, _ = service.TestNode(ctx, 8)
	if calls := probeCooldownCalls(quarantiner); len(calls) != 0 {
		t.Fatalf("first failure must not mark: %v", calls)
	}
	prober.set(aliveProbeResult())
	_, _ = service.TestNode(ctx, 8)
	prober.set(deadProbeResult())
	_, _ = service.TestNode(ctx, 8)
	if calls := probeCooldownCalls(quarantiner); len(calls) != 0 {
		t.Fatalf("healthy observation must reset the counter: %v", calls)
	}
	_, _ = service.TestNode(ctx, 8)
	if calls := probeCooldownCalls(quarantiner); len(calls) != 1 || calls[0] != 8 {
		t.Fatalf("second consecutive failure must mark exactly once: %v", calls)
	}
}

// 第一次失败后自动补测: 两次失败无需人工重试即完成确认。
func TestProbeDeadConfirmationReprobeMarks(t *testing.T) {
	withProbeDeadConfirmDelay(t, 5*time.Millisecond)
	node := probeDeadTestNode(t, 9, nil)
	service, _, prober, quarantiner := newProbeDeadTestService(t, node, deadProbeResult())
	if _, err := service.TestNode(context.Background(), 9); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(probeCooldownCalls(quarantiner)) == 1 {
			if prober.calls < 2 {
				t.Fatalf("confirmation re-probe did not run: calls=%d", prober.calls)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("confirmation re-probe never marked the node")
}

// 质量隔离中的节点归质量闭环管理, 不施加 transport 冷却。
func TestProbeDeadSkipsQualityQuarantined(t *testing.T) {
	withProbeDeadConfirmDelay(t, time.Hour)
	until := time.Now().UTC().Add(time.Hour)
	node := probeDeadTestNode(t, 10, func(n *domain.Node) {
		n.CooldownUntil = &until
		n.LastError = domain.LastErrorExitIPQuality
	})
	service, _, _, quarantiner := newProbeDeadTestService(t, node, deadProbeResult())
	_, _ = service.TestNode(context.Background(), 10)
	_, _ = service.TestNode(context.Background(), 10)
	if calls := probeCooldownCalls(quarantiner); len(calls) != 0 {
		t.Fatalf("quality-quarantined node must be skipped: %v", calls)
	}
}

// 代理池模式节点豁免(与调度豁免口径一致)。
func TestProbeDeadSkipsProxyPoolNode(t *testing.T) {
	withProbeDeadConfirmDelay(t, time.Hour)
	node := probeDeadTestNode(t, 11, func(n *domain.Node) { n.ProxyPool = true })
	service, _, _, quarantiner := newProbeDeadTestService(t, node, deadProbeResult())
	_, _ = service.TestNode(context.Background(), 11)
	_, _ = service.TestNode(context.Background(), 11)
	if calls := probeCooldownCalls(quarantiner); len(calls) != 0 {
		t.Fatalf("proxy-pool node must be skipped: %v", calls)
	}
}
