package rsc

import (
	"strings"
	"testing"
)

// TestRedactSecretsContract 锁定 RSC 载荷脱敏契约（round 22）：
// 规则由 rsc 包独占，持久层落库与风险服务日志共用——两端口径不可漂移。
func TestRedactSecretsContract(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"access_token pair", "a=1&access_token=sso-secret-value", "a=1&access_token=[REDACTED]"},
		{"refresh_token underscore", "refresh_token=tok", "refresh_token=[REDACTED]"},
		{"ssn 不误伤", "details=flagged,billing_state=ok", "details=flagged,billing_state=ok"},
		{"普通散文不误伤", "registration;policy=deny;event=blocked", "registration;policy=deny;event=blocked"},
		{"client_secret", "client_secret=abc123", "client_secret=[REDACTED]"},
	}
	for _, tc := range cases {
		if got := RedactSecrets(tc.in); got != tc.want {
			t.Errorf("%s: RedactSecrets(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
	if strings.Contains(RedactSecrets("x=1&session_token=live-secret"), "live-secret") {
		t.Fatal("session_token 值泄漏")
	}
}
