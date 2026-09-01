package egress

import (
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// TestClientCacheIgnoresCookieChangesInFingerprint 锁定指纹契约:
// cookies 不进客户端缓存键。客户端构造不接收 cookies(按请求头携带、
// 无 jar),cookie 变化若进入指纹只会作废整池热连接——clearance 例行
// 刷新(默认 10m 一次)与多账号 cookie 轮换都会触发无谓的整池重握手。
// 传输层形态真实相关的分量(proxyURL/userAgent)变化仍必须换键。
func TestClientCacheIgnoresCookieChangesInFingerprint(t *testing.T) {
	manager, _ := newPoolTestManager(t)
	manager.accountIsolated.Store(false)

	first, err := manager.clientForWithOptions(7, domain.ScopeWeb, "socks5://proxy:1080", "UA/1.0", "cf_clearance=aaa", false, "shared", clientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 同出口、同 UA、不同 cookies:必须复用同一客户端实例(连接池保留)。
	second, err := manager.clientForWithOptions(7, domain.ScopeWeb, "socks5://proxy:1080", "UA/1.0", "cf_clearance=bbb", false, "shared", clientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.client != second.client {
		t.Fatal("cookie change must not rebuild the client: warm connection pool would be discarded")
	}

	// 代理 URL 变化(传输层形态变化)仍必须换新客户端。
	third, err := manager.clientForWithOptions(7, domain.ScopeWeb, "socks5://proxy:2080", "UA/1.0", "cf_clearance=bbb", false, "shared", clientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if third.client == second.client {
		t.Fatal("proxy URL change must produce a new client")
	}

	// UA 变化(驱动 TLS profile)同样必须换新客户端。
	fourth, err := manager.clientForWithOptions(7, domain.ScopeWeb, "socks5://proxy:1080", "UA/2.0", "cf_clearance=bbb", false, "shared", clientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fourth.client == second.client {
		t.Fatal("user agent change must produce a new client")
	}
	_ = time.Now
}
