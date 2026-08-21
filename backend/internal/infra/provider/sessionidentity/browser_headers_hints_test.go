package sessionidentity

import (
	"testing"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

// TestBrowserHeadersClientHintsConsistency 锁定 r35 修复向 session 探测
// 的传导：iPhone UA 下 Sec-Ch-Ua-Platform 必须是 iOS（而非 UA 字面里
// "like Mac OS X" 触发的 macOS），且移动位与平台一致——探测请求的
// 浏览器指纹不能自相矛盾（grok.com 的风控可检测）。
func TestBrowserHeadersClientHintsConsistency(t *testing.T) {
	iphone := "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/126.0.6478.54 Mobile/15E148 Safari/604.1"
	lease := &infraegress.Lease{UserAgent: iphone}
	header := browserHeaders("sso-token", "https://grok.com", lease)
	if got := header.Get("User-Agent"); got != iphone {
		t.Fatalf("UA 透传 = %q", got)
	}
	if got := header.Get("Sec-Ch-Ua-Platform"); got != "\"iOS\"" {
		t.Fatalf("iPhone 平台 = %q, 应为 iOS（r35 修复传导）", got)
	}
	if got := header.Get("Sec-Ch-Ua-Mobile"); got != "?1" {
		t.Fatalf("移动位 = %q, 应为 ?1", got)
	}
	// 桌面默认 UA 对照：macOS + 非移动。
	lease = &infraegress.Lease{UserAgent: infraegress.DefaultUserAgent}
	header = browserHeaders("sso-token", "https://grok.com", lease)
	if got := header.Get("Sec-Ch-Ua-Platform"); got != "\"macOS\"" {
		t.Fatalf("桌面平台 = %q, 应为 macOS", got)
	}
	if got := header.Get("Sec-Ch-Ua-Mobile"); got != "?0" {
		t.Fatalf("桌面移动位 = %q, 应为 ?0", got)
	}
}
