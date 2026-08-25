package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

// TestLogSecureCookiesHint：secureCookies=true + 无可信代理是运维陷阱——
// 浏览器经纯 HTTP 直连拒收 Secure cookie，表现为登录成功但会话不保持
// （登录循环），服务端此前无任何可观测线索。必须：无代理 WARN（可操作
// 提示）、有代理 INFO（假定 TLS 由代理终结）、关闭时不输出。
func TestLogSecureCookiesHint(t *testing.T) {
	capture := func() *bytes.Buffer {
		out := &bytes.Buffer{}
		return out
	}

	t.Run("secure without proxies warns", func(t *testing.T) {
		out := capture()
		logger := slog.New(slog.NewTextHandler(out, nil))
		logSecureCookiesHint(logger, config.Config{Auth: config.AuthConfig{SecureCookies: true}})
		if !strings.Contains(out.String(), "secure_cookies_over_plain_http") || !strings.Contains(out.String(), "WARN") {
			t.Fatalf("expected WARN secure_cookies_over_plain_http, got: %s", out.String())
		}
	})

	t.Run("secure behind proxy stays informational", func(t *testing.T) {
		out := capture()
		logger := slog.New(slog.NewTextHandler(out, nil))
		logSecureCookiesHint(logger, config.Config{
			Auth:   config.AuthConfig{SecureCookies: true},
			Server: config.ServerConfig{TrustedProxies: []string{"127.0.0.1"}},
		})
		if !strings.Contains(out.String(), "secure_cookies_enabled_behind_proxy") || strings.Contains(out.String(), "WARN") {
			t.Fatalf("expected INFO secure_cookies_enabled_behind_proxy, got: %s", out.String())
		}
	})

	t.Run("insecure cookies stay silent", func(t *testing.T) {
		out := capture()
		logger := slog.New(slog.NewTextHandler(out, nil))
		logSecureCookiesHint(logger, config.Config{})
		if out.Len() != 0 {
			t.Fatalf("expected no output, got: %s", out.String())
		}
	})
}
