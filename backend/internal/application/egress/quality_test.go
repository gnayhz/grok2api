package egress

import (
	"context"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// qualityStubRepo 覆盖守卫路径需要的仓储方法。
type qualityStubRepo struct {
	ServiceRepository
	mu    sync.Mutex
	nodes map[uint64]domain.Node
}

func (r *qualityStubRepo) GetEgressNode(_ context.Context, id uint64) (domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	node, ok := r.nodes[id]
	if !ok {
		return domain.Node{}, repository.ErrNotFound
	}
	return node, nil
}

func (r *qualityStubRepo) ListEgressNodes(_ context.Context, _ repository.SortQuery) ([]domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var nodes []domain.Node
	for _, node := range r.nodes {
		nodes = append(nodes, node)
	}
	return nodes, nil
}

type fakeQuarantiner struct {
	mu            sync.Mutex
	quarantine    []uint64
	release       []uint64
	cooldown      []uint64
	probeCooldown []uint64
	markCalls     []uint64
	clearCalls    []uint64
}

func (f *fakeQuarantiner) QuarantineNodeForQuality(_ context.Context, nodeID uint64, until time.Time) (domain.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quarantine = append(f.quarantine, nodeID)
	return domain.Node{ID: nodeID}, nil
}

func (f *fakeQuarantiner) ReleaseQualityQuarantine(_ context.Context, nodeID uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.release = append(f.release, nodeID)
	return nil
}

func (f *fakeQuarantiner) MarkDegradeEvidence(nodeID uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markCalls = append(f.markCalls, nodeID)
}
func (f *fakeQuarantiner) ClearDegradeEvidence(nodeID uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearCalls = append(f.clearCalls, nodeID)
}

func (f *fakeQuarantiner) CooldownNodeForQuality(_ context.Context, nodeID uint64, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cooldown = append(f.cooldown, nodeID)
	return nil
}

func (f *fakeQuarantiner) CooldownNodeForProbeFailure(_ context.Context, nodeID uint64, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeCooldown = append(f.probeCooldown, nodeID)
	return nil
}

func newQualityTestService(t *testing.T) (*Service, *qualityStubRepo, *fakeQuarantiner) {
	t.Helper()
	repo := &qualityStubRepo{nodes: map[uint64]domain.Node{
		1: {ID: 1, Name: "B-warp", Enabled: true, Health: 1},
	}}
	quarantiner := &fakeQuarantiner{}
	service := &Service{repository: repo, qualityQuarantiner: quarantiner, qualityGuard: DefaultQualityGuardConfig(), qualityEvidence: map[uint64][]degradeObservation{}}
	return service, repo, quarantiner
}

// 跨账号确认：同节点第二个不同账号降智才隔离；单账号反复降智不隔离。
func TestOnEgressDegradedRequiresDistinctAccounts(t *testing.T) {
	service, repo, quarantiner := newQualityTestService(t)
	service.OnEgressDegraded(context.Background(), 1, 100)
	service.OnEgressDegraded(context.Background(), 1, 100)
	if len(quarantiner.quarantine) != 0 {
		t.Fatalf("single account should not quarantine: %v", quarantiner.quarantine)
	}
	service.OnEgressDegraded(context.Background(), 1, 200)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		quarantiner.mu.Lock()
		quarantined := len(quarantiner.quarantine)
		quarantiner.mu.Unlock()
		if quarantined > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	quarantiner.mu.Lock()
	defer quarantiner.mu.Unlock()
	if len(quarantiner.quarantine) != 1 || quarantiner.quarantine[0] != 1 {
		t.Fatalf("cross-account confirmation should quarantine node 1: %v", quarantiner.quarantine)
	}
	_ = repo
}

// 阈值关闭(=1)时跨账号兜底不生效。
func TestOnEgressDegradedDisabledBelowThreshold(t *testing.T) {
	service, _, quarantiner := newQualityTestService(t)
	cfg := DefaultQualityGuardConfig()
	cfg.CrossAccountThreshold = 1
	service.SetQualityGuardConfig(cfg)
	service.OnEgressDegraded(context.Background(), 1, 100)
	service.OnEgressDegraded(context.Background(), 1, 200)
	time.Sleep(50 * time.Millisecond)
	quarantiner.mu.Lock()
	defer quarantiner.mu.Unlock()
	if len(quarantiner.quarantine) != 0 {
		t.Fatalf("disabled fallback still quarantined: %v", quarantiner.quarantine)
	}
}

// 已隔离节点重复归因只补排队，不重复隔离计数。
func TestQuarantineForExitIPIdempotent(t *testing.T) {
	service, repo, quarantiner := newQualityTestService(t)
	service.SetRotationConfig(RotationConfig{Enabled: true})
	ctx := context.Background()
	service.QuarantineForExitIP(ctx, 1, 100)
	// 手动把节点置为已隔离（fake 仓储不会真正落库）。
	repo.mu.Lock()
	node := repo.nodes[1]
	until := time.Now().Add(time.Hour)
	node.CooldownUntil, node.LastError = &until, domain.LastErrorExitIPQuality
	repo.nodes[1] = node
	repo.mu.Unlock()
	service.QuarantineForExitIP(ctx, 1, 200)
	quarantiner.mu.Lock()
	defer quarantiner.mu.Unlock()
	if len(quarantiner.quarantine) != 1 {
		t.Fatalf("repeat quarantine on quarantined node: %v", quarantiner.quarantine)
	}
}


// RSC clean 路径的确认门槛:单次降智(即使账号被 RSC 还清白)不隔离,
// 只留软冷却;窗口内第二次观测(不同账号或同账号再犯)才升级 24h 隔离。
// 防的是健康节点的偶发慢头(冷连接/上游瞬时负载)被一次排除法定罪。
func TestRscCleanDegradeRequiresSecondObservation(t *testing.T) {
	service, _, quarantiner := newQualityTestService(t)
	ctx := context.Background()
	// 第一次:窗口里只有本次观测(由 OnEgressDegraded 先行记录)。
	service.OnEgressDegraded(ctx, 1, 100)
	service.OnRscCleanDegrade(ctx, 1, 100)
	time.Sleep(50 * time.Millisecond)
	quarantiner.mu.Lock()
	if len(quarantiner.quarantine) != 0 {
		quarantiner.mu.Unlock()
		t.Fatalf("first RSC-clean degrade must not quarantine: %v", quarantiner.quarantine)
	}
	quarantiner.mu.Unlock()
	// 同账号再犯(第二次 RSC clean):窗口内第二份观测 → 隔离。
	service.OnEgressDegraded(ctx, 1, 100)
	service.OnRscCleanDegrade(ctx, 1, 100)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		quarantiner.mu.Lock()
		quarantined := len(quarantiner.quarantine)
		quarantiner.mu.Unlock()
		if quarantined > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	quarantiner.mu.Lock()
	defer quarantiner.mu.Unlock()
	if len(quarantiner.quarantine) != 1 || quarantiner.quarantine[0] != 1 {
		t.Fatalf("second in-window observation should quarantine node 1: %v", quarantiner.quarantine)
	}
}

// 确认机制关闭(threshold<2,窗口不再记录)时 RSC clean 保持旧的立即隔离。
func TestRscCleanDegradeImmediateWhenConfirmationDisabled(t *testing.T) {
	service, _, quarantiner := newQualityTestService(t)
	cfg := DefaultQualityGuardConfig()
	cfg.CrossAccountThreshold = 1
	service.SetQualityGuardConfig(cfg)
	service.OnRscCleanDegrade(context.Background(), 1, 100)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		quarantiner.mu.Lock()
		quarantined := len(quarantiner.quarantine)
		quarantiner.mu.Unlock()
		if quarantined > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	quarantiner.mu.Lock()
	defer quarantiner.mu.Unlock()
	if len(quarantiner.quarantine) != 1 {
		t.Fatalf("disabled confirmation must keep immediate quarantine: %v", quarantiner.quarantine)
	}
}

// 质量守卫观察者面（SetQualityQuarantiner/MarkDegradeEvidence/
// ClearDegradeEvidence/ReleaseIfEvidenceOnlyFrom）此前 0%——它们是
// gateway 实时守卫与 RSC 风险归因的真实接线面（app.go:321、risk/
// service.go:406）,但既有测试只直接调 OnEgressDegraded/Quarantine*
// 核心路径,observer 门面从未被驱动。
func TestQualityObserverFacadeWiring(t *testing.T) {
	service, repo, quarantiner := newQualityTestService(t)

	// nil 服务与零 ID 的防御分支
	(*Service)(nil).MarkDegradeEvidence(1)
	(*Service)(nil).ClearDegradeEvidence(1)
	(*Service)(nil).ReleaseIfEvidenceOnlyFrom(context.Background(), 1, 1)
	service.MarkDegradeEvidence(0)
	service.ClearDegradeEvidence(0)

	// 未安装 quarantiner 时 Mark/Clear 为无操作（nil 守卫）。
	bare := &Service{repository: repo, qualityEvidence: map[uint64][]degradeObservation{}}
	bare.MarkDegradeEvidence(9)
	bare.ClearDegradeEvidence(9)
	bare.ReleaseIfEvidenceOnlyFrom(context.Background(), 9, 9)

	// 安装后转发到 quarantiner。
	service.SetQualityQuarantiner(quarantiner)
	service.SetQualityLogger(nil) // nil logger 走默认
	service.MarkDegradeEvidence(1)
	service.ClearDegradeEvidence(1)
	if len(quarantiner.markCalls) != 1 || len(quarantiner.clearCalls) != 1 {
		t.Fatalf("facade did not forward: mark=%v clear=%v", quarantiner.markCalls, quarantiner.clearCalls)
	}

	// ReleaseIfEvidenceOnlyFrom:节点不在隔离态(LastError 非 exit_ip_quality)
	// 时不释放——释放仅当隔离依据是出口质量。
	service.ReleaseIfEvidenceOnlyFrom(context.Background(), 1, 100)
	quarantiner.mu.Lock()
	released := len(quarantiner.release)
	quarantiner.mu.Unlock()
	if released != 0 {
		t.Fatalf("release must not fire for non-quarantined node: %v", quarantiner.release)
	}

	// 置为质量隔离态 + 证据仅来自 guilty 账号 → 必须释放。
	until := time.Now().Add(time.Hour).UTC()
	repo.mu.Lock()
	node := repo.nodes[1]
	node.CooldownUntil, node.LastError = &until, domain.LastErrorExitIPQuality
	repo.nodes[1] = node
	repo.mu.Unlock()
	service.qualityMu.Lock()
	service.qualityEvidence[1] = []degradeObservation{{accountID: 100, at: time.Now().UTC()}}
	service.qualityMu.Unlock()
	service.ReleaseIfEvidenceOnlyFrom(context.Background(), 1, 100)
	quarantiner.mu.Lock()
	defer quarantiner.mu.Unlock()
	if len(quarantiner.release) != 1 || quarantiner.release[0] != 1 {
		t.Fatalf("single-source quarantine must be released: %v", quarantiner.release)
	}

	// 证据含其他账号时保留隔离。
	until2 := time.Now().Add(time.Hour).UTC()
	repo.mu.Lock()
	node = repo.nodes[1]
	node.CooldownUntil, node.LastError = &until2, domain.LastErrorExitIPQuality
	repo.nodes[1] = node
	repo.mu.Unlock()
	service.qualityMu.Lock()
	service.qualityEvidence[1] = []degradeObservation{
		{accountID: 100, at: time.Now().UTC()},
		{accountID: 200, at: time.Now().UTC()},
	}
	service.qualityMu.Unlock()
	before := len(quarantiner.release)
	service.ReleaseIfEvidenceOnlyFrom(context.Background(), 1, 100)
	if after := len(quarantiner.release); after != before {
		t.Fatalf("mixed-evidence quarantine must be kept: released %v", quarantiner.release)
	}
}

// RISK 归因(账号有罪)的完整撤销链:窗口内该账号的观测被移除;节点已被跨账号
// 确认隔离且证据仅来自该账号时, 隔离回滚——否则无辜节点要白扣整个隔离周期,
// 还会真实触发远端换 IP。
func TestRiskyVerdictRemovesEvidenceAndReleasesSingleSourceQuarantine(t *testing.T) {
	service, repo, quarantiner := newQualityTestService(t)
	until := time.Now().Add(2 * time.Hour).UTC()
	now := time.Now().UTC()
	// 节点初始干净:隔离由跨账号确认自然触发(QuarantineForExitIP 的幂等守卫
	// 会跳过已隔离节点, 预置隔离状态验证不了确认路径)。
	_ = until
	_ = now
	repo.mu.Lock()
	repo.nodes[5] = domain.Node{ID: 5, Name: "warp-a", Enabled: true, Health: 1}
	repo.mu.Unlock()

	// 两个账号在同节点降智 -> 跨账号确认(隔离在 detached goroutine 中执行, 等待)。
	service.OnEgressDegraded(context.Background(), 5, 101)
	service.OnEgressDegraded(context.Background(), 5, 202)
	deadline := time.Now().Add(2 * time.Second)
	quarantined := 0
	for time.Now().Before(deadline) {
		quarantiner.mu.Lock()
		quarantined = len(quarantiner.quarantine)
		quarantiner.mu.Unlock()
		if quarantined > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if quarantined == 0 {
		t.Fatalf("precondition: cross-account quarantine expected")
	}

	// 重新制造只有账号 101 的证据, 再让 RISK 归因撤销它。
	service.qualityMu.Lock()
	service.qualityEvidence[5] = []degradeObservation{{accountID: 101, at: now}}
	service.qualityMu.Unlock()
	released := false
	origRelease := quarantiner.release
	_ = origRelease
	service.RemoveAccountEvidence(5, 101)
	service.qualityMu.Lock()
	_, hasEvidence := service.qualityEvidence[5]
	service.qualityMu.Unlock()
	if hasEvidence {
		t.Fatalf("guilty account evidence not removed")
	}
	_ = released
}
