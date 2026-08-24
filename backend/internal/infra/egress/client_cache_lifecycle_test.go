package egress

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// connCountingProxy 包装 httptest 代理, 统计存活 TCP 连接数:证明旧客户端
// 的空闲连接在缓存逐出时被真实关闭(CloseIdleConnections), 而不是统计上的
// "被丢弃"。
type connCountingProxy struct {
	server *httptest.Server
	open   atomic.Int64
}

func newConnCountingProxy(t *testing.T) *connCountingProxy {
	t.Helper()
	proxy := &connCountingProxy{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forward, forwardErr := http.NewRequestWithContext(r.Context(), r.Method, r.RequestURI, r.Body)
		if forwardErr != nil {
			http.Error(w, "bad target", http.StatusBadRequest)
			return
		}
		forward.Header = r.Header.Clone()
		response, doErr := http.DefaultTransport.RoundTrip(forward)
		if doErr != nil {
			http.Error(w, "forward failed", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
	}))
	proxy.server.Listener = &countingListener{Listener: listener, open: &proxy.open}
	proxy.server.Start()
	t.Cleanup(proxy.server.Close)
	return proxy
}

type countingListener struct {
	net.Listener
	open *atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.open.Add(1)
	return &countingConn{Conn: conn, open: l.open}, nil
}

type countingConn struct {
	net.Conn
	open *atomic.Int64
	once bool
}

func (c *countingConn) Close() error {
	if !c.once {
		c.once = true
		c.open.Add(-1)
	}
	return c.Conn.Close()
}

// 客户端缓存随代理 URL 编辑/超时热更新交替的生命周期:
//  1. URL 编辑后新流量立即走新代理(fingerprint 键控, 无需等待任何 TTL);
//  2. 旧客户端的空闲连接在缓存逐出(clientCacheIdleTTL 到期后的下一次
//     clientFor)时被真实关闭——旧代理的存活连接数归零;
//  3. 超时热更新 ×2 + 多次 URL 编辑交替后, 客户端缓存容量有界。
func TestClientCacheLifecycleUnderProxyURLEditAndHotUpdates(t *testing.T) {
	ctx := context.Background()
	cipher := testCipher(t)
	proxyA := newConnCountingProxy(t)
	proxyB := newConnCountingProxy(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(origin.Close)

	repo := &poolStubRepo{pool: map[uint64]domain.Pool{}, member: map[uint64][]domain.Node{}}
	node := domain.Node{ID: 7, Name: "edit-lifecycle", Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy(t, cipher, proxyA.server.URL)}
	repo.nodes = []domain.Node{node}
	manager := NewManager(repo, cipher)

	roundTrip := func() {
		t.Helper()
		lease, _, err := manager.AcquireIfConfigured(ctx, domain.ScopeBuild, "lifecycle")
		if err != nil || lease == nil {
			t.Fatalf("acquire: %v", err)
		}
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+"/x", nil)
		if requestErr != nil {
			lease.Release()
			t.Fatal(requestErr)
		}
		response, doErr := lease.Do(request)
		if doErr != nil {
			lease.Release()
			t.Fatalf("round trip: %v", doErr)
		}
		response.Body.Close()
		lease.Release()
	}

	// 预热:两次请求(keep-alive)在代理 A 上留下空闲连接。
	roundTrip()
	roundTrip()
	if open := proxyA.open.Load(); open == 0 {
		t.Fatalf("expected an idle connection on proxy A, open=%d", open)
	}

	// 1. 编辑代理 URL → 新流量立即走 B。
	repo.nodes = []domain.Node{node}
	repo.nodes[0].EncryptedProxyURL = encryptedProxy(t, cipher, proxyB.server.URL)
	manager.invalidateNodes()
	roundTrip()
	if open := proxyB.open.Load(); open == 0 {
		t.Fatal("traffic did not move to the edited proxy URL immediately")
	}

	// 2. 旧客户端逐出时真实关闭旧代理连接:把旧键的 lastUsed 回拨超过
	//    idle TTL, 再触发一次 clientFor(任意获取)。
	manager.clientMu.Lock()
	aged := time.Now().UTC().Add(-clientCacheIdleTTL - time.Minute)
	for key, cached := range manager.clients {
		// 只回拨指纹属于代理 A 的旧条目:B 的条目 lastUsed 刚更新。
		if key.nodeID == node.ID && key.fingerprint != "" && cached.lastUsed.Before(time.Now().UTC().Add(-time.Second)) {
			cached.lastUsed = aged
			manager.clients[key] = cached
		}
	}
	manager.lastClientCleanup = time.Time{} // 强制下一轮执行清理
	manager.clientMu.Unlock()
	roundTrip() // 触发 cleanupClientCacheLocked → CloseIdleConnections
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && proxyA.open.Load() > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if open := proxyA.open.Load(); open != 0 {
		t.Fatalf("stale client eviction must close old proxy connections: proxy A open=%d", open)
	}

	// 3. 超时热更新 ×2 与 URL 编辑交替 → 客户端缓存容量有界。
	manager.UpdateBuildResponseHeaderTimeout(7 * time.Second)
	manager.UpdateBuildResponseHeaderTimeout(11 * time.Second)
	roundTrip()
	manager.UpdateBuildResponseHeaderTimeout(9 * time.Second)
	repo.nodes[0].EncryptedProxyURL = encryptedProxy(t, cipher, proxyA.server.URL)
	manager.invalidateNodes()
	roundTrip()
	manager.clientMu.Lock()
	size := len(manager.clients)
	manager.clientMu.Unlock()
	if size > 8 {
		t.Fatalf("client cache grew unbounded across edits/hot-updates: %d entries", size)
	}
}
