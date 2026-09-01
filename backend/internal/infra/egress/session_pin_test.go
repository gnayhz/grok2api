package egress

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// sessionPinRepo 支持在测试中改写可用节点集,模拟冷却/恢复/上下线。
type sessionPinRepo struct {
	egressRepositoryTestStub
	mu    sync.Mutex
	nodes []domain.Node
}

func (r *sessionPinRepo) ListEgressNodes(context.Context, repository.SortQuery) ([]domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Node(nil), r.nodes...), nil
}

func (r *sessionPinRepo) setNodes(nodes ...domain.Node) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes = nodes
}

func sessionTestNodes(ids ...uint64) []domain.Node {
	nodes := make([]domain.Node, 0, len(ids))
	for _, id := range ids {
		nodes = append(nodes, domain.Node{ID: id, Name: fmt.Sprintf("node-%d", id), Enabled: true, Health: 1})
	}
	return nodes
}

func TestWithBuildSessionDigestStableAndOpaque(t *testing.T) {
	base := context.Background()
	first := buildSessionFromContext(WithBuildSession(base, "conv-alpha"))
	second := buildSessionFromContext(WithBuildSession(context.Background(), "conv-alpha"))
	other := buildSessionFromContext(WithBuildSession(base, "conv-beta"))
	if first == "" || first != second {
		t.Fatalf("同一会话键摘要不稳定: %q vs %q", first, second)
	}
	if first == other {
		t.Fatalf("不同会话键生成了相同摘要")
	}
	if first == "conv-alpha" || len(first) != 24 {
		t.Fatalf("摘要应为 12 字节十六进制且不等于原始键: %q", first)
	}
	if got := buildSessionFromContext(WithBuildSession(base, "   ")); got != "" {
		t.Fatalf("空白会话键应视为无会话,得到 %q", got)
	}
	if got := buildSessionFromContext(base); got != "" {
		t.Fatalf("无会话上下文应返回空串,得到 %q", got)
	}
}

func TestSessionNodePinStabilizesAcrossAvailabilityChanges(t *testing.T) {
	repo := &sessionPinRepo{}
	repo.setNodes(sessionTestNodes(1, 2, 3)...)
	manager := NewManager(repo, nil)
	acquire := func() uint64 {
		t.Helper()
		lease, _, err := manager.AcquireIfConfigured(WithBuildSession(context.Background(), "sess-stable"), domain.ScopeBuild, "acct-1")
		if err != nil || lease == nil {
			t.Fatalf("获取失败: %v", err)
		}
		nodeID := lease.NodeID
		lease.Release()
		return nodeID
	}
	// 节点列表有进程内快照缓存,变更可用集后显式失效,模拟配置变更钩子。
	refresh := func(nodes ...domain.Node) {
		repo.setNodes(nodes...)
		manager.invalidateNodes()
	}

	pinned := acquire()

	// 可用集扩张(新节点上线)会改变账号哈希取模落点;会话必须钉在原节点。
	refresh(sessionTestNodes(1, 2, 3, 4, 5)...)
	if got := acquire(); got != pinned {
		t.Fatalf("可用集扩张后钉点漂移: %d -> %d", pinned, got)
	}

	// 钉点彻底消失(冷却/下线)时必须立即重钉到可用节点,而不是失败。
	remaining := make([]domain.Node, 0, 4)
	for _, node := range sessionTestNodes(1, 2, 3, 4, 5) {
		if node.ID != pinned {
			remaining = append(remaining, node)
		}
	}
	refresh(remaining...)
	repinned := acquire()
	if repinned == pinned {
		t.Fatalf("钉点节点已下线却仍被选中: %d", repinned)
	}

	// 原节点回归后不应"偷回"流量:钉扎保持稳定,避免再次无谓换出口。
	refresh(sessionTestNodes(1, 2, 3, 4, 5)...)
	if got := acquire(); got != repinned {
		t.Fatalf("原节点回归后钉点漂移: %d -> %d", repinned, got)
	}
}

