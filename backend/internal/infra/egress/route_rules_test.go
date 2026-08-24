package egress

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type routeRuleRepository struct {
	egressRepositoryTestStub
	node   domain.Node
	config domain.OperationsConfig
}

func (r *routeRuleRepository) GetEgressNode(_ context.Context, id uint64) (domain.Node, error) {
	if r.node.ID == 0 || r.node.ID != id {
		return domain.Node{}, repository.ErrNotFound
	}
	return r.node, nil
}

func (r *routeRuleRepository) GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error) {
	return r.config, nil
}

func newRouteRuleTestManager(t *testing.T, node domain.Node, config domain.OperationsConfig) *Manager {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	if node.EncryptedProxyURL == "" {
		node.EncryptedProxyURL = mustEncryptRouteRuleProxy(t, cipher, "http://proxy.example:8080")
	}
	manager := NewManager(&routeRuleRepository{node: node, config: config}, cipher)
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		return &scriptedRequestClient{do: func(int, *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		}}, nil
	}
	return manager
}

func mustEncryptRouteRuleProxy(t *testing.T, cipher *security.Cipher, value string) string {
	t.Helper()
	encrypted, err := cipher.Encrypt(value)
	if err != nil {
		t.Fatal(err)
	}
	return encrypted
}

// 流量类别路由:billing 类固定节点,由 acquire 内部的 target 解析命中。
func TestAcquireUsesClassRoutingTargetNode(t *testing.T) {
	node := domain.Node{ID: 11, Name: "cheap", Enabled: true}
	config := domain.OperationsConfig{
		ClassTargets: map[domain.TrafficClass]domain.RoutingTarget{
			domain.TrafficClassBilling: {Mode: domain.RoutingTargetNode, NodeID: 11},
		},
	}
	manager := newRouteRuleTestManager(t, node, config)

	lease, err := manager.Acquire(WithTrafficClass(context.Background(), domain.TrafficClassBilling), domain.ScopeBuild, "acct")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 11 || lease.ProxyURL == "" {
		t.Fatalf("lease = node %d proxy %q", lease.NodeID, lease.ProxyURL)
	}
	if stat := findRoutingStat("class:billing", "node"); stat == nil || stat.Hit < 1 {
		t.Fatalf("class routing stat = %+v", stat)
	}
}

// 作用域路由:asset 作用域归并到父族(web),继承父族目标。
func TestAcquireScopeTargetInheritsAssetScopes(t *testing.T) {
	node := domain.Node{ID: 11, Name: "web-exit", Enabled: true}
	config := domain.OperationsConfig{
		ScopeTargets: map[domain.Scope]domain.RoutingTarget{
			domain.ScopeWeb: {Mode: domain.RoutingTargetNode, NodeID: 11},
		},
	}
	manager := newRouteRuleTestManager(t, node, config)

	for _, scope := range []domain.Scope{domain.ScopeWeb, domain.ScopeWebAsset} {
		lease, err := manager.Acquire(context.Background(), scope, "acct")
		if err != nil {
			t.Fatal(err)
		}
		if lease.NodeID != 11 {
			t.Fatalf("scope %q lease node = %d, want 11", scope, lease.NodeID)
		}
		lease.Release()
	}
}

// 固定节点目标=强绑定:目标不可用(不存在/停用/冷却/池成员节点)时请求
// 快速失败,绝不静默改道自动调度里的其它节点。需要容错应配置代理池。
func TestRoutingTargetNodeUnavailableFailsStrict(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	autoProxy, err := cipher.Encrypt("http://auto.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	cooldown := time.Now().UTC().Add(5 * time.Minute)
	variants := map[string]domain.Node{
		"missing node": {ID: 0},
		"disabled":     {ID: 11, Enabled: false},
		"no proxy":     {ID: 11, Enabled: true, EncryptedProxyURL: "-"},
		"proxy pool":   {ID: 11, Enabled: true, ProxyPool: true},
		"cooling down": {ID: 11, Enabled: true, CooldownUntil: &cooldown},
	}
	config := domain.OperationsConfig{
		DefaultTarget: domain.RoutingTarget{Mode: domain.RoutingTargetNode, NodeID: 11},
	}
	for name, node := range variants {
		fallback := domain.Node{ID: 22, Name: "auto", Enabled: true, Health: 1, EncryptedProxyURL: autoProxy}
		manager := newRouteRuleTestManager(t, node, config)
		manager.repository.(*routeRuleRepository).nodes = []domain.Node{fallback}
		ctx := WithTrafficClass(context.Background(), domain.TrafficClassBilling)
		lease, acquireErr := manager.Acquire(ctx, domain.ScopeBuild, "acct")
		if acquireErr == nil {
			lease.Release()
			t.Errorf("%s: strict binding must fail when the target is unavailable, got lease node %d", name, lease.NodeID)
			continue
		}
		if !errors.Is(acquireErr, ErrRoutingTargetUnavailable) {
			t.Errorf("%s: err = %v, want ErrRoutingTargetUnavailable", name, acquireErr)
		}
		// 目标配置在总出口层:统计归因到 default 行,而不是请求的流量类别。
		// (进程内计数器是全局的,不断言 class 行为零——前序用例可能已写入。)
		if stat := findRoutingStat("default", "node"); stat == nil || stat.Fallback < 1 {
			t.Errorf("%s: fallback outcome not recorded: %+v", name, stat)
		}
	}
}

// 读取 operations config 失败时请求失败(fail closed at config layer),
// 由外层重试决定是否降级。
func TestAcquireFailsWhenOperationsConfigUnreadable(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewManager(&failingRouteRuleConfigRepository{}, cipher)
	if _, _, err := fresh.AcquireIfConfigured(context.Background(), domain.ScopeBuild, "acct"); err == nil {
		t.Fatal("config read failure must surface as an error")
	}
}

type failingRouteRuleConfigRepository struct {
	egressRepositoryTestStub
}

func (r *failingRouteRuleConfigRepository) GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error) {
	return domain.OperationsConfig{}, errors.New("db down")
}

