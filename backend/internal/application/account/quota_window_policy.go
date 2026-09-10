package account

import (
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

const consolePredictedQuotaProbeDelay = 24 * time.Hour

// quotaWindowTiming adds the account owner's conservative recovery estimate to
// a complete Console sample. Source describes the amount's origin; Console's
// legacy chat timing fields describe a probe estimate, never a reset guarantee.
// Preserve explicit times and the sample's observation time, including on replay.
func quotaWindowTiming(providerValue accountdomain.Provider, window accountdomain.QuotaWindow, observedAt time.Time) accountdomain.QuotaWindow {
	if providerValue != accountdomain.ProviderConsole || window.Mode != "console" {
		return window
	}
	if window.WindowSeconds == 0 {
		window.WindowSeconds = int(consolePredictedQuotaProbeDelay / time.Second)
	}
	if window.Remaining == 0 && window.ResetAt == nil {
		predicted := observedAt.Add(consolePredictedQuotaProbeDelay)
		window.ResetAt = &predicted
	}
	return window
}

// quotaRecoveryDueAt keeps upstream quota exhaustion recoverable even when
// the Provider reports no reset timestamp. Console uses a conservative
// predicted 24-hour probe window; generic remote windows retain the shorter
// fallback and transport failures use the recovery queue's bounded backoff.
func quotaRecoveryDueAt(window accountdomain.QuotaWindow, now time.Time, exhausted bool) *time.Time {
	if !exhausted {
		return nil
	}
	if window.ResetAt != nil && window.ResetAt.After(now) {
		value := *window.ResetAt
		return &value
	}
	if isConsoleUsageQuotaMode(window.Mode) {
		value := now.Add(consolePredictedQuotaProbeDelay)
		return &value
	}
	if window.Source == accountdomain.QuotaSourceUpstream {
		value := now.Add(unknownRemoteQuotaProbeDelay)
		return &value
	}
	return nil
}
