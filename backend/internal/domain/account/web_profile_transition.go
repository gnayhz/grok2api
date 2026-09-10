package account

import (
	"errors"
	"strings"
	"time"
)

type WebProfileEventKind string

const (
	WebProfileTermsAccepted WebProfileEventKind = "terms_accepted"
	WebProfileBirthDateSet  WebProfileEventKind = "birth_date_set"
	WebProfileNSFWEnabled   WebProfileEventKind = "nsfw_enabled"
)

// WebProfileObservation records a known successful upstream action. It never
// claims that a different material generation performed that action.
type WebProfileObservation struct {
	Kind         WebProfileEventKind
	OccurredAt   time.Time
	TermsVersion int
}

type WebProfileState struct {
	Material             CredentialRef
	NSFWEnabledAt        *time.Time
	TermsAcceptedAt      *time.Time
	TermsAcceptedVersion int
	BirthDateSetAt       *time.Time
}

type WebProfileResult struct {
	Applied bool
	Changed bool
	State   WebProfileState
}

// WebProfileIdentityChanged invalidates old completion markers only when two
// explicit upstream user IDs conflict. Material rotation and legacy unknown
// identity retain the established import behavior; absence is not a new user.
func WebProfileIdentityChanged(previousUserID, nextUserID string) bool {
	previousUserID, nextUserID = strings.TrimSpace(previousUserID), strings.TrimSpace(nextUserID)
	return previousUserID != "" && nextUserID != "" && previousUserID != nextUserID
}

func TransitionWebProfile(current WebProfileState, observed CredentialRef, event WebProfileObservation) (WebProfileResult, error) {
	result := WebProfileResult{State: current}
	if current.Material != observed {
		return result, nil
	}
	if current.Material.Provider != ProviderWeb {
		return result, errors.New("Web profile observation requires a Web account")
	}
	if event.OccurredAt.IsZero() {
		return result, errors.New("Web profile observation requires a completion time")
	}
	at := event.OccurredAt.UTC()
	switch event.Kind {
	case WebProfileTermsAccepted:
		if event.TermsVersion <= 0 {
			return result, errors.New("Web terms observation requires a positive version")
		}
		if current.TermsAcceptedVersion < event.TermsVersion || current.TermsAcceptedAt == nil && current.TermsAcceptedVersion == event.TermsVersion {
			result.State.TermsAcceptedAt, result.State.TermsAcceptedVersion = &at, event.TermsVersion
			result.Changed = true
		}
	case WebProfileBirthDateSet:
		if current.BirthDateSetAt == nil {
			result.State.BirthDateSetAt, result.Changed = &at, true
		}
	case WebProfileNSFWEnabled:
		if current.NSFWEnabledAt == nil {
			result.State.NSFWEnabledAt, result.Changed = &at, true
		}
	default:
		return result, errors.New("unsupported Web profile observation")
	}
	result.Applied = true
	return result, nil
}
