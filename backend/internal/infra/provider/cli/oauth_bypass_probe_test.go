package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestOAuthRedactionNestedJSONAndURLEncoded 锁定 round 21 修复的两种绕过
// 形态（PoC 实证后修，此处防回归）：
// 1. 字符串值内嵌 JSON——键级规则必须穿透到内层 access_token；
// 2. URL 编码的敏感对——命中即整体脱敏。
// 对照：直接键、普通散文、CJK 描述不受影响。
func TestOAuthRedactionNestedJSONAndURLEncoded(t *testing.T) {
	// 嵌套 JSON：内层 token 必须被键规则命中。
	nested := map[string]any{
		"detail": string(mustJSON(map[string]any{"access_token": "sso-short-secret-value-123"})),
	}
	encoded, _ := json.Marshal(redactOAuthDiagnosticValue("root", nested))
	if strings.Contains(string(encoded), "sso-short-secret-value-123") {
		t.Fatalf("嵌套 JSON 内层 token 泄漏: %s", encoded)
	}
	// URL 编码形态：整体脱敏。
	urlForm := map[string]any{"redirect": "https://x.ai/cb?access_token%3Dsso-short-secret-value-123"}
	encoded2, _ := json.Marshal(redactOAuthDiagnosticValue("root", urlForm))
	if strings.Contains(string(encoded2), "sso-short-secret-value-123") {
		t.Fatalf("URL 编码 token 泄漏: %s", encoded2)
	}
	// 普通散文与 CJK 描述不受影响。
	prose := redactOAuthDiagnosticText("invalid_grant: the provided authorization grant is invalid")
	if prose != "invalid_grant: the provided authorization grant is invalid" {
		t.Fatalf("普通散文被误伤: %q", prose)
	}
	cjk := redactOAuthDiagnosticText("授权码已过期，请重新发起授权流程")
	if cjk != "授权码已过期，请重新发起授权流程" {
		t.Fatalf("CJK 描述被误伤: %q", cjk)
	}
	// Bearer 形态维持既有行为。
	if out := redactOAuthDiagnosticText("header: Bearer abc.def.ghi.jklmno"); strings.Contains(out, "abc.def") {
		t.Fatalf("Bearer 泄漏: %q", out)
	}
}
