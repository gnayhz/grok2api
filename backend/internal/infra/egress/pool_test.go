package egress

import (
	"context"
	"errors"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// poolStubRepo 覆盖池化选路所需的仓储面。
type poolStubRepo struct {
	egressRepositoryTestStub
	pool   map[uint64]domain.Pool
	member map[uint64][]domain.Node
}

func newPoolStubRepo() *poolStubRepo {
	return &poolStubRepo{pool: map[uint64]domain.Pool{}, member: map[uint64][]domain.Node{}}
}

func (r *poolStubRepo) GetEgressPool(_ context.Context, id uint64) (domain.Pool, error) {
	if pool, ok := r.pool[id]; ok {
		return pool, nil
	}
	return domain.Pool{}, repository.ErrNotFound
}

func (r *poolStubRepo) ListEgressNodesByPool(_ context.Context, poolID uint64) ([]domain.Node, error) {
	return append([]domain.Node(nil), r.member[poolID]...), nil
}

func encryptedProxy(t *testing.T, cipher security.Cryptor, value string) string {
	t.Helper()
	encrypted, err := cipher.Encrypt(value)
	if err != nil {
		t.Fatal(err)
	}
	return encrypted
}

func newPoolTestManager(t *testing.T) (*Manager, *poolStubRepo) {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := newPoolStubRepo()
	return NewManager(repo, cipher), repo
}

// affinity(默认,rendezvous):同账号稳定落同一节点;节点移除只扰动落在它上的账号。
func TestPoolSelectionAffinityStability(t *testing.T) {
	manager, repo := newPoolTestManager(t)
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyAffinity, FallbackMode: domain.PoolFallbackNone}
	for i := uint64(1); i <= 5; i++ {
		repo.member[1] = append(repo.member[1], domain.Node{ID: i, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy(t, manager.cipher, "http://127.0.0.1:900"+string(rune('0'+i)))})
	}
	// 记录 20 个账号的落点
	placement := map[uint64]uint64{}
	for account := 0; account < 20; account++ {
		affinity := string(rune('A' + account))
		node := manager.selectPoolNode(repo.pool[1], repo.member[1], repo.member[1], affinity)
		placement[uint64(account)] = node.ID
	}
	// 移除节点 3:只有落在 3 上的账号应变化
	remaining := make([]domain.Node, 0, 4)
	for _, node := range repo.member[1] {
		if node.ID != 3 {
			remaining = append(remaining, node)
		}
	}
	moved, kept := 0, 0
	for account := 0; account < 20; account++ {
		affinity := string(rune('A' + account))
		node := manager.selectPoolNode(repo.pool[1], remaining, remaining, affinity)
		if placement[uint64(account)] == 3 {
			if node.ID == 3 {
				t.Fatalf("removed node still selected")
			}
			moved++
		} else if node.ID != placement[uint64(account)] {
			t.Fatalf("account %d reshuffled unnecessarily: %d -> %d", account, placement[uint64(account)], node.ID)
		} else {
			kept++
		}
	}
	if moved == 0 {
		t.Fatalf("no account was placed on removed node; test placement invalid")
	}
	t.Logf("moved=%d kept=%d (minimal-reshuffle property)", moved, kept)
}

// 零值策略归一化为 affinity:存量池保持旧 rendezvous 行为。
func TestPoolStrategyZeroValueNormalizesToAffinity(t *testing.T) {
	manager, _ := newPoolTestManager(t)
	if domain.PoolStrategy("").Normalized() != domain.PoolStrategyAffinity {
		t.Fatalf("zero strategy = %q, want affinity", domain.PoolStrategy("").Normalized())
	}
	if domain.PoolStrategy("bogus").Normalized() != domain.PoolStrategyAffinity {
		t.Fatalf("bogus strategy = %q, want affinity", domain.PoolStrategy("bogus").Normalized())
	}
	nodes := []domain.Node{{ID: 1, Health: 1}, {ID: 2, Health: 1}}
	first := manager.selectPoolNode(domain.Pool{ID: 1, Enabled: true}, nodes, nodes, "account")
	second := manager.selectPoolNode(domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyAffinity}, nodes, nodes, "account")
	if first.ID != second.ID {
		t.Fatalf("zero-value strategy diverged from affinity: %d vs %d", first.ID, second.ID)
	}
}

