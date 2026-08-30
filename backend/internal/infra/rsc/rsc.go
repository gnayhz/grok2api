// Package rsc classifies registration risk for a Web SSO identity.
//
// The transport is the SSO thinking probe (SSOProbeChecker, see
// ssoprobe.go): since grok.com stopped rendering botFlag fields into its
// Next.js RSC payload, the surviving account-level signal is the reasoning
// stream of a real (temporary) mgw conversation. The legacy homepage RSC
// payload parser (a Go port of regc verify --checks rsc that read every
// account as clean after the grok.com redesign) was removed with the
// homepage method: its "rollback" behavior was indistinguishable from
// disabling accountRisk, at the cost of 285 lines and false-clean cache
// pollution. Rollback to no-attribution is the enabled switch.
//
// Confirmed denied/flagged verdicts stay trusted for DeniedTTL; callers
// must not treat them as permanently fresh.
package rsc

import (
	"regexp"
	"time"
)

// Verdict classifies one RSC check.
type Verdict string

const (
	// VerdictClean means the probe saw a real thinking stream.
	VerdictClean Verdict = "clean"
	// VerdictDenied means the answer arrived with no thinking at all.
	VerdictDenied Verdict = "denied"
	// VerdictFlagged is a legacy-data class: it was only ever produced by
	// the removed homepage parser (botFlagSource 1/2 without an explicit
	// deny). No code produces it today, but Risky() and the patrol/reconcile
	// paths must keep honoring historical rows until DeniedTTL ages them out
	// and a re-probe overwrites them — do NOT delete as dead.
	VerdictFlagged Verdict = "flagged"
	// VerdictError means the check could not classify (transport, challenge,
	// or structure change). It must never be treated as clean.
	VerdictError Verdict = "error"
)

// Result is the outcome of one RSC check. BotFlagDetails carries the
// probe's diagnostic summary (persisted as verdict details); the homepage
// era's BotFlagSource/RiskScore numeric outputs died with that parser.
type Result struct {
	Verdict        Verdict
	BotFlagDetails string
	HTTPStatus     int
	Error          string
	CheckedAt      time.Time
	// Suppressed marks a denied downgraded to error by the SSO probe's
	// channel-vocabulary breaker; callers may answer with witness
	// re-validation instead of retrying blindly.
	Suppressed bool
}

// Risky reports whether the verdict marks the identity as risk.
func (r Result) Risky() bool {
	return r.Verdict == VerdictDenied || r.Verdict == VerdictFlagged
}

// secretPairPattern strips credential-shaped key=value pairs from upstream
// RSC payload text so a compromised upstream cannot smuggle secrets into
// persisted verdict diagnostics or risk logs (defense in depth; the payload
// is upstream-controlled). Shared by the persistence layer (pre-save) and
// the risk service (pre-log) — both sides must see the same rule.
var secretPairPattern = regexp.MustCompile(`(?i)((?:access|refresh|id|sso|session|device)[_-]?token|client[_-]?secret|password|authorization|cookie|code[_-]?verifier)=[^&\s'<>]+`)

// RedactSecrets removes credential-shaped pairs from upstream RSC text.
// It is exported so every consumer of RSC payload fragments (logs, storage)
// applies the identical rule instead of drifting copies.
func RedactSecrets(value string) string {
	return secretPairPattern.ReplaceAllString(value, `$1=[REDACTED]`)
}
