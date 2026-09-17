package account

import "time"

// ConsolePredictedQuotaProbeDelay is the fixed re-probe interval for Console
// usage quotas. /usage reports consumption without a reset timestamp, so an
// exhausted Console usage window is only rechecked after this conservative
// prediction window. It is the single owner of that duration: persistence,
// reconciliation and the recovery worker all consume this constant.
const ConsolePredictedQuotaProbeDelay = 24 * time.Hour

// RemoteQuotaProbeFallback is the generic re-probe delay for a remote window
// that reports exhaustion without any reset deadline, so recovery stays
// bounded instead of parking the account forever.
const RemoteQuotaProbeFallback = 5 * time.Minute

// QuotaWindowProbeAt reports when a persisted window should be probed again.
// ok is false when the window carries no bounded retry deadline at all: a
// local window without an explicit reset time has nothing to wait for, so its
// caller must not schedule a probe.
func QuotaWindowProbeAt(window QuotaWindow, now time.Time) (time.Time, bool) {
	if window.ResetAt != nil && window.ResetAt.After(now) {
		return *window.ResetAt, true
	}
	if IsConsoleUsageQuotaMode(window.Mode) {
		return now.Add(ConsolePredictedQuotaProbeDelay), true
	}
	if window.Source == QuotaSourceUpstream {
		return now.Add(RemoteQuotaProbeFallback), true
	}
	return time.Time{}, false
}

// QuotaWindowDeservesRecovery is the single recovery-eligibility rule for a
// persisted window: it must control routing for its provider (Console keeps
// non-usage billing snapshots from blocking any route), it must be exhausted,
// and it must own a bounded retry deadline. Storage returns raw candidates and
// the composition root replays raw candidates; both apply this predicate
// instead of encoding eligibility again in SQL or in wiring.
func QuotaWindowDeservesRecovery(provider Provider, window QuotaWindow, now time.Time) bool {
	if !QuotaWindowControlsRouting(provider, window.Mode) || window.Remaining > 0 {
		return false
	}
	_, ok := QuotaWindowProbeAt(window, now)
	return ok
}