// random:每次请求随机选成员,大样本下所有成员都会被覆盖。
func TestPoolStrategyRandomSpreadsAcrossMembers(t *testing.T) {
	manager, _ := newPoolTestManager(t)
	pool := domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyRandom}
	nodes := []domain.Node{{ID: 1, Health: 1}, {ID: 2, Health: 1}, {ID: 3, Health: 1}}
	seen := map[uint64]int{}
	const rounds = 300
	for range rounds {
		seen[manager.selectPoolNode(pool, nodes, nodes, "same-account").ID]++
	}
	if len(seen) != len(nodes) {
		t.Fatalf("random strategy covered %d/%d members: %v", len(seen), len(nodes), seen)
	}
	// 同一 affinity 也不该被钉死在一个成员上。
	if len(seen) == 1 {
		t.Fatalf("random strategy pinned one member: %v", seen)
	}
}

// sticky:恒定选择稳定顺序中的第一个可用成员;首成员下线后才迁移。
func TestPoolStrategyStickyPicksFirstSchedulableMember(t *testing.T) {
	manager, _ := newPoolTestManager(t)
	pool := domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategySticky}
	nodes := []domain.Node{{ID: 5, Health: 1}, {ID: 2, Health: 1}, {ID: 9, Health: 1}}
	for range 10 {
		for _, affinity := range []string{"a", "b", "c"} {
			if selected := manager.selectPoolNode(pool, nodes, nodes, affinity); selected.ID != 5 {
				t.Fatalf("sticky selected %d, want first member 5", selected.ID)
			}
		}
	}
	remaining := nodes[1:]
	for _, affinity := range []string{"a", "b", "c"} {
		if selected := manager.selectPoolNode(pool, remaining, remaining, affinity); selected.ID != 2 {
			t.Fatalf("sticky after removal selected %d, want member 2", selected.ID)
		}
	}
}

// 软冷却:证据触发→全池避开;重复证据指数递增;RISK 解除恢复。
func TestSoftCooldownLifecycle(t *testing.T) {
	manager, repo := newPoolTestManager(t)
	manager.SetDegradeEvidenceCooldowns(5*time.Minute, time.Hour)
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, FallbackMode: domain.PoolFallbackNone}
	repo.member[1] = []domain.Node{
		{ID: 1, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy(t, manager.cipher, "http://10.0.0.1:1")},
		{ID: 2, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy(t, manager.cipher, "http://10.0.0.2:2")},
	}
	affinity := "soft-test-account"
	if got := manager.selectPoolNode(repo.pool[1], repo.member[1], repo.member[1], affinity).ID; got != 1 && got != 2 {
		t.Fatalf("unexpected placement %d", got)
	}
	target := manager.selectPoolNode(repo.pool[1], repo.member[1], repo.member[1], affinity).ID

	manager.MarkDegradeEvidence(target)
	now := time.Now().UTC()
	if !manager.nodeSoftCooled(target, now) {
		t.Fatalf("soft cooldown not applied")
	}
	if manager.nodeSoftCooled(target%2+1, now) {
		t.Fatalf("soft cooldown leaked to the other node")
	}
	// 池内选路必须避开软冷却的固定节点
	candidates := manager.poolCandidates(context.Background(), repo.member[1], now)
	for _, node := range candidates {
		if node.ID == target {
			t.Fatalf("soft-cooled fixed node still a pool candidate")
		}
	}
	// 代理池模式成员豁免 L2 软冷却:旋转端点的单次降智不代表端点坏,
	// 只靠请求内排除(L1)兜底——否则小规模 resin 池会被一次证据迅速耗尽。
	rotating := domain.Node{ID: 7, Enabled: true, Health: 1, ProxyPool: true, RotationEnabled: true, EncryptedProxyURL: encryptedProxy(t, manager.cipher, "http://10.0.0.7:7")}
	manager.MarkDegradeEvidence(7)
	if !manager.nodeSoftCooled(7, now) {
		t.Fatalf("precondition: rotating member expected soft-cooled")
	}
	exempt := false
	for _, node := range manager.poolCandidates(context.Background(), []domain.Node{rotating}, now) {
		if node.ID == 7 {
			exempt = true
		}
	}
	if !exempt {
		t.Fatalf("proxy-pool member must be exempt from soft cooldown")
	}
	manager.ClearDegradeEvidence(7)
	// 指数递增:第二次证据冷却时长翻倍
	manager.MarkDegradeEvidence(target)
	manager.ClearDegradeEvidence(target)
	if manager.nodeSoftCooled(target, time.Now().UTC()) {
		t.Fatalf("soft cooldown not lifted after clear")
	}
}

