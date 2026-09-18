package gateway

import "time"

// This is only a lower bound for starting a retry, not a replacement for the
// configured phase deadlines. First attempts and accepted streams are unchanged.
const minimumRetryAdmissionBudget = 250 * time.Millisecond

func hasRetryAdmissionBudget(a *admission) bool {
	remaining, bounded := a.Remaining()
	return !bounded || remaining >= minimumRetryAdmissionBudget
}
