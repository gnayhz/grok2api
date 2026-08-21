package gateway

import (
	"strings"
	"testing"
)

// TestSanitizeDiagnosticURLEncodedPairs 锁定 round 23 修复：
// %3D 编码的敏感对脱敏；camelCase 明文（清单描述部分失实——裸 token
// 子词 + (?i) 已覆盖，PoC 实证）；非敏感 %3D 与普通 URL 不误伤。
func TestSanitizeDiagnosticURLEncodedPairs(t *testing.T) {
	if out := sanitizeDiagnosticText("access_token%3Dsuper-secret-value", 4096); strings.Contains(out, "super-secret") {
		t.Fatalf("URL 编码敏感对泄漏: %q", out)
	}
	if out := sanitizeDiagnosticText("accessToken%3Dsuper-secret-value", 4096); strings.Contains(out, "super-secret") {
		t.Fatalf("URL 编码驼峰泄漏: %q", out)
	}
	keep := "state%3Dabc123&scope%3Dread"
	if out := sanitizeDiagnosticText(keep, 4096); out != keep {
		t.Fatalf("非敏感编码对被误伤: %q", out)
	}
	if out := sanitizeDiagnosticText("see https://x.ai/docs/guide", 4096); !strings.Contains(out, "https://x.ai/docs/guide") {
		t.Fatalf("普通 URL 被误伤: %q", out)
	}
	if out := sanitizeDiagnosticText("accessToken=super-secret-value", 4096); strings.Contains(out, "super-secret") {
		t.Fatalf("camelCase 明文泄漏: %q", out)
	}
}