func TestSessionNodePinHonorsNodeExclusions(t *testing.T) {
	repo := &sessionPinRepo{}
	repo.setNodes(sessionTestNodes(1, 2)...)
	manager := NewManager(repo, nil)
	sessionCtx := WithBuildSession(context.Background(), "sess-exclude")

	lease, _, err := manager.AcquireIfConfigured(sessionCtx, domain.ScopeBuild, "acct-1")
	if err != nil || lease == nil {
		t.Fatalf("首次获取失败: %v", err)
	}
	pinned := lease.NodeID
	lease.Release()

	excluded := map[uint64]struct{}{pinned: {}}
	retryCtx := WithNodeExclusions(WithBuildSession(context.Background(), "sess-exclude"), excluded)
	leaseRetry, _, err := manager.AcquireIfConfigured(retryCtx, domain.ScopeBuild, "acct-1")
	if err != nil || leaseRetry == nil {
		t.Fatalf("排除重试获取失败: %v", err)
	}
	defer leaseRetry.Release()
	if leaseRetry.NodeID == pinned {
		t.Fatalf("质量守卫排除了钉点节点 %d,重试仍落回同一节点", pinned)
	}
}

func TestSessionClientSeparatePoolPerSessionAndAccountIndependence(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	first, err := manager.clientForWithOptions(7, domain.ScopeBuild, "", "", "", false, "acct-A", clientOptions{sessionKey: "sess-1"})
	if err != nil {
		t.Fatalf("会话客户端创建失败: %v", err)
	}
	// 同一会话换账号:必须复用同一连接池(缓存跟连接走,不跟账号走)。
	afterSwitch, err := manager.clientForWithOptions(7, domain.ScopeBuild, "", "", "", false, "acct-B", clientOptions{sessionKey: "sess-1"})
	if err != nil {
		t.Fatalf("换号后获取失败: %v", err)
	}
	if first.client != afterSwitch.client {
		t.Fatalf("同会话换账号拿到了不同连接池,会话缓存会被无辜切断")
	}
	// 隔离开关翻转不得让会话客户端走失效重试循环。
	manager.UpdateAccountIsolatedConnections(true)
	afterToggle, err := manager.clientForWithOptions(7, domain.ScopeBuild, "", "", "", false, "acct-A", clientOptions{sessionKey: "sess-1"})
	if err != nil {
		t.Fatalf("隔离开启后获取失败: %v", err)
	}
	if first.client != afterToggle.client {
		t.Fatalf("隔离开关翻转切断了会话连接池")
	}
	// 不同会话必须拿到不同连接池。
	other, err := manager.clientForWithOptions(7, domain.ScopeBuild, "", "", "", false, "acct-A", clientOptions{sessionKey: "sess-2"})
	if err != nil {
		t.Fatalf("第二会话客户端创建失败: %v", err)
	}
	if first.client == other.client {
		t.Fatalf("两个会话共享了连接池,单连接钉扎失效")
	}
	sessions := 0
	manager.clientMu.Lock()
	for key := range manager.clients {
		if key.sessionKey == "sess-1" {
			sessions++
		}
	}
	manager.clientMu.Unlock()
	if sessions != 1 {
		t.Fatalf("sess-1 应只对应一个缓存条目,实际 %d", sessions)
	}
}

func TestSessionClientEvictionExemptions(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	if _, err := manager.clientForWithOptions(7, domain.ScopeBuild, "", "", "", false, "acct-A", clientOptions{}); err != nil {
		t.Fatalf("共享池创建失败: %v", err)
	}
	// 会话客户端的出现不得逐出同节点共享池。
	if _, err := manager.clientForWithOptions(7, domain.ScopeBuild, "", "", "", false, "acct-A", clientOptions{sessionKey: "sess-keep"}); err != nil {
		t.Fatalf("会话客户端创建失败: %v", err)
	}
	if !managerHasClientForKey(manager, clientCacheKey{nodeID: 7, scope: domain.ScopeBuild, fingerprint: sharedFingerprint(t, manager, 7)}) {
		t.Fatalf("会话客户端出现后共享池被逐出")
	}
	// 共享池的节点切换清理不得回收会话客户端。
	if _, err := manager.clientForWithOptions(7, domain.ScopeBuild, "", "", "", false, "acct-B", clientOptions{}); err != nil {
		t.Fatalf("第二共享池创建失败: %v", err)
	}
	if !managerHasClientForSession(manager, "sess-keep") {
		t.Fatalf("共享池切换节点时回收了会话客户端")
	}
}

