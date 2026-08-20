package relational

import "regexp"

// rscRedactSecrets strips credential-shaped key=value pairs from upstream
// RSC payload text so a compromised upstream cannot smuggle secrets into
// persisted verdict diagnostics or risk logs (defense in depth; the payload
// is upstream-controlled).
var rscSecretPairPattern = regexp.MustCompile(`(?i)((?:access|refresh|id|sso|session|device)[_-]?token|client[_-]?secret|password|authorization|cookie|code[_-]?verifier)=[^&\s'<>]+`)

func rscRedactSecrets(value string) string {
	return rscSecretPairPattern.ReplaceAllString(value, `$1=[REDACTED]`)
}

func truncateRSCDetail(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return value
}
