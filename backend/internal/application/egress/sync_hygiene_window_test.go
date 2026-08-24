package egress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

// racingOperationsRepo 把“卫生检查读取配置快照 → 管理员并发提交 → 卫生检查
// 基于旧快照整行写回”这一丢失更新窗口压缩成确定性的单线程序列:第一次
// GetEgressOperationsConfig(即卫生检查的读取)在返回快照之前,先用真实
// 仓储提交一次管理员风格的配置修改(检测间隔 900 → 120)。
type racingOperationsRepo struct {
	*relational.EgressRepository
	raced atomic.Bool
}

func (r *racingOperationsRepo) GetEgressOperationsConfig(ctx context.Context) (domain.OperationsConfig, error) {
	snapshot, err := r.EgressRepository.GetEgressOperationsConfig(ctx)
	if err != nil {
		return domain.OperationsConfig{}, err
	}
	if !r.raced.Swap(true) {
		modified := snapshot
		modified.ProbeIntervalSeconds = 120
		modified.UpdatedAt = time.Now().UTC()
		if _, err := r.EgressRepository.SaveEgressOperationsConfig(ctx, modified); err != nil {
			return domain.OperationsConfig{}, err
		}
	}
	return snapshot, nil
}

func seedOperationsConfig(ctx context.Context, t *testing.T, repo *relational.EgressRepository, target domain.RoutingTarget) {
	t.Helper()
	if _, err := repo.SaveEgressOperationsConfig(ctx, domain.OperationsConfig{
		ProbeProvider: domain.ProbeProviderCloudflare, ProbeIntervalSeconds: 900,
		DefaultTarget: target, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func assertAdminConfigSaveSurvived(ctx context.Context, t *testing.T, repo *relational.EgressRepository) domain.OperationsConfig {
	t.Helper()
	final, err := repo.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if final.ProbeIntervalSeconds != 120 {
		t.Fatalf("concurrent admin config save was silently reverted by the sync hygiene writer: interval=%d, want 120", final.ProbeIntervalSeconds)
	}
	return final
}

// 回归锁(丢失更新, P2):订阅同步后的路由卫生检查曾对运营配置做无条件
// 整行写回——即使没有任何目标需要剥离。每次订阅同步都会打开一个
// [读取快照, 写回] 窗口,窗口内落库的管理员提交(检测间隔、探测提供方、
// 路由目标)被静默回滚。本测试在真实 SQLite 服务路径上复现该交错。
func TestSyncHygieneDoesNotRevertConcurrentAdminConfigSave(t *testing.T) {
	ctx, service, repo := newPoolServiceFixture(t)

	feed := atomic.Value{}
	feed.Store("http://feed-node.example:8080" + "\n")
	// 订阅 URL 必须是公网地址(SSRF 防护拒绝私网目标);真实抓取经由本地
	// 代理服务器完成——它对任意请求直接回订阅正文,与既有测试同构。
	proxy := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(feed.Load().(string)))
	}))
	t.Cleanup(proxy.Close)

	seedOperationsConfig(ctx, t, repo, domain.RoutingTarget{Mode: domain.RoutingTargetAuto})

	// 真实落库一条订阅源(upsert 按外键约束回指 source 行),再加密其
	// 拉取地址与拉取代理。
	created, err := repo.CreateEgressSource(ctx, domain.SubscriptionSource{
		Name: "race-source", Enabled: true, RefreshIntervalSeconds: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	cipher := newRotationCipher(t)
	encryptedProxyURL, err := cipher.Encrypt(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	encryptedURL, err := cipher.Encrypt("http://1.1.1.1/subscription")
	if err != nil {
		t.Fatal(err)
	}
	source := domain.SubscriptionSource{
		ID: created.ID, Name: created.Name, Enabled: true,
		EncryptedURL:      encryptedURL,
		EncryptedProxyURL: encryptedProxyURL, RefreshIntervalSeconds: 900,
	}

	racing := &racingOperationsRepo{EgressRepository: repo}
	if _, err := service.syncSource(ctx, racing, source); err != nil {
		t.Fatalf("sync source: %v", err)
	}

	assertAdminConfigSaveSurvived(ctx, t, repo)
}

// 回归锁(丢失更新 + 剥离语义, P2):卫生检查必须在剥离“固定目标已变成
// 账号模板节点”这类仓储层不可见的目标的同时,保留窗口内并发落库的管理
// 员配置修改。模板节点由服务层直接创建(创建路径接受账号模板),配置由
// 仓储直写构造(等价于模板校验存在前的历史遗留状态——这正是卫生检查
// 存在的理由)。
func TestSyncHygieneStripsTemplateTargetWithoutLosingAdminSave(t *testing.T) {
	ctx, service, repo := newPoolServiceFixture(t)

	node, err := service.Create(ctx, Input{
		Name: "template-node", Enabled: true,
		ProxyURL: ptrStr("http://user-{account}:pass@template.example:8080"),
	})
	if err != nil {
		t.Fatalf("create template node: %v", err)
	}
	seedOperationsConfig(ctx, t, repo, domain.RoutingTarget{Mode: domain.RoutingTargetNode, NodeID: node.ID})

	racing := &racingOperationsRepo{EgressRepository: repo}
	if err := service.enforceRoutingHygieneAfterSync(ctx, racing); err != nil {
		t.Fatalf("enforce routing hygiene: %v", err)
	}

	final := assertAdminConfigSaveSurvived(ctx, t, repo)
	if final.DefaultTarget.Mode.Normalized() == domain.RoutingTargetNode && final.DefaultTarget.NodeID == node.ID {
		t.Fatalf("template node target must be stripped by hygiene, got %+v", final.DefaultTarget)
	}
}
