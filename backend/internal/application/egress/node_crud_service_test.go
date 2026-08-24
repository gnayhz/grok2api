package egress

import (
	"context"
	"errors"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// 节点 CRUD 服务层覆盖补齐：Create/Update/List/ListAll/ProxyURL/RotationURL/
// Delete/DeleteMany/UpdateManyEnabled/RefreshClearance 在本包内此前 0%——
// handler 测试用 stub 仓储、持久层测试绕过 Service，节点资源自身的服务
// 路径（校验、404 归一、reveal 语义、固定路由目标守卫、批量语义、
// Clearance 生命周期钩子）从未被整体锁定。本组测试用真实 SQLite 走完整
// 服务路径。
func ptrStr(value string) *string { return &value }

func ptrBool(value bool) *bool { return &value }

type fakeClearanceManager struct {
	refreshed []uint64
	forgotten []uint64
}

func (f *fakeClearanceManager) RefreshClearance(ctx context.Context, id uint64) error {
	f.refreshed = append(f.refreshed, id)
	return nil
}

func (f *fakeClearanceManager) ForgetClearance(id uint64) {
	f.forgotten = append(f.forgotten, id)
}

// P1 回归锁:GORM 曾对 enabled 列的 default:true 标签在 INSERT 时把显式
// false 静默替换为 true——三种资源(节点/池/订阅源)的“创建即停用”全部
// 被复活。语义后果:未验证代理立即承流、停用池参与调度(回退链终止语义
// 被破坏)、暂停的源被维护循环立即拉取。修复:移除三个模型 enabled 列的
// default 标签(models.go),GORM 不再替换零值。
func TestDisabledCreateStaysDisabled(t *testing.T) {
	ctx, service, _ := newPoolServiceFixture(t)

	// 节点:创建即停用
	node, err := service.Create(ctx, Input{Name: "staged-node", Enabled: false})
	if err != nil {
		t.Fatalf("create disabled node: %v", err)
	}
	if node.Enabled {
		t.Fatal("node created disabled came back enabled")
	}

	// 池:创建即停用
	pool, err := service.CreatePool(ctx, PoolInput{Name: "staged-pool", Strategy: "random", FallbackMode: "none", Enabled: ptrBool(false)})
	if err != nil {
		t.Fatalf("create disabled pool: %v", err)
	}
	if pool.Enabled {
		t.Fatal("pool created disabled came back enabled")
	}

	// 订阅源:创建即停用
	url := "https://subscription.example/disabled-feed"
	source, err := service.CreateSource(ctx, SubscriptionSourceInput{Name: "staged-source", Enabled: false, URL: &url})
	if err != nil {
		t.Fatalf("create disabled source: %v", err)
	}
	if source.Enabled {
		t.Fatal("source created disabled came back enabled")
	}

	// 列表投影同样必须保持停用语义
	nodes, _, err := service.List(ctx, 1, 10, "staged", ListFilter{Enabled: "disabled"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Name != "staged-node" {
		t.Fatalf("disabled filter = %+v", nodes)
	}
}

func TestNodeCRUDServicePath(t *testing.T) {
	ctx, service, repo := newPoolServiceFixture(t)

	proxy := "socks5://127.0.0.1:52883"

	// --- Create: 合法节点(带代理) ---
	created, err := service.Create(ctx, Input{Name: "alpha-node", Enabled: true, ProxyURL: &proxy})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if created.ID == 0 || !created.ProxyConfigured || created.ProxyDisplay == "" || created.ProxyFingerprint == "" {
		t.Fatalf("created node projection = %+v", created)
	}

	// --- Create: 输入校验 ---
	if _, err := service.Create(ctx, Input{Name: "", Enabled: true}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty name = %v, want ErrInvalidInput", err)
	}
	longName := make([]byte, 161)
	for i := range longName {
		longName[i] = 'n'
	}
	if _, err := service.Create(ctx, Input{Name: string(longName), Enabled: true}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("161-char name = %v, want ErrInvalidInput", err)
	}
	if _, err := service.Create(ctx, Input{Name: "pool-no-proxy", Enabled: true, ProxyPool: ptrBool(true)}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("pool mode without proxy = %v, want ErrInvalidInput", err)
	}
	badScheme := "ftp://127.0.0.1:21"
	if _, err := service.Create(ctx, Input{Name: "bad-scheme", Enabled: true, ProxyURL: &badScheme}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad proxy scheme = %v, want ErrInvalidInput", err)
	}

	// --- Create: 直连节点(无代理) + 第二个代理节点 ---
	direct, err := service.Create(ctx, Input{Name: "direct-node", Enabled: true})
	if err != nil {
		t.Fatalf("create direct node: %v", err)
	}
	if direct.ProxyConfigured {
		t.Fatal("node without proxy input must not report configured proxy")
	}
	beta, err := service.Create(ctx, Input{Name: "beta-node", Enabled: false, ProxyURL: &proxy})
	if err != nil {
		t.Fatalf("create beta: %v", err)
	}

	// --- List: 过滤/排序校验 ---
	if _, _, err := service.List(ctx, 1, 10, "", ListFilter{Enabled: "bogus"}); !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("bogus enabled filter = %v, want ErrInvalidFilter", err)
	}
	if _, _, err := service.List(ctx, 1, 10, "", ListFilter{ProbeStatus: "bogus"}); !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("bogus probe filter = %v, want ErrInvalidFilter", err)
	}
	if _, _, err := service.List(ctx, 1, 10, "", ListFilter{Sort: repository.SortQuery{Field: "evil", Direction: repository.SortAscending}}); !errors.Is(err, ErrInvalidSort) {
		t.Fatalf("evil sort field = %v, want ErrInvalidSort", err)
	}

	// --- List: 分页 + 搜索 + enabled 过滤 ---
	items, total, err := service.List(ctx, 1, 2, "node", ListFilter{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if total != 3 || len(items) != 2 {
		t.Fatalf("paged list = %d items total=%d, want 2/3", len(items), total)
	}
	enabledOnly, total, err := service.List(ctx, 1, 10, "", ListFilter{Enabled: "enabled"})
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if total != 2 || len(enabledOnly) != 2 {
		t.Fatalf("enabled filter total=%d items=%d, want 2/2", total, len(enabledOnly))
	}
	sorted, _, err := service.List(ctx, 1, 10, "", ListFilter{Sort: repository.SortQuery{Field: "name", Direction: repository.SortAscending}})
	if err != nil {
		t.Fatalf("sorted list: %v", err)
	}
	if len(sorted) != 3 || sorted[0].Name != "alpha-node" || sorted[2].Name != "direct-node" {
		t.Fatalf("name-sorted = %s,%s,%s", sorted[0].Name, sorted[1].Name, sorted[2].Name)
	}

	// --- ListAll ---
	all, err := service.ListAll(ctx, repository.SortQuery{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListAll = %d, want 3", len(all))
	}
	if _, err := service.ListAll(ctx, repository.SortQuery{Field: "evil", Direction: repository.SortAscending}); !errors.Is(err, ErrInvalidSort) {
		t.Fatalf("ListAll evil sort = %v, want ErrInvalidSort", err)
	}

	// --- ProxyURL reveal: 往返一致 / 未配置拒绝 / 缺失 404 ---
	revealed, err := service.ProxyURL(ctx, created.ID)
	if err != nil {
		t.Fatalf("reveal proxy: %v", err)
	}
	if revealed != proxy {
		t.Fatalf("revealed proxy = %q, want %q", revealed, proxy)
	}
	if _, err := service.ProxyURL(ctx, direct.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unconfigured reveal = %v, want ErrInvalidInput", err)
	}
	if _, err := service.ProxyURL(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing reveal = %v, want ErrNotFound", err)
	}

	// --- RotationURL: 配置 → reveal 往返; 未配置拒绝; 缺失 404 ---
	webhook := "http://127.0.0.1:52884/rotate?token=t"
	if _, err := service.Update(ctx, created.ID, Input{Name: "alpha-node", Enabled: true, RotationURL: &webhook}); err != nil {
		t.Fatalf("set rotation url: %v", err)
	}
	rotationRevealed, err := service.RotationURL(ctx, created.ID)
	if err != nil {
		t.Fatalf("reveal rotation: %v", err)
	}
	if rotationRevealed != webhook {
		t.Fatalf("revealed rotation = %q, want %q", rotationRevealed, webhook)
	}
	if _, err := service.RotationURL(ctx, beta.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unconfigured rotation = %v, want ErrInvalidInput", err)
	}
	if _, err := service.RotationURL(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing rotation = %v, want ErrNotFound", err)
	}

	// --- Update: 缺失 404 / 正常改名 ---
	if _, err := service.Update(ctx, 9999, Input{Name: "ghost", Enabled: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing = %v, want ErrNotFound", err)
	}
	renamed, err := service.Update(ctx, created.ID, Input{Name: "alpha-renamed", Enabled: true})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.Name != "alpha-renamed" {
		t.Fatalf("renamed = %q", renamed.Name)
	}

	// --- 固定路由目标守卫: 总出口指向 alpha 后, 破坏性编辑必须拒绝 ---
	if _, err := service.UpdateOperationsConfig(ctx, OperationsConfigInput{
		ProbeIntervalSeconds: 900,
		DefaultTarget:        &RoutingTargetInput{Mode: domain.RoutingTargetNode, NodeID: created.ID},
	}); err != nil {
		t.Fatalf("set default target: %v", err)
	}
	if _, err := service.Update(ctx, created.ID, Input{Name: "alpha-renamed", Enabled: false}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("disable fixed target = %v, want ErrInvalidInput", err)
	}
	if _, err := service.Update(ctx, created.ID, Input{Name: "alpha-renamed", Enabled: true, ClearProxyURL: true}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("clear proxy on fixed target = %v, want ErrInvalidInput", err)
	}
	accountTemplate := "socks5h://Default.{account}:token@resin:2260"
	if _, err := service.Update(ctx, created.ID, Input{Name: "alpha-renamed", Enabled: true, ProxyURL: &accountTemplate}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("account template on fixed target = %v, want ErrInvalidInput", err)
	}
	// 纯改名不受影响
	if _, err := service.Update(ctx, created.ID, Input{Name: "alpha-still", Enabled: true}); err != nil {
		t.Fatalf("rename on fixed target: %v", err)
	}

	// --- UpdateManyEnabled: 空列表拒绝 / 固定目标禁用拒绝 / 正常启用 ---
	if _, err := service.UpdateManyEnabled(ctx, nil, true); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty batch = %v, want ErrInvalidInput", err)
	}
	if _, err := service.UpdateManyEnabled(ctx, []uint64{created.ID}, false); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("batch disable fixed target = %v, want ErrInvalidInput", err)
	}
	enabled, err := service.UpdateManyEnabled(ctx, []uint64{beta.ID, beta.ID, 0, 9999}, true)
	if err != nil {
		t.Fatalf("batch enable: %v", err)
	}
	if enabled != 1 {
		t.Fatalf("batch enable count = %d, want 1 (dedup, zero-skip, missing-skip)", enabled)
	}

	// --- RefreshClearance: 缺失 404 优先 / 无管理器不可用 / 有管理器转发 ---
	if err := service.RefreshClearance(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refresh missing = %v, want ErrNotFound", err)
	}
	if err := service.RefreshClearance(ctx, created.ID); !errors.Is(err, ErrClearanceUnavailable) {
		t.Fatalf("refresh without manager = %v, want ErrClearanceUnavailable", err)
	}
	manager := &fakeClearanceManager{}
	service.SetClearanceManager(manager)
	if err := service.RefreshClearance(ctx, created.ID); err != nil {
		t.Fatalf("refresh with manager: %v", err)
	}
	if len(manager.refreshed) != 1 || manager.refreshed[0] != created.ID {
		t.Fatalf("manager refreshed = %v, want [%d]", manager.refreshed, created.ID)
	}

	// --- Delete / DeleteMany: 计数正确, 删除触发 Clearance 遗忘 ---
	if err := service.Delete(ctx, beta.ID); err != nil {
		t.Fatalf("delete beta: %v", err)
	}
	if err := service.Delete(ctx, beta.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}
	if len(manager.forgotten) != 1 || manager.forgotten[0] != beta.ID {
		t.Fatalf("delete forgot clearance = %v, want [%d]", manager.forgotten, beta.ID)
	}
	deleted, err := service.DeleteMany(ctx, []uint64{direct.ID, direct.ID, 9999})
	if err != nil {
		t.Fatalf("delete many: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("delete many count = %d, want 1", deleted)
	}
	remaining, err := repo.ListEgressNodes(ctx, repository.SortQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != created.ID {
		t.Fatalf("remaining nodes = %+v, want only fixed target", remaining)
	}
}
