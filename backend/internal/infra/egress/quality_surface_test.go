package egress

import (
	"context"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// qualityStateRepo 实现窄写面(UpdateEgressNodeHealth),记录调用供断言。
type qualityStateRepo struct {
	egressRepositoryTestStub
	node    domain.Node
	updates []domain.Node
}

func (r *qualityStateRepo) GetEgressNode(_ context.Context, _ uint64) (domain.Node, error) {
	return r.node, nil
}

func (r *qualityStateRepo) ListEgressNodes(_ context.Context, _ repository.SortQuery) ([]domain.Node, error) {
	return []domain.Node{r.node}, nil
}

func (r *qualityStateRepo) UpdateEgressNodeHealth(_ context.Context, _ uint64, health float64, failureCount int, cooldownUntil *time.Time, lastError string) error {
	r.node.Health = health
	r.node.FailureCount = failureCount
	r.node.CooldownUntil = cooldownUntil
	r.node.LastError = lastError
	r.updates = append(r.updates, r.node)
	return nil
}

func (r *qualityStateRepo) UpdateEgressNodeClearance(context.Context, uint64, string, string, string, string, time.Time) error {
	return nil
}

func (r *qualityStateRepo) UpdateEgressNodeLastError(_ context.Context, _ uint64, lastError string) error {
	r.node.LastError = lastError
	return nil
}

// 真实 Manager 的暂定冷却/解除隔离面此前 0%(应用层轮换测试用的是 fake)。
// 锁定契约:非质量错误保留(属传输层)、质量错误清除、degrade_count 是累计
// 历史(UI 徽章过去时文案"已被归因 N 次"),释放不重置。
func TestQualityReleaseAndCooldownManagerContracts(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	newCase := func(lastError string, degrade int) (*Manager, *qualityStateRepo) {
		repo := &qualityStateRepo{}
		repo.node = domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 0.05, FailureCount: 5, LastError: lastError, DegradeCount: degrade}
		return NewManager(repo, cipher), repo
	}

	// (1) 解除隔离:质量错误清除、健康/失败/冷却复位、degrade 计数保留。
	mgr, repo := newCase(domain.LastErrorExitIPQuality, 4)
	if err := mgr.ReleaseQualityQuarantine(ctx, 3); err != nil {
		t.Fatalf("release: %v", err)
	}
	if repo.node.LastError != "" || repo.node.Health != 1 || repo.node.FailureCount != 0 || repo.node.CooldownUntil != nil {
		t.Fatalf("release state = %+v", repo.node)
	}
	if repo.node.DegradeCount != 4 {
		t.Fatalf("degrade_count is cumulative history, release must not reset: %d", repo.node.DegradeCount)
	}

	// (2) 解除隔离保留非质量错误(属传输层)。
	mgr2, repo2 := newCase("dial timeout", 1)
	if err := mgr2.ReleaseQualityQuarantine(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if repo2.node.LastError != "dial timeout" {
		t.Fatalf("transport error wiped by quality release: %q", repo2.node.LastError)
	}

	// (3) 暂定冷却:质量错误清除但冷却落位(短冷却回池,被动守卫兜底)。
	mgr3, repo3 := newCase(domain.LastErrorExitIPQuality, 2)
	cooldownUntil := time.Now().UTC().Add(30 * time.Minute)
	if err := mgr3.CooldownNodeForQuality(ctx, 3, cooldownUntil); err != nil {
		t.Fatal(err)
	}
	if repo3.node.LastError != "" || repo3.node.Health != 1 || repo3.node.FailureCount != 0 {
		t.Fatalf("cooldown state = %+v", repo3.node)
	}
	if repo3.node.CooldownUntil == nil || !repo3.node.CooldownUntil.After(time.Now().UTC().Add(25*time.Minute)) {
		t.Fatalf("tentative cooldown not installed: %+v", repo3.node.CooldownUntil)
	}
}
