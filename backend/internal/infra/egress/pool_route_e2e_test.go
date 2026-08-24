package egress

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// e2eRepo 扮演真实持久层:健康更新会写回节点存储,后续读取可见
// (与真实 DB 的 UpdateEgressNodeHealth 语义一致,区别仅在内存)。
type e2eRepo struct {
	egressRepositoryTestStub
	mu     sync.Mutex
	config domain.OperationsConfig
	pools  map[uint64]domain.Pool
	health atomic.Int64
}

// nodes 经嵌入 stub 的字段存取,UpdateEgressNodeHealth 原地改写该字段,
// 模拟真实 DB 写读一致。

func (r *e2eRepo) GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error) {
	return r.config, nil
}

func (r *e2eRepo) ListEgressNodes(context.Context, repository.SortQuery) ([]domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Node(nil), r.egressRepositoryTestStub.nodes...), nil
}

func (r *e2eRepo) GetEgressNode(_ context.Context, id uint64) (domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, node := range r.egressRepositoryTestStub.nodes {
		if node.ID == id {
			return node, nil
		}
	}
	return domain.Node{}, repository.ErrNotFound
}

func (r *e2eRepo) GetEgressPool(_ context.Context, id uint64) (domain.Pool, error) {
	if pool, ok := r.pools[id]; ok {
		return pool, nil
	}
	return domain.Pool{}, repository.ErrNotFound
}

func (r *e2eRepo) ListEgressNodesByPool(context.Context, uint64) ([]domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Node(nil), r.egressRepositoryTestStub.nodes...), nil
}

func (r *e2eRepo) UpdateEgressNodeHealth(_ context.Context, id uint64, health float64, failures int, cooldown *time.Time, lastErr string) error {
	r.health.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	for index, node := range r.egressRepositoryTestStub.nodes {
		if node.ID == id {
			node.Health, node.FailureCount, node.CooldownUntil, node.LastError = health, failures, cooldown, lastErr
			r.egressRepositoryTestStub.nodes[index] = node
		}
	}
	return nil
}

// newForwardProxy 启动一个真实的 HTTP 正向代理(绝对形式请求转发),
// 并统计经它出站的请求数——这是"请求确实经由该节点出站"的物理证据。
func newForwardProxy(t *testing.T, origin *httptest.Server, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		forward, err := http.NewRequestWithContext(r.Context(), r.Method, r.RequestURI, r.Body)
		if err != nil {
			http.Error(w, "proxy: bad target", http.StatusBadRequest)
			return
		}
		forward.Header = r.Header.Clone()
		response, err := client.Do(forward)
		if err != nil {
			http.Error(w, "proxy: upstream failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		io.Copy(w, response.Body)
	}))
}