// 池获取:成员过滤 + 回退链(pool→pool) + direct 回退。
func TestAcquirePoolRoutedFallbackChain(t *testing.T) {
	manager, repo := newPoolTestManager(t)
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: 2}
	repo.pool[2] = domain.Pool{ID: 2, Enabled: true, FallbackMode: domain.PoolFallbackNone}
	// 池1 成员全在硬隔离
	until := time.Now().Add(time.Hour)
	repo.member[1] = []domain.Node{{ID: 10, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy(t, manager.cipher, "http://10.0.0.10:10"), CooldownUntil: &until}}
	repo.member[2] = []domain.Node{{ID: 20, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy(t, manager.cipher, "http://10.0.0.20:20")}}

	lease, outcome, err := manager.AcquirePoolRouted(context.Background(), domain.ScopeBuild, "acct", 1, false, "")
	if err != nil || outcome == PoolRouteNone || lease == nil {
		t.Fatalf("fallback pool not used: outcome=%v err=%v", outcome, err)
	}
	if outcome != PoolRouteChainedPool {
		t.Fatalf("expected chained-pool outcome, got %v", outcome)
	}
	if lease.NodeID != 20 {
		t.Fatalf("expected fallback node 20, got %d", lease.NodeID)
	}
	lease.Release()

	// 池2 也耗尽 → 回退 direct 模式
	pool2 := repo.pool[2]
	pool2.FallbackMode = domain.PoolFallbackDirect
	repo.pool[2] = pool2
	repo.member[2] = nil
	manager.InvalidatePoolCache()
	lease2, outcome2, err2 := manager.AcquirePoolRouted(context.Background(), domain.ScopeBuild, "acct", 1, true, "")
	if err2 != nil || outcome2 == PoolRouteNone || lease2 == nil {
		t.Fatalf("direct fallback not applied: outcome=%v err=%v", outcome2, err2)
	}
	if outcome2 != PoolRouteDirect {
		t.Fatalf("expected direct outcome, got %v", outcome2)
	}
	lease2.Release()

	// allowDirect=false 时 direct 回退不可用
	manager.InvalidatePoolCache()
	_, outcome3, err3 := manager.AcquirePoolRouted(context.Background(), domain.ScopeBuild, "acct", 1, false, "")
	if err3 != nil || outcome3 != PoolRouteNone {
		t.Fatalf("direct fallback should be unavailable: outcome=%v err=%v", outcome3, err3)
	}
}

// 回退环检测:A→B→A 必须中止而非死循环。
func TestAcquirePoolRoutedCycleGuard(t *testing.T) {
	manager, repo := newPoolTestManager(t)
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: 2}
	repo.pool[2] = domain.Pool{ID: 2, Enabled: true, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: 1}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, outcome, _ := manager.AcquirePoolRouted(context.Background(), domain.ScopeBuild, "acct", 1, false, "")
		if outcome != PoolRouteNone {
			t.Errorf("cycle should not produce a lease")
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("fallback cycle caused infinite loop")
	}
}

// 目标池自身选出成员 = Member;链到别的池 = ChainedPool。统计口径依赖
// 该区分:链式回退是降级,不能计入目标池命中。
func TestAcquirePoolRoutedOutcomeClassification(t *testing.T) {
	manager, repo := newPoolTestManager(t)
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: 2, Strategy: domain.PoolStrategySticky}
	repo.pool[2] = domain.Pool{ID: 2, Enabled: true, FallbackMode: domain.PoolFallbackNone}
	repo.member[2] = []domain.Node{{ID: 20, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy(t, manager.cipher, "http://10.0.0.20:20")}}

	// 池 1 有成员:必须是 Member。
	repo.member[1] = []domain.Node{{ID: 10, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy(t, manager.cipher, "http://10.0.0.10:10")}}
	lease, outcome, err := manager.AcquirePoolRouted(context.Background(), domain.ScopeBuild, "acct", 1, true, "")
	if err != nil || outcome != PoolRouteMember || lease == nil {
		t.Fatalf("member selection: outcome=%v err=%v", outcome, err)
	}
	lease.Release()

	// 池 1 清空:链到池 2,必须是 ChainedPool 而非 Member。
	repo.member[1] = nil
	manager.InvalidatePoolCache()
	lease2, outcome2, err2 := manager.AcquirePoolRouted(context.Background(), domain.ScopeBuild, "acct", 1, true, "")
	if err2 != nil || outcome2 != PoolRouteChainedPool || lease2 == nil {
		t.Fatalf("chained selection: outcome=%v err=%v", outcome2, err2)
	}
	lease2.Release()
}

