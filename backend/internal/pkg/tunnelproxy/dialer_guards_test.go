package tunnelproxy

import (
	"context"
	"strings"
	"testing"
)

// TestDialerGuardClauses：拨号器入口防护——未初始化、非 TCP 网络、
// Shadowsocks 无效目标地址必须返回明确错误而非 panic/静默。
func TestDialerGuardClauses(t *testing.T) {
	var nilDialer *Dialer
	if _, err := nilDialer.DialContext(context.Background(), "tcp", "example.com:443"); err == nil || !strings.Contains(err.Error(), "未初始化") {
		t.Fatalf("nil dialer must fail with init error, got %v", err)
	}

	dialer := &Dialer{}
	if _, err := dialer.Dial("tcp", "example.com:443"); err == nil || !strings.Contains(err.Error(), "未初始化") {
		t.Fatalf("empty dialer must fail with init error, got %v", err)
	}

	ss, err := NewDialer("ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ=@127.0.0.1:1080#test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.DialContext(context.Background(), "udp", "1.1.1.1:53"); err == nil || !strings.Contains(err.Error(), "不支持网络") {
		t.Fatalf("udp must be rejected, got %v", err)
	}
	if _, err := ss.DialContext(context.Background(), "tcp", "not-a-valid-addr"); err == nil {
		t.Fatal("invalid ss target address must fail")
	}
}

// TestParseRejectsUnknownScheme：解析器对未知 scheme 的拒绝语义。
func TestParseRejectsUnknownScheme(t *testing.T) {
	if _, err := Parse("hysteria://example.com:443"); err == nil {
		t.Fatal("unknown scheme must be rejected")
	}
	if _, err := Parse("trojan://@example.com:443"); err == nil {
		t.Fatal("trogon without credential must be rejected")
	}
}
