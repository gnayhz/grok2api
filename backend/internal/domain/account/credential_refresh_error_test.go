package account

import (
	"strings"
	"testing"
)

func TestRecoverableRefreshErrorCodesAreTheComplementOfPermanent(t *testing.T) {
	if IsRecoverableCredentialRefreshErrorCode("invalid_grant") {
		t.Fatal("terminal refresh-token errors must not be recoverable")
	}
	if !IsRecoverableCredentialRefreshErrorCode("credential_decrypt_failed") {
		t.Fatal("local decrypt failures must remain recoverable")
	}
	if !IsRecoverableCredentialRefreshErrorCode("") {
		t.Fatal("empty codes must remain recoverable")
	}
}

func TestNormalizeCredentialRefreshErrorDiagnostics(t *testing.T) {
	got := NormalizeCredentialRefreshErrorMessage("  line1\nline2\x00  ")
	if got != "line1 line2" {
		t.Fatalf("message = %q", got)
	}
	long := strings.Repeat("a", 600)
	n := NormalizeCredentialRefreshErrorMessage(long)
	if len([]rune(n)) != 512 || !strings.HasSuffix(n, "…") {
		t.Fatalf("message length = %d value=%q", len([]rune(n)), n)
	}
	body := NormalizeCredentialRefreshErrorResponse("{\n\"error\":\"x\"\x7f}")
	if body != "{ \"error\":\"x\" }" {
		t.Fatalf("response = %q", body)
	}
}
