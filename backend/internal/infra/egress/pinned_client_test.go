package egress

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// DoPinnedHTTPS 的 SSRF 校验面:URL 主机必须是公网 IP、HTTPS 443、Host 头
// 与 TLS ServerName 一致。这是固定地址出口的安全边界——校验放行私网/环回
// 地址即重新打开 SSRF 缺口(注释明示此护栏的存在理由)。
func TestDoPinnedHTTPSSSRFGuards(t *testing.T) {
	lease := &Lease{}

	mustReject := func(name, rawURL, serverName, host string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			request.Host = host
		} else {
			request.Host = strings.TrimPrefix(serverName, "https://")
		}
		if _, err := lease.DoPinnedHTTPS(request, serverName); err == nil {
			t.Fatalf("%s: 私网/环回/非 IP 主机未被拒绝", name)
		}
	}

	// 私网/保留/环回/链路本地/组播/未指定——全部必须拒绝。
	mustReject("private-ipv4", "https://192.168.1.10:443/x", "api.grok.com", "api.grok.com")
	mustReject("private-cidr-10", "https://10.0.0.5:443/x", "api.grok.com", "api.grok.com")
	mustReject("private-cidr-172", "https://172.16.0.5:443/x", "api.grok.com", "api.grok.com")
	mustReject("loopback", "https://127.0.0.1:443/x", "api.grok.com", "api.grok.com")
	mustReject("loopback-v6", "https://[::1]:443/x", "api.grok.com", "api.grok.com")
	mustReject("link-local", "https://169.254.1.1:443/x", "api.grok.com", "api.grok.com")
	mustReject("multicast", "https://224.0.0.1:443/x", "api.grok.com", "api.grok.com")
	mustReject("unspecified", "https://0.0.0.0:443/x", "api.grok.com", "api.grok.com")

	// 非 IP 主机(DNS 名)必须拒绝——固定地址语义就是绕过 DNS。
	mustReject("dns-host", "https://api.grok.com:443/x", "api.grok.com", "api.grok.com")

	// 非 443 端口与非 HTTPS 必须拒绝。
	mustReject("wrong-port", "https://203.0.113.10:8443/x", "api.grok.com", "api.grok.com")

	// Host 头与 ServerName 不一致必须拒绝(防 SNI 与实际 Host 分离的绕过)。
	mustReject("host-mismatch", "https://203.0.113.10:443/x", "api.grok.com", "evil.example")

	// 放行路径的边界证据:公网 IP + Host 一致时校验通过,失败发生在拨号层
	// 而非校验层——错误信息含拨号语义(dial/timeout),不含任何校验拒绝文案。
	// 上下文限界:证明校验放行即可,拨号失败快慢无关(避免 TEST-NET 无路由等待)。
	acceptCtx, cancelAccept := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelAccept()
	accept, err := http.NewRequestWithContext(acceptCtx, http.MethodGet, "https://203.0.113.10:443/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	accept.Host = "api.grok.com"
	_, err = lease.DoPinnedHTTPS(accept, "api.grok.com")
	if err == nil {
		t.Fatal("unexpectedly connected to TEST-NET address")
	}
	for _, validation := range []string{"必须使用", "必须一致", "不能为空"} {
		if strings.Contains(err.Error(), validation) {
			t.Fatalf("public-IP + matching host rejected at validation: %v", err)
		}
	}
}