// 浏览器作用域走池时必须进入 Clearance 托管生命周期:池只是分组,
// Web/Console 的 FlareSolverr 需求不因经过池而消失。托管模式开启但
// FlareSolverr 未配置时,Web 走池必须报错(进入生命周期)而不是带着空
// cookie"成功";Build 作用域不涉及浏览器 Clearance,不受影响。
func TestPoolRoutePropagatesManagedClearance(t *testing.T) {
	manager, repo := newPoolTestManager(t)
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategySticky, FallbackMode: domain.PoolFallbackNone}
	repo.member[1] = []domain.Node{{ID: 10, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy(t, manager.cipher, "http://10.0.0.10:10")}}
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr"})

	if _, _, err := manager.AcquirePoolRouted(context.Background(), domain.ScopeWeb, "acct", 1, true, ""); err == nil {
		t.Fatal("managed clearance must engage for web traffic through a pool (missing solver must surface)")
	}
	// Build 作用域无浏览器 Clearance,必须正常取到成员。
	lease, outcome, err := manager.AcquirePoolRouted(context.Background(), domain.ScopeBuild, "acct", 1, true, "")
	if err != nil || outcome != PoolRouteMember || lease == nil {
		t.Fatalf("build via pool must not need clearance: outcome=%v err=%v", outcome, err)
	}
	lease.Release()

	// 手动模式(manual)不进入托管生命周期,Web 走池也不触发 Clearance。
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "manual"})
	manager.InvalidatePoolCache()
	if _, _, err := manager.AcquirePoolRouted(context.Background(), domain.ScopeWeb, "acct", 1, true, ""); err != nil {
		t.Fatalf("manual mode web via pool must not engage clearance: %v", err)
	}
}

type poolRoutingConfigRepo struct {
	poolStubRepo
	config domain.OperationsConfig
}

func (r *poolRoutingConfigRepo) GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error) {
	return r.config, nil
}

// 池的 direct 回退是降级而非主路由决策:经 acquire 走池时必须遵守
// 调用方的 allowDirect 契约。AcquireIfConfigured(allowDirect=false)在
// 池耗尽 + FallbackMode=direct 时不得拿到 manager 直连租约——那会绕过
// 调用方 fallback transport 的 HTTP_PROXY 语义;池目标是强绑定,耗尽且
// 无法在边界内回退时快速失败(ErrRoutingTargetUnavailable),绝不逃逸到
// 自动调度。allowDirect=true 的调用方仍拿池内直连回退租约。
func TestPoolRouteDirectFallbackHonorsAllowDirect(t *testing.T) {
	config := domain.DefaultOperationsConfig()
	config.DefaultTarget = domain.RoutingTarget{Mode: domain.RoutingTargetPool, PoolID: 1}
	repo := &poolRoutingConfigRepo{config: config}
	repo.pool = map[uint64]domain.Pool{}
	repo.member = map[uint64][]domain.Node{}
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, FallbackMode: domain.PoolFallbackDirect}
	// 池无成员,自动调度也无可用节点:唯一去向是回退决策。
	manager := NewManager(repo, nil)

	lease, outcome, err := manager.AcquirePoolRouted(context.Background(), domain.ScopeBuild, "acct", 1, false, "")
	if err != nil || outcome != PoolRouteNone || lease != nil {
		t.Fatalf("allowDirect=false must refuse pool-direct: outcome=%v lease=%v err=%v", outcome, lease, err)
	}

	acquired, configured, err := manager.AcquireIfConfigured(context.Background(), domain.ScopeBuild, "acct")
	if acquired != nil || !configured || !errors.Is(err, ErrRoutingTargetUnavailable) {
		t.Fatalf("exhausted pool target must fail strict without a lease: lease=%v configured=%v err=%v", acquired, configured, err)
	}

	// allowDirect=true 的调用方(如 AcquireCredential)仍应拿到回退直连租约。
	directLease, directOutcome, err := manager.AcquirePoolRouted(context.Background(), domain.ScopeBuild, "acct", 1, true, "")
	if err != nil || directOutcome != PoolRouteDirect || directLease == nil {
		t.Fatalf("allowDirect=true must keep direct fallback: outcome=%v lease=%v err=%v", directOutcome, directLease, err)
	}
	directLease.Release()
}

