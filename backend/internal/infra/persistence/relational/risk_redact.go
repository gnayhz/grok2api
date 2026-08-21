package relational

import "github.com/chenyme/grok2api/backend/internal/infra/rsc"

// rscRedactSecrets strips credential-shaped key=value pairs from upstream
// RSC payload text so a compromised upstream cannot smuggle secrets into
// persisted verdict diagnostics or risk logs (defense in depth; the payload
// is upstream-controlled). The rule itself lives in the rsc package so the
// risk service logs and this persistence layer cannot drift apart.
func rscRedactSecrets(value string) string {
	return rsc.RedactSecrets(value)
}

func truncateRSCDetail(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return value
}
