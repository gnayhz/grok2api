package model

import (
	"strconv"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// LegacyHold is the finite restriction replacing an identifiable old health
// marker. A disabled account still requires operator review; its admin state
// cannot be inferred from a legacy quality marker.
type LegacyHold struct {
	Owner     string
	Reason    string
	ExpiresAt time.Time
}

func PlanLegacyHold(state account.HealthState, now time.Time) (LegacyHold, bool) {
	kind := state.LegacyQualityHold()
	if kind == account.LegacyQualityNone {
		return LegacyHold{}, false
	}
	until := now.UTC().Add(2 * time.Minute)
	if state.CooldownUntil != nil && state.CooldownUntil.Before(until) {
		until = *state.CooldownUntil
	}
	reason := "legacy_quality_cooldown"
	if kind == account.LegacyQualityDisabled {
		reason = "legacy_disabled_review"
	}
	return LegacyHold{Owner: "legacy/account/" + strconv.FormatUint(state.AccountID, 10), Reason: reason, ExpiresAt: until}, true
}