// 端到端(非 mock):真实请求从业务入口(Acquire + WithTrafficClass)出发,
// 经 路由解析(类别规则→池) → 池调度(affinity 选成员) → 真实 HTTP 出站
// (客户端 → 本地真实代理 → 真实源站),由代理与源站的命中计数证明
// 请求确实经由所选节点发出,且同 affinity 稳定落同一节点。
func TestE2EPoolRouteRealProxyRoundTrip(t *testing.T) {
	var originHits, proxyAHits, proxyBHits atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		fmt.Fprintf(w, "origin-ok %s", r.URL.Path)
	}))
	defer origin.Close()
	proxyA := newForwardProxy(t, origin, &proxyAHits)
	defer proxyA.Close()
	proxyB := newForwardProxy(t, origin, &proxyBHits)
	defer proxyB.Close()

	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	nodeA := domain.Node{ID: 10, Name: "exit-a", Enabled: true, Health: 1, PoolIDs: []uint64{1}}
	nodeB := domain.Node{ID: 20, Name: "exit-b", Enabled: true, Health: 1, PoolIDs: []uint64{1}}
	nodeA.EncryptedProxyURL = encryptedProxy(t, cipher, proxyA.URL)
	nodeB.EncryptedProxyURL = encryptedProxy(t, cipher, proxyB.URL)
	config := domain.DefaultOperationsConfig()
	config.ClassTargets = map[domain.TrafficClass]domain.RoutingTarget{
		domain.TrafficClassInference: {Mode: domain.RoutingTargetPool, PoolID: 1},
	}
	repo := &e2eRepo{
		egressRepositoryTestStub: egressRepositoryTestStub{nodes: []domain.Node{nodeA, nodeB}},
		config:                   config,
		pools:                    map[uint64]domain.Pool{1: {ID: 1, Enabled: true, Strategy: domain.PoolStrategyAffinity, FallbackMode: domain.PoolFallbackNone}},
	}
	manager := NewManager(repo, cipher)

	first, err := manager.Acquire(WithTrafficClass(context.Background(), domain.TrafficClassInference), domain.ScopeBuild, "e2e-account")
	if err != nil || first == nil {
		t.Fatalf("acquire: lease=%v err=%v", first, err)
	}
	response, err := first.Do(newE2ERequest(origin.URL + "/echo"))
	if err != nil {
		first.Release()
		t.Fatalf("real round trip through pool member failed: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	first.Release()
	if string(body) != "origin-ok /echo" {
		t.Fatalf("body = %q, want origin marker", body)
	}
	if originHits.Load() != 1 {
		t.Fatalf("origin hits = %d, want 1", originHits.Load())
	}
	viaA, viaB := proxyAHits.Load(), proxyBHits.Load()
	if viaA+viaB != 1 {
		t.Fatalf("proxy hits A=%d B=%d, want exactly one exit to serve", viaA, viaB)
	}
	if (viaA == 1) != (first.NodeID == 10) || (viaB == 1) != (first.NodeID == 20) {
		t.Fatalf("served exit (A=%d,B=%d) does not match selected node %d", viaA, viaB, first.NodeID)
	}

	// 同 affinity 必须稳定落同一节点并再次真实出站。
	second, err := manager.Acquire(WithTrafficClass(context.Background(), domain.TrafficClassInference), domain.ScopeBuild, "e2e-account")
	if err != nil || second == nil {
		t.Fatalf("second acquire: %v", err)
	}
	response2, err := second.Do(newE2ERequest(origin.URL + "/again"))
	if err != nil {
		second.Release()
		t.Fatalf("second round trip failed: %v", err)
	}
	response2.Body.Close()
	second.Release()
	if second.NodeID != first.NodeID {
		t.Fatalf("affinity stability broken: %d then %d", first.NodeID, second.NodeID)
	}
	if proxyAHits.Load()+proxyBHits.Load() != 2 {
		t.Fatalf("expected both requests to traverse real proxies, A=%d B=%d", proxyAHits.Load(), proxyBHits.Load())
	}

	// 故障注入:两个真实出口端点全部下线 → 请求必须以真实网络错误失败,
	// 反馈把节点写入硬冷却;此后路由按显式策略失败,绝不静默直连。
	proxyA.Close()
	proxyB.Close()
	for _, affinity := range []string{"e2e-account", "e2e-account-b"} {
		lease, err := manager.Acquire(WithTrafficClass(context.Background(), domain.TrafficClassInference), domain.ScopeBuild, affinity)
		if err != nil || lease == nil {
			t.Fatalf("fault acquire(%s): %v", affinity, err)
		}
		_, dialErr := lease.Do(newE2ERequest(origin.URL + "/dead"))
		lease.Release()
		if dialErr == nil {
			t.Fatal("request to a dead exit must fail with a real network error")
		}
		manager.FeedbackForScope(context.Background(), domain.ScopeBuild, lease.NodeID, 0, dialErr)
	}
	manager.InvalidatePoolCache()
	if repo.health.Load() != 2 {
		t.Fatalf("health updates persisted = %d, want 2", repo.health.Load())
	}
	_, configured, err := manager.AcquireIfConfigured(WithTrafficClass(context.Background(), domain.TrafficClassInference), domain.ScopeBuild, "e2e-account")
	if err == nil {
		t.Fatal("all exits down: routing must fail explicitly, not hand out a lease")
	}
	if !configured {
		t.Fatal("pool target is configured; exhaustion must surface as an explicit error, not as unconfigured")
	}
	// allowDirect=true 的调用方同样不得拿到直连:节点存在但全部冷却,
	// 显式失败优先于未经授权的直连降级。
	if _, err := manager.Acquire(WithTrafficClass(context.Background(), domain.TrafficClassInference), domain.ScopeBuild, "e2e-account"); err == nil {
		t.Fatal("allowDirect caller must also fail while every exit is cooling down")
	}
}

func newE2ERequest(target string) *http.Request {
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		panic(err)
	}
	return request
}

func (r *e2eRepo) UpdateEgressNodeClearance(context.Context, uint64, string, string, string, string, time.Time) error {
	return nil
}

func (r *e2eRepo) UpdateEgressNodeLastError(_ context.Context, id uint64, lastErr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index, node := range r.egressRepositoryTestStub.nodes {
		if node.ID == id {
			node.LastError = lastErr
			r.egressRepositoryTestStub.nodes[index] = node
		}
	}
	return nil
}
