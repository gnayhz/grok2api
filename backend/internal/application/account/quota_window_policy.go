package account

import (
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// quotaWindowTiming adds the account owner's conservative recovery estimate to
// a complete Console chat sample. Source describes the amount's origin; only
// Console chat exposes the legacy timing fields that describe a probe
// estimate, never a reset guarantee. Preserve explicit times and the sample's
// observation time, including on replay.
func quotaWindowTiming(providerValue accountdomain.Provider, window accountdomain.QuotaWindow, observedAt time.Time) accountdomain.QuotaWindow {
	if providerValue != accountdomain.ProviderConsole || window.Mode != accountdomain.QuotaModeConsole {
		return window
	}
	if window.WindowSeconds == 0 {
		window.WindowSeconds = int(accountdomain.ConsolePredictedQuotaProbeDelay / time.Second)
	}
	if window.Remaining == 0 && window.ResetAt == nil {
		predicted := observedAt.Add(accountdomain.ConsolePredictedQuotaProbeDelay)
		window.ResetAt = &predicted
	}
	return window
}
