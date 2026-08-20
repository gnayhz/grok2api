package cli

import (
	"strings"
	"testing"
)

// TestOAuthStructuredErrorRedactsSecrets: upstream OAuth error fields can
// echo submitted secrets (refresh_token=... / client_secret=... / password=...).
// The structured-message path must redact them before the message is
// persisted as LastRefreshErrorMessage and returned by the admin API.
func TestOAuthStructuredErrorRedactsSecrets(t *testing.T) {
	body := `{"error":"invalid_grant","error_description":"refresh token is invalid: refresh_token=abc-def-ghi&client_secret=super-secret-value","message":"password=hunter2-admin-pw rejected"}`
	details := parseOAuthErrorResponse([]byte(body), 400)
	leaks := []string{"abc-def-ghi", "super-secret-value", "hunter2-admin-pw"}
	combined := details.Message + " " + details.Response
	for _, leak := range leaks {
		if strings.Contains(combined, leak) {
			t.Fatalf("secret %q leaked into OAuth diagnostics: message=%q response=%q", leak, details.Message, details.Response)
		}
	}
}
func TestOAuthRedactPreservesCJKDiagnostics(t *testing.T) {
	// A long Chinese error description has no whitespace: strings.Fields keeps
	// it as one field, and the old >=80-byte long-field heuristic wiped it
	// entirely once structured messages were routed through the redactor.
	description := strings.Repeat("上游拒绝了该请求，请稍后重试。", 12) // >80 bytes, no spaces
	redacted := redactOAuthDiagnosticText(description)
	if redacted == "" || strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("legitimate CJK diagnostic erased: %q", redacted)
	}
	// ASCII token material of the same length must still be redacted.
	tokenish := strings.Repeat("a9Z3-", 20)
	if redacted := redactOAuthDiagnosticText(tokenish); !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("long ASCII token material survived: %q", redacted)
	}
}
