package egress

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// authRecordingProxy 是真实 HTTP 正向代理:记录每个请求的 Proxy-Authorization
// 用户名, 用于证明"渲染出的账号子身份真的到达了线路"。
type authRecordingProxy struct {
	server *httptest.Server
	mu     sync.Mutex
	users  []string
}

func newAuthRecordingProxy(t *testing.T) *authRecordingProxy {
	t.Helper()
	proxy := &authRecordingProxy{}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(origin.Close)
	client := &http.Client{Timeout: 5 * time.Second}
	proxy.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Proxy-Authorization"); strings.HasPrefix(auth, "Basic ") {
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
			if err == nil {
				if user, _, ok := strings.Cut(string(decoded), ":"); ok {
					proxy.mu.Lock()
					proxy.users = append(proxy.users, user)
					proxy.mu.Unlock()
				}
			}
		}
		forward, err := http.NewRequestWithContext(r.Context(), r.Method, r.RequestURI, r.Body)
		if err != nil {
			http.Error(w, "bad target", http.StatusBadRequest)
			return
		}
		forward.Header = r.Header.Clone()
		response, err := client.Do(forward)
		if err != nil {
			http.Error(w, "forward failed", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
	}))
	t.Cleanup(proxy.server.Close)
	return proxy
}

func (p *authRecordingProxy) usernames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.users...)
}

// 粘性 {account} 模板节点作为池成员的组合生命周期:
//  1. 同一账号多次获取 → 亲和稳定落同一成员, 渲染出的账号子身份出现在
//     租约 ProxyURL 与真实线路(代理 Basic 用户名)上;
//  2. 另一账号 → 自己的子身份, 与账号 77 的互不串扰;
//  3. 无账号身份且 affinity 为空 → 显式失败("粘性代理需要有效的账号
//     身份"), 不产生租约、不静默回退。
func TestStickyAccountTemplatePoolMemberLifecycle(t *testing.T) {
	ctx := context.Background()
	cipher := testCipher(t)
	proxyA := newAuthRecordingProxy(t)
	proxyB := newAuthRecordingProxy(t)
	templateA := "http://{account}:pw@" + strings.TrimPrefix(proxyA.server.URL, "http://")
	templateB := "http://{account}:pw@" + strings.TrimPrefix(proxyB.server.URL, "http://")

	repo := newPoolStubRepo()
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyAffinity, FallbackMode: domain.PoolFallbackNone}
	repo.member[1] = []domain.Node{
		{ID: 10, Name: "resin-a", Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy(t, cipher, templateA)},
		{ID: 20, Name: "resin-b", Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy(t, cipher, templateB)},
	}
	manager := NewManager(repo, cipher)
	repo.nodes = repo.member[1]

	acquire := func(accountCtx context.Context, affinity string) (*Lease, error) {
		lease, _, err := manager.AcquirePoolRouted(accountCtx, domain.ScopeBuild, affinity, 1, false, "")
		return lease, err
	}
	roundTrip := func(t *testing.T, lease *Lease) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://sticky-origin.example/probe", nil)
		if err != nil {
			lease.Release()
			t.Fatal(err)
		}
		response, err := lease.Do(request)
		if err != nil {
			lease.Release()
			t.Fatalf("round trip: %v", err)
		}
		response.Body.Close()
	}

	t.Run("same account stable with rendered identity on the wire", func(t *testing.T) {
		first, err := acquire(WithAccount(ctx, "grok_build", 77), "acct-77")
		if err != nil || first == nil {
			t.Fatalf("acquire: lease=%v err=%v", first, err)
		}
		roundTrip(t, first)
		firstIdentity := first.ProxyURL
		firstNode := first.NodeID
		first.Release()
		if !strings.Contains(firstIdentity, "grok_build77") {
			t.Fatalf("rendered proxy URL %q does not carry the account identity", firstIdentity)
		}
		for i := 0; i < 3; i++ {
			again, err := acquire(WithAccount(ctx, "grok_build", 77), "acct-77")
			if err != nil || again == nil {
				t.Fatalf("re-acquire %d: %v", i, err)
			}
			roundTrip(t, again)
			if again.NodeID != firstNode || again.ProxyURL != firstIdentity {
				t.Fatalf("affinity or rendering unstable: node %d->%d url %q->%q", firstNode, again.NodeID, firstIdentity, again.ProxyURL)
			}
			again.Release()
		}
		seenA, seenB := proxyA.usernames(), proxyB.usernames()
		total := len(seenA) + len(seenB)
		if total != 4 {
			t.Fatalf("expected 4 proxied round trips, saw %d (%v / %v)", total, seenA, seenB)
		}
		for _, user := range append(seenA, seenB...) {
			if user != "grok_build77" {
				t.Fatalf("wire identity %q != rendered grok_build77", user)
			}
		}
	})

	t.Run("second account isolated identity", func(t *testing.T) {
		lease, err := acquire(WithAccount(ctx, "grok_build", 88), "acct-88")
		if err != nil || lease == nil {
			t.Fatalf("acquire: %v", err)
		}
		roundTrip(t, lease)
		lease.Release()
		if !strings.Contains(lease.ProxyURL, "grok_build88") {
			t.Fatalf("second account rendered URL %q", lease.ProxyURL)
		}
		for _, user := range append(proxyA.usernames(), proxyB.usernames()...) {
			if user == "grok_build88" && !strings.Contains(lease.ProxyURL, "grok_build88") {
				t.Fatalf("identity bleed: %v", user)
			}
		}
		found := false
		for _, user := range append(proxyA.usernames(), proxyB.usernames()...) {
			if user == "grok_build88" {
				found = true
			}
		}
		if !found {
			t.Fatalf("second account identity never reached the wire: %v / %v", proxyA.usernames(), proxyB.usernames())
		}
	})

	t.Run("no identity fails explicitly", func(t *testing.T) {
		lease, err := acquire(context.Background(), "")
		if err == nil || lease != nil {
			t.Fatalf("identity-less sticky acquire must fail explicitly: lease=%v err=%v", lease, err)
		}
		if !strings.Contains(err.Error(), "账号身份") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