// sharedFingerprint 找回共享池条目的指纹,避免测试重复实现键派生。
func sharedFingerprint(t *testing.T, manager *Manager, nodeID uint64) string {
	t.Helper()
	manager.clientMu.Lock()
	defer manager.clientMu.Unlock()
	for key := range manager.clients {
		if key.nodeID == nodeID && key.sessionKey == "" && key.accountIdentity == "" {
			return key.fingerprint
		}
	}
	t.Fatalf("节点 %d 没有共享池条目", nodeID)
	return ""
}

func managerHasClientForKey(manager *Manager, key clientCacheKey) bool {
	manager.clientMu.Lock()
	defer manager.clientMu.Unlock()
	_, ok := manager.clients[key]
	return ok
}

func managerHasClientForSession(manager *Manager, sessionKey string) bool {
	manager.clientMu.Lock()
	defer manager.clientMu.Unlock()
	for key := range manager.clients {
		if key.sessionKey == sessionKey {
			return true
		}
	}
	return false
}

type noopRequestClient struct{}

func (noopRequestClient) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(nil)}, nil
}

func (noopRequestClient) CloseIdleConnections() {}

func TestSessionClientCapacityBudgetSeparation(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	shared, err := manager.clientForWithOptions(7, domain.ScopeBuild, "", "", "", false, "acct-A", clientOptions{})
	if err != nil {
		t.Fatalf("共享池创建失败: %v", err)
	}
	sharedFp := sharedFingerprint(t, manager, 7)

	base := time.Now().UTC().Add(-time.Hour)
	manager.clientMu.Lock()
	for i := 0; i < maxSessionCachedClients; i++ {
		manager.clients[clientCacheKey{
			nodeID:      uint64(100 + i%8),
			scope:       domain.ScopeBuild,
			fingerprint: fmt.Sprintf("session-fp-%d", i),
			sessionKey:  fmt.Sprintf("sess-%d", i),
		}] = cachedClient{client: noopRequestClient{}, lastUsed: base.Add(time.Duration(i) * time.Second)}
	}
	stale := manager.ensureClientCacheCapacityLocked()
	manager.clientMu.Unlock()
	for _, client := range stale {
		client.CloseIdleConnections()
	}

	if !managerHasClientForKey(manager, clientCacheKey{nodeID: 7, scope: domain.ScopeBuild, fingerprint: sharedFp}) {
		t.Fatalf("会话条目满额时把共享池挤出了容量预算")
	}
	sessionCount := 0
	manager.clientMu.Lock()
	for key := range manager.clients {
		if key.sessionKey != "" {
			sessionCount++
		}
	}
	manager.clientMu.Unlock()
	if sessionCount != maxSessionCachedClients-1 {
		t.Fatalf("会话预算应淘汰到 %d 条,实际 %d", maxSessionCachedClients-1, sessionCount)
	}
	_ = shared
}

func TestLeaseFreshTunnelKeepsSessionConnection(t *testing.T) {
	newRequest := func() *http.Request {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://upstream.example/path", nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	plain := &Lease{Scope: domain.ScopeBuild, freshTunnel: true, client: noopRequestClient{}}
	reqPlain := newRequest()
	if _, err := plain.doRequest(reqPlain, false); err != nil {
		t.Fatalf("普通旋转池请求失败: %v", err)
	}
	if !reqPlain.Close {
		t.Fatalf("无会话的旋转池请求应强制新隧道(Connection close)")
	}

	pinned := &Lease{Scope: domain.ScopeBuild, freshTunnel: true, sessionKey: "sess-tunnel", client: noopRequestClient{}}
	reqSession := newRequest()
	if _, err := pinned.doRequest(reqSession, false); err != nil {
		t.Fatalf("会话请求失败: %v", err)
	}
	if reqSession.Close {
		t.Fatalf("会话钉扎的请求不应关闭连接:保连接即保提示缓存")
	}
}
