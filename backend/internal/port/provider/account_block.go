package provider

// Text-signal rules for account blocks and DPoP challenges are stable
// fact-level checks (port-owned); body/JSON interpretation lives at the
// dialect boundary in infra/provider/accountblock.go .
import "strings"

func IsDefinitiveAccountBlockText(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "blocked-user") || strings.Contains(value, "user is blocked")
}

func IsDPoPProofRequiredText(value string) bool {
	normalized := strings.NewReplacer("-", "_", ":", "_", ".", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
	return strings.Contains(normalized, "unauthorized_dpop_required") || strings.Contains(normalized, "dpop_proof_required")
}
