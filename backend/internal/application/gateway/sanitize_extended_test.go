package gateway

import (
	"strings"
	"testing"
)

// TestSanitizeDiagnosticRedactsExtendedSecretKeys locks the widened sanitizer
// contract: upstream diagnostic bodies echoed into audit records must not
// retain client_secret / password / sso token / session token key-value
// pairs (the original pattern only covered authorization/api/access/refresh/
// id token keys — security audit P1).
func TestSanitizeDiagnosticRedactsExtendedSecretKeys(t *testing.T) {
	body := `upstream rejected: client_secret="topsecret-client-value" password=hunter2 sso_token=sso-abc123 session_token=sess-xyz`
	sanitized := sanitizeDiagnosticText(body, 4096)
	for _, leak := range []string{"topsecret-client-value", "hunter2", "sso-abc123", "sess-xyz"} {
		if strings.Contains(sanitized, leak) {
			t.Fatalf("secret %q survived sanitization: %s", leak, sanitized)
		}
	}
}
