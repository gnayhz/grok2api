package egress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// WS 变体的 403 委托语义此前登记为未验证项(R35:浏览器 TLS WS 端点成本高)。
// 实际上 403 分支不需要成功的 WS 握手:ws://(非 TLS)端点经浏览器客户端的
// NetDialContext 直连,握手被 403 拒绝即触发待验证的委托分支。本组测试与
// HTTP 侧 DoDeferredForbidden(已有测试)共同锁定"403 由调用方分类后才失效
// Clearance"的契约在两个传输面上行为一致。
func newWSForbiddenFixture(t *testing.T, statusCode int) (*Lease, *Manager, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(statusCode)
	}))
	t.Cleanup(server.Close)

	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	browser, err := newBrowserClient("", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")
	if err != nil {
		t.Fatalf("newBrowserClient: %v", err)
	}
	t.Cleanup(browser.CloseIdleConnections)
	manager := NewManager(&e2eRepo{}, cipher)
	key := "ws-deferred-test-key"
	manager.clearanceMu.Lock()
	manager.clearances[key] = clearanceState{cookies: "cf_clearance=x", userAgent: "ua", refreshedAt: time.Now().UTC(), used: true}
	manager.clearanceMu.Unlock()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	lease := &Lease{browser: browser, clearanceManager: manager, clearanceKey: key}
	return lease, manager, endpoint
}

func wsInvalidated(t *testing.T, manager *Manager, key string) bool {
	t.Helper()
	manager.clearanceMu.Lock()
	defer manager.clearanceMu.Unlock()
	return manager.clearances[key].invalid
}

// 延迟失效变体:403 握手响应不得在拨号内联失效 Clearance——分类权留给调用
// 方,分类确认为出口相关后由调用方显式 InvalidateClearance。
func TestDialWebSocketDeferredForbiddenDefersInvalidation(t *testing.T) {
	lease, manager, endpoint := newWSForbiddenFixture(t, http.StatusForbidden)

	connection, response, err := lease.DialWebSocketDeferredForbidden(context.Background(), endpoint, fhttp.Header{}, 2*time.Second)

	if err == nil || connection != nil {
		t.Fatalf("403 handshake must fail the dial: conn=%v err=%v", connection, err)
	}
	if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("handshake response must surface the 403: %+v", response)
	}
	if wsInvalidated(t, manager, "ws-deferred-test-key") {
		t.Fatal("deferred variant must NOT invalidate the clearance inside the dial")
	}
	lease.InvalidateClearance()
	if !wsInvalidated(t, manager, "ws-deferred-test-key") {
		t.Fatal("explicit InvalidateClearance after classification must invalidate")
	}
}

// 普通变体(内联失效):403 握手响应在拨号内直接失效浏览器会话绑定。
func TestDialWebSocketInvalidatesOnForbidden(t *testing.T) {
	lease, manager, endpoint := newWSForbiddenFixture(t, http.StatusForbidden)

	_, response, err := lease.DialWebSocket(context.Background(), endpoint, fhttp.Header{}, 2*time.Second)

	if err == nil {
		t.Fatal("403 handshake must fail the dial")
	}
	if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("handshake response must surface the 403: %+v", response)
	}
	if !wsInvalidated(t, manager, "ws-deferred-test-key") {
		t.Fatal("plain variant must invalidate the clearance inline on 403")
	}
}

// 负向控制:非 403 握手失败(500)在两个变体上都不得失效——失效语义严格
// 限定为 Cloudflare 拦截信号。
func TestDialWebSocketNonForbiddenNeverInvalidates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		defer_ bool
	}{
		{"deferred", true},
		{"plain", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease, manager, endpoint := newWSForbiddenFixture(t, http.StatusInternalServerError)
			var err error
			if tc.defer_ {
				_, _, err = lease.DialWebSocketDeferredForbidden(context.Background(), endpoint, fhttp.Header{}, 2*time.Second)
			} else {
				_, _, err = lease.DialWebSocket(context.Background(), endpoint, fhttp.Header{}, 2*time.Second)
			}
			if err == nil {
				t.Fatal("500 handshake must fail the dial")
			}
			if wsInvalidated(t, manager, "ws-deferred-test-key") {
				t.Fatalf("%s variant must not invalidate on non-403 handshake failure", tc.name)
			}
		})
	}
}
