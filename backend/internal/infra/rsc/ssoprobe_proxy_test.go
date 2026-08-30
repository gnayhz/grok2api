package rsc

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 生产事故回归:探针出口可配置(空=直连)。配置了非法代理必须
// 以 error verdict 失败(可重试),绝不能退回直连——否则脏出口部署会在
// 无感知的情况下继续用裸 IP 误判。
func TestSSOProbeInvalidProxyFailsAsError(t *testing.T) {
	probe := NewSSOProbeChecker(2 * time.Second)
	probe.ProxyURL = "socks5://" + strings.Repeat("0", 300) // 非法 URL
	result := probe.Check(context.Background(), "token")
	if result.Verdict != VerdictError {
		t.Fatalf("verdict = %q want error (invalid proxy must not fall back to direct)", result.Verdict)
	}
	if result.Error == "" {
		t.Fatal("error verdict must carry a cause")
	}
}

// 配置了不可达代理时同样必须得到 error(而非直连放行);空代理 = 直连
// 保持原行为。这里用保留地址保证拨号失败,不断言具体错误文本。
func TestSSOProbeUnreachableProxyIsErrorNotClean(t *testing.T) {
	probe := NewSSOProbeChecker(500 * time.Millisecond)
	probe.ProxyURL = "socks5://127.0.0.1:1" // 保留端口,几乎必然连接拒绝
	result := probe.Check(context.Background(), "token")
	if result.Verdict == VerdictClean || result.Verdict == VerdictDenied {
		t.Fatalf("unreachable proxy must not produce a decisive verdict, got %q", result.Verdict)
	}
}

func TestSSOProbeEmptyProxyStaysDirect(t *testing.T) {
	probe := NewSSOProbeChecker(time.Second)
	if probe.ProxyURL != "" {
		t.Fatal("default probe must be direct (empty proxy)")
	}
	client, err := probe.newClient()
	if err != nil {
		t.Fatalf("direct client construction failed: %v", err)
	}
	client.CloseIdleConnections()
}
