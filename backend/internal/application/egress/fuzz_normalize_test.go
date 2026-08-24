package egress

import (
	"strings"
	"testing"
	"time"
)

// FuzzNormalizeProxyURL 模糊 NormalizeProxyURL——所有节点/订阅代理地址的
// 统一入口(管理员输入+订阅文本)。任意畸形 URL 不得 panic; 结果若非空
// 必须可被再次归一化(幂等), 这是客户端缓存指纹稳定的前提。
func FuzzNormalizeProxyURL(f *testing.F) {
	seeds := []string{
		"http://user:pw@10.0.0.1:8080",
		"socks5h://[2001:db8::1]:1080",
		"HTTP://UPPER.Host:80",
		"http://user:{account}@resin:2260",
		"socks5://host",
		"://",
		"http://\x00\x01",
		strings.Repeat("http://a.b/", 500),
		"ws://odd:1",
		"http://host:99999",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		done := make(chan struct{})
		var first string
		var firstErr error
		go func() {
			defer close(done)
			first, firstErr = NormalizeProxyURL(input)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("normalize exceeded 5s on len %d", len(input))
		}
		if firstErr != nil || first == "" {
			return
		}
		second, secondErr := NormalizeProxyURL(first)
		if secondErr != nil {
			t.Fatalf("accepted %q then rejected its own output: %v (input len %d)", first, secondErr, len(input))
		}
		if second != first {
			t.Fatalf("not idempotent: %q -> %q -> %q", input, first, second)
		}
	})
}
