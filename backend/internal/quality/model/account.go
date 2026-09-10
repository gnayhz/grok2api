package model

// AccountState is the quality-axis state of a Build account. Transport health
// (quota, credentials, and concurrency) belongs to the base account system.
type AccountState string

const (
	// AccountActive participates in normal scheduling.
	AccountActive AccountState = "active"
	// AccountRemanded is the finite investigation hold after a degradation.
	AccountRemanded AccountState = "remanded"
	// AccountSentenced is the terminal account verdict and is never scheduled.
	AccountSentenced AccountState = "sentenced"
)

// Schedulable reports whether the quality layer permits scheduling.
func (s AccountState) Schedulable() bool { return s == AccountActive }

// accountTransitionLegal is the complete direct-loop transition matrix.
var accountTransitionLegal = map[AccountState]map[AccountState]struct{}{
	AccountRemanded: {
		AccountActive: {},
	},
	AccountSentenced: {
		AccountRemanded: {},
	},
	AccountActive: {
		AccountRemanded: {},
	},
}

// CanTransitionAccount reports whether a direct-loop transition is valid.
func CanTransitionAccount(from, to AccountState) bool {
	if from == to {
		return from == AccountActive
	}
	sources, ok := accountTransitionLegal[to]
	if !ok {
		return false
	}
	_, ok = sources[from]
	return ok
}