type poolCancelRepo struct {
	poolRoutingConfigRepo
}

func (r *poolCancelRepo) GetEgressPool(ctx context.Context, id uint64) (domain.Pool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Pool{}, err
	}
	return r.pool[id], nil
}

// 已取消的请求不得在池读失败后继续落入自动调度:那会为死请求租约
// 节点、抬高 inflight 并触发无意义的健康反馈。ctx 取消时 acquire
// 必须直接返回取消错误且不产生租约。
func TestAcquirePoolRouteCanceledContextStopsBeforeAutoSchedule(t *testing.T) {
	config := domain.DefaultOperationsConfig()
	config.DefaultTarget = domain.RoutingTarget{Mode: domain.RoutingTargetPool, PoolID: 1}
	repo := &poolCancelRepo{}
	repo.egressRepositoryTestStub.nodes = []domain.Node{{ID: 7, Name: "auto", Enabled: true, Health: 1}}
	repo.poolRoutingConfigRepo.config = config
	repo.pool = map[uint64]domain.Pool{}
	repo.member = map[uint64][]domain.Node{}
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, FallbackMode: domain.PoolFallbackNone}
	manager := NewManager(repo, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lease, _, err := manager.AcquireIfConfigured(ctx, domain.ScopeBuild, "acct")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire must return context.Canceled, got lease=%v err=%v", lease, err)
	}
	if lease != nil {
		lease.Release()
		t.Fatal("canceled acquire must not produce a lease from the automatic schedule")
	}
	// 对照:同配置下未取消的请求不再落入自动调度——空池且无回退是配置
	// 边界内的整体失效,严格失败并归因哨兵错误。
	strictLease, _, strictErr := manager.AcquireIfConfigured(context.Background(), domain.ScopeBuild, "acct")
	if strictLease != nil || !errors.Is(strictErr, ErrRoutingTargetUnavailable) {
		t.Fatalf("uncanceled acquire must fail strict on an exhausted pool: lease=%v err=%v", strictLease, strictErr)
	}
}

// 成员重写后,持久化簿记必须与游标一起重置:按旧成员序推进的滞留写协程
// 在写回循环里发现 state == nil 即退出,不会把旧游标重新登记进内存簿记。
func TestInvalidatePoolCacheResetsRotationPersistState(t *testing.T) {
	manager, repo := newPoolTestManager(t)
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, FallbackMode: domain.PoolFallbackNone}

	manager.persistRotationCursor(1, 10, 20)
	manager.rotationMu.Lock()
	if _, ok := manager.rotationPersists[1]; !ok {
		manager.rotationMu.Unlock()
		t.Fatal("precondition: persist state expected after persistRotationCursor")
	}
	manager.rotationMu.Unlock()

	manager.InvalidatePoolCache()
	manager.rotationMu.Lock()
	if len(manager.rotationCursors) != 0 || len(manager.rotationPersists) != 0 {
		manager.rotationMu.Unlock()
		t.Fatalf("rotation bookkeeping not reset: cursors=%d persists=%d", len(manager.rotationCursors), len(manager.rotationPersists))
	}
	manager.rotationMu.Unlock()

	// 模拟失效前已派生的滞留写协程:不得重新登记任何簿记。
	manager.writeRotationCursor(1, 10, 20)
	manager.rotationMu.Lock()
	defer manager.rotationMu.Unlock()
	if len(manager.rotationPersists) != 0 {
		t.Fatalf("stale writer resurrected persist state after invalidation: %d", len(manager.rotationPersists))
	}
}

