package account

import (
	"errors"
	"strings"
)

// IdentityObservation belongs to the exact material used to ask a Provider.
// Empty fields preserve current metadata; unrelated account state is untouched.
type IdentityObservation struct {
	Email  string
	UserID string
	TeamID string
}

// IdentityState is a secret-free projection read under the account lock.
type IdentityState struct {
	Material CredentialRef
	Identity IdentityObservation
}

type IdentityResult struct {
	Applied bool
	State   IdentityState
}

// TransitionIdentity applies a successful identity observation only to the
// material that produced it. A replacement may reuse the same account ID.
func TransitionIdentity(current IdentityState, observed CredentialRef, identity IdentityObservation) (IdentityResult, error) {
	result := IdentityResult{State: current}
	if current.Material != observed {
		return result, nil
	}
	if current.Material.Provider != ProviderWeb && current.Material.Provider != ProviderConsole {
		return result, errors.New("identity observation requires a Web or Console account")
	}
	if len(identity.Email) > 255 || len(identity.UserID) > 255 || len(identity.TeamID) > 255 {
		return result, errors.New("account identity fields exceed the supported length")
	}
	if email := strings.TrimSpace(identity.Email); email != "" {
		result.State.Identity.Email = email
	}
	if userID := strings.TrimSpace(identity.UserID); userID != "" {
		result.State.Identity.UserID = userID
	}
	if teamID := strings.TrimSpace(identity.TeamID); teamID != "" {
		result.State.Identity.TeamID = teamID
	}
	result.Applied = true
	return result, nil
}
