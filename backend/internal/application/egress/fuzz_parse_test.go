package egress

import (
	"strings"
	"testing"
	"time"
)

// FuzzParseProxySubscription 是订阅解析器的模糊测试:任意畸形输入(截断/
// 超长/控制字符/嵌套 base64/clash 混合)不得 panic——解析器处理来自远端
// 订阅服务的不可信文本。仅断言不崩溃与耗时上限, 语义正确性由
// TestImportTextInputBoundaries 等覆盖。
func FuzzParseProxySubscription(f *testing.F) {
	seeds := []string{
		"http://10.0.0.1:1111\nsocks5://[::1]:1080",
		"aHR0cDovL2V4YW1wbGUuY29tOjgwODA=",
		"proxies:\n  - {name: a, server: 1.2.3.4, port: 443, type: ss}",
		"\ufeffhttp://ok:1\n\x00\x01\x02",
		strings.Repeat("#", 100000),
		"{account}",
		"http://user:{account}@host:1",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			entries, skipped, err := parseProxySubscription(input)
			_ = entries
			_ = skipped
			_ = err
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("parser exceeded 5s on input of len %d", len(input))
		}
	})
}