// 配置冲突组合矩阵(现有覆盖之外):
//  1. 回退池已停用 → 链在此终止(PoolRouteNone), 即便 allowDirect=true
//     也不直连——停用是管理员显式动作, 不是可用性故障;
//  2. 池自引用回退(A→A) → 环守卫拦截(visited 初始即含目标池);
//  3. 三层链 A→B→C, 尾池健康 → 由 C 出成员, 记 ChainedPool;
//  4. 三层链尾池回退 direct × allowDirect=false → 无租约、绝不直连;
//     同链 × allowDirect=true → 直连租约(NodeID=0)。
func TestPoolFallbackChainConflictMatrix(t *testing.T) {
	ctx := context.Background()

	t.Run("disabled fallback pool terminates chain without direct", func(t *testing.T) {
		manager, repo := newPoolTestManager(t)
		repo.pool[1] = domain.Pool{ID: 1, Enabled: true, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: 2}
		repo.pool[2] = domain.Pool{ID: 2, Enabled: false, FallbackMode: domain.PoolFallbackDirect}
		lease, outcome, err := manager.AcquirePoolRouted(ctx, domain.ScopeBuild, "acct", 1, true, "")
		if err != nil || outcome != PoolRouteNone || lease != nil {
			t.Fatalf("disabled fallback pool: lease=%v outcome=%v err=%v, want no lease / none / nil (even with allowDirect=true)", lease, outcome, err)
		}
	})

	t.Run("self-referencing fallback is a cycle", func(t *testing.T) {
		manager, repo := newPoolTestManager(t)
		repo.pool[1] = domain.Pool{ID: 1, Enabled: true, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: 1}
		lease, outcome, err := manager.AcquirePoolRouted(ctx, domain.ScopeBuild, "acct", 1, true, "")
		if err != nil || outcome != PoolRouteNone || lease != nil {
			t.Fatalf("self-reference: lease=%v outcome=%v err=%v, want cycle guard / none", lease, outcome, err)
		}
	})

	t.Run("three-level chain serves from healthy tail", func(t *testing.T) {
		manager, repo := newPoolTestManager(t)
		repo.pool[1] = domain.Pool{ID: 1, Enabled: true, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: 2}
		repo.pool[2] = domain.Pool{ID: 2, Enabled: true, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: 3}
		repo.pool[3] = domain.Pool{ID: 3, Enabled: true, FallbackMode: domain.PoolFallbackNone}
		repo.member[3] = []domain.Node{{ID: 30, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy(t, manager.cipher, "http://10.0.0.30:30")}}
		lease, outcome, err := manager.AcquirePoolRouted(ctx, domain.ScopeBuild, "acct", 1, false, "")
		if err != nil || outcome != PoolRouteChainedPool || lease == nil || lease.NodeID != 30 {
			t.Fatalf("three-level chain: lease=%v outcome=%v err=%v, want node 30 via chained pool", lease, outcome, err)
		}
		lease.Release()
	})

	t.Run("chain tail direct fallback honors allowDirect", func(t *testing.T) {
		newChain := func(t *testing.T) (*Manager, *poolStubRepo) {
			manager, repo := newPoolTestManager(t)
			repo.pool[1] = domain.Pool{ID: 1, Enabled: true, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: 2}
			repo.pool[2] = domain.Pool{ID: 2, Enabled: true, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: 3}
			repo.pool[3] = domain.Pool{ID: 3, Enabled: true, FallbackMode: domain.PoolFallbackDirect}
			return manager, repo
		}
		blocked, _ := newChain(t)
		lease, outcome, err := blocked.AcquirePoolRouted(ctx, domain.ScopeBuild, "acct", 1, false, "")
		if err != nil || outcome != PoolRouteNone || lease != nil {
			t.Fatalf("allowDirect=false: lease=%v outcome=%v err=%v, want none (no unauthorized direct)", lease, outcome, err)
		}
		allowed, _ := newChain(t)
		lease, outcome, err = allowed.AcquirePoolRouted(ctx, domain.ScopeBuild, "acct", 1, true, "")
		if err != nil || outcome != PoolRouteDirect || lease == nil || lease.NodeID != 0 {
			t.Fatalf("allowDirect=true: lease=%v outcome=%v err=%v, want direct lease", lease, outcome, err)
		}
		lease.Release()
	})
}
