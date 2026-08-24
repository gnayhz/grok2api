package egress

import (
	"context"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// 多副本语义(与启动警告 deployment_topology_guard_state_replica_local 一致):
// 跨账号降智证据窗口是进程内状态。同一节点的降智观测分散到两个副本时,
// 每个副本各自只见 1 个账号——阈值 2 不会被任何单副本凑满, 节点不隔离。
// 这正是运维按单副本语义配置 threshold=2 在多副本下得到"阈值实际放大"
// 的具体形态; 全部观测落在单副本时才按配置阈值隔离。
func TestCrossAccountEvidenceIsReplicaLocal(t *testing.T) {
	repo := &qualityStubRepo{nodes: map[uint64]domain.Node{
		1: {ID: 1, Name: "shared-node", Enabled: true, Health: 1},
	}}
	// 两个"副本":各自独立的证据窗口, 共享同一仓储与隔离器(等价共享 DB)。
	quarantiner := &fakeQuarantiner{}
	replicaA := &Service{repository: repo, qualityQuarantiner: quarantiner, qualityGuard: DefaultQualityGuardConfig(), qualityEvidence: map[uint64][]degradeObservation{}}
	replicaB := &Service{repository: repo, qualityQuarantiner: quarantiner, qualityGuard: DefaultQualityGuardConfig(), qualityEvidence: map[uint64][]degradeObservation{}}

	// 账号 100 在副本 A 降智, 账号 200 在副本 B 降智:两副本各见 1 个账号。
	replicaA.OnEgressDegraded(context.Background(), 1, 100)
	replicaB.OnEgressDegraded(context.Background(), 1, 200)
	time.Sleep(100 * time.Millisecond)
	quarantiner.mu.Lock()
	quarantined := len(quarantiner.quarantine)
	quarantiner.mu.Unlock()
	if quarantined != 0 {
		t.Fatalf("evidence split across replicas must not confirm on either replica (amplified threshold), quarantine=%d", quarantined)
	}

	// 对照:第三个账号落在副本 A, 副本 A 凑满 2 个不同账号 → 隔离。
	replicaA.OnEgressDegraded(context.Background(), 1, 300)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		quarantiner.mu.Lock()
		quarantined = len(quarantiner.quarantine)
		quarantiner.mu.Unlock()
		if quarantined > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if quarantined != 1 {
		t.Fatalf("single replica reaching threshold must quarantine: %d", quarantined)
	}
}

// L2 软冷却的解除(RSC 判账号有罪)同样只在观测副本内生效:副本 A 解除,
// 副本 B 若独立标记过同节点仍保持自己的软冷却——副本间互不补偿。
func TestSoftCooldownClearIsReplicaScoped(t *testing.T) {
	repo := &qualityStubRepo{nodes: map[uint64]domain.Node{
		1: {ID: 1, Name: "shared-node", Enabled: true, Health: 1},
	}}
	quarantiner := &fakeQuarantiner{}
	replicaA := &Service{repository: repo, qualityQuarantiner: quarantiner, qualityGuard: DefaultQualityGuardConfig(), qualityEvidence: map[uint64][]degradeObservation{}}
	replicaB := &Service{repository: repo, qualityQuarantiner: quarantiner, qualityGuard: DefaultQualityGuardConfig(), qualityEvidence: map[uint64][]degradeObservation{}}

	// fakeQuarantiner 不带管理器, 这里直接验证服务层簿记互不影响:
	// A 记录观测并整体清空自己的窗口, B 的窗口保持独立。
	replicaA.OnEgressDegraded(context.Background(), 1, 100)
	replicaA.RemoveAccountEvidence(1, 100)
	windowB := func() []degradeObservation {
		replicaB.qualityMu.Lock()
		defer replicaB.qualityMu.Unlock()
		return append([]degradeObservation(nil), replicaB.qualityEvidence[1]...)
	}
	if len(windowB()) != 0 {
		t.Fatalf("replica B window must stay independent: %v", replicaB.qualityEvidence[1])
	}
	replicaB.OnEgressDegraded(context.Background(), 1, 200)
	windowA := func() []degradeObservation {
		replicaA.qualityMu.Lock()
		defer replicaA.qualityMu.Unlock()
		return append([]degradeObservation(nil), replicaA.qualityEvidence[1]...)
	}
	if len(windowA()) != 0 {
		t.Fatalf("replica A window must stay cleared: %v", windowA())
	}
	if len(windowB()) != 1 {
		t.Fatalf("replica B window should hold its own observation: %v", windowB())
	}
}
