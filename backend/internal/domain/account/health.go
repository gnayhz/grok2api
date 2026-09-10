package account

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// HealthEvent describes a fact or an explicit account command. Callers never
// supply an absolute failure count derived from a selected credential.
type HealthEvent struct {
	Kind             HealthEventKind
	ObservedRevision uint64
	Status           int
	RetryAfter       time.Duration
	AfterSuccess     bool
	CooldownBase     time.Duration
	CooldownMax      time.Duration
	MinHold          time.Duration
}

type HealthEventKind string

const (
	HealthFailure              HealthEventKind = "failure"
	HealthSoftFailure          HealthEventKind = "soft_failure"
	HealthSuccess              HealthEventKind = "success"
	HealthClearCooldown        HealthEventKind = "clear_cooldown"
	HealthQualityIdle          HealthEventKind = "quality_idle"
	HealthClearQuality         HealthEventKind = "clear_quality"
	HealthMigrateLegacyQuality HealthEventKind = "migrate_legacy_quality"
	SoftFailureCooldown                        = 5 * time.Second
	DefaultQualityIdleCooldown                 = 15 * time.Minute
)

// HealthState is the committed account health projection. Revision belongs to
// SQL state, independently of notification delivery order or wall clocks.
type HealthState struct {
	AccountID        uint64
	Provider         Provider
	Revision         uint64
	FailureCount     int
	CooldownUntil    *time.Time
	CooldownMarkedAt *time.Time
	LastError        string
}

type HealthResult struct {
	State   HealthState
	Applied bool
}

func (v Credential) HealthState() HealthState {
	return HealthState{AccountID: v.ID, Provider: v.Provider, Revision: v.HealthRevision, FailureCount: v.FailureCount, CooldownUntil: v.CooldownUntil, CooldownMarkedAt: v.CooldownMarkedAt, LastError: v.LastError}
}

// TransitionHealth is the M07 policy, evaluated against locked current state.
// A success can reset only the state observed by its request. Failures from
// concurrent requests accumulate against the current count; a successful
// response header may reset its own baseline but never a newer failure.
func TransitionHealth(current HealthState, event HealthEvent, now time.Time) (HealthResult, error) {
	result := HealthResult{State: current}
	next := current
	now = now.UTC()
	clear := func() { next.FailureCount = 0; next.CooldownUntil = nil; next.CooldownMarkedAt = nil }
	switch event.Kind {
	case HealthSuccess:
		if event.ObservedRevision != current.Revision {
			return result, nil
		}
		clear()
		next.LastError = ""
		if current.LastError == LastErrorMissingThinking || current.LastError == LastErrorMissingThinkingDisabled {
			next.LastError = LastErrorMissingThinking
		}
	case HealthClearCooldown:
		clear()
		next.LastError = NormalizeHealthMarker(current.LastError)
	case HealthClearQuality:
		if NormalizeHealthMarker(current.LastError) == "" {
			return result, nil
		}
		if event.MinHold > 0 && current.CooldownMarkedAt != nil && now.Sub(*current.CooldownMarkedAt) < event.MinHold {
			return result, nil
		}
		clear()
		next.LastError = ""
	case HealthMigrateLegacyQuality:
		if current.LegacyQualityHold() == LegacyQualityNone {
			return result, nil
		}
		// Historical quality markers never owned the independent failure
		// counter or the administrator's enabled flag.
		next.LastError, next.CooldownUntil, next.CooldownMarkedAt = "", nil, nil
	case HealthQualityIdle:
		cooldown := event.RetryAfter
		if cooldown <= 0 {
			cooldown = DefaultQualityIdleCooldown
		}
		until := now.Add(cooldown)
		next.CooldownUntil, next.CooldownMarkedAt, next.LastError = &until, &now, LastErrorQualityIdle
	case HealthFailure, HealthSoftFailure:
		if event.AfterSuccess && event.ObservedRevision == current.Revision {
			next.FailureCount = 0
		}
		cooldown := SoftFailureCooldown
		if event.Kind == HealthFailure {
			if next.FailureCount == math.MaxInt {
				return result, errors.New("account failure count exhausted")
			}
			next.FailureCount++
			cooldown = max(time.Duration(0), event.CooldownBase)
			limit := max(time.Duration(0), event.CooldownMax)
			for i := 1; i < next.FailureCount && cooldown < limit && cooldown > 0; i++ {
				if cooldown > limit/2 {
					cooldown = limit
					break
				}
				cooldown *= 2
			}
			cooldown = min(cooldown, limit)
		}
		cooldown = max(cooldown, event.RetryAfter)
		until := now.Add(cooldown)
		next.CooldownUntil, next.CooldownMarkedAt = &until, &now
		next.LastError = fmt.Sprintf("upstream status %d", event.Status)
		// A late result may extend a restriction but cannot shorten the newer
		// cooldown it never observed. Fresh sequential soft failures retain the
		// existing bounded short-cooldown policy.
		if event.ObservedRevision != current.Revision && current.CooldownUntil != nil && current.CooldownUntil.After(until) {
			next.CooldownUntil, next.CooldownMarkedAt, next.LastError = current.CooldownUntil, current.CooldownMarkedAt, current.LastError
		}
	default:
		return result, errors.New("invalid account health event")
	}
	if current.Revision >= math.MaxInt64 {
		return result, errors.New("account health revision exhausted")
	}
	next.Revision++
	return HealthResult{State: next, Applied: true}, nil
}

// LegacyQualityKind identifies only the historical fields whose quality owner
// is known. Other transport, idle and authentication state is not migratable.
type LegacyQualityKind uint8

const (
	LegacyQualityNone LegacyQualityKind = iota
	LegacyQualityCooldown
	LegacyQualityDisabled
)

func (s HealthState) LegacyQualityHold() LegacyQualityKind {
	switch s.LastError {
	case LastErrorMissingThinking:
		return LegacyQualityCooldown
	case LastErrorMissingThinkingDisabled:
		return LegacyQualityDisabled
	default:
		return LegacyQualityNone
	}
}

// LegacyQualityHealthMarkers narrows a migration scan. The locked state is
// still classified again before any transition is applied.
func LegacyQualityHealthMarkers() []string {
	return []string{LastErrorMissingThinking, LastErrorMissingThinkingDisabled}
}
