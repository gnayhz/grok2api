package egress

import (
	"testing"
	"time"
)

// FuzzParseClashSubscription 模糊 Clash 订阅解析器:任意 YAML 形态文本
// 不得 panic/超时。这是第三个不可信输入入口(明文列表/Clash YAML/base64
// 中结构最复杂的一个)。语义由既有确定用例覆盖, 此处锁健壮性。
func FuzzParseClashSubscription(f *testing.F) {
	f.Add("proxies:\n  - {name: a, server: 1.2.3.4, port: 443, type: ss, cipher: aes-128-gcm, password: pw}")
	f.Add("proxies: []")
	f.Add("proxies:\n  - type: trojan\n    server: h\n    port: 443\n    password: p\n    name: t")
	f.Add("not: yaml: at: all")
	f.Add("proxies:\n  - {port: 'not-a-number'}")
	f.Add("proxies:\n  - {port: -1}")
	f.Add("proxies:\n  - {port: 99999}")
	f.Add(string(make([]byte, 0)))
	f.Fuzz(func(t *testing.T, input string) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			entries, _, _ := parseClashSubscription(input)
			_ = entries
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("clash parser exceeded 5s on len %d", len(input))
		}
	})
}
