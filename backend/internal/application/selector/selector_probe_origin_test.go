package selector

import (
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// probeSession builds a minimal session: candidate 0 (ID 7) entered via the
// quota-probe lane, candidate 1 (ID 8) via the normal lane.
func probeSession() *selectionSession {
	return &selectionSession{
		values: []accountdomain.RoutingCandidate{
			{Credential: accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}},
			{Credential: accountdomain.Credential{ID: 8, Provider: accountdomain.ProviderBuild}},
		},
		normalCandidates: []int{1},
		probeCandidates:  []int{0},
	}
}

// TestProbeOriginExcludedFromSameAccountRetry locks the session-level contract:
// an account that entered via the quota-probe lane must never be re-queued by
// RetryAccount, even after paid-probe promotion flips lease.QuotaProbe to false.
func TestProbeOriginExcludedFromSameAccountRetry(t *testing.T) {
	t.Parallel()
	session := probeSession()

	// RetryAccount re-queues only normal candidates: the probe account cannot
	// masquerade as a same-account retry target.
	session.RetryAccount(7)
	if session.retryAccountID == 7 {
		t.Fatal("RetryAccount must never re-queue a probe-lane account")
	}
	session.RetryAccount(8)
	if session.retryAccountID != 8 {
		t.Fatal("RetryAccount must re-queue a normal-lane account")
	}

	// Zero ID is a no-op.
	session.RetryAccount(0)
	if session.retryAccountID != 8 {
		t.Fatal("RetryAccount(0) must be a no-op")
	}
}