// The 1s target cache must serve repeated rule hits without extra DB reads
// and must stay per-manager so different managers never share entries.
func TestRoutingTargetNodeCacheIsPerManager(t *testing.T) {
	node := domain.Node{ID: 21, Name: "cached", Enabled: true}
	config := domain.OperationsConfig{
		DefaultTarget: domain.RoutingTarget{Mode: domain.RoutingTargetNode, NodeID: 21},
	}
	manager := newRouteRuleTestManager(t, node, config)

	lease, err := manager.Acquire(context.Background(), domain.ScopeBuild, "acct")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()

	manager.routeRuleNodeMu.Lock()
	cached, ok := manager.routeRuleNodeCache[21]
	manager.routeRuleNodeMu.Unlock()
	if !ok || cached.node.ID != 21 {
		t.Fatalf("target node was not cached: %+v ok=%v", cached, ok)
	}

	// A second manager with the same node ID must not see the first cache.
	other := newRouteRuleTestManager(t, node, config)
	other.routeRuleNodeMu.Lock()
	_, shared := other.routeRuleNodeCache[21]
	other.routeRuleNodeMu.Unlock()
	if shared {
		t.Fatal("route-rule target cache must be per-manager")
	}
}

func findRoutingStat(level, mode string) *RoutingStat {
	for _, stat := range RoutingStatsSnapshot() {
		if stat.Level == level && stat.Mode == mode {
			copied := stat
			return &copied
		}
	}
	return nil
}

// 降智守卫的请求内排除必须对固定节点目标生效:否则守卫对固定路由
// 配置完全失效。严格绑定语义下,排除即不可用——重试快速失败而不是
// 撞回同一坏出口,也不是改道其它出口。
func TestFixedTargetHonorsNodeExclusions(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	targetProxy, err := cipher.Encrypt("http://target.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	autoProxy, err := cipher.Encrypt("http://auto.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	target := domain.Node{ID: 31, Name: "fixed", Enabled: true, Health: 1, EncryptedProxyURL: targetProxy}
	fallback := domain.Node{ID: 32, Name: "auto", Enabled: true, Health: 1, EncryptedProxyURL: autoProxy}
	config := domain.OperationsConfig{
		DefaultTarget: domain.RoutingTarget{Mode: domain.RoutingTargetNode, NodeID: 31},
	}
	manager := newRouteRuleTestManager(t, target, config)
	manager.repository.(*routeRuleRepository).nodes = []domain.Node{target, fallback}

	// 正常路径:命中固定目标 31。
	lease, err := manager.Acquire(context.Background(), domain.ScopeBuild, "acct")
	if err != nil {
		t.Fatal(err)
	}
	if lease.NodeID != 31 {
		t.Fatalf("expected fixed target 31, got %d", lease.NodeID)
	}
	lease.Release()

	// 守卫排除 31 后:固定目标是强绑定,重试不得改道其它出口——账号出口
	// IP 的中途突变本身就是风险。必须以 ErrRoutingTargetUnavailable 快速
	// 失败,让操作者看到配置的出口已不适合服务。
	ctx := WithNodeExclusions(context.Background(), map[uint64]struct{}{31: {}})
	lease2, err := manager.Acquire(ctx, domain.ScopeBuild, "acct")
	if lease2 != nil {
		lease2.Release()
	}
	if !errors.Is(err, ErrRoutingTargetUnavailable) {
		t.Fatalf("excluded fixed target must fail strict, got lease=%v err=%v", lease2, err)
	}
}

type routeRuleDbErrorRepo struct {
	routeRuleRepository
	dbErr error
}

func (r *routeRuleDbErrorRepo) GetEgressNode(context.Context, uint64) (domain.Node, error) {
	return domain.Node{}, r.dbErr
}

// DB 抖动不得把固定目标静默降级为自动调度:GetEgressNode 的非 NotFound
// 错误必须按读失败语义上抛(请求失败并留 Warn 日志),而不是当作
// "节点不存在"退回自动调度让流量无声绕开配置的出口。
func TestFixedTargetDbErrorFailsInsteadOfSilentFallback(t *testing.T) {
	node := domain.Node{ID: 11, Name: "fixed", Enabled: true, Health: 1}
	config := domain.OperationsConfig{
		ClassTargets: map[domain.TrafficClass]domain.RoutingTarget{
			domain.TrafficClassBilling: {Mode: domain.RoutingTargetNode, NodeID: 11},
		},
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	node.EncryptedProxyURL = mustEncryptRouteRuleProxy(t, cipher, "http://proxy.example:8080")
	dbErr := errors.New("db temporarily unavailable")
	manager := NewManager(&routeRuleDbErrorRepo{routeRuleRepository{node: node, config: config}, dbErr}, cipher)

	lease, err := manager.Acquire(WithTrafficClass(context.Background(), domain.TrafficClassBilling), domain.ScopeBuild, "acct")
	if err == nil {
		if lease != nil {
			lease.Release()
		}
		t.Fatal("DB read failure must fail the request, not silently fall back to the automatic schedule")
	}
	if !errors.Is(err, dbErr) {
		t.Fatalf("error must wrap the DB failure: %v", err)
	}
	if lease != nil {
		t.Fatalf("no lease expected on failure, got %#v", lease)
	}
}
