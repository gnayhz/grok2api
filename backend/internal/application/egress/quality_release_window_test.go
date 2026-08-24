package egress

import (
	"context"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func quarantineNodeForTest(repo *qualityStubRepo, id uint64) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	until := time.Now().Add(2 * time.Hour).UTC()
	node := repo.nodes[id]
	node.CooldownUntil, node.LastError = &until, domain.LastErrorExitIPQuality
	repo.nodes[id] = node
}

func releaseCount(t *testing.T, quarantiner *fakeQuarantiner) int {
	t.Helper()
	quarantiner.mu.Lock()
	defer quarantiner.mu.Unlock()
	return len(quarantiner.release)
}

// 回归锁(P2,空窗口空洞释放):节点处于出口质量隔离但跨账号证据窗口为空
// (两种常态:确认隔离时证据被刻意删除;30m 窗口在 24h 隔离内自然过期)时,
// 单个 RISK 判决不得释放隔离——空集上的"证据全部来自有罪账号"是空洞真值,
// 该 RISK 只否定了该账号自己的降智贡献,并未否定跨账号确认的隔离依据。
// 旧实现空窗口直接释放,把两个账号确认过的坏 IP 节点放回调度。
func TestReleaseIfEvidenceOnlyFromKeepsQuarantineOnEmptyWindow(t *testing.T) {
	service, repo, quarantiner := newQualityTestService(t)
	quarantineNodeForTest(repo, 1)

	// 窗口为空:确认时已删除(OnEgressDegraded 确认分支)或已自然过期。
	service.qualityMu.Lock()
	delete(service.qualityEvidence, 1)
	service.qualityMu.Unlock()

	service.ReleaseIfEvidenceOnlyFrom(context.Background(), 1, 300)

	if got := releaseCount(t, quarantiner); got != 0 {
		t.Fatalf("empty evidence window must not release a quality quarantine (vacuous truth), released %d", got)
	}
}

// 正向:窗口确实只含有罪账号的观测时必须释放——归因已否定隔离的全部依据。
// (按 risk 服务的新调用序:释放检查先于观测移除,此处直接预置窗口。)
func TestReleaseIfEvidenceOnlyFromReleasesWhenSoleSource(t *testing.T) {
	service, repo, quarantiner := newQualityTestService(t)
	quarantineNodeForTest(repo, 1)

	service.qualityMu.Lock()
	service.qualityEvidence[1] = []degradeObservation{{accountID: 300, at: time.Now().UTC()}}
	service.qualityMu.Unlock()

	service.ReleaseIfEvidenceOnlyFrom(context.Background(), 1, 300)

	if got := releaseCount(t, quarantiner); got != 1 {
		t.Fatalf("sole-source evidence must release the quarantine, released %d", got)
	}
}

// 混合证据:仍含其他账号观测时保留隔离。
func TestReleaseIfEvidenceOnlyFromKeepsQuarantineOnMixedEvidence(t *testing.T) {
	service, repo, quarantiner := newQualityTestService(t)
	quarantineNodeForTest(repo, 1)

	service.qualityMu.Lock()
	service.qualityEvidence[1] = []degradeObservation{
		{accountID: 300, at: time.Now().UTC()},
		{accountID: 400, at: time.Now().UTC()},
	}
	service.qualityMu.Unlock()

	service.ReleaseIfEvidenceOnlyFrom(context.Background(), 1, 300)

	if got := releaseCount(t, quarantiner); got != 0 {
		t.Fatalf("mixed evidence must keep the quarantine, released %d", got)
	}
}

// risk 服务的完整序(RISK 判决):释放检查必须在观测移除之前看到有罪观测,
// 否则"仅含有罪账号"永远不可判定;移除照常清理残留观测。
func TestRiskSequenceReleaseBeforeRemoval(t *testing.T) {
	service, repo, quarantiner := newQualityTestService(t)
	quarantineNodeForTest(repo, 1)
	service.qualityMu.Lock()
	service.qualityEvidence[1] = []degradeObservation{{accountID: 300, at: time.Now().UTC()}}
	service.qualityMu.Unlock()

	// 新序:先释放检查,再移除观测。
	service.ReleaseIfEvidenceOnlyFrom(context.Background(), 1, 300)
	service.RemoveAccountEvidence(1, 300)

	if got := releaseCount(t, quarantiner); got != 1 {
		t.Fatalf("sole-source release must fire before evidence removal, released %d", got)
	}
	service.qualityMu.Lock()
	_, hasEvidence := service.qualityEvidence[1]
	service.qualityMu.Unlock()
	if hasEvidence {
		t.Fatal("guilty account observations must still be removed after the release check")
	}
}
